package peermetrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/ring"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

// peerPerfRingCapacity matches the local perf monitor (~1h at 5s samples).
const peerPerfRingCapacity = 720

// FetcherConfig holds configuration for the peer metrics fetcher.
type FetcherConfig struct {
	Interval time.Duration `yaml:"interval" json:"interval"`
	Timeout  time.Duration `yaml:"timeout" json:"timeout"`
}

// DefaultFetcherConfig returns sensible defaults.
func DefaultFetcherConfig() FetcherConfig {
	return FetcherConfig{
		Interval: 15 * time.Second,
		Timeout:  5 * time.Second,
	}
}

// PeerSnapshot holds metrics and performance data from a single peer.
type PeerSnapshot struct {
	PeerName string                   `json:"peer_name"`
	Success  bool                     `json:"success"`
	Error    string                   `json:"error,omitempty"`
	Metrics  []shared.PeerMetricEntry `json:"metrics,omitempty"`
	SysStats []perf.SysStat           `json:"sys_stats,omitempty"`
	GpuStats []perf.GpuStat           `json:"gpu_stats,omitempty"`
	LastSeen time.Time                `json:"last_seen"`
}

// LatestPeerMetrics holds aggregated data from all peers at a single poll.
type LatestPeerMetrics struct {
	PollTime time.Time               `json:"poll_time"`
	Peers    map[string]PeerSnapshot `json:"peers"`
}

// PeerPerformanceUpdateEvent is emitted when peer sys/GPU rings are refreshed.
type PeerPerformanceUpdateEvent struct {
	Snapshot LatestPeerMetrics
}

func (e PeerPerformanceUpdateEvent) Type() uint32 {
	return shared.PeerPerformanceUpdateEventID
}

type peerPerfState struct {
	lastAfter time.Time
	sysRing   ring.Buffer[perf.SysStat]
	gpuRing   ring.Buffer[perf.GpuStat]
}

// Fetcher periodically polls peers for their metrics and performance data.
type Fetcher struct {
	peers  config.PeerDictionaryConfig
	cfg    FetcherConfig
	logger *logmon.Monitor

	mu       sync.RWMutex
	latest   LatestPeerMetrics
	history  ring.Buffer[LatestPeerMetrics]
	peerPerf map[string]*peerPerfState

	cancel context.CancelFunc
	done   chan struct{}
}

// NewFetcher creates a Fetcher.
func NewFetcher(peers config.PeerDictionaryConfig, cfg FetcherConfig, logger *logmon.Monitor) *Fetcher {
	if cfg.Interval <= 0 {
		cfg = DefaultFetcherConfig()
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	peerPerf := make(map[string]*peerPerfState, len(peers))
	for name := range peers {
		peerPerf[name] = newPeerPerfState()
	}
	return &Fetcher{
		peers:    peers,
		cfg:      cfg,
		logger:   logger,
		peerPerf: peerPerf,
		history:  ring.NewBuffer[LatestPeerMetrics](100),
		done:     make(chan struct{}),
	}
}

func newPeerPerfState() *peerPerfState {
	return &peerPerfState{
		sysRing: ring.NewBuffer[perf.SysStat](peerPerfRingCapacity),
		gpuRing: ring.NewBuffer[perf.GpuStat](peerPerfRingCapacity),
	}
}

// Start begins periodic polling in a background goroutine.
func (f *Fetcher) Start(ctx context.Context) {
	ctx, f.cancel = context.WithCancel(ctx)
	go f.run(ctx)
}

// Stop cancels the polling goroutine and waits for it to exit.
func (f *Fetcher) Stop() {
	if f.cancel != nil {
		f.cancel()
		<-f.done
	}
}

// GetLatest returns a copy of the most recent poll results.
func (f *Fetcher) GetLatest() LatestPeerMetrics {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.latest
}

// GetHistory returns the last N poll snapshots.
func (f *Fetcher) GetHistory() []LatestPeerMetrics {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.history.Slice()
}

func (f *Fetcher) peerPerfSnapshot(name string) (sys []perf.SysStat, gpu []perf.GpuStat) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	state, ok := f.peerPerf[name]
	if !ok || state == nil {
		return nil, nil
	}
	return state.sysRing.Slice(), state.gpuRing.Slice()
}

func (f *Fetcher) mergePeerPerf(name string, sys []perf.SysStat, gpu []perf.GpuStat) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.peerPerf[name]
	if !ok || state == nil {
		state = newPeerPerfState()
		f.peerPerf[name] = state
	}
	after := state.lastAfter
	for _, st := range sys {
		if !after.IsZero() && !st.Timestamp.After(after) {
			continue
		}
		state.sysRing.Push(st)
		if st.Timestamp.After(state.lastAfter) {
			state.lastAfter = st.Timestamp
		}
	}
	for _, g := range gpu {
		if !after.IsZero() && !g.Timestamp.After(after) {
			continue
		}
		state.gpuRing.Push(g)
		if g.Timestamp.After(state.lastAfter) {
			state.lastAfter = g.Timestamp
		}
	}
}

// run is the main polling loop.
func (f *Fetcher) run(ctx context.Context) {
	defer close(f.done)

	ticker := time.NewTicker(f.cfg.Interval)
	defer ticker.Stop()

	f.pollAll()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.pollAll()
		}
	}
}

