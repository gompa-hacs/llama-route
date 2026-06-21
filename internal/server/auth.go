package server

import (
	"net/http"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

// CreateInferenceAuthMiddleware validates inference API keys when any are
// configured (static YAML keys or UI-managed keys).
func CreateInferenceAuthMiddleware(m *auth.Manager) chain.Middleware {
	return func(next http.Handler) http.Handler {
		if m == nil || !m.InferenceRequired() {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m.ValidateInferenceKey(shared.ExtractAPIKey(r)) {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="llama-swap"`)
			shared.SendResponse(w, r, http.StatusUnauthorized, "unauthorized: invalid or missing API key")
		})
	}
}

// CreateAdminAuthMiddleware protects dashboard routes. When admin login is
// configured, a valid session cookie is required (API keys also work). When
// only inference keys are configured, those are required instead.
func CreateAdminAuthMiddleware(m *auth.Manager) chain.Middleware {
	return func(next http.Handler) http.Handler {
		if m == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m.AdminRequired() {
				if m.SessionValid(sessionToken(r)) || m.ValidateInferenceKey(shared.ExtractAPIKey(r)) {
					next.ServeHTTP(w, r)
					return
				}
				w.Header().Set("WWW-Authenticate", `Basic realm="llama-swap"`)
				shared.SendResponse(w, r, http.StatusUnauthorized, "unauthorized: admin login required")
				return
			}
			if m.InferenceRequired() {
				if m.ValidateInferenceKey(shared.ExtractAPIKey(r)) {
					next.ServeHTTP(w, r)
					return
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="llama-swap"`)
				shared.SendResponse(w, r, http.StatusUnauthorized, "unauthorized: invalid or missing API key")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CreateAuthMiddleware preserves backward compatibility for tests: inference key
// check only.
func CreateAuthMiddleware(m *auth.Manager) chain.Middleware {
	return CreateInferenceAuthMiddleware(m)
}

// CreateRequestContextMiddleware returns middleware that extracts model and
// auth info from the request into the context. Requests where no model can be
// identified are rejected with a 404.
func CreateRequestContextMiddleware(cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			data, err := shared.FetchContext(r, cfg)
			if err != nil {
				shared.SendError(w, r, shared.ErrNoModelInContext)
				return
			}
			_ = data
			next.ServeHTTP(w, r)
		})
	}
}

// CreateCORSMiddleware returns middleware that answers OPTIONS preflight
// requests with permissive CORS headers (see issues #81, #77, #42). Non-OPTIONS
// requests pass through untouched.
func CreateCORSMiddleware() chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			if headers := r.Header.Get("Access-Control-Request-Headers"); headers != "" {
				w.Header().Set("Access-Control-Allow-Headers", sanitizeAccessControlRequestHeaderValues(headers))
			} else {
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, X-Requested-With")
			}
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
		})
	}
}

func isTokenChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
	case r >= 'A' && r <= 'Z':
	case r >= '0' && r <= '9':
	case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
	default:
		return false
	}
	return true
}

// sanitizeAccessControlRequestHeaderValues drops any header names that contain
// characters outside the HTTP token grammar before echoing them back.
func sanitizeAccessControlRequestHeaderValues(headerValues string) string {
	parts := strings.Split(headerValues, ",")
	valid := make([]string, 0, len(parts))

	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}

		validPart := true
		for _, c := range v {
			if !isTokenChar(c) {
				validPart = false
				break
			}
		}
		if validPart {
			valid = append(valid, v)
		}
	}

	return strings.Join(valid, ", ")
}
