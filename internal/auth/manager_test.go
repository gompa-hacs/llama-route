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

	pub, secret, err := m.CreateKey("test-key", KeyLimits{})
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

func TestManager_KeyLimits(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(config.Config{
		Admin: config.AdminConfig{KeysFile: filepath.Join(dir, "keys.json")},
	})
	if err != nil {
		t.Fatal(err)
	}

	pub, secret, err := m.CreateKey("limited", KeyLimits{
		Models:    []string{"llama-70b", " whisper "},
		MaxTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pub.Models) != 2 || pub.Models[0] != "llama-70b" || pub.Models[1] != "whisper" {
		t.Fatalf("models = %#v", pub.Models)
	}
	if pub.MaxTokens != 2048 {
		t.Fatalf("maxTokens = %d", pub.MaxTokens)
	}

	policy, ok := m.AuthenticateInferenceKey(secret)
	if !ok {
		t.Fatal("expected auth success")
	}
	if !policy.AllowsModel("llama-70b", "llama-70b") {
		t.Fatal("expected llama-70b allowed")
	}
	if !policy.AllowsModel("alias", "whisper") {
		t.Fatal("expected alias resolved to whisper allowed")
	}
	if policy.AllowsModel("other", "other") {
		t.Fatal("expected other model denied")
	}
	if policy.MaxTokens != 2048 {
		t.Fatalf("policy maxTokens = %d", policy.MaxTokens)
	}

	name := "renamed"
	max := int64(512)
	updated, err := m.UpdateKey(pub.ID, &name, &KeyLimits{Models: []string{"only-one"}, MaxTokens: max})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "renamed" || updated.MaxTokens != 512 || len(updated.Models) != 1 {
		t.Fatalf("update result = %#v", updated)
	}

	// Empty models clears the allow-list.
	cleared, err := m.UpdateKey(pub.ID, nil, &KeyLimits{Models: nil, MaxTokens: 512})
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared.Models) != 0 {
		t.Fatalf("expected cleared models, got %#v", cleared.Models)
	}
	policy, _ = m.AuthenticateInferenceKey(secret)
	if !policy.AllowsModel("anything", "anything") {
		t.Fatal("empty allow-list should permit all models")
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

func TestKeyPolicy_AllowsModel(t *testing.T) {
	open := KeyPolicy{}
	if !open.AllowsModel("a", "b") {
		t.Fatal("empty models should allow all")
	}
	p := KeyPolicy{Models: []string{"m1"}}
	if !p.AllowsModel("m1", "real") {
		t.Fatal("requested name match")
	}
	if !p.AllowsModel("alias", "m1") {
		t.Fatal("resolved id match")
	}
	if p.AllowsModel("x", "y") {
		t.Fatal("should deny")
	}
}
