package router

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/shared"
	"github.com/tidwall/gjson"
)

// ExtractAffinityKey returns a stable session key from the request using the
// configured rule chain. An empty string means no stickiness for this request.
//
// A nil rules slice uses the default LLM sticky chain. A non-nil empty slice
// disables stickiness entirely (used for whisper.cpp pools).
func ExtractAffinityKey(r *http.Request, rules []config.AffinityRule) string {
	if rules == nil {
		rules = config.DefaultAffinityRules()
	}
	if len(rules) == 0 {
		return ""
	}

	body := peekRequestBody(r)
	apiKey := shared.ExtractAPIKey(r)
	if apiKey == "" {
		if data, ok := shared.ReadContext(r.Context()); ok {
			apiKey = data.ApiKey
		}
	}

	for _, rule := range rules {
		switch {
		case rule.JSON != "":
			if body != nil {
				if v := strings.TrimSpace(gjson.GetBytes(body, rule.JSON).String()); v != "" {
					return hashAffinity("json:" + rule.JSON + ":" + v)
				}
			}
		case rule.Header != "":
			if v := strings.TrimSpace(r.Header.Get(rule.Header)); v != "" {
				return hashAffinity("hdr:" + rule.Header + ":" + v)
			}
		case rule.APIKey:
			if apiKey != "" {
				return hashAffinity("key:" + apiKey)
			}
		}
	}
	return ""
}

func peekRequestBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return bodyBytes
}

func hashAffinity(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}
