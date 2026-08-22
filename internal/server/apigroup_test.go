package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/peermetrics"
	"github.com/mostlygeek/llama-swap/internal/perf"
)

func TestServer_InflightMiddleware(t *testing.T) {
	c := &inflightCounter{}
	mw := CreateInflightMiddleware(c)

	var duringRequest int64
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		duringRequest = c.Current()
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if duringRequest != 1 {
		t.Errorf("counter during request = %d, want 1", duringRequest)
	}
	if got := c.Current(); got != 0 {
		t.Errorf("counter after request = %d, want 0", got)
	}
}

func TestServer_APIVersion(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""))
	s.build = BuildInfo{Version: "1.2.3", Commit: "deadbeef", Date: "2026-05-19"}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/version", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["version"] != "1.2.3" || got["commit"] != "deadbeef" || got["build_date"] != "2026-05-19" {
		t.Errorf("body = %v", got)
	}
}

func TestServer_APIMetrics_Empty(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/metrics", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if body := strings.TrimSpace(w.Body.String()); body != "[]" {
		t.Errorf("body = %q, want []", body)
	}
}

func TestServer_APIPoolMetrics_Empty(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/pool-metrics", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got struct {
		Models []any `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Models == nil {
		t.Fatal("models should be a non-nil slice")
	}
	if len(got.Models) != 0 {
		t.Errorf("models = %v, want empty", got.Models)
	}
}

func TestServer_APIPerformance_Unavailable(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/performance", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestServer_APIEvents_InitialPayload(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.ServeHTTP(w, req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}

	body := w.Body.String()
	for _, want := range []string{`"type":"modelStatus"`, `"type":"inflight"`, `"type":"logData"`, `"type":"poolMetrics"`} {
		if !strings.Contains(body, want) {
			t.Errorf("initial SSE payload missing %s; body=%q", want, body)
		}
	}
}

func TestServer_APIEvents_PeerPerformanceInitial(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/metrics":
			w.Write([]byte("[]"))
		case "/api/performance":
			json.NewEncoder(w).Encode(map[string]any{
				"sys_stats": []perf.SysStat{{Timestamp: t0}},
				"gpu_stats": []perf.GpuStat{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer peerSrv.Close()

	u, err := url.Parse(peerSrv.URL)
	if err != nil {
		t.Fatalf("parse peer URL: %v", err)
	}

	f := peermetrics.NewFetcher(config.PeerDictionaryConfig{
		"peer1": {Proxy: peerSrv.URL, ProxyURL: u},
	}, peermetrics.FetcherConfig{
		Interval: time.Hour,
		Timeout:  2 * time.Second,
	}, logmon.NewWriter(io.Discard))

	pollCtx, pollCancel := context.WithCancel(context.Background())
	f.Start(pollCtx)
	time.Sleep(100 * time.Millisecond)
	pollCancel()
	f.Stop()

	s := newTestServer(newStubRouter(nil, ""))
	s.peerMetrics = f

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.ServeHTTP(w, req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}

	if !strings.Contains(w.Body.String(), `"type":"peerPerformance"`) {
		t.Errorf("initial SSE payload missing peerPerformance; body=%q", w.Body.String())
	}
}

func TestServer_APIEvents_PeerPerformanceUpdate(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.ServeHTTP(w, req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)

	event.Emit(peermetrics.PeerPerformanceUpdateEvent{
		Snapshot: peermetrics.LatestPeerMetrics{
			PollTime: time.Now(),
			Peers: map[string]peermetrics.PeerSnapshot{
				"peer1": {PeerName: "peer1", Success: true},
			},
		},
	})

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}

	if !strings.Contains(w.Body.String(), `"type":"peerPerformance"`) {
		t.Errorf("SSE payload missing peerPerformance update; body=%q", w.Body.String())
	}
}
