package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"golang.org/x/crypto/bcrypt"
)

const (
	SessionCookieName = "llama-swap-session"
	keyPrefix         = "sk-ls-"
	bcryptCost        = bcrypt.DefaultCost
)

// Manager handles admin sessions and inference API keys.
type Manager struct {
	mu sync.RWMutex

	cfg          config.Config
	adminEnabled bool
	adminHash    []byte
	sessionTTL   time.Duration
	keysPath     string

	staticKeys []string
	store      keyStore
	sessions   map[string]time.Time
}

type keyStore struct {
	Keys []storedKey `json:"keys"`
}

type storedKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	Hash      string    `json:"hash"`
	Created   time.Time `json:"created"`
	LastUsed  time.Time `json:"lastUsed,omitzero"`
	Revoked   bool      `json:"revoked,omitempty"`
	Models    []string  `json:"models,omitempty"`
	MaxTokens int64     `json:"maxTokens,omitempty"`
}

// PublicKey is returned by list/create APIs (never includes the secret).
type PublicKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	Created   time.Time `json:"created"`
	LastUsed  time.Time `json:"lastUsed,omitzero"`
	Revoked   bool      `json:"revoked,omitempty"`
	Models    []string  `json:"models,omitempty"`
	MaxTokens int64     `json:"maxTokens,omitempty"`
}

// KeyLimits are optional restrictions applied to a UI-managed API key.
// Empty Models means all models are allowed. MaxTokens of 0 means no cap.
type KeyLimits struct {
	Models    []string `json:"models"`
	MaxTokens int64    `json:"maxTokens"`
}

// KeyPolicy is the effective access policy for an authenticated inference key.
type KeyPolicy struct {
	ID        string
	Models    []string
	MaxTokens int64
	Static    bool
}

type keyPolicyContextKey struct{}

// ContextWithKeyPolicy stores the authenticated key policy on the request context.
func ContextWithKeyPolicy(ctx context.Context, p KeyPolicy) context.Context {
	return context.WithValue(ctx, keyPolicyContextKey{}, p)
}

// KeyPolicyFromContext returns the policy attached by inference auth middleware.
func KeyPolicyFromContext(ctx context.Context) (KeyPolicy, bool) {
	p, ok := ctx.Value(keyPolicyContextKey{}).(KeyPolicy)
	return p, ok
}

// AllowsModel reports whether the policy permits the requested model name or
// its resolved model ID. An empty allow-list means unrestricted.
func (p KeyPolicy) AllowsModel(requested, modelID string) bool {
	if len(p.Models) == 0 {
		return true
	}
	for _, m := range p.Models {
		if m == requested || (modelID != "" && m == modelID) {
			return true
		}
	}
	return false
}

// NewManager loads dynamic keys and prepares session state.
func NewManager(cfg config.Config) (*Manager, error) {
	m := &Manager{
		cfg:        cfg,
		sessionTTL: cfg.Admin.SessionDuration(),
		staticKeys: append([]string(nil), cfg.RequiredAPIKeys...),
		sessions:   make(map[string]time.Time),
	}

	if p := cfg.Admin.KeysFile; p != "" {
		m.keysPath = p
	} else {
		m.keysPath = "llama-swap-keys.json"
	}

	if err := m.loadKeys(); err != nil {
		return nil, err
	}

	pw := cfg.Admin.Password
	if pw != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
		if err != nil {
			return nil, fmt.Errorf("hash admin password: %w", err)
		}
		m.adminHash = hash
		m.adminEnabled = true
	}

	return m, nil
}

func (m *Manager) SessionCookieName() string { return SessionCookieName }

func (m *Manager) AdminRequired() bool { return m.adminEnabled }

func (m *Manager) InferenceRequired() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.staticKeys) > 0 || m.activeDynamicKeyCountLocked() > 0
}

func (m *Manager) activeDynamicKeyCountLocked() int {
	n := 0
	for _, k := range m.store.Keys {
		if !k.Revoked {
			n++
		}
	}
	return n
}

func (m *Manager) Login(password string) (string, time.Time, error) {
	if !m.adminEnabled {
		return "", time.Time{}, errors.New("admin login is not configured")
	}
	if err := bcrypt.CompareHashAndPassword(m.adminHash, []byte(password)); err != nil {
		return "", time.Time{}, errors.New("invalid password")
	}
	token, err := randomToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	exp := time.Now().Add(m.sessionTTL)
	m.mu.Lock()
	m.sessions[token] = exp
	m.pruneSessionsLocked(time.Now())
	m.mu.Unlock()
	return token, exp, nil
}

func (m *Manager) Logout(token string) {
	if token == "" {
		return
	}
	m.mu.Lock()
	delete(m.sessions, token)
	m.mu.Unlock()
}

func (m *Manager) SessionValid(token string) bool {
	if token == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.sessions[token]
	if !ok || time.Now().After(exp) {
		delete(m.sessions, token)
		return false
	}
	return true
}

// ValidateInferenceKey reports whether key is a valid static or UI-managed key.
func (m *Manager) ValidateInferenceKey(key string) bool {
	_, ok := m.AuthenticateInferenceKey(key)
	return ok
}

