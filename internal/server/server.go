package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/router"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

// Server owns the HTTP mux, cross-cutting middleware, and the local/peer model
// dispatch. It supersedes router.Server: it builds the local and peer routers
// directly and dispatches between them itself.
type Server struct {
	cfg config.Config

	muxlog      *logmon.Monitor
	proxylog    *logmon.Monitor
	upstreamlog *logmon.Monitor

	perf     *perf.Monitor
	inflight *inflightCounter
	metrics  *metricsMonitor
	build    BuildInfo

	local router.LocalRouter
	peer  router.Router
	pool  router.Router
	auth  *auth.Manager

	mux     *http.ServeMux
	handler http.Handler

	shutdownCtx  context.Context
	shutdownFn   context.CancelFunc
	shuttingDown atomic.Bool
}

// modelPostJSONRoutes are endpoints with a model id in the JSON request body.
var modelPostJSONRoutes = []string{
	"/v1/chat/completions",
	"/v1/responses",
	"/v1/completions",
	"/v1/messages",
	"/v1/messages/count_tokens",
	"/v1/embeddings",
	"/reranking",
	"/rerank",
	"/v1/rerank",
	"/v1/reranking",
	"/infill",
	"/completion",
	"/v1/audio/speech",
	"/v1/audio/voices",
	"/v1/images/generations",
	"/sdapi/v1/txt2img",
	"/sdapi/v1/img2img",

	// versionless routes, the /v/ is stripped before the request is forwarded upstream
	// see issue #728
	"/v/chat/completions",
	"/v/responses",
	"/v/completions",
	"/v/messages",
	"/v/messages/count_tokens",
	"/v/embeddings",
	"/v/rerank",
	"/v/reranking",
}

// modelPostFormRoutes are multipart/form-data endpoints with a model id in the form data
var modelPostFormRoutes = []string{
	"/v1/audio/transcriptions",
	"/v1/images/edits",
}

// modelGetRoutes are model-dispatched GET endpoints (the model arrives as a
// query parameter).
var modelGetRoutes = []string{
	"/v1/audio/voices",
	"/sdapi/v1/loras",
}

// isMetricsRecordPath reports whether path is one of the model-dispatched
// endpoints that the metrics middleware records in the activity log.
func isMetricsRecordPath(path string) bool {
	for _, p := range modelPostJSONRoutes {
		if p == path {
			return true
		}
	}
	for _, p := range modelPostFormRoutes {
		if p == path {
			return true
		}
	}
	for _, p := range modelGetRoutes {
		if p == path {
			return true
		}
	}
	return false
}

// BuildInfo carries version metadata surfaced by GET /api/version.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

func New(cfg config.Config, muxlog *logmon.Monitor, proxylog *logmon.Monitor, upstreamlog *logmon.Monitor, perfMon *perf.Monitor, build BuildInfo) (*Server, error) {
	authMgr, err := auth.NewManager(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating auth manager: %w", err)
	}

	var local router.LocalRouter

	switch cfg.Routing.Router.Use {
	case "matrix":
		local, err = router.NewMatrix(cfg, proxylog, upstreamlog)
		if err != nil {
			return nil, fmt.Errorf("creating matrix router: %w", err)
		}
	default: // "group"
		local, err = router.NewGroup(cfg, proxylog, upstreamlog)
		if err != nil {
			return nil, fmt.Errorf("creating group router: %w", err)
		}
	}

	peer, err := router.NewPeer(cfg, proxylog)
	if err != nil {
		return nil, fmt.Errorf("creating peer router: %w", err)
	}

	pool, err := router.NewPool(cfg, proxylog)
	if err != nil {
		return nil, fmt.Errorf("creating pool router: %w", err)
	}

	shutdownCtx, shutdownFn := context.WithCancel(context.Background())
	s := &Server{
		cfg:         cfg,
		muxlog:      muxlog,
		proxylog:    proxylog,
		upstreamlog: upstreamlog,
		perf:        perfMon,
		inflight:    &inflightCounter{},
		metrics:     newMetricsMonitor(proxylog, cfg.MetricsMaxInMemory, cfg.CaptureBuffer),
		build:       build,
		local:       local,
		peer:        peer,
		pool:        pool,
		auth:        authMgr,
		shutdownCtx: shutdownCtx,
		shutdownFn:  shutdownFn,
	}
	s.routes()
	s.startPreload()
	return s, nil
}

// localPeerHandler dispatches a model-routed request to the local or peer
// router. The model is resolved once via shared.FetchContext.
func (s *Server) localPeerHandler(w http.ResponseWriter, r *http.Request) {
	stripVersionPrefix(r)

	data, err := shared.FetchContext(r, s.cfg)
	if err != nil {
		shared.SendError(w, r, shared.ErrNoModelInContext)
		return
	}

	switch {
	case s.pool != nil && s.pool.Handles(data.ModelID):
		s.proxylog.Debugf("dispatch: using pool for model: %s", data.ModelID)
		s.pool.ServeHTTP(w, r)
	case s.local.Handles(data.ModelID):
		s.proxylog.Debugf("dispatch: using local process for model: %s", data.ModelID)
		s.local.ServeHTTP(w, r)
	case s.peer.Handles(data.ModelID):
		s.proxylog.Debugf("dispatch: using peer for model: %s", data.ModelID)
		s.peer.ServeHTTP(w, r)
	default:
		shared.SendError(w, r, router.ErrNoRouterFound)
	}
}

