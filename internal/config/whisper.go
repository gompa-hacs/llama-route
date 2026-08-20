package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

const whisperCompatName = "whisper"

// IsWhisperCmd reports whether cmd appears to launch whisper-server.
func IsWhisperCmd(cmd string) bool {
	for field := range strings.FieldsSeq(cmd) {
		if strings.HasPrefix(field, "#") {
			continue
		}
		base := filepath.Base(field)
		base = strings.TrimSuffix(base, ".exe")
		if base == "whisper-server" {
			return true
		}
	}
	return false
}

// CmdHasRequestPath reports whether cmd already configures whisper's
// --request-path (OpenAI-shaped upstream; do not rewrite).
func CmdHasRequestPath(cmd string) bool {
	return strings.Contains(cmd, "--request-path")
}

// ResolveWhisperCompat returns whether OpenAI transcription paths should be
// rewritten to whisper.cpp's default /inference.
func ResolveWhisperCompat(compat, cmd string) bool {
	if CmdHasRequestPath(cmd) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(compat), whisperCompatName) {
		return true
	}
	return IsWhisperCmd(cmd)
}

// applyWhisperCompat resolves whisper.cpp OpenAI path rewriting and health
// defaults for a model (and its pool backends).
func applyWhisperCompat(modelID string, mc *ModelConfig) error {
	if err := validateCompat(modelID, "", mc.Compat); err != nil {
		return err
	}
	if mc.UsesPool() {
		for i := range mc.Pool.Backends {
			b := &mc.Pool.Backends[i]
			if err := validateCompat(modelID, fmt.Sprintf("pool.backends[%d]", i), b.Compat); err != nil {
				return err
			}
			compat := b.Compat
			if compat == "" {
				compat = mc.Compat
			}
			b.WhisperCompat = ResolveWhisperCompat(compat, b.Cmd)
			if b.WhisperCompat && b.CheckEndpoint == "" {
				b.CheckEndpoint = "/"
			}
		}
		return nil
	}

	mc.WhisperCompat = ResolveWhisperCompat(mc.Compat, mc.Cmd)
	// UnmarshalYAML defaults checkEndpoint to /health; stock whisper has no /health.
	if mc.WhisperCompat && (mc.CheckEndpoint == "" || mc.CheckEndpoint == "/health") {
		mc.CheckEndpoint = "/"
	}
	return nil
}

func validateCompat(modelID, fieldPrefix, compat string) error {
	compat = strings.TrimSpace(compat)
	if compat == "" {
		return nil
	}
	if strings.EqualFold(compat, whisperCompatName) {
		return nil
	}
	where := "compat"
	if fieldPrefix != "" {
		where = fieldPrefix + ".compat"
	}
	return fmt.Errorf("model %s: %s %q is not supported (valid: whisper)", modelID, where, compat)
}
