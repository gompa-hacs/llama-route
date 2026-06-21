package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

func TestExtractAffinityKey_PromptCacheKey(t *testing.T) {
	body := `{"model":"m","prompt_cache_key":"conv-1"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
	key := ExtractAffinityKey(r, nil)
	if key == "" {
		t.Fatal("expected affinity key")
	}

	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
	key2 := ExtractAffinityKey(r2, nil)
	if key != key2 {
		t.Fatalf("same prompt_cache_key should produce same affinity: %q vs %q", key, key2)
	}
}

func TestPool_StickyLeastInflight(t *testing.T) {
	var hits [2]atomic.Int32
	srv0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[0].Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv0.Close()
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[1].Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv1.Close()

	u0, _ := url.Parse(srv0.URL)
	u1, _ := url.Parse(srv1.URL)

	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"pooled": {
				Pool: &config.PoolConfig{
					Backends: []config.PoolBackend{
						{Proxy: srv0.URL, ProxyURL: u0},
						{Proxy: srv1.URL, ProxyURL: u1},
					},
				},
			},
		},
	}
	logger := logmon.NewWriter(io.Discard)
	pool, err := NewPool(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"model":"pooled","user":"alice"}`
	for range 3 {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		pool.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}

	if hits[0].Load() == 0 && hits[1].Load() == 0 {
		t.Fatal("expected requests to hit a backend")
	}
	if hits[0].Load() > 0 && hits[1].Load() > 0 {
		t.Fatalf("sticky user should stay on one backend: %d vs %d", hits[0].Load(), hits[1].Load())
	}
}