// stripVersionPrefix rewrites versionless /v/... requests to their /... form
// before forwarding upstream (issue #728).
func stripVersionPrefix(r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v/") {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/v")
	}
}

// routes builds the mux, registers every route, and wraps the mux with the
// global CORS middleware.
func (s *Server) routes() {

	inferenceAuth := CreateInferenceAuthMiddleware(s.auth)
	dashboardAuth := CreateAdminAuthMiddleware(s.auth)

	modelChain := chain.New(
		inferenceAuth,
		CreateRequestContextMiddleware(s.cfg),
		CreateFilterMiddleware(s.cfg),
		CreateFormFilterMiddleware(s.cfg),
		CreateInflightMiddleware(s.inflight),
		CreateMetricsMiddleware(s.metrics, s.cfg),
	)
	dashboardChain := chain.New(dashboardAuth)

	mux := http.NewServeMux()
	dispatch := http.HandlerFunc(s.localPeerHandler)

	for _, path := range modelPostJSONRoutes {
		mux.Handle("POST "+path, modelChain.Then(dispatch))
	}
	for _, path := range modelPostFormRoutes {
		mux.Handle("POST "+path, modelChain.Then(dispatch))
	}
	for _, path := range modelGetRoutes {
		mux.Handle("GET "+path, modelChain.Then(dispatch))
	}

	// OpenAI model listing uses inference keys.
	mux.Handle("GET /v1/models", chain.New(inferenceAuth).ThenFunc(s.handleListModels))

	// Public auth endpoints for the dashboard login flow.
	mux.HandleFunc("GET /api/auth/session", s.handleAuthSession)
	mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)

	mux.Handle("GET /logs", dashboardChain.ThenFunc(s.handleLogs))
	mux.Handle("GET /logs/stream", dashboardChain.ThenFunc(s.handleLogStream))
	mux.Handle("GET /logs/stream/{logMonitorID...}", dashboardChain.ThenFunc(s.handleLogStream))

	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /wol-health", handleHealth)
	mux.HandleFunc("GET /{$}", handleRootRedirect)

	mux.Handle("GET /ui/", dashboardChain.ThenFunc(s.handleUI))
	mux.Handle("GET /favicon.ico", dashboardChain.ThenFunc(s.handleFavicon))

	mux.Handle("GET /metrics", dashboardChain.ThenFunc(s.handleMetrics))

	mux.Handle("GET /unload", dashboardChain.ThenFunc(s.handleUnload))
	mux.Handle("GET /running", dashboardChain.ThenFunc(s.handleRunning))

	upstreamChain := dashboardChain.Append(CreateMetricsMiddleware(s.metrics, s.cfg))
	mux.HandleFunc("GET /upstream", handleUpstreamRedirect)
	mux.Handle("/upstream/{upstreamPath...}", upstreamChain.ThenFunc(s.handleUpstream))

	mux.Handle("POST /api/models/unload", dashboardChain.ThenFunc(s.handleAPIUnloadAll))
	mux.Handle("POST /api/models/unload/{model...}", dashboardChain.ThenFunc(s.handleAPIUnloadModel))
	mux.Handle("GET /api/events", dashboardChain.ThenFunc(s.handleAPIEvents))
	mux.Handle("GET /api/metrics", dashboardChain.ThenFunc(s.handleAPIMetrics))
	mux.Handle("GET /api/performance", dashboardChain.ThenFunc(s.handleAPIPerformance))
	mux.Handle("GET /api/version", dashboardChain.ThenFunc(s.handleAPIVersion))
	mux.Handle("GET /api/captures/{id}", dashboardChain.ThenFunc(s.handleAPICapture))
	mux.Handle("GET /api/admin/keys", dashboardChain.ThenFunc(s.handleAdminListKeys))
	mux.Handle("POST /api/admin/keys", dashboardChain.ThenFunc(s.handleAdminCreateKey))
	mux.Handle("DELETE /api/admin/keys/{id}", dashboardChain.ThenFunc(s.handleAdminRevokeKey))

	s.mux = mux
	s.handler = chain.New(CreateRequestLogMiddleware(s.proxylog), CreateCORSMiddleware()).Then(mux)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// CloseStreams cancels long-lived response streams (Server-Sent Events) so a
// graceful httpServer.Shutdown can drain without blocking on them. It does not
// tear down routers; call Shutdown for that. Safe to call repeatedly.
func (s *Server) CloseStreams() {
	s.shutdownFn()
}

// Shutdown stops the local and peer routers in parallel. It is idempotent;
// repeated calls return nil without re-running shutdown.
//
// Callers must drain inflight HTTP requests (httpServer.Shutdown) before
// calling this, otherwise inflight requests 502 when their processes are torn
// down. Call CloseStreams before httpServer.Shutdown so SSE streams do not
// block the drain.
func (s *Server) Shutdown(timeout time.Duration) error {
	if !s.shuttingDown.CompareAndSwap(false, true) {
		return nil
	}
	s.shutdownFn()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	for _, rt := range []router.Router{s.local, s.pool, s.peer} {
		if rt == nil {
			continue
		}
		wg.Add(1)
		go func(rt router.Router) {
			defer wg.Done()
			if err := rt.Shutdown(timeout); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(rt)
	}

	wg.Wait()
	return errors.Join(errs...)
}
