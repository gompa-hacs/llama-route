package peermetrics

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/perf"
)

func testPeerFetcher(t *testing.T, handler http.HandlerFunc) *Fetcher {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse peer URL: %v", err)
	}

	peers := config.PeerDictionaryConfig{
		"peer1": {
			Proxy:    srv.URL,
			ProxyURL: u,
		},
	}
	return NewFetcher(peers, FetcherConfig{
		Interval: time.Hour,
		Timeout:  2 * time.Second,
	}, logmon.NewWriter(io.Discard))
}

func TestFetcher_IncrementalPerformance(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Second)
	t2 := t0.Add(10 * time.Second)

	var perfCalls atomic.Int32

	f := testPeerFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/metrics":
			w.Write([]byte("[]"))
		case "/api/performance":
			call := int(perfCalls.Add(1))
			after := r.URL.Query().Get("after")
			switch call {
			case 1:
				if after != "" {
					t.Errorf("bootstrap request sent after=%q", after)
				}
				json.NewEncoder(w).Encode(map[string]any{
					"sys_stats": []perf.SysStat{
						{Timestamp: t0},
						{Timestamp: t1},
					},
					"gpu_stats": []perf.GpuStat{},
				})
			case 2:
				if after != t1.Format(time.RFC3339) {
					t.Errorf("incremental after = %q, want %q", after, t1.Format(time.RFC3339))
				}
				json.NewEncoder(w).Encode(map[string]any{
					"sys_stats": []perf.SysStat{{Timestamp: t2}},
					"gpu_stats": []perf.GpuStat{},
				})
			default:
				t.Errorf("unexpected performance request #%d", call)
				http.Error(w, "unexpected call", http.StatusInternalServerError)
			}
		default:
			http.NotFound(w, r)
		}
	})

	f.pollAll()

	latest := f.GetLatest()
	sys := latest.Peers["peer1"].SysStats
	if len(sys) != 2 {
		t.Fatalf("bootstrap sys stats = %d, want 2", len(sys))
	}

	f.pollAll()

	latest = f.GetLatest()
	sys = latest.Peers["peer1"].SysStats
	if len(sys) != 3 {
		t.Fatalf("merged sys stats = %d, want 3", len(sys))
	}

	seen := make(map[time.Time]bool, len(sys))
	for _, st := range sys {
		if seen[st.Timestamp] {
			t.Errorf("duplicate timestamp %v", st.Timestamp)
		}
		seen[st.Timestamp] = true
	}

	if got := int(perfCalls.Load()); got != 2 {
		t.Errorf("performance calls = %d, want 2", got)
	}
}

func TestFetcher_PerfFailureKeepsWatermark(t *testing.T) {
	t0 := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Second)

	var perfCalls atomic.Int32
	var lastAfter atomic.Value

	f := testPeerFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/metrics":
			w.Write([]byte("[]"))
		case "/api/performance":
			call := int(perfCalls.Add(1))
			lastAfter.Store(r.URL.Query().Get("after"))
			switch call {
			case 1:
				json.NewEncoder(w).Encode(map[string]any{
					"sys_stats": []perf.SysStat{{Timestamp: t0}},
					"gpu_stats": []perf.GpuStat{},
				})
			case 2:
				http.Error(w, "peer unavailable", http.StatusInternalServerError)
			case 3:
				json.NewEncoder(w).Encode(map[string]any{
					"sys_stats": []perf.SysStat{{Timestamp: t1}},
					"gpu_stats": []perf.GpuStat{},
				})
			default:
				t.Errorf("unexpected performance request #%d", call)
				http.Error(w, "unexpected call", http.StatusInternalServerError)
			}
		default:
			http.NotFound(w, r)
		}
	})

	f.pollAll()

	latest := f.GetLatest()
	if len(latest.Peers["peer1"].SysStats) != 1 {
		t.Fatalf("bootstrap sys stats = %d, want 1", len(latest.Peers["peer1"].SysStats))
	}

	f.pollAll()

	latest = f.GetLatest()
	if len(latest.Peers["peer1"].SysStats) != 1 {
		t.Fatalf("after failed poll sys stats = %d, want 1", len(latest.Peers["peer1"].SysStats))
	}
	if latest.Peers["peer1"].Success {
		t.Fatal("expected snapshot success=false after performance failure")
	}

	f.pollAll()

	if got, _ := lastAfter.Load().(string); got != t0.Format(time.RFC3339) {
		t.Errorf("watermark after recovery = %q, want %q", got, t0.Format(time.RFC3339))
	}

	latest = f.GetLatest()
	if len(latest.Peers["peer1"].SysStats) != 2 {
		t.Fatalf("after recovery sys stats = %d, want 2", len(latest.Peers["peer1"].SysStats))
	}
}
