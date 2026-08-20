package discovery

import (
	"context"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

const DefaultInterval = 30 * time.Second

// PoolApplier installs discovery-owned auto-pools.
type PoolApplier interface {
	ReplaceDiscovered(pools []DiscoveredPool) error
}

// PeerApplier installs discovery-owned peer routes (bare + FQ).
type PeerApplier interface {
	ReplaceDiscovered(routes map[string]PeerRoute) error
}

// Manager polls peers, reconciles offers, and applies routes.
type Manager struct {
	cfg      config.Config
	logger   *logmon.Monitor
	interval time.Duration
	pool     PoolApplier
	peer     PeerApplier
	client   *http.Client

	mu          sync.RWMutex
	plan        RoutePlan
	lastGood    map[string][]Offer // peerID -> last successful offers
	filterIndex map[string]FilterResolution
	cancel      context.CancelFunc
	done        chan struct{}
}

// NewManager creates a discovery manager. Call Start to begin polling.
func NewManager(cfg config.Config, logger *logmon.Monitor, pool PoolApplier, peer PeerApplier) *Manager {
	return &Manager{
		cfg:      cfg,
		logger:   logger,
		interval: DefaultInterval,
		pool:     pool,
		peer:     peer,
		client: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:        32,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
		lastGood:    make(map[string][]Offer),
		filterIndex: make(map[string]FilterResolution),
		done:        make(chan struct{}),
	}
}

// Start begins the discovery loop. The first poll runs synchronously so routes
// exist before Start returns; later refreshes run in the background.
func (m *Manager) Start(ctx context.Context) {
	ctx, m.cancel = context.WithCancel(ctx)
	m.pollOnce(ctx)
	go m.run(ctx)
}

// Stop cancels polling and waits for the loop to exit.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
		<-m.done
	}
}

// SetPlanForTest installs a reconciled plan without polling (tests only).
func (m *Manager) SetPlanForTest(plan RoutePlan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plan = plan
	m.filterIndex = buildFilterIndex(plan)
}

// Listings returns the current discovered model listings (copy).
func (m *Manager) Listings() []Listing {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Listing, len(m.plan.Listings))
	copy(out, m.plan.Listings)
	return out
}

// ResolveFilters returns useModelName / peer filters for a discovered route.
func (m *Manager) ResolveFilters(requested string) FilterResolution {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.filterIndex[requested]
}

func (m *Manager) run(ctx context.Context) {
	defer close(m.done)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pollOnce(ctx)
		}
	}
}

func (m *Manager) pollOnce(ctx context.Context) {
	if len(m.cfg.Peers) == 0 {
		return
	}

	type result struct {
		peerID string
		offers []Offer
		err    error
	}

	ch := make(chan result, len(m.cfg.Peers))
	var wg sync.WaitGroup
	for peerID, peer := range m.cfg.Peers {
		wg.Add(1)
		go func(peerID string, peer config.PeerConfig) {
			defer wg.Done()
			peerCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
			defer cancel()
			offers, err := fetchPeerOffers(peerCtx, peerID, peer, m.client)
			ch <- result{peerID: peerID, offers: offers, err: err}
		}(peerID, peer)
	}
	wg.Wait()
	close(ch)

	m.mu.Lock()
	for res := range ch {
		if res.err != nil {
			if m.logger != nil {
				m.logger.Warnf("discovery: peer %s: %v (keeping last-good)", res.peerID, res.err)
			}
			continue
		}
		m.lastGood[res.peerID] = res.offers
	}

	all := make([]Offer, 0)
	for _, peerID := range sortedPeerIDs(m.cfg.Peers) {
		all = append(all, m.lastGood[peerID]...)
	}
	plan := Reconcile(all, m.cfg)
	filterIndex := buildFilterIndex(plan)
	m.plan = plan
	m.filterIndex = filterIndex
	m.mu.Unlock()

	if m.pool != nil {
		if err := m.pool.ReplaceDiscovered(plan.Pools); err != nil && m.logger != nil {
			m.logger.Warnf("discovery: applying pools: %v", err)
		}
	}
	if m.peer != nil {
		if err := m.peer.ReplaceDiscovered(plan.PeerRoutes); err != nil && m.logger != nil {
			m.logger.Warnf("discovery: applying peer routes: %v", err)
		}
	}

	if m.logger != nil {
		m.logger.Infof("discovery: %d listings, %d auto-pools, %d peer routes",
			len(plan.Listings), len(plan.Pools), len(plan.PeerRoutes))
	}
}

func buildFilterIndex(plan RoutePlan) map[string]FilterResolution {
	idx := make(map[string]FilterResolution)

	for key, route := range plan.PeerRoutes {
		useName := ""
		if key != route.UpstreamModelID {
			useName = route.UpstreamModelID
		}
		idx[key] = FilterResolution{
			UseModelName: useName,
			Filters:      route.Filters,
			OK:           true,
		}
	}

	for _, pool := range plan.Pools {
		for _, key := range pool.RouteKeys {
			useName := ""
			if key != pool.UpstreamModelID {
				useName = pool.UpstreamModelID
			}
			idx[key] = FilterResolution{
				UseModelName: useName,
				Filters:      config.Filters{}, // pooled peers may differ; rewrite only
				OK:           true,
			}
		}
	}
	return idx
}

func sortedPeerIDs(peers config.PeerDictionaryConfig) []string {
	ids := make([]string, 0, len(peers))
	for id := range peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
