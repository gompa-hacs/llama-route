package discovery_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/discovery"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/router"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

func TestDiscovery_E2E_AutoPoolAndRoute(t *testing.T) {
	var hitsA, hitsB atomic.Int32

	peerA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "qwen", "context_length": 8192}},
			})
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			hitsA.Add(1)
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"model":"qwen"`) {
				t.Errorf("peer A got body %s", body)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":"a"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer peerA.Close()

	peerB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "qwen", "context_length": 8192}},
			})
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			hitsB.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":"b"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer peerB.Close()

	urlA, _ := url.Parse(peerA.URL)
	urlB, _ := url.Parse(peerB.URL)
	cfg := config.Config{
		Peers: config.PeerDictionaryConfig{
			"A": {Proxy: peerA.URL, ProxyURL: urlA},
			"B": {Proxy: peerB.URL, ProxyURL: urlB},
		},
	}

	logger := logmon.NewWriter(io.Discard)
	pool, err := router.NewPool(cfg, logger, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown(0)
	peer, err := router.NewPeer(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Shutdown(0)

	mgr := discovery.NewManager(cfg, logger, pool, peer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	defer mgr.Stop()

	if !pool.Handles("qwen") {
		t.Fatal("expected bare qwen auto-pool after Start")
	}
	if !pool.Handles("A/qwen") || !pool.Handles("B/qwen") {
		t.Fatal("expected FQ pool aliases")
	}
	if peer.Handles("qwen") {
		t.Fatal("pooled model should not also be a peer route")
	}
	if len(mgr.Listings()) != 1 || mgr.Listings()[0].ID != "qwen" {
		t.Fatalf("listings=%v want only bare qwen", mgr.Listings())
	}

	body := strings.NewReader(`{"model":"qwen","messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	*req = *req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{
		Model: "qwen", ModelID: "qwen", Metadata: map[string]string{},
	}))
	w := httptest.NewRecorder()
	pool.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if hitsA.Load()+hitsB.Load() != 1 {
		t.Fatalf("hits A=%d B=%d", hitsA.Load(), hitsB.Load())
	}

	body = strings.NewReader(`{"model":"A/qwen","messages":[]}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	*req = *req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{
		Model: "A/qwen", ModelID: "A/qwen", Metadata: map[string]string{},
	}))
	w = httptest.NewRecorder()
	pool.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("FQ status=%d body=%s", w.Code, w.Body.String())
	}
	if hitsA.Load()+hitsB.Load() != 2 {
		t.Fatalf("after FQ hits A=%d B=%d", hitsA.Load(), hitsB.Load())
	}
}
