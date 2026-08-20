package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/discovery"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/shared"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type poolBackend struct {
	id           int
	proxy        string
	apiKey       string
	reverseProxy *httputil.ReverseProxy
	healthy      atomic.Bool

	// Spawnable sidecar (optional).
	proc           process.Process
	procID         string
	restartOnCrash bool
	healthTimeout  time.Duration

	supOnce sync.Once
	model   *poolModel // owning model; set after backends are appended
}

type poolModel struct {
	modelID         string
	upstreamModelID string // rewrite request model when serving FQ aliases
	cfg             config.ModelConfig
	rules           []config.AffinityRule
	ttl             time.Duration
	startMode       string
	discovered      bool // owned by ReplaceDiscovered; YAML pools stay false

	backends     []poolBackend
	contextSizes []atomic.Int64 // parallel to backends; 0 = unknown/unlimited

	mu       sync.Mutex
	inflight []int
	affinity map[string]affinityEntry
}

type affinityEntry struct {
	backendID int
	expires   time.Time
}

// Pool forwards pooled models to upstream backends with sticky least-inflight
// load balancing. Backends may be static URLs or spawnable processes.
type Pool struct {
	cfg         config.Config
	logger      *logmon.Monitor
	upstreamlog *logmon.Monitor
	models      map[string]*poolModel
	modelsMu    sync.RWMutex

	shutdownCtx  context.Context
	shutdownFn   context.CancelFunc
	procCtx      context.Context
	procCancel   context.CancelFunc
	shuttingDown atomic.Bool
	inflight     sync.WaitGroup
}

func NewPool(cfg config.Config, proxylog, upstreamlog *logmon.Monitor) (*Pool, error) {
	if upstreamlog == nil {
		upstreamlog = proxylog
	}
	shutdownCtx, shutdownFn := context.WithCancel(context.Background())
	procCtx, procCancel := context.WithCancel(context.Background())
	pool := &Pool{
		cfg:         cfg,
		logger:      proxylog,
		upstreamlog: upstreamlog,
		models:      make(map[string]*poolModel),
		shutdownCtx: shutdownCtx,
		shutdownFn:  shutdownFn,
		procCtx:     procCtx,
		procCancel:  procCancel,
	}

	for id, mc := range cfg.Models {
		if !mc.UsesPool() {
			continue
		}
		pm, err := pool.newPoolModel(id, mc)
		if err != nil {
			shutdownFn()
			procCancel()
			return nil, err
		}
		pool.models[id] = pm
		for _, alias := range mc.Aliases {
			if _, dup := pool.models[alias]; dup {
				shutdownFn()
				procCancel()
				return nil, fmt.Errorf("pool alias %q already mapped", alias)
			}
			pool.models[alias] = pm
		}
	}

	pool.startProber()
	pool.startMetricsEmitter()

	// Preload spawn backends marked pool.start: preload.
	for _, pm := range pool.uniqueModels() {
		if pm.startMode == "preload" {
			for i := range pm.backends {
				if pm.backends[i].proc != nil {
					pm.backends[i].kickSupervise(pool)
				}
			}
		}
	}

	return pool, nil
}

func (p *Pool) uniqueModels() []*poolModel {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	seen := make(map[*poolModel]struct{}, len(p.models))
	out := make([]*poolModel, 0, len(p.models))
	for _, pm := range p.models {
		if _, ok := seen[pm]; ok {
			continue
		}
		seen[pm] = struct{}{}
		out = append(out, pm)
	}
	return out
}

