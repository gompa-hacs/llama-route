package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// PoolConfig routes a model to one or more upstream backends with sticky
// least-inflight load balancing. Backends may be static URLs or spawnable
// sidecars (cmd + optional proxy).
type PoolConfig struct {
	Strategy    string `yaml:"strategy"`
	AffinityTTL string `yaml:"affinityTTL"`
	// Start controls when spawnable backends are launched:
	//   on_demand (default) — start on first request that needs the backend
	//   preload             — start all spawn backends when the pool is created
	Start string `yaml:"start"`
	// RestartOnCrash restarts spawn backends that exit unexpectedly.
	// nil / omitted defaults to true.
	RestartOnCrash *bool          `yaml:"restartOnCrash"`
	Backends       []PoolBackend  `yaml:"backends"`
	Affinity       []AffinityRule `yaml:"affinity"`
}

// PoolBackend is a single upstream for a pooled model. Provide either a static
// proxy URL, or a cmd (spawnable sidecar). When cmd is set and proxy is empty,
// proxy defaults to http://127.0.0.1:${PORT}.
type PoolBackend struct {
	Proxy       string   `yaml:"proxy"`
	ContextSize int      `yaml:"contextSize"`
	Cmd         string   `yaml:"cmd"`
	CmdStop     string   `yaml:"cmdStop"`
	Env         []string `yaml:"env"`
	// CheckEndpoint overrides the model-level health check path for this backend.
	CheckEndpoint string `yaml:"checkEndpoint"`
	// Compat selects upstream protocol shims (e.g. "whisper"). Empty inherits
	// the model-level compat value.
	Compat string `yaml:"compat"`

	ProxyURL *url.URL `yaml:"-"`

	// WhisperCompat is set during config load when OpenAI→/inference rewrite
	// should be applied for this backend.
	WhisperCompat bool `yaml:"-"`
}

// IsSpawn reports whether this backend is started by llama-swap.
func (b PoolBackend) IsSpawn() bool {
	return strings.TrimSpace(b.Cmd) != ""
}

// AffinityRule names one source for a sticky-session key. Rules are tried in
// order; the first non-empty value wins.
type AffinityRule struct {
	JSON   string `yaml:"json"`
	Header string `yaml:"header"`
	APIKey bool   `yaml:"apiKey"`
}

func (c *Config) IsPoolModel(modelID string) bool {
	real, ok := c.RealModelName(modelID)
	if !ok {
		if mc, found := c.Models[modelID]; found {
			return mc.Pool != nil && len(mc.Pool.Backends) > 0
		}
		return false
	}
	mc, ok := c.Models[real]
	return ok && mc.Pool != nil && len(mc.Pool.Backends) > 0
}

func (p *PoolConfig) AffinityDuration() time.Duration {
	if p == nil || p.AffinityTTL == "" {
		return 30 * time.Minute
	}
	d, err := time.ParseDuration(p.AffinityTTL)
	if err != nil {
		return 30 * time.Minute
	}
	// 0 disables sticky affinity mappings.
	if d <= 0 {
		return 0
	}
	return d
}

func (p *PoolConfig) StrategyName() string {
	if p == nil || p.Strategy == "" {
		return "sticky_least_inflight"
	}
	return p.Strategy
}

// StartMode returns when spawn backends should be launched.
func (p *PoolConfig) StartMode() string {
	if p == nil || p.Start == "" {
		return "on_demand"
	}
	return p.Start
}

// ShouldRestartOnCrash reports whether spawn backends auto-restart.
func (p *PoolConfig) ShouldRestartOnCrash() bool {
	if p == nil || p.RestartOnCrash == nil {
		return true
	}
	return *p.RestartOnCrash
}

func (p *PoolConfig) HasSpawnBackends() bool {
	if p == nil {
		return false
	}
	for _, b := range p.Backends {
		if b.IsSpawn() {
			return true
		}
	}
	return false
}

