package router

import (
	"io"
	"net/url"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/discovery"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

func TestPool_ReplaceDiscovered_DoesNotTouchYAMLPools(t *testing.T) {
	proxyURL, _ := url.Parse("http://127.0.0.1:9")
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"yaml-pool": {
				Pool: &config.PoolConfig{
					Backends: []config.PoolBackend{{Proxy: "http://127.0.0.1:9", ProxyURL: proxyURL}},
				},
			},
		},
	}
	logger := logmon.NewWriter(io.Discard)
	pool, err := NewPool(cfg, logger, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown(0)

	if !pool.Handles("yaml-pool") {
		t.Fatal("yaml pool missing")
	}

	err = pool.ReplaceDiscovered([]discovery.DiscoveredPool{{
		CanonicalID:     "discovered",
		UpstreamModelID: "discovered",
		RouteKeys:       []string{"discovered", "A/discovered"},
		Backends: []discovery.DiscoveredBackend{
			{PeerID: "A", Proxy: "http://127.0.0.1:10", ContextSize: 8192},
			{PeerID: "B", Proxy: "http://127.0.0.1:11", ContextSize: 8192},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	if !pool.Handles("yaml-pool") {
		t.Fatal("yaml pool should remain")
	}
	if !pool.Handles("discovered") || !pool.Handles("A/discovered") {
		t.Fatal("discovered routes missing")
	}

	// Removing discovered pools should keep YAML.
	if err := pool.ReplaceDiscovered(nil); err != nil {
		t.Fatal(err)
	}
	if !pool.Handles("yaml-pool") {
		t.Fatal("yaml pool removed incorrectly")
	}
	if pool.Handles("discovered") {
		t.Fatal("discovered route should be gone")
	}
}