func (p *Pool) newPoolModel(modelID string, mc config.ModelConfig) (*poolModel, error) {
	poolCfg := mc.Pool
	if poolCfg.StrategyName() != "sticky_least_inflight" {
		return nil, fmt.Errorf("model %s: unsupported pool strategy %q", modelID, poolCfg.Strategy)
	}

	n := len(poolCfg.Backends)
	pm := &poolModel{
		modelID:      modelID,
		cfg:          mc,
		rules:        config.ResolveAffinityRules(poolCfg),
		ttl:          poolCfg.AffinityDuration(),
		startMode:    poolCfg.StartMode(),
		backends:     make([]poolBackend, n),
		contextSizes: make([]atomic.Int64, n),
		inflight:     make([]int, n),
		affinity:     make(map[string]affinityEntry),
	}

	healthTimeout := max(time.Duration(mc.HealthCheckTimeout)*time.Second, 15*time.Second)

	for i, b := range poolCfg.Backends {
		if err := p.buildBackend(modelID, mc, i, b, healthTimeout, &pm.backends[i]); err != nil {
			return nil, err
		}
		pm.backends[i].model = pm
		if b.ContextSize > 0 {
			pm.contextSizes[i].Store(int64(b.ContextSize))
		}
	}

	return pm, nil
}

func (p *Pool) buildBackend(modelID string, mc config.ModelConfig, i int, b config.PoolBackend, healthTimeout time.Duration, backend *poolBackend) error {
	*backend = poolBackend{
		id:             i,
		proxy:          b.Proxy,
		restartOnCrash: mc.Pool.ShouldRestartOnCrash(),
		healthTimeout:  healthTimeout,
	}
	backend.healthy.Store(true)

	if b.IsSpawn() {
		procID := fmt.Sprintf("%s#%d", modelID, i)
		checkEndpoint := b.CheckEndpoint
		if checkEndpoint == "" {
			checkEndpoint = mc.CheckEndpoint
		}
		if checkEndpoint == "" {
			checkEndpoint = "/health"
		}
		cmdStop := b.CmdStop
		if cmdStop == "" {
			cmdStop = mc.CmdStop
		}
		synth := config.ModelConfig{
			Cmd:                b.Cmd,
			CmdStop:            cmdStop,
			Proxy:              b.Proxy,
			Env:                append([]string{}, b.Env...),
			CheckEndpoint:      checkEndpoint,
			UnloadAfter:        0, // pool sidecars stay warm
			Timeouts:           mc.Timeouts,
			HealthCheckTimeout: mc.HealthCheckTimeout,
			WhisperCompat:      b.WhisperCompat,
		}
		procLog := logmon.NewWriter(p.upstreamlog)
		proc, err := process.New(p.procCtx, procID, synth, procLog, p.logger)
		if err != nil {
			return fmt.Errorf("model %s backend %d: creating process: %w", modelID, i, err)
		}
		backend.proc = proc
		backend.procID = procID
		// Spawn backends start unhealthy until WaitReady succeeds.
		backend.healthy.Store(false)
		return nil
	}

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
	rp := shared.NewSingleHostReverseProxy(target, b.WhisperCompat)
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
	backend.reverseProxy = rp
	return nil
}

func (b *poolBackend) kickSupervise(p *Pool) {
	if b.proc == nil {
		return
	}
	b.supOnce.Do(func() {
		go b.supervise(p)
	})
}

