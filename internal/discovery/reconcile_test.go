package discovery

import (
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func offer(peer, model string, ctx int) Offer {
	return Offer{PeerID: peer, ModelID: model, ContextSize: ctx, Proxy: "http://" + peer}
}

func TestReconcile_UniqueBareAndFQ(t *testing.T) {
	plan := Reconcile([]Offer{offer("A", "qwen", 32768)}, config.Config{})
	if len(plan.PeerRoutes) != 2 {
		t.Fatalf("routes=%d want 2", len(plan.PeerRoutes))
	}
	if _, ok := plan.PeerRoutes["qwen"]; !ok {
		t.Fatal("missing bare qwen")
	}
	if _, ok := plan.PeerRoutes["A/qwen"]; !ok {
		t.Fatal("missing FQ route")
	}
	if len(plan.Pools) != 0 {
		t.Fatalf("pools=%d", len(plan.Pools))
	}
	if len(plan.Listings) != 1 || plan.Listings[0].ID != "qwen" {
		t.Fatalf("listings=%v want only bare qwen", plan.Listings)
	}
}

func TestReconcile_SameContextAutoPool(t *testing.T) {
	plan := Reconcile([]Offer{
		offer("A", "qwen", 32768),
		offer("B", "qwen", 32768),
	}, config.Config{})
	if len(plan.Pools) != 1 {
		t.Fatalf("pools=%d want 1", len(plan.Pools))
	}
	if len(plan.PeerRoutes) != 0 {
		t.Fatalf("peer routes=%d want 0", len(plan.PeerRoutes))
	}
	pool := plan.Pools[0]
	if pool.CanonicalID != "qwen" {
		t.Fatalf("canonical=%q", pool.CanonicalID)
	}
	if len(pool.Backends) != 2 {
		t.Fatalf("backends=%d", len(pool.Backends))
	}
	for _, key := range []string{"qwen", "A/qwen", "B/qwen"} {
		if _, ok := plan.PoolAliases[key]; !ok {
			t.Fatalf("missing pool alias %s", key)
		}
	}
	if len(plan.Listings) != 1 || plan.Listings[0].ID != "qwen" {
		t.Fatalf("listings=%v want only bare qwen", plan.Listings)
	}
}

func TestReconcile_DifferentContextFQOnly(t *testing.T) {
	plan := Reconcile([]Offer{
		offer("A", "qwen", 8192),
		offer("B", "qwen", 32768),
	}, config.Config{})
	if len(plan.Pools) != 0 {
		t.Fatalf("pools=%d", len(plan.Pools))
	}
	if _, ok := plan.PeerRoutes["qwen"]; ok {
		t.Fatal("bare qwen should not be registered")
	}
	if _, ok := plan.PeerRoutes["A/qwen"]; !ok {
		t.Fatal("missing A/qwen")
	}
	if _, ok := plan.PeerRoutes["B/qwen"]; !ok {
		t.Fatal("missing B/qwen")
	}
}

func TestReconcile_MixedContextWithPoolSubgroup(t *testing.T) {
	plan := Reconcile([]Offer{
		offer("A", "qwen", 8192),
		offer("B", "qwen", 32768),
		offer("C", "qwen", 32768),
	}, config.Config{})
	if _, ok := plan.PeerRoutes["qwen"]; ok {
		t.Fatal("bare should be absent when contexts conflict")
	}
	if _, ok := plan.PeerRoutes["A/qwen"]; !ok {
		t.Fatal("missing A/qwen")
	}
	if len(plan.Pools) != 1 {
		t.Fatalf("pools=%d want 1", len(plan.Pools))
	}
	if _, ok := plan.PoolAliases["B/qwen"]; !ok {
		t.Fatal("B/qwen should alias pool")
	}
	if _, ok := plan.PoolAliases["qwen"]; ok {
		t.Fatal("bare should not alias pool when multi-ctx")
	}
}

func TestReconcile_LocalShadowsBare(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{"qwen": {}}}
	plan := Reconcile([]Offer{offer("A", "qwen", 32768)}, cfg)
	if _, ok := plan.PeerRoutes["qwen"]; ok {
		t.Fatal("local owns bare qwen")
	}
	if _, ok := plan.PeerRoutes["A/qwen"]; !ok {
		t.Fatal("FQ should remain")
	}
}

func TestReconcile_LocalAliasShadowsBare(t *testing.T) {
	cfg, err := config.LoadConfigFromReader(strings.NewReader(`
models:
  real:
    cmd: /bin/true
    proxy: http://127.0.0.1:1
    aliases:
      - qwen
`))
	if err != nil {
		t.Fatal(err)
	}
	plan := Reconcile([]Offer{offer("A", "qwen", 32768)}, cfg)
	if _, ok := plan.PeerRoutes["qwen"]; ok {
		t.Fatal("local alias should own bare qwen")
	}
	if _, ok := plan.PeerRoutes["A/qwen"]; !ok {
		t.Fatal("FQ should remain")
	}
}

func TestReconcile_AllowlistViaOffersOnly(t *testing.T) {
	plan := Reconcile([]Offer{offer("A", "keep", 0)}, config.Config{})
	if len(plan.Listings) != 1 || plan.Listings[0].ID != "keep" {
		t.Fatalf("listings=%v want only bare keep", plan.Listings)
	}
}

func TestReconcile_UnknownContextPoolsTogether(t *testing.T) {
	plan := Reconcile([]Offer{
		offer("A", "m", 0),
		offer("B", "m", 0),
	}, config.Config{})
	if len(plan.Pools) != 1 {
		t.Fatalf("pools=%d", len(plan.Pools))
	}
}
