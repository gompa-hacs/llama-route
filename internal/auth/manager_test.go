package auth

import (
	"path/filepath"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestManager_CreateAndValidateKey(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		Admin: config.AdminConfig{KeysFile: filepath.Join(dir, "keys.json")},
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}

	pub, secret, err := m.CreateKey("test-key")
	if err != nil {
		t.Fatal(err)
	}
	if pub.ID == "" || secret == "" {
		t.Fatal("expected id and secret")
	}
	if !m.ValidateInferenceKey(secret) {
		t.Fatal("created key should validate")
	}
	if m.ValidateInferenceKey("wrong") {
		t.Fatal("wrong key should not validate")
	}
}

func TestManager_AdminLogin(t *testing.T) {
	cfg := config.Config{Admin: config.AdminConfig{Password: "hunter2"}}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !m.AdminRequired() {
		t.Fatal("admin should be required")
	}
	if _, _, err := m.Login("wrong"); err == nil {
		t.Fatal("expected login failure")
	}
	tok, _, err := m.Login("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !m.SessionValid(tok) {
		t.Fatal("session should be valid")
	}
}