func (b *poolBackend) supervise(p *Pool) {
	for {
		if p.shuttingDown.Load() {
			return
		}
		select {
		case <-p.shutdownCtx.Done():
			return
		default:
		}

		err := b.proc.Run(b.healthTimeout)
		b.healthy.Store(false)
		if b.model != nil {
			b.model.clearAffinityFor(b.id)
		}

		if p.shuttingDown.Load() {
			return
		}
		if err != nil {
			p.logger.Warnf("pool: process %s exited: %v", b.procID, err)
		} else {
			p.logger.Infof("pool: process %s exited", b.procID)
		}
		if !b.restartOnCrash {
			return
		}
		select {
		case <-p.shutdownCtx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (b *poolBackend) ensureReady(ctx context.Context, p *Pool) error {
	if b.proc == nil {
		return nil
	}
	b.kickSupervise(p)
	if err := b.proc.WaitReady(ctx); err != nil {
		b.healthy.Store(false)
		return err
	}
	b.healthy.Store(true)
	return nil
}

func (pm *poolModel) clearAffinityFor(backendID int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for k, v := range pm.affinity {
		if v.backendID == backendID {
			delete(pm.affinity, k)
		}
	}
}

func (p *Pool) Handles(model string) bool {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	_, ok := p.models[model]
	return ok
}

// ReplaceDiscovered swaps discovery-owned auto-pools. YAML-configured pools are
// left untouched. route keys (bare + FQ) all map to the same poolModel.
func (p *Pool) ReplaceDiscovered(pools []discovery.DiscoveredPool) error {
	p.modelsMu.Lock()
	defer p.modelsMu.Unlock()

	// Drop previous discovery-owned entries.
	for key, pm := range p.models {
		if pm.discovered {
			delete(p.models, key)
		}
	}

	for _, spec := range pools {
		if len(spec.Backends) == 0 || len(spec.RouteKeys) == 0 {
			continue
		}
		// Do not override YAML pool / alias keys.
		skip := false
		for _, key := range spec.RouteKeys {
			if existing, ok := p.models[key]; ok && !existing.discovered {
				p.logger.Warnf("discovery pool: route key %q owned by config, skipping pool %s", key, spec.CanonicalID)
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		pm, err := p.newDiscoveredPoolModel(spec)
		if err != nil {
			return err
		}
		for _, key := range spec.RouteKeys {
			p.models[key] = pm
		}
	}
	return nil
}

func (p *Pool) newDiscoveredPoolModel(spec discovery.DiscoveredPool) (*poolModel, error) {
	n := len(spec.Backends)
	mc := config.ModelConfig{
		Pool: &config.PoolConfig{
			Strategy: "sticky_least_inflight",
		},
		Timeouts: config.TimeoutsConfig{
			Connect:        30,
			KeepAlive:      30,
			ResponseHeader: 60,
			TLSHandshake:   10,
			ExpectContinue: 1,
			IdleConn:       90,
		},
		HealthCheckTimeout: 120,
	}
	// Populate backends on Pool config for buildBackend.
	backends := make([]config.PoolBackend, n)
	for i, b := range spec.Backends {
		proxyURL, err := url.Parse(b.Proxy)
		if err != nil {
			return nil, fmt.Errorf("discovered pool %s backend %d: %w", spec.CanonicalID, i, err)
		}
		backends[i] = config.PoolBackend{
			Proxy:       b.Proxy,
			ProxyURL:    proxyURL,
			ContextSize: b.ContextSize,
		}
	}
	mc.Pool.Backends = backends

	pm := &poolModel{
		modelID:         spec.CanonicalID,
		upstreamModelID: spec.UpstreamModelID,
		cfg:             mc,
		rules:           config.DefaultAffinityRules(),
		ttl:             30 * time.Minute,
		startMode:       "on_demand",
		discovered:      true,
		backends:        make([]poolBackend, n),
		contextSizes:    make([]atomic.Int64, n),
		inflight:        make([]int, n),
		affinity:        make(map[string]affinityEntry),
	}

	healthTimeout := 15 * time.Second
	for i, b := range backends {
		if err := p.buildBackend(spec.CanonicalID, mc, i, b, healthTimeout, &pm.backends[i]); err != nil {
			return nil, err
		}
		pm.backends[i].model = pm
		pm.backends[i].apiKey = spec.Backends[i].ApiKey
		if b.ContextSize > 0 {
			pm.contextSizes[i].Store(int64(b.ContextSize))
		}
	}
	return pm, nil
}

// Preload starts all spawn backends for models with pool.start: preload.
// Also starts spawn backends when modelID is listed in hooks preload.
func (p *Pool) Preload(modelID string) {
	p.modelsMu.RLock()
	pm, ok := p.models[modelID]
	p.modelsMu.RUnlock()
	if !ok {
		return
	}
	for i := range pm.backends {
		if pm.backends[i].proc != nil {
			pm.backends[i].kickSupervise(p)
		}
	}
}

func (p *Pool) Shutdown(timeout time.Duration) error {
	if !p.shuttingDown.CompareAndSwap(false, true) {
		return fmt.Errorf("shutdown already in progress")
	}

	p.shutdownFn()

	done := make(chan struct{})
	go func() {
		p.inflight.Wait()
		close(done)
	}()

	if timeout == 0 {
		<-done
	} else {
		select {
		case <-done:
		case <-time.After(timeout):
			p.inflight.Wait()
		}
	}

	stopTimeout := timeout
	if stopTimeout <= 0 {
		stopTimeout = 15 * time.Second
	}
	var wg sync.WaitGroup
	for _, pm := range p.uniqueModels() {
		for i := range pm.backends {
			b := &pm.backends[i]
			if b.proc == nil {
				continue
			}
			wg.Add(1)
			go func(proc process.Process, id string) {
				defer wg.Done()
				if err := proc.Stop(stopTimeout); err != nil {
					p.logger.Warnf("pool: stopping %s: %v", id, err)
				}
			}(b.proc, b.procID)
		}
	}
	wg.Wait()
	p.procCancel()
	return nil
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

	p.modelsMu.RLock()
	pm, ok := p.models[data.ModelID]
	p.modelsMu.RUnlock()
	if !ok {
		shared.SendError(w, req, ErrNoRouterFound)
		return
	}

	affinityKey := ExtractAffinityKey(req, pm.rules)
	reqNctx := extractRequestNctx(req)
	backendID, err := pm.pickAndTrack(affinityKey, reqNctx)
	if err != nil {
		shared.SendError(w, req, err)
		return
	}
	backend := &pm.backends[backendID]

	if err := backend.ensureReady(req.Context(), p); err != nil {
		pm.trackDone(backendID)
		shared.SendResponse(w, req, http.StatusBadGateway, fmt.Sprintf("pool backend %d not ready: %v", backendID, err))
		return
	}
	if backend.proc != nil {
		if cs := backend.proc.UpstreamContextLength(); cs > 0 && pm.contextSizes[backendID].Load() == 0 {
			pm.contextSizes[backendID].Store(int64(cs))
		}
	}

	_ = shared.SetReqData(req.Context(), "pool_backend_id", strconv.Itoa(backendID))
	_ = shared.SetReqData(req.Context(), "pool_backend", backend.proxy)

	p.logger.Debugf("pool: model %s affinity=%q n_ctx=%d backend=%d (%s)",
		pm.modelID, affinityKey, reqNctx, backendID, backend.proxy)

	defer pm.trackDone(backendID)

	if backend.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+backend.apiKey)
		req.Header.Set("x-api-key", backend.apiKey)
	}

	// Rewrite FQ client model IDs to the upstream-native ID when needed.
	if pm.upstreamModelID != "" && data.Model != pm.upstreamModelID {
		if err := rewriteRequestModel(req, pm.upstreamModelID); err != nil {
			shared.SendError(w, req, err)
			return
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopReq := context.AfterFunc(req.Context(), cancel)
	stopShutdown := context.AfterFunc(p.shutdownCtx, cancel)
	req = req.WithContext(ctx)

	if backend.proc != nil {
		backend.proc.ServeHTTP(w, req)
	} else {
		backend.reverseProxy.ServeHTTP(w, req)
	}

	stopShutdown()
	stopReq()
	cancel()
}

// rewriteRequestModel sets the JSON "model" field when the body is JSON.
func rewriteRequestModel(req *http.Request, modelID string) error {
	ct := req.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") || req.Body == nil {
		return nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return fmt.Errorf("reading body for model rewrite: %w", err)
	}
	_ = req.Body.Close()
	body, err = sjson.SetBytes(body, "model", modelID)
	if err != nil {
		return fmt.Errorf("rewriting model: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	req.Header.Del("Transfer-Encoding")
	return nil
}

// pickAndTrack selects an eligible backend and increments its inflight counter.
//
// Sticky affinity is honored only when the mapped backend is idle (zero
// in-flight). If that "home" slot is busy, the request is load-balanced to
// another backend without moving the affinity mapping, so later requests
// prefer the home slot again once it is free.
func (pm *poolModel) pickAndTrack(affinityKey string, reqNctx int) (int, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	now := time.Now()
	pm.pruneAffinity(now)

	if affinityKey != "" {
		if entry, ok := pm.affinity[affinityKey]; ok && entry.backendID >= 0 && entry.backendID < len(pm.backends) {
			if pm.isRoutableLocked(entry.backendID, reqNctx) {
				if pm.ttl > 0 {
					pm.affinity[affinityKey] = affinityEntry{backendID: entry.backendID, expires: now.Add(pm.ttl)}
				}
				if pm.inflight[entry.backendID] == 0 {
					pm.inflight[entry.backendID]++
					return entry.backendID, nil
				}
				// Home is busy — keep the mapping and fall through to least-inflight.
			} else {
				delete(pm.affinity, affinityKey)
			}
		}
	}

	best := pm.pickLeastInflightLocked(reqNctx, true)
	if best < 0 {
		best = pm.pickLeastInflightLocked(reqNctx, false)
	}
	if best < 0 {
		// Last resort: any backend that can be started or is static.
		for i := range pm.backends {
			if pm.canUseLocked(i) {
				best = i
				break
			}
		}
	}
	if best < 0 {
		return -1, fmt.Errorf("pool %s: no eligible backends", pm.modelID)
	}

	// Establish affinity for new keys only. Do not re-home away from a busy
	// sticky backend — temporary overflow should not steal the session.
	if affinityKey != "" && pm.ttl > 0 {
		if _, exists := pm.affinity[affinityKey]; !exists {
			pm.affinity[affinityKey] = affinityEntry{backendID: best, expires: now.Add(pm.ttl)}
		}
	}
	pm.inflight[best]++
	return best, nil
}

// isRoutableLocked prefers ready/healthy backends that fit context.
func (pm *poolModel) isRoutableLocked(i int, reqNctx int) bool {
	if !backendFits(&pm.contextSizes[i], reqNctx) {
		return false
	}
	return pm.isReadyLocked(i)
}

func (pm *poolModel) isReadyLocked(i int) bool {
	b := &pm.backends[i]
	if b.proc != nil {
		return b.proc.State() == process.StateReady
	}
	return b.healthy.Load()
}

func (pm *poolModel) canUseLocked(i int) bool {
	b := &pm.backends[i]
	if b.proc != nil {
		st := b.proc.State()
		return st == process.StateReady || st == process.StateStarting || st == process.StateStopped
	}
	return true // static: attempt even if marked unhealthy
}

func (pm *poolModel) pickLeastInflightLocked(reqNctx int, readyOnly bool) int {
	best := -1
	bestLoad := int(^uint(0) >> 1)
	for i := range pm.backends {
		if !backendFits(&pm.contextSizes[i], reqNctx) {
			continue
		}
		if readyOnly {
			if !pm.isReadyLocked(i) {
				continue
			}
		} else if !pm.canUseLocked(i) {
			continue
		}
		if pm.inflight[i] < bestLoad {
			best = i
			bestLoad = pm.inflight[i]
		}
	}
	return best
}

func backendFits(cs *atomic.Int64, nctx int) bool {
	v := int(cs.Load())
	if v == 0 || nctx <= 0 {
		return true
	}
	return v >= nctx
}

func extractRequestNctx(r *http.Request) int {
	if r.Body == nil {
		return 0
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) == 0 {
		return 0
	}
	if v := gjson.GetBytes(body, "n_ctx"); v.Exists() {
		n, err := strconv.Atoi(v.String())
		if err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func (pm *poolModel) pruneAffinity(now time.Time) {
	for k, v := range pm.affinity {
		if now.After(v.expires) {
			delete(pm.affinity, k)
		}
	}
}

func (pm *poolModel) trackDone(id int) {
	pm.mu.Lock()
	pm.inflight[id]--
	if pm.inflight[id] < 0 {
		pm.inflight[id] = 0
	}
	pm.mu.Unlock()
}

// Stats returns a point-in-time snapshot of every pooled model's backend load.
func (p *Pool) Stats() shared.PoolMetricsSnapshot {
	snap := shared.PoolMetricsSnapshot{
		Timestamp: time.Now(),
		Models:    make([]shared.PoolModelMetric, 0),
	}
	if p == nil {
		return snap
	}

	p.modelsMu.RLock()
	unique := make(map[string]*poolModel, len(p.models))
	for _, pm := range p.models {
		unique[pm.modelID] = pm
	}
	p.modelsMu.RUnlock()

	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		snap.Models = append(snap.Models, unique[id].stats())
	}
	return snap
}

func (pm *poolModel) stats() shared.PoolModelMetric {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.pruneAffinity(time.Now())

	affinityPerBackend := make([]int, len(pm.backends))
	for _, entry := range pm.affinity {
		if entry.backendID >= 0 && entry.backendID < len(pm.backends) {
			affinityPerBackend[entry.backendID]++
		}
	}

	backends := make([]shared.PoolBackendMetric, len(pm.backends))
	for i := range pm.backends {
		b := &pm.backends[i]
		m := shared.PoolBackendMetric{
			ID:               b.id,
			Proxy:            b.proxy,
			Inflight:         pm.inflight[i],
			ContextSize:      int(pm.contextSizes[i].Load()),
			AffinitySessions: affinityPerBackend[i],
			Healthy:          b.healthy.Load(),
			Kind:             "static",
		}
		if b.proc != nil {
			m.Kind = "spawn"
			m.State = string(b.proc.State())
			m.ProcessID = b.procID
			m.Healthy = b.proc.State() == process.StateReady
		}
		backends[i] = m
	}

	return shared.PoolModelMetric{
		ModelID:          pm.modelID,
		Strategy:         pm.cfg.Pool.StrategyName(),
		Backends:         backends,
		AffinitySessions: len(pm.affinity),
	}
}

func (p *Pool) startMetricsEmitter() {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.shutdownCtx.Done():
				return
			case <-ticker.C:
				event.Emit(shared.PoolMetricsUpdateEvent{Snapshot: p.Stats()})
			}
		}
	}()
}

func (p *Pool) startProber() {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.shutdownCtx.Done():
				return
			case <-ticker.C:
				p.probeBackends()
			}
		}
	}()
}

func (p *Pool) probeBackends() {
	for _, pm := range p.uniqueModels() {
		for i := range pm.backends {
			b := &pm.backends[i]
			if b.proc != nil {
				// Spawn health comes from process state / WaitReady.
				if b.proc.State() == process.StateReady {
					b.healthy.Store(true)
					if cs := b.proc.UpstreamContextLength(); cs > 0 && pm.contextSizes[i].Load() == 0 {
						pm.contextSizes[i].Store(int64(cs))
					}
				} else if b.proc.State() != process.StateStarting {
					b.healthy.Store(false)
				}
				continue
			}
			cs, ok := probeBackend(b.proxy, p.shutdownCtx)
			b.healthy.Store(ok)
			if ok && pm.contextSizes[i].Load() == 0 && cs > 0 {
				pm.contextSizes[i].Store(int64(cs))
				p.logger.Infof("pool: backend %d (%s) auto-detected context size: %d", i, b.proxy, cs)
			}
		}
	}
}

func probeBackend(proxyURL string, parentCtx context.Context) (contextSize int, ok bool) {
	target, err := url.Parse(proxyURL)
	if err != nil {
		return 0, false
	}
	target = target.JoinPath("/props")

	reqCtx, cancel := context.WithTimeout(parentCtx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		return 0, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return 0, false
	}

	v := gjson.GetBytes(body, "default_generation_settings.n_ctx")
	if !v.Exists() {
		v = gjson.GetBytes(body, "n_ctx")
	}
	if n, err := strconv.Atoi(v.String()); err == nil && n > 0 {
		return n, true
	}
	return 0, true
}
