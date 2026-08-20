package config

import (
	"strings"
	"testing"
)

func TestLoadConfig_PoolStaticClearsDefaultProxy(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`
models:
  w:
    pool:
      backends:
        - proxy: http://127.0.0.1:8101
`))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Models["w"]
	if m.Proxy != "" {
		t.Fatalf("pool model proxy should be cleared, got %q", m.Proxy)
	}
	if !m.UsesPool() {
		t.Fatal("expected UsesPool")
	}
}

func TestLoadConfig_PoolSpawnAllocatesPorts(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`
startPort: 9000
models:
  whisper:
    pool:
      start: preload
      backends:
        - cmd: whisper-server --port ${PORT}
          env: ["CUDA_VISIBLE_DEVICES=0"]
        - cmd: whisper-server --port ${PORT}
          env: ["CUDA_VISIBLE_DEVICES=1"]
`))
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Models["whisper"].Pool.Backends
	if len(b) != 2 {
		t.Fatalf("backends=%d", len(b))
	}
	if !b[0].IsSpawn() || !b[1].IsSpawn() {
		t.Fatal("expected spawn backends")
	}
	if b[0].Proxy != "http://127.0.0.1:9000" {
		t.Fatalf("backend0 proxy=%q", b[0].Proxy)
	}
	if b[1].Proxy != "http://127.0.0.1:9001" {
		t.Fatalf("backend1 proxy=%q", b[1].Proxy)
	}
	if !strings.Contains(b[0].Cmd, "--port 9000") {
		t.Fatalf("backend0 cmd=%q", b[0].Cmd)
	}
	if !strings.Contains(b[1].Cmd, "--port 9001") {
		t.Fatalf("backend1 cmd=%q", b[1].Cmd)
	}
	if b[0].ProxyURL == nil || b[1].ProxyURL == nil {
		t.Fatal("expected ProxyURL parsed")
	}
}

func TestLoadConfig_PoolRejectsModelCmd(t *testing.T) {
	_, err := LoadConfigFromReader(strings.NewReader(`
models:
  w:
    cmd: something
    pool:
      backends:
        - proxy: http://127.0.0.1:1
`))
	if err == nil || !strings.Contains(err.Error(), "cannot set both model-level cmd and pool") {
		t.Fatalf("err=%v", err)
	}
}
