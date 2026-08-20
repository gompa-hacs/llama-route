package router

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/shared"
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
	pool, err := NewPool(cfg, logger, logger)
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

func TestPool_StickyYieldsWhenBusy(t *testing.T) {
	var hits [2]atomic.Int32
	block0 := make(chan struct{})
	started0 := make(chan struct{})
	srv0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[0].Add(1)
		select {
		case <-started0:
		default:
			close(started0)
		}
		<-block0
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
	pool, err := NewPool(cfg, logmon.NewWriter(io.Discard), logmon.NewWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown(0)

	body := `{"model":"pooled","user":"alice"}`
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		pool.ServeHTTP(w, r)
	}()
	<-started0

	// Same sticky key while home is busy → overflow to idle backend.
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
	r2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	pool.ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("overflow status=%d body=%s", w2.Code, w2.Body.String())
	}
	if hits[1].Load() != 1 {
		t.Fatalf("expected overflow to backend1, hits=%d/%d", hits[0].Load(), hits[1].Load())
	}

	close(block0)
	<-done

	// After home is idle again, sticky should return to backend0.
	r3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
	r3.Header.Set("Content-Type", "application/json")
	w3 := httptest.NewRecorder()
	pool.ServeHTTP(w3, r3)
	if w3.Code != http.StatusOK {
		t.Fatalf("return status=%d", w3.Code)
	}
	if hits[0].Load() != 2 {
		t.Fatalf("expected sticky return to backend0, hits=%d/%d", hits[0].Load(), hits[1].Load())
	}
}

func TestPool_WhisperSpreadsSameAPIKey(t *testing.T) {
	var hits [2]atomic.Int32
	block0 := make(chan struct{})
	started0 := make(chan struct{})
	srv0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[0].Add(1)
		close(started0)
		<-block0
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
			"whisper": {
				Pool: &config.PoolConfig{
					Backends: []config.PoolBackend{
						{Proxy: srv0.URL, ProxyURL: u0, WhisperCompat: true},
						{Proxy: srv1.URL, ProxyURL: u1, WhisperCompat: true},
					},
				},
			},
		},
	}
	pool, err := NewPool(cfg, logmon.NewWriter(io.Discard), logmon.NewWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown(0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader("file=x&model=whisper"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Authorization", "Bearer sk-shared")
		w := httptest.NewRecorder()
		pool.ServeHTTP(w, r)
	}()

	<-started0

	r2 := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader("file=y&model=whisper"))
	r2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r2.Header.Set("Authorization", "Bearer sk-shared")
	w2 := httptest.NewRecorder()
	pool.ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("second request status=%d body=%s", w2.Code, w2.Body.String())
	}
	if hits[1].Load() != 1 {
		t.Fatalf("expected second request on idle backend1, hits=%d/%d", hits[0].Load(), hits[1].Load())
	}

	close(block0)
	<-done
}

func TestExtractAffinityKey_EmptyRulesDisableSticky(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader("model=w"))
	r.Header.Set("Authorization", "Bearer sk-x")
	if key := ExtractAffinityKey(r, []config.AffinityRule{}); key != "" {
		t.Fatalf("empty rules should disable sticky, got %q", key)
	}
}

func TestPool_Stats(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer blocking.Close()

	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv1.Close()

	bu, _ := url.Parse(blocking.URL)
	u1, _ := url.Parse(srv1.URL)
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"pooled": {
				Aliases: []string{"pooled-alias"},
				Pool: &config.PoolConfig{
					Backends: []config.PoolBackend{
						{Proxy: blocking.URL, ProxyURL: bu, ContextSize: 4096},
						{Proxy: srv1.URL, ProxyURL: u1, ContextSize: 8192},
					},
				},
			},
		},
	}
	pool, err := NewPool(cfg, logmon.NewWriter(io.Discard), logmon.NewWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown(0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		body := `{"model":"pooled","user":"bob"}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		pool.ServeHTTP(w, r)
	}()

	<-started
	snap := pool.Stats()
	if len(snap.Models) != 1 {
		t.Fatalf("models=%d want 1 (alias must not duplicate)", len(snap.Models))
	}
	m := snap.Models[0]
	if m.ModelID != "pooled" {
		t.Fatalf("model_id=%q", m.ModelID)
	}
	if len(m.Backends) != 2 {
		t.Fatalf("backends=%d", len(m.Backends))
	}
	if m.Backends[0].Inflight != 1 {
		t.Fatalf("backend0 inflight=%d want 1", m.Backends[0].Inflight)
	}
	if m.Backends[0].ContextSize != 4096 || m.Backends[1].ContextSize != 8192 {
		t.Fatalf("context sizes: %+v", m.Backends)
	}
	if m.AffinitySessions != 1 || m.Backends[0].AffinitySessions != 1 {
		t.Fatalf("affinity: model=%d backend0=%d", m.AffinitySessions, m.Backends[0].AffinitySessions)
	}

	close(release)
	<-done

	snap = pool.Stats()
	if snap.Models[0].Backends[0].Inflight != 0 {
		t.Fatalf("after done inflight=%d", snap.Models[0].Backends[0].Inflight)
	}
}

func TestPool_TagsBackendMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"pooled": {
				Pool: &config.PoolConfig{
					Backends: []config.PoolBackend{
						{Proxy: srv.URL, ProxyURL: u},
					},
				},
			},
		},
	}
	pool, err := NewPool(cfg, logmon.NewWriter(io.Discard), logmon.NewWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}

	body := `{"model":"pooled"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	pool.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}

	ctxData, ok := shared.ReadContext(r.Context())
	if !ok {
		t.Fatal("missing request context")
	}
	if got := ctxData.Metadata["pool_backend_id"]; got != "0" {
		t.Fatalf("pool_backend_id=%q want 0", got)
	}
	if got := ctxData.Metadata["pool_backend"]; got != srv.URL {
		t.Fatalf("pool_backend=%q want %q", got, srv.URL)
	}
}

func TestPool_SpawnBackends(t *testing.T) {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	path := filepath.Join("..", "..", "build", fmt.Sprintf("simple-responder_%s_%s", goos, goarch))
	if goos == "windows" {
		path = filepath.Join("..", "..", "build", "simple-responder.exe")
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skipf("simple-responder not found at %s", path)
	}
	cmdPath := filepath.ToSlash(path)

	yaml := fmt.Sprintf(`
healthCheckTimeout: 30
startPort: 19100
models:
  pooled:
    pool:
      start: on_demand
      restartOnCrash: false
      backends:
        - cmd: %s -port ${PORT} -silent
        - cmd: %s -port ${PORT} -silent
`, cmdPath, cmdPath)

	cfg, err := config.LoadConfigFromReader(strings.NewReader(yaml))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(cfg, logmon.NewWriter(io.Discard), logmon.NewWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown(5 * time.Second)

	for i, user := range []string{"alice", "bob", "carol"} {
		body := fmt.Sprintf(`{"model":"pooled","user":%q}`, user)
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		pool.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("req %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}

	snap := pool.Stats()
	if len(snap.Models) != 1 || len(snap.Models[0].Backends) != 2 {
		t.Fatalf("stats=%+v", snap)
	}
	ready := 0
	for _, b := range snap.Models[0].Backends {
		if b.Kind != "spawn" {
			t.Fatalf("kind=%q", b.Kind)
		}
		if b.State == "ready" {
			ready++
		}
	}
	if ready < 1 {
		t.Fatalf("expected at least one ready spawn backend, got %+v", snap.Models[0].Backends)
	}
}
