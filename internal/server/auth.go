package server

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/shared"
	"github.com/tidwall/gjson"
)

// CreateInferenceAuthMiddleware validates inference API keys when any are
// configured (static YAML keys or UI-managed keys) and attaches the key policy
// to the request context for downstream limit checks.
func CreateInferenceAuthMiddleware(m *auth.Manager) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m == nil || !m.InferenceRequired() {
				next.ServeHTTP(w, r)
				return
			}
			policy, ok := m.AuthenticateInferenceKey(shared.ExtractAPIKey(r))
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="llama-swap"`)
				shared.SendResponse(w, r, http.StatusUnauthorized, "unauthorized: invalid or missing API key")
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.ContextWithKeyPolicy(r.Context(), policy)))
		})
	}
}

// CreateKeyLimitsMiddleware enforces per-key model allow-lists and max token
// caps after the request model has been resolved.
func CreateKeyLimitsMiddleware(cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			policy, ok := auth.KeyPolicyFromContext(r.Context())
			if !ok || (len(policy.Models) == 0 && policy.MaxTokens <= 0) {
				next.ServeHTTP(w, r)
				return
			}

			data, err := shared.FetchContext(r, cfg)
			if err != nil {
				shared.SendError(w, r, shared.ErrNoModelInContext)
				return
			}

			if !policy.AllowsModel(data.Model, data.ModelID) {
				shared.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not allowed to use this model")
				return
			}

			if policy.MaxTokens > 0 && strings.Contains(r.Header.Get("Content-Type"), "application/json") {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					shared.SendResponse(w, r, http.StatusBadRequest, "could not read request body")
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				if exceeded, field, value := requestedTokensExceed(body, policy.MaxTokens); exceeded {
					shared.SendResponse(w, r, http.StatusForbidden,
						"forbidden: "+field+" ("+strconv.FormatInt(value, 10)+") exceeds API key maxTokens ("+strconv.FormatInt(policy.MaxTokens, 10)+")")
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

// requestedTokensExceed reports whether a JSON body asks for more tokens than
// the key allows via max_tokens or max_completion_tokens.
func requestedTokensExceed(body []byte, limit int64) (bool, string, int64) {
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		v := gjson.GetBytes(body, field)
		if !v.Exists() {
			continue
		}
		n := v.Int()
		if n > limit {
			return true, field, n
		}
	}
	return false, "", 0
}

// CreateAdminAuthMiddleware protects dashboard routes.
//
//   - When admin.password is set: require a valid session cookie (inference
//     API keys do not unlock the dashboard).
//   - When only inference keys are configured: require a valid API key.
//   - When neither is configured: allow (default-allow).
func CreateAdminAuthMiddleware(m *auth.Manager) chain.Middleware {
	return func(next http.Handler) http.Handler {
		if m == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m.AdminRequired() {
				if m.SessionValid(sessionToken(r)) {
					next.ServeHTTP(w, r)
					return
				}
				w.Header().Set("WWW-Authenticate", `Basic realm="llama-swap"`)
				shared.SendResponse(w, r, http.StatusUnauthorized, "unauthorized: admin login required")
				return
			}
			if m.InferenceRequired() {
				policy, ok := m.AuthenticateInferenceKey(shared.ExtractAPIKey(r))
				if !ok {
					w.Header().Set("WWW-Authenticate", `Bearer realm="llama-swap"`)
					shared.SendResponse(w, r, http.StatusUnauthorized, "unauthorized: invalid or missing API key")
					return
				}
				r = r.WithContext(auth.ContextWithKeyPolicy(r.Context(), policy))
				next.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CreateStripClientAuthMiddleware removes gateway credentials from the request
// before they are forwarded to upstream backends. Call this after inference
// auth and request-context extraction so affinity can still use the API key
// from ReqContextData.
func CreateStripClientAuthMiddleware() chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" && r.Header.Get("X-Api-Key") == "" {
				next.ServeHTTP(w, r)
				return
			}
			r2 := r.Clone(r.Context())
			r2.Header.Del("Authorization")
			r2.Header.Del("X-Api-Key")
			next.ServeHTTP(w, r2)
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