// AuthenticateInferenceKey validates key and returns its access policy.
// Static YAML keys are unrestricted. UI-managed keys may restrict models and
// max_tokens.
func (m *Manager) AuthenticateInferenceKey(key string) (KeyPolicy, bool) {
	if key == "" {
		return KeyPolicy{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, k := range m.staticKeys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(key)) == 1 {
			return KeyPolicy{Static: true}, true
		}
	}

	for i := range m.store.Keys {
		sk := &m.store.Keys[i]
		if sk.Revoked {
			continue
		}
		if err := bcrypt.CompareHashAndPassword([]byte(sk.Hash), []byte(key)); err == nil {
			sk.LastUsed = time.Now()
			_ = m.saveKeysLocked()
			return KeyPolicy{
				ID:        sk.ID,
				Models:    append([]string(nil), sk.Models...),
				MaxTokens: sk.MaxTokens,
			}, true
		}
	}
	return KeyPolicy{}, false
}

func (m *Manager) ListKeys() []PublicKey {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]PublicKey, 0, len(m.store.Keys))
	for _, k := range m.store.Keys {
		out = append(out, toPublicKey(k))
	}
	return out
}

func (m *Manager) CreateKey(name string, limits KeyLimits) (PublicKey, string, error) {
	name = trimName(name)
	models, err := normalizeModels(limits.Models)
	if err != nil {
		return PublicKey{}, "", err
	}
	if limits.MaxTokens < 0 {
		return PublicKey{}, "", fmt.Errorf("maxTokens must be >= 0")
	}

	secret, err := randomToken(32)
	if err != nil {
		return PublicKey{}, "", err
	}
	full := keyPrefix + secret
	prefix := full[:12] + "..."

	hash, err := bcrypt.GenerateFromPassword([]byte(full), bcryptCost)
	if err != nil {
		return PublicKey{}, "", err
	}

	id, err := randomToken(8)
	if err != nil {
		return PublicKey{}, "", err
	}

	rec := storedKey{
		ID:        id,
		Name:      name,
		Prefix:    prefix,
		Hash:      string(hash),
		Created:   time.Now(),
		Models:    models,
		MaxTokens: limits.MaxTokens,
	}

	m.mu.Lock()
	m.store.Keys = append(m.store.Keys, rec)
	if err := m.saveKeysLocked(); err != nil {
		m.store.Keys = m.store.Keys[:len(m.store.Keys)-1]
		m.mu.Unlock()
		return PublicKey{}, "", err
	}
	m.mu.Unlock()

	return toPublicKey(rec), full, nil
}

// UpdateKey updates the name and/or limits of an existing key.
func (m *Manager) UpdateKey(id string, name *string, limits *KeyLimits) (PublicKey, error) {
	if id == "" {
		return PublicKey{}, fmt.Errorf("key not found")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.store.Keys {
		sk := &m.store.Keys[i]
		if sk.ID != id {
			continue
		}
		if sk.Revoked {
			return PublicKey{}, fmt.Errorf("key is revoked")
		}
		if name != nil {
			sk.Name = trimName(*name)
		}
		if limits != nil {
			models, err := normalizeModels(limits.Models)
			if err != nil {
				return PublicKey{}, err
			}
			if limits.MaxTokens < 0 {
				return PublicKey{}, fmt.Errorf("maxTokens must be >= 0")
			}
			sk.Models = models
			sk.MaxTokens = limits.MaxTokens
		}
		if err := m.saveKeysLocked(); err != nil {
			return PublicKey{}, err
		}
		return toPublicKey(*sk), nil
	}
	return PublicKey{}, fmt.Errorf("key not found")
}

func (m *Manager) RevokeKey(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.store.Keys {
		if m.store.Keys[i].ID == id {
			m.store.Keys[i].Revoked = true
			return m.saveKeysLocked()
		}
	}
	return fmt.Errorf("key not found")
}

func (m *Manager) loadKeys() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := os.ReadFile(m.keysPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read keys file %s: %w", m.keysPath, err)
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, &m.store); err != nil {
		return fmt.Errorf("parse keys file %s: %w", m.keysPath, err)
	}
	return nil
}

func (m *Manager) saveKeysLocked() error {
	if dir := filepath.Dir(m.keysPath); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(m.store, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.keysPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.keysPath)
}

func (m *Manager) pruneSessionsLocked(now time.Time) {
	for tok, exp := range m.sessions {
		if now.After(exp) {
			delete(m.sessions, tok)
		}
	}
}

func toPublicKey(k storedKey) PublicKey {
	return PublicKey{
		ID:        k.ID,
		Name:      k.Name,
		Prefix:    k.Prefix,
		Created:   k.Created,
		LastUsed:  k.LastUsed,
		Revoked:   k.Revoked,
		Models:    append([]string(nil), k.Models...),
		MaxTokens: k.MaxTokens,
	}
}

func normalizeModels(models []string) ([]string, error) {
	if len(models) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			return nil, fmt.Errorf("models entries must be non-empty")
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out, nil
}

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func trimName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unnamed"
	}
	if len(name) > 64 {
		return name[:64]
	}
	return name
}
