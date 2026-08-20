package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

func TestServer_SanitizeAccessControlRequestHeaders(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Content-Type, Authorization", "Content-Type, Authorization"},
		{"  X-Custom ,  Accept ", "X-Custom, Accept"},
		{"Valid, Bad Header", "Valid"},
		{"Bad@Header", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitizeAccessControlRequestHeaderValues(c.in); got != c.want {
			t.Errorf("sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestServer_IsTokenChar(t *testing.T) {
	for _, r := range "abcXYZ0129!#$%&'*+-.^_`|~" {
		if !isTokenChar(r) {
			t.Errorf("isTokenChar(%q) = false, want true", r)
		}
	}
	for _, r := range " @()/\t\"" {
		if isTokenChar(r) {
			t.Errorf("isTokenChar(%q) = true, want false", r)
		}
	}
}

func TestServer_RequestContextMiddleware(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"llama3": {},
		},
	}

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mw := CreateRequestContextMiddleware(cfg)

	t.Run("known model passes through", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama3"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("missing model returns 404", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})
}

func TestServer_AuthMiddleware(t *testing.T) {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("no keys configured passes through", func(t *testing.T) {
		mgr, err := auth.NewManager(config.Config{})
		if err != nil {
			t.Fatal(err)
		}
		mw := CreateAuthMiddleware(mgr)
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	cfg := config.Config{RequiredAPIKeys: []string{"secret"}}
	mgr, err := auth.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("valid key", func(t *testing.T) {
		mw := CreateAuthMiddleware(mgr)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("invalid key", func(t *testing.T) {
		mw := CreateAuthMiddleware(mgr)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Error("missing WWW-Authenticate header")
		}
	})
}

func TestServer_KeyLimitsMiddleware(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"llama-70b": {},
			"whisper":   {},
		},
	}
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw := CreateKeyLimitsMiddleware(cfg)

	withPolicy := func(body string, policy auth.KeyPolicy) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(auth.ContextWithKeyPolicy(r.Context(), policy))
		// Seed request context as CreateRequestContextMiddleware would.
		if _, err := shared.FetchContext(r, cfg); err != nil {
			t.Fatalf("FetchContext: %v", err)
		}
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		return w
	}

	t.Run("allowed model passes", func(t *testing.T) {
		w := withPolicy(`{"model":"llama-70b","max_tokens":100}`, auth.KeyPolicy{
			Models:    []string{"llama-70b"},
			MaxTokens: 2048,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("disallowed model forbidden", func(t *testing.T) {
		w := withPolicy(`{"model":"whisper"}`, auth.KeyPolicy{Models: []string{"llama-70b"}})
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})

	t.Run("max_tokens over limit forbidden", func(t *testing.T) {
		w := withPolicy(`{"model":"llama-70b","max_tokens":9000}`, auth.KeyPolicy{MaxTokens: 2048})
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("max_completion_tokens over limit forbidden", func(t *testing.T) {
		w := withPolicy(`{"model":"llama-70b","max_completion_tokens":5000}`, auth.KeyPolicy{MaxTokens: 100})
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})
}

func TestServer_RequestedTokensExceed(t *testing.T) {
	ok, field, n := requestedTokensExceed([]byte(`{"max_tokens":100}`), 50)
	if !ok || field != "max_tokens" || n != 100 {
		t.Fatalf("got %v %q %d", ok, field, n)
	}
	ok, _, _ = requestedTokensExceed([]byte(`{"max_tokens":50}`), 50)
	if ok {
		t.Fatal("equal should not exceed")
	}
}

func TestServer_AdminAuthMiddleware_SessionOnly(t *testing.T) {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	cfg := config.Config{
		RequiredAPIKeys: []string{"infer-key"},
		Admin:           config.AdminConfig{Password: "admin-pass"},
	}
	mgr, err := auth.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mw := CreateAdminAuthMiddleware(mgr)

	t.Run("inference key alone is rejected", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
		r.Header.Set("Authorization", "Bearer infer-key")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	})

	t.Run("session cookie is accepted", func(t *testing.T) {
		token, _, err := mgr.Login("admin-pass")
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})

	t.Run("no basic auth challenge", func(t *testing.T) {
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/metrics", nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if got := w.Header().Get("WWW-Authenticate"); got != "" {
			t.Fatalf("WWW-Authenticate = %q, want empty (browser Basic dialog)", got)
		}
	})
}

func TestServer_UI_PublicWhenAdminRequired(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""))
	mgr, err := auth.NewManager(config.Config{Admin: config.AdminConfig{Password: "pw"}})
	if err != nil {
		t.Fatal(err)
	}
	s.auth = mgr
	s.routes()

	ui := httptest.NewRecorder()
	s.ServeHTTP(ui, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	if ui.Code == http.StatusUnauthorized {
		t.Fatal("GET /ui/ must stay public so the login form can load")
	}

	running := httptest.NewRecorder()
	s.ServeHTTP(running, httptest.NewRequest(http.MethodGet, "/running", nil))
	if running.Code != http.StatusUnauthorized {
		t.Fatalf("GET /running status = %d, want 401", running.Code)
	}
}

func TestServer_StripClientAuthMiddleware(t *testing.T) {
	var sawAuth, sawKey string
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	})
	mw := CreateStripClientAuthMiddleware()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("X-Api-Key", "secret")
	mw(final).ServeHTTP(httptest.NewRecorder(), r)
	if sawAuth != "" || sawKey != "" {
		t.Fatalf("upstream still saw auth headers: Authorization=%q X-Api-Key=%q", sawAuth, sawKey)
	}
}

func TestServer_AuthLogin_SecureCookieBehindProxy(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""))
	mgr, err := auth.NewManager(config.Config{Admin: config.AdminConfig{Password: "pw"}})
	if err != nil {
		t.Fatal(err)
	}
	s.auth = mgr

	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"pw"}`))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected session cookie")
	}
	if !cookies[0].Secure {
		t.Fatal("expected Secure cookie when X-Forwarded-Proto=https from trusted proxy")
	}
}
