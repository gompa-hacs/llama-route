package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/discovery"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

var testLogger = logmon.NewWriter(os.Stdout)

func init() {
	testLogger.SetLogLevel(logmon.LevelWarn)
}

func mustPeer(t *testing.T, peers config.PeerDictionaryConfig, routes map[string]discovery.PeerRoute) *Peer {
	t.Helper()
	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	if routes != nil {
		if err := pr.ReplaceDiscovered(routes); err != nil {
			t.Fatal(err)
		}
	}
	return pr
}

func makePeerRoute(peerID, model, apiKey string) discovery.PeerRoute {
	return discovery.PeerRoute{
		PeerID:          peerID,
		UpstreamModelID: model,
		ApiKey:          apiKey,
	}
}

func TestNewPeer_EmptyPeers(t *testing.T) {
	pr, err := NewPeer(config.Config{}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.members) != 0 {
		t.Fatalf("expected empty members, got %d", len(pr.members))
	}
	if len(pr.routes) != 0 {
		t.Fatalf("expected empty routes, got %d", len(pr.routes))
	}
}

func TestNewPeer_MembersWithoutRoutes(t *testing.T) {
	proxyURL, _ := url.Parse("http://peer1.example.com:8080")
	peers := config.PeerDictionaryConfig{
		"peer1": config.PeerConfig{
			Proxy:    "http://peer1.example.com:8080",
			ProxyURL: proxyURL,
		},
	}
	pr := mustPeer(t, peers, nil)
	if len(pr.members) != 1 {
		t.Fatalf("members=%d", len(pr.members))
	}
	if pr.Handles("model-a") {
		t.Fatal("routes should be empty until ReplaceDiscovered")
	}
}

func TestPeer_ReplaceDiscovered(t *testing.T) {
	proxyURL1, _ := url.Parse("http://peer1.example.com:8080")
	proxyURL2, _ := url.Parse("http://peer2.example.com:8080")
	peers := config.PeerDictionaryConfig{
		"peer1": {Proxy: "http://peer1.example.com:8080", ProxyURL: proxyURL1},
		"peer2": {Proxy: "http://peer2.example.com:8080", ProxyURL: proxyURL2},
	}
	pr := mustPeer(t, peers, map[string]discovery.PeerRoute{
		"model-a":       makePeerRoute("peer1", "model-a", ""),
		"peer1/model-a": makePeerRoute("peer1", "model-a", ""),
		"model-c":       makePeerRoute("peer2", "model-c", ""),
	})
	for _, m := range []string{"model-a", "peer1/model-a", "model-c"} {
		if !pr.Handles(m) {
			t.Errorf("expected %s", m)
		}
	}
}

func TestPeer_ServeHTTP_Success(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("response from peer"))
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	pr := mustPeer(t, config.PeerDictionaryConfig{
		"peer1": {Proxy: testServer.URL, ProxyURL: proxyURL},
	}, map[string]discovery.PeerRoute{
		"test-model": makePeerRoute("peer1", "test-model", ""),
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{Model: "test-model", ModelID: "test-model"}))
	w := httptest.NewRecorder()
	pr.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "response from peer" {
		t.Errorf("got %q", w.Body.String())
	}
}