func validatePoolConfig(modelID string, pool *PoolConfig, modelCmd string) error {
	if pool == nil {
		return nil
	}
	if len(pool.Backends) == 0 {
		return fmt.Errorf("model %s: pool.backends must not be empty", modelID)
	}
	if strings.TrimSpace(modelCmd) != "" {
		return fmt.Errorf("model %s: cannot set both model-level cmd and pool (put cmd on pool.backends[] instead)", modelID)
	}
	switch pool.StrategyName() {
	case "sticky_least_inflight":
	default:
		return fmt.Errorf("model %s: pool.strategy %q is not supported (valid: sticky_least_inflight)", modelID, pool.Strategy)
	}
	switch pool.StartMode() {
	case "on_demand", "preload":
	default:
		return fmt.Errorf("model %s: pool.start %q is not supported (valid: on_demand, preload)", modelID, pool.Start)
	}
	if pool.AffinityTTL != "" {
		if _, err := time.ParseDuration(pool.AffinityTTL); err != nil {
			return fmt.Errorf("model %s: invalid pool.affinityTTL %q", modelID, pool.AffinityTTL)
		}
	}
	for i := range pool.Backends {
		b := &pool.Backends[i]
		b.Cmd = StripComments(b.Cmd)
		b.CmdStop = StripComments(b.CmdStop)

		if b.IsSpawn() {
			if b.Proxy == "" {
				b.Proxy = "http://127.0.0.1:${PORT}"
			}
		} else if b.Proxy == "" {
			return fmt.Errorf("model %s: pool.backends[%d].proxy is required (or set cmd to spawn a sidecar)", modelID, i)
		}

		if strings.Contains(b.Proxy, "${PORT}") {
			// Final URL parse happens after PORT allocation in config load.
		} else {
			parsed, err := url.Parse(b.Proxy)
			if err != nil {
				return fmt.Errorf("model %s: pool.backends[%d].proxy invalid URL: %w", modelID, i, err)
			}
			b.ProxyURL = parsed
		}

		if b.ContextSize < 0 {
			return fmt.Errorf("model %s: pool.backends[%d].contextSize must be >= 0", modelID, i)
		}
	}
	return nil
}

// finalizePoolBackendURLs parses ProxyURL after PORT substitution.
func finalizePoolBackendURLs(modelID string, pool *PoolConfig) error {
	if pool == nil {
		return nil
	}
	for i := range pool.Backends {
		b := &pool.Backends[i]
		parsed, err := url.Parse(b.Proxy)
		if err != nil {
			return fmt.Errorf("model %s: pool.backends[%d].proxy invalid URL: %w", modelID, i, err)
		}
		b.ProxyURL = parsed
	}
	return nil
}

func defaultAffinityRules() []AffinityRule {
	return []AffinityRule{
		{JSON: "id_slot"},
		{JSON: "previous_response_id"},
		{JSON: "conversation.id"},
		{JSON: "prompt_cache_key"},
		{JSON: "user"},
		{JSON: "safety_identifier"},
		{JSON: "metadata.user_id"},
		{JSON: "metadata.session_id"},
		{JSON: "metadata.conversation_id"},
		{Header: "X-Conversation-Id"},
		{Header: "X-Session-Id"},
		{APIKey: true},
	}
}

// DefaultAffinityRules is the sticky-session rule chain used when pool.affinity
// is omitted.
func DefaultAffinityRules() []AffinityRule {
	return defaultAffinityRules()
}

func (p *PoolConfig) EffectiveAffinityRules() []AffinityRule {
	if p == nil || len(p.Affinity) == 0 {
		return defaultAffinityRules()
	}
	return p.Affinity
}

// ResolveAffinityRules returns the affinity rules for a pool model.
// Explicit pool.affinity wins. Otherwise whisper.cpp backends get no sticky
// rules (API-key stickiness would pin all transcriptions to one GPU). Other
// pools keep the default LLM-oriented chain.
func ResolveAffinityRules(p *PoolConfig) []AffinityRule {
	if p == nil {
		return defaultAffinityRules()
	}
	if len(p.Affinity) > 0 {
		return p.Affinity
	}
	if p.AllWhisperCompat() {
		return []AffinityRule{}
	}
	return defaultAffinityRules()
}

// AllWhisperCompat reports whether every backend uses whisper.cpp path rewriting.
func (p *PoolConfig) AllWhisperCompat() bool {
	if p == nil || len(p.Backends) == 0 {
		return false
	}
	for _, b := range p.Backends {
		if !b.WhisperCompat {
			return false
		}
	}
	return true
}