// pollAll polls all peers in parallel and aggregates the results.
func (f *Fetcher) pollAll() {
	latest := LatestPeerMetrics{
		PollTime: time.Now(),
		Peers:    make(map[string]PeerSnapshot, len(f.peers)),
	}

	var wg sync.WaitGroup
	var mu sync.Mutex

	for name, peer := range f.peers {
		wg.Add(1)
		go func(name string, peer config.PeerConfig) {
			defer wg.Done()
			snapshot := f.pollPeer(name, peer)

			mu.Lock()
			latest.Peers[name] = snapshot
			mu.Unlock()
		}(name, peer)
	}

	wg.Wait()

	f.mu.Lock()
	f.latest = latest
	f.history.Push(latest)
	f.mu.Unlock()

	f.emitEvents(latest)
}

// pollPeer fetches metrics and performance data from a single peer.
func (f *Fetcher) pollPeer(name string, peer config.PeerConfig) PeerSnapshot {
	snapshot := PeerSnapshot{
		PeerName: name,
		LastSeen: time.Now(),
		Success:  true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), f.cfg.Timeout)
	defer cancel()

	client := &http.Client{Timeout: f.cfg.Timeout}

	metricsURL := peer.ProxyURL.JoinPath("/api/metrics").String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metricsURL, nil)
	if err != nil {
		snapshot.Success = false
		snapshot.Error = fmt.Sprintf("creating metrics request: %v", err)
		snapshot.SysStats, snapshot.GpuStats = f.peerPerfSnapshot(name)
		return snapshot
	}
	if peer.ApiKey != "" {
		req.Header.Set("Authorization", "Bearer "+peer.ApiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		snapshot.Success = false
		snapshot.Error = fmt.Sprintf("fetching metrics: %v", err)
		snapshot.SysStats, snapshot.GpuStats = f.peerPerfSnapshot(name)
		return snapshot
	}

	if resp.StatusCode == http.StatusOK {
		var entries []struct {
			Model     string    `json:"model"`
			Timestamp time.Time `json:"timestamp"`
			ReqPath   string    `json:"req_path"`
			Tokens    struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"tokens"`
			DurationMs int `json:"duration_ms"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
			resp.Body.Close()
			snapshot.Success = false
			snapshot.Error = fmt.Sprintf("decoding metrics: %v", err)
			snapshot.SysStats, snapshot.GpuStats = f.peerPerfSnapshot(name)
			return snapshot
		}
		resp.Body.Close()

		metrics := make([]shared.PeerMetricEntry, 0, len(entries))
		for _, e := range entries {
			metrics = append(metrics, shared.PeerMetricEntry{
				PeerName:  name,
				Model:     e.Model,
				Timestamp: e.Timestamp,
				ReqPath:   e.ReqPath,
				Tokens: map[string]int{
					"input_tokens":  e.Tokens.InputTokens,
					"output_tokens": e.Tokens.OutputTokens,
				},
				DurationMs: e.DurationMs,
			})
		}
		snapshot.Metrics = metrics
	} else {
		resp.Body.Close()
	}

	sys, gpu, perfErr := f.fetchPeerPerformance(ctx, client, peer, name)
	if perfErr != nil {
		if snapshot.Error == "" {
			snapshot.Success = false
			snapshot.Error = perfErr.Error()
		}
		snapshot.SysStats, snapshot.GpuStats = f.peerPerfSnapshot(name)
		return snapshot
	}

	f.mergePeerPerf(name, sys, gpu)
	snapshot.SysStats, snapshot.GpuStats = f.peerPerfSnapshot(name)
	return snapshot
}

func (f *Fetcher) fetchPeerPerformance(ctx context.Context, client *http.Client, peer config.PeerConfig, peerName string) ([]perf.SysStat, []perf.GpuStat, error) {
	f.mu.RLock()
	var after time.Time
	if state := f.peerPerf[peerName]; state != nil {
		after = state.lastAfter
	}
	f.mu.RUnlock()

	perfURL := peer.ProxyURL.JoinPath("/api/performance").String()
	if !after.IsZero() {
		u, err := url.Parse(perfURL)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing performance URL: %w", err)
		}
		q := u.Query()
		q.Set("after", after.Format(time.RFC3339))
		u.RawQuery = q.Encode()
		perfURL = u.String()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, perfURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("creating performance request: %v", err)
	}
	if peer.ApiKey != "" {
		req.Header.Set("Authorization", "Bearer "+peer.ApiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching performance: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("performance status %d", resp.StatusCode)
	}

	var perfData struct {
		SysStats []perf.SysStat `json:"sys_stats"`
		GpuStats []perf.GpuStat `json:"gpu_stats"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&perfData); err != nil {
		return nil, nil, fmt.Errorf("decoding performance: %v", err)
	}
	return perfData.SysStats, perfData.GpuStats, nil
}

func (f *Fetcher) emitEvents(latest LatestPeerMetrics) {
	var allMetrics []shared.PeerMetricEntry
	for _, peer := range latest.Peers {
		allMetrics = append(allMetrics, peer.Metrics...)
	}
	event.Emit(shared.PeerMetricsUpdateEvent{Peers: allMetrics})
	event.Emit(PeerPerformanceUpdateEvent{Snapshot: latest})
}
