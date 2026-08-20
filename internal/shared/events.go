package shared

import "time"

const ProcessStateChangeEventID = 0x01
const ConfigFileChangedEventID = 0x03
const ActivityLogEventID = 0x05
const ModelPreloadedEventID = 0x06
const InFlightRequestsEventID = 0x07
const PeerMetricsUpdateEventID = 0x08
const PoolMetricsUpdateEventID = 0x09

// ProcessStateChangeEvent is emitted whenever a process transitions between
// lifecycle states. States are carried as strings so this package stays a leaf
// (no import of internal/process).
type ProcessStateChangeEvent struct {
	ProcessName string
	OldState    string
	NewState    string
}

func (e ProcessStateChangeEvent) Type() uint32 {
	return ProcessStateChangeEventID
}

type ReloadingState int

const (
	ReloadingStateStart ReloadingState = iota
	ReloadingStateEnd
)

type ConfigFileChangedEvent struct {
	State ReloadingState
}

func (e ConfigFileChangedEvent) Type() uint32 {
	return ConfigFileChangedEventID
}

type ModelPreloadedEvent struct {
	ModelName string
	Success   bool
}

func (e ModelPreloadedEvent) Type() uint32 {
	return ModelPreloadedEventID
}

type InFlightRequestsEvent struct {
	Total int
}

func (e InFlightRequestsEvent) Type() uint32 {
	return InFlightRequestsEventID
}

// PeerMetricEntry is a metric from a peer, labeled with the peer name.
type PeerMetricEntry struct {
	PeerName   string         `json:"peer_name"`
	Model      string         `json:"model"`
	Timestamp  time.Time      `json:"timestamp"`
	ReqPath    string         `json:"req_path"`
	Tokens     map[string]int `json:"tokens"`
	DurationMs int            `json:"duration_ms"`
}

// PeerMetricsUpdateEvent is emitted whenever the central server completes a
// poll of peer metrics/performance data.
type PeerMetricsUpdateEvent struct {
	Peers []PeerMetricEntry `json:"peers"`
}

func (e PeerMetricsUpdateEvent) Type() uint32 {
	return PeerMetricsUpdateEventID
}

// PoolBackendMetric is one upstream backend's live load-balancer state.
type PoolBackendMetric struct {
	ID               int    `json:"id"`
	Proxy            string `json:"proxy"`
	Inflight         int    `json:"inflight"`
	ContextSize      int    `json:"context_size"`
	AffinitySessions int    `json:"affinity_sessions"`
	Healthy          bool   `json:"healthy"`
	Kind             string `json:"kind"`            // "static" or "spawn"
	State            string `json:"state,omitempty"` // process state for spawn backends
	ProcessID        string `json:"process_id,omitempty"`
}

// PoolModelMetric is the aggregated state for one pooled model.
type PoolModelMetric struct {
	ModelID          string              `json:"model_id"`
	Strategy         string              `json:"strategy"`
	Backends         []PoolBackendMetric `json:"backends"`
	AffinitySessions int                 `json:"affinity_sessions"`
}

// PoolMetricsSnapshot is a point-in-time view of all pool models.
type PoolMetricsSnapshot struct {
	Timestamp time.Time         `json:"timestamp"`
	Models    []PoolModelMetric `json:"models"`
}

// PoolMetricsUpdateEvent is emitted when pool backend load state is refreshed.
type PoolMetricsUpdateEvent struct {
	Snapshot PoolMetricsSnapshot `json:"snapshot"`
}

func (e PoolMetricsUpdateEvent) Type() uint32 {
	return PoolMetricsUpdateEventID
}
