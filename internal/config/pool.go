package config

import (
	"fmt"
	"net/url"
	"time"
)

// PoolConfig routes a model to one or more always-on upstream backends with
// sticky least-inflight load balancing.
type PoolConfig struct {
	Strategy    string         `yaml:"strategy"`
	AffinityTTL string         `yaml:"affinityTTL"`
	Backends    []PoolBackend  `yaml:"backends"`
	Affinity    []AffinityRule `yaml:"affinity"`
}

// PoolBackend is a single upstream base URL for a pooled model.
type PoolBackend struct {
	Proxy    string   `yaml:"proxy"`
	ProxyURL *url.URL `yaml:"-"`
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
	if err != nil || d <= 0 {
		return 30 * time.Minute
	}
	return d
}

func (p *PoolConfig) StrategyName() string {
	if p == nil || p.Strategy == "" {
		return "sticky_least_inflight"
	}
	return p.Strategy
}

func validatePoolConfig(modelID string, pool *PoolConfig) error {
	if pool == nil {
		return nil
	}
	if len(pool.Backends) == 0 {
		return fmt.Errorf("model %s: pool.backends must not be empty", modelID)
	}
	switch pool.StrategyName() {
	case "sticky_least_inflight":
	default:
		return fmt.Errorf("model %s: pool.strategy %q is not supported (valid: sticky_least_inflight)", modelID, pool.Strategy)
	}
	if pool.AffinityTTL != "" {
		if d, err := time.ParseDuration(pool.AffinityTTL); err != nil || d <= 0 {
			return fmt.Errorf("model %s: invalid pool.affinityTTL %q", modelID, pool.AffinityTTL)
		}
	}
	for i := range pool.Backends {
		b := &pool.Backends[i]
		if b.Proxy == "" {
			return fmt.Errorf("model %s: pool.backends[%d].proxy is required", modelID, i)
		}
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

func (p *PoolConfig) EffectiveAffinityRules() []AffinityRule {
	if p == nil || len(p.Affinity) == 0 {
		return defaultAffinityRules()
	}
	return p.Affinity
}
