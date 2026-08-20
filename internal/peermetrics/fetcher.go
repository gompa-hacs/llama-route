package peermetrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/ring"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

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

// Fetcher periodically polls peers for their metrics and performance data.
type Fetcher struct {
	peers  config.PeerDictionaryConfig
	cfg    FetcherConfig
	logger *logmon.Monitor

	mu      sync.RWMutex
	latest  LatestPeerMetrics
	history ring.Buffer[LatestPeerMetrics]

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
	return &Fetcher{
		peers:   peers,
		cfg:     cfg,
		logger:  logger,
		history: ring.NewBuffer[LatestPeerMetrics](100),
		done:    make(chan struct{}),
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

// run is the main polling loop.
func (f *Fetcher) run(ctx context.Context) {
	defer close(f.done)

	ticker := time.NewTicker(f.cfg.Interval)
	defer ticker.Stop()

	// Initial poll.
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

	// Emit event so SSE subscribers can update.
	f.emitEvent(latest)
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

	// Fetch activity metrics.
	metricsURL := peer.ProxyURL.JoinPath("/api/metrics").String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metricsURL, nil)
	if err != nil {
		snapshot.Success = false
		snapshot.Error = fmt.Sprintf("creating metrics request: %v", err)
		return snapshot
	}
	if peer.ApiKey != "" {
		req.Header.Set("Authorization", "Bearer "+peer.ApiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		snapshot.Success = false
		snapshot.Error = fmt.Sprintf("fetching metrics: %v", err)
		return snapshot
	}

	if resp.StatusCode == http.StatusOK {
		// Decode the ActivityLogEntry array and convert to PeerMetricEntry.
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

	// Fetch performance data.
	perfURL := peer.ProxyURL.JoinPath("/api/performance").String()
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, perfURL, nil)
	if err != nil {
		snapshot.Success = false
		snapshot.Error = fmt.Sprintf("creating performance request: %v", err)
		return snapshot
	}
	if peer.ApiKey != "" {
		req.Header.Set("Authorization", "Bearer "+peer.ApiKey)
	}

	resp, err = client.Do(req)
	if err != nil {
		snapshot.Success = false
		snapshot.Error = fmt.Sprintf("fetching performance: %v", err)
		return snapshot
	}

	if resp.StatusCode == http.StatusOK {
		var perfData struct {
			SysStats []perf.SysStat `json:"sys_stats"`
			GpuStats []perf.GpuStat `json:"gpu_stats"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&perfData); err != nil {
			resp.Body.Close()
			snapshot.Success = false
			snapshot.Error = fmt.Sprintf("decoding performance: %v", err)
			return snapshot
		}
		resp.Body.Close()
		snapshot.SysStats = perfData.SysStats
		snapshot.GpuStats = perfData.GpuStats
	} else {
		resp.Body.Close()
	}

	return snapshot
}

// emitEvent emits a PeerMetricsUpdateEvent to the event bus.
func (f *Fetcher) emitEvent(latest LatestPeerMetrics) {
	var allMetrics []shared.PeerMetricEntry
	for _, peer := range latest.Peers {
		allMetrics = append(allMetrics, peer.Metrics...)
	}
	event.Emit(shared.PeerMetricsUpdateEvent{Peers: allMetrics})
}