func TestPeer_ServeHTTP_FQRewrite(t *testing.T) {
	var gotBody string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	pr := mustPeer(t, config.PeerDictionaryConfig{
		"peer1": {Proxy: testServer.URL, ProxyURL: proxyURL},
	}, map[string]discovery.PeerRoute{
		"peer1/qwen": makePeerRoute("peer1", "qwen", ""),
	})

	body := strings.NewReader(`{"model":"peer1/qwen","prompt":"hi"}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	*req = *req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{Model: "peer1/qwen", ModelID: "peer1/qwen"}))
	w := httptest.NewRecorder()
	pr.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if !strings.Contains(gotBody, `"model":"qwen"`) {
		t.Fatalf("expected upstream model rewrite, got %s", gotBody)
	}
}

func TestPeer_ServeHTTP_ModelNotFoundInContext(t *testing.T) {
	pr, err := NewPeer(config.Config{}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	pr.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPeer_ServeHTTP_PeerModelNotFound(t *testing.T) {
	pr, err := NewPeer(config.Config{}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{Model: "nonexistent-model", ModelID: "nonexistent-model"}))
	w := httptest.NewRecorder()
	pr.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPeer_ServeHTTP_ApiKeyInjection(t *testing.T) {
	var receivedAuthHeader string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	pr := mustPeer(t, config.PeerDictionaryConfig{
		"peer1": {Proxy: testServer.URL, ProxyURL: proxyURL},
	}, map[string]discovery.PeerRoute{
		"test-model": makePeerRoute("peer1", "test-model", "secret-api-key"),
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{Model: "test-model", ModelID: "test-model"}))
	w := httptest.NewRecorder()
	pr.ServeHTTP(w, req)

	if receivedAuthHeader != "Bearer secret-api-key" {
		t.Errorf("got %q", receivedAuthHeader)
	}
}

func TestPeer_ServeHTTP_NoApiKey(t *testing.T) {
	var receivedAuthHeader string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	pr := mustPeer(t, config.PeerDictionaryConfig{
		"peer1": {Proxy: testServer.URL, ProxyURL: proxyURL},
	}, map[string]discovery.PeerRoute{
		"test-model": makePeerRoute("peer1", "test-model", ""),
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{Model: "test-model", ModelID: "test-model"}))
	w := httptest.NewRecorder()
	pr.ServeHTTP(w, req)

	if receivedAuthHeader != "" {
		t.Errorf("expected no auth header, got %q", receivedAuthHeader)
	}
}

func TestPeer_ServeHTTP_HostHeaderSet(t *testing.T) {
	var receivedHost string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	pr := mustPeer(t, config.PeerDictionaryConfig{
		"peer1": {Proxy: testServer.URL, ProxyURL: proxyURL},
	}, map[string]discovery.PeerRoute{
		"test-model": makePeerRoute("peer1", "test-model", ""),
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{Model: "test-model", ModelID: "test-model"}))
	w := httptest.NewRecorder()
	pr.ServeHTTP(w, req)

	if !strings.HasPrefix(receivedHost, "127.0.0.1:") {
		t.Errorf("got %q", receivedHost)
	}
}

func TestPeer_ServeHTTP_SSEHeaderModification(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	pr := mustPeer(t, config.PeerDictionaryConfig{
		"peer1": {Proxy: testServer.URL, ProxyURL: proxyURL},
	}, map[string]discovery.PeerRoute{
		"test-model": makePeerRoute("peer1", "test-model", ""),
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{Model: "test-model", ModelID: "test-model"}))
	w := httptest.NewRecorder()
	pr.ServeHTTP(w, req)

	if w.Header().Get("X-Accel-Buffering") != "no" {
		t.Errorf("got %q", w.Header().Get("X-Accel-Buffering"))
	}
}

func TestPeer_ServeHTTP_ShutdownRejectsNewRequests(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	pr := mustPeer(t, config.PeerDictionaryConfig{
		"peer1": {Proxy: testServer.URL, ProxyURL: proxyURL},
	}, map[string]discovery.PeerRoute{
		"test-model": makePeerRoute("peer1", "test-model", ""),
	})

	if err := pr.Shutdown(0); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{Model: "test-model", ModelID: "test-model"}))
	w := httptest.NewRecorder()
	pr.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "shutting down") {
		t.Errorf("got %q", w.Body.String())
	}
}

func TestPeer_ServeHTTP_WaitsForInflightDuringShutdown(t *testing.T) {
	started := make(chan struct{})
	released := make(chan struct{})
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-released
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	pr := mustPeer(t, config.PeerDictionaryConfig{
		"peer1": {Proxy: testServer.URL, ProxyURL: proxyURL},
	}, map[string]discovery.PeerRoute{
		"test-model": makePeerRoute("peer1", "test-model", ""),
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{Model: "test-model", ModelID: "test-model"}))

	var wg sync.WaitGroup
	wg.Go(func() {
		w := httptest.NewRecorder()
		pr.ServeHTTP(w, req)
	})

	<-started

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- pr.Shutdown(500 * time.Millisecond)
	}()

	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-shutdownDone:
		t.Errorf("shutdown completed before inflight finished: %v", err)
	default:
	}

	close(released)
	wg.Wait()

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Errorf("shutdown errored: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("shutdown did not complete")
	}
}

func TestPeer_ServeHTTP_ShutdownTimeoutCancelsInflight(t *testing.T) {
	started := make(chan struct{})
	released := make(chan struct{})
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-released
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	pr := mustPeer(t, config.PeerDictionaryConfig{
		"peer1": {Proxy: testServer.URL, ProxyURL: proxyURL},
	}, map[string]discovery.PeerRoute{
		"test-model": makePeerRoute("peer1", "test-model", ""),
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{Model: "test-model", ModelID: "test-model"}))

	var wg sync.WaitGroup
	wg.Go(func() {
		w := httptest.NewRecorder()
		pr.ServeHTTP(w, req)
	})

	<-started

	err := pr.Shutdown(100 * time.Millisecond)
	if err == nil {
		t.Error("expected timeout error from shutdown")
	}

	close(released)
	wg.Wait()
}

func TestPeer_ShutdownMultiple(t *testing.T) {
	pr, err := NewPeer(config.Config{}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	if err := pr.Shutdown(0); err != nil {
		t.Fatal(err)
	}
	err = pr.Shutdown(0)
	if err == nil {
		t.Error("expected error on second shutdown")
	}
	if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("got %q", err.Error())
	}
}

func TestPeer_ServeHTTP_ModelExtractedFromBody(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	pr := mustPeer(t, config.PeerDictionaryConfig{
		"peer1": {Proxy: testServer.URL, ProxyURL: proxyURL},
	}, map[string]discovery.PeerRoute{
		"extracted-model": makePeerRoute("peer1", "extracted-model", ""),
	})

	body := strings.NewReader(`{"model":"extracted-model","prompt":"hello"}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	pr.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPeer_ServeHTTP_ContextOverridesBodyModel(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	pr := mustPeer(t, config.PeerDictionaryConfig{
		"peer1": {Proxy: testServer.URL, ProxyURL: proxyURL},
		"peer2": {Proxy: testServer.URL, ProxyURL: proxyURL},
	}, map[string]discovery.PeerRoute{
		"context-model": makePeerRoute("peer1", "context-model", ""),
		"body-model":    makePeerRoute("peer2", "body-model", ""),
	})

	body := strings.NewReader(`{"model":"body-model","prompt":"hello"}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	*req = *req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{Model: "context-model", ModelID: "context-model"}))
	w := httptest.NewRecorder()
	pr.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestNewPeer_CustomTimeouts(t *testing.T) {
	proxyURL, _ := url.Parse("http://localhost:8080")
	peers := config.PeerDictionaryConfig{
		"test-peer": config.PeerConfig{
			Proxy:    "http://localhost:8080",
			ProxyURL: proxyURL,
			Timeouts: config.TimeoutsConfig{
				Connect:        45,
				ResponseHeader: 300,
				TLSHandshake:   15,
				ExpectContinue: 2,
				IdleConn:       120,
			},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	member, ok := pr.members["test-peer"]
	if !ok {
		t.Fatal("expected test-peer member")
	}

	transport, ok := member.reverseProxy.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected Transport to be *http.Transport")
	}

	if transport.ResponseHeaderTimeout != 300*time.Second {
		t.Errorf("ResponseHeaderTimeout=%v", transport.ResponseHeaderTimeout)
	}
	if transport.TLSHandshakeTimeout != 15*time.Second {
		t.Errorf("TLSHandshakeTimeout=%v", transport.TLSHandshakeTimeout)
	}
	if transport.ExpectContinueTimeout != 2*time.Second {
		t.Errorf("ExpectContinueTimeout=%v", transport.ExpectContinueTimeout)
	}
	if transport.IdleConnTimeout != 120*time.Second {
		t.Errorf("IdleConnTimeout=%v", transport.IdleConnTimeout)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Error("expected ForceAttemptHTTP2")
	}
}
