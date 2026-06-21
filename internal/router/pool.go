package router

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

type poolBackend struct {
	id           int
	proxy        string
	reverseProxy *httputil.ReverseProxy
}

type poolModel struct {
	modelID string
	cfg     config.ModelConfig
	rules   []config.AffinityRule
	ttl     time.Duration

	backends []poolBackend

	mu       sync.Mutex
	inflight []int
	affinity map[string]affinityEntry
}

type affinityEntry struct {
	backendID int
	expires   time.Time
}

// Pool forwards pooled models to always-on upstream backends with sticky
// least-inflight load balancing.
type Pool struct {
	cfg    config.Config
	logger *logmon.Monitor
	models map[string]*poolModel

	shutdownCtx  context.Context
	shutdownFn   context.CancelFunc
	shuttingDown atomic.Bool
	inflight     sync.WaitGroup
}

func NewPool(cfg config.Config, logger *logmon.Monitor) (*Pool, error) {
	models := make(map[string]*poolModel)
	for id, mc := range cfg.Models {
		if !mc.UsesPool() {
			continue
		}
		pm, err := newPoolModel(id, mc)
		if err != nil {
			return nil, err
		}
		models[id] = pm
		for _, alias := range mc.Aliases {
			if _, dup := models[alias]; dup {
				return nil, fmt.Errorf("pool alias %q already mapped", alias)
			}
			models[alias] = pm
		}
	}

	shutdownCtx, shutdownFn := context.WithCancel(context.Background())
	return &Pool{
		cfg:         cfg,
		logger:      logger,
		models:      models,
		shutdownCtx: shutdownCtx,
		shutdownFn:  shutdownFn,
	}, nil
}

func newPoolModel(modelID string, mc config.ModelConfig) (*poolModel, error) {
	pool := mc.Pool
	if pool.StrategyName() != "sticky_least_inflight" {
		return nil, fmt.Errorf("model %s: unsupported pool strategy %q", modelID, pool.Strategy)
	}

	pm := &poolModel{
		modelID:  modelID,
		cfg:      mc,
		rules:    pool.EffectiveAffinityRules(),
		ttl:      pool.AffinityDuration(),
		inflight: make([]int, len(pool.Backends)),
		affinity: make(map[string]affinityEntry),
	}

	for i, b := range pool.Backends {
		transport := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   time.Duration(mc.Timeouts.Connect) * time.Second,
				KeepAlive: time.Duration(mc.Timeouts.KeepAlive) * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   time.Duration(mc.Timeouts.TLSHandshake) * time.Second,
			ResponseHeaderTimeout: time.Duration(mc.Timeouts.ResponseHeader) * time.Second,
			ExpectContinueTimeout: time.Duration(mc.Timeouts.ExpectContinue) * time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       time.Duration(mc.Timeouts.IdleConn) * time.Second,
		}

		target := b.ProxyURL
		rp := httputil.NewSingleHostReverseProxy(target)
		rp.Transport = transport
		rp.ModifyResponse = func(resp *http.Response) error {
			if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
				resp.Header.Set("X-Accel-Buffering", "no")
			}
			return nil
		}
		proxyURL := b.Proxy
		rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			msg := fmt.Sprintf("pool %s backend %s: proxy error: %v", modelID, proxyURL, err)
			if runtime.GOOS == "darwin" && strings.Contains(err.Error(), "connect: no route to host") {
				msg += " (hint: on macOS, check System Settings > Privacy & Security > Local Network permissions)"
			}
			http.Error(w, msg, http.StatusBadGateway)
		}

		pm.backends = append(pm.backends, poolBackend{
			id:           i,
			proxy:        b.Proxy,
			reverseProxy: rp,
		})
	}

	return pm, nil
}

func (p *Pool) Handles(model string) bool {
	_, ok := p.models[model]
	return ok
}

func (p *Pool) Shutdown(timeout time.Duration) error {
	if !p.shuttingDown.CompareAndSwap(false, true) {
		return fmt.Errorf("shutdown already in progress")
	}

	if timeout == 0 {
		p.shutdownFn()
		p.inflight.Wait()
		return nil
	}

	done := make(chan struct{})
	go func() {
		p.inflight.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		p.shutdownFn()
		p.inflight.Wait()
		return fmt.Errorf("pool shutdown timed out after %v", timeout)
	}
}

func (p *Pool) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if p.shuttingDown.Load() {
		shared.SendError(w, req, fmt.Errorf("pool proxy is shutting down"))
		return
	}
	p.inflight.Add(1)
	defer p.inflight.Done()

	data, err := shared.FetchContext(req, p.cfg)
	if err != nil {
		shared.SendError(w, req, err)
		return
	}

	pm, ok := p.models[data.ModelID]
	if !ok {
		shared.SendError(w, req, ErrNoRouterFound)
		return
	}

	affinityKey := ExtractAffinityKey(req, pm.rules)
	backendID := pm.pickBackend(affinityKey)
	backend := pm.backends[backendID]

	p.logger.Debugf("pool: model %s affinity=%q backend=%d (%s)", pm.modelID, affinityKey, backendID, backend.proxy)

	pm.trackStart(backendID)
	defer pm.trackDone(backendID)

	ctx, cancel := context.WithCancel(context.Background())
	stopReq := context.AfterFunc(req.Context(), cancel)
	stopShutdown := context.AfterFunc(p.shutdownCtx, cancel)
	req = req.WithContext(ctx)

	backend.reverseProxy.ServeHTTP(w, req)

	stopShutdown()
	stopReq()
	cancel()
}

func (pm *poolModel) pickBackend(affinityKey string) int {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	now := time.Now()
	pm.pruneAffinity(now)

	if affinityKey != "" {
		if entry, ok := pm.affinity[affinityKey]; ok && entry.backendID >= 0 && entry.backendID < len(pm.backends) {
			pm.affinity[affinityKey] = affinityEntry{backendID: entry.backendID, expires: now.Add(pm.ttl)}
			return entry.backendID
		}
	}

	best := 0
	bestLoad := pm.inflight[0]
	for i := 1; i < len(pm.backends); i++ {
		if pm.inflight[i] < bestLoad {
			best = i
			bestLoad = pm.inflight[i]
		}
	}

	if affinityKey != "" {
		pm.affinity[affinityKey] = affinityEntry{backendID: best, expires: now.Add(pm.ttl)}
	}
	return best
}

func (pm *poolModel) pruneAffinity(now time.Time) {
	for k, v := range pm.affinity {
		if now.After(v.expires) {
			delete(pm.affinity, k)
		}
	}
}

func (pm *poolModel) trackStart(id int) {
	pm.mu.Lock()
	pm.inflight[id]++
	pm.mu.Unlock()
}

func (pm *poolModel) trackDone(id int) {
	pm.mu.Lock()
	pm.inflight[id]--
	if pm.inflight[id] < 0 {
		pm.inflight[id] = 0
	}
	pm.mu.Unlock()
}
