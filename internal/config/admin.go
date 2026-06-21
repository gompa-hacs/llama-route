package config

import (
	"fmt"
	"time"
)

// AdminConfig controls dashboard login sessions and the path for UI-managed
// inference API keys.
type AdminConfig struct {
	Password   string `yaml:"password"`
	SessionTTL string `yaml:"sessionTTL"`
	KeysFile   string `yaml:"keysFile"`
}

func (a *AdminConfig) SessionDuration() time.Duration {
	if a == nil || a.SessionTTL == "" {
		return 24 * time.Hour
	}
	d, err := time.ParseDuration(a.SessionTTL)
	if err != nil || d <= 0 {
		return 24 * time.Hour
	}
	return d
}

func validateAdminConfig(admin AdminConfig) error {
	if admin.SessionTTL != "" {
		if d, err := time.ParseDuration(admin.SessionTTL); err != nil || d <= 0 {
			return fmt.Errorf("admin.sessionTTL: invalid duration %q", admin.SessionTTL)
		}
	}
	return nil
}
