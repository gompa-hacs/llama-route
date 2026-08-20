package discovery

import "github.com/mostlygeek/llama-swap/internal/config"

// Offer is one model advertised by a peer after allowlist filtering.
type Offer struct {
	PeerID      string
	ModelID     string
	ContextSize int
	Proxy       string
	ApiKey      string
	Filters     config.Filters
}

// FQName returns the fully qualified peer model ID peerID/modelID.
func FQName(peerID, modelID string) string {
	return peerID + "/" + modelID
}

// DiscoveredBackend is one static upstream in an auto-pool.
type DiscoveredBackend struct {
	PeerID          string
	Proxy           string
	ContextSize     int
	ApiKey          string
	UpstreamModelID string
}

// DiscoveredPool is a sticky pool synthesized from identical (model, context) offers.
type DiscoveredPool struct {
	// CanonicalID is used for logging; typically the bare model ID when
	// unambiguous, otherwise the first FQ alias.
	CanonicalID     string
	UpstreamModelID string
	RouteKeys       []string // bare and/or FQ keys that map to this pool
	Backends        []DiscoveredBackend
}

// PeerRoute is a single-backend discovered route (not pooled).
type PeerRoute struct {
	PeerID          string
	UpstreamModelID string
	Proxy           string
	ApiKey          string
	Filters         config.Filters
	ContextSize     int
}

// Listing is one entry for /v1/models and the dashboard.
type Listing struct {
	ID              string // client-visible ID (bare or FQ)
	PeerID          string // empty for bare pooled listings
	UpstreamModelID string
	ContextSize     int
	Pooled          bool
	Discovered      bool
}

// RoutePlan is the reconciled result of discovery.
type RoutePlan struct {
	Pools      []DiscoveredPool
	PeerRoutes map[string]PeerRoute // route key (bare or FQ) -> peer route
	// PoolAliases maps every pool route key to the pool index in Pools.
	PoolAliases map[string]int
	Listings    []Listing
}

// FilterResolution is used by request-filter middleware for discovered models.
type FilterResolution struct {
	UseModelName string
	Filters      config.Filters
	OK           bool
}
