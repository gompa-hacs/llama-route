package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/tidwall/gjson"
)

// modelsResponse is the OpenAI-compatible /v1/models payload.
type modelsResponse struct {
	Data []struct {
		ID            string `json:"id"`
		ContextLength int    `json:"context_length"`
	} `json:"data"`
}

// fetchPeerOffers GETs peer /v1/models and optionally /props for missing context.
func fetchPeerOffers(ctx context.Context, peerID string, peer config.PeerConfig, client *http.Client) ([]Offer, error) {
	base := peer.Proxy
	if peer.ProxyURL != nil {
		base = peer.ProxyURL.String()
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("peer %s: invalid proxy: %w", peerID, err)
	}
	listURL := u.JoinPath("/v1/models").String()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, err
	}
	if peer.ApiKey != "" {
		req.Header.Set("Authorization", "Bearer "+peer.ApiKey)
		req.Header.Set("x-api-key", peer.ApiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("peer %s: /v1/models: %w", peerID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("peer %s: /v1/models status %d: %s", peerID, resp.StatusCode, string(body))
	}

	var payload modelsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("peer %s: decode /v1/models: %w", peerID, err)
	}

	fallbackCtx := 0
	needFallback := false
	for _, m := range payload.Data {
		if m.ID == "" {
			continue
		}
		if !peer.AllowedModel(m.ID) {
			continue
		}
		if m.ContextLength <= 0 {
			needFallback = true
			break
		}
	}
	if needFallback {
		fallbackCtx = fetchPropsContext(ctx, client, u, peer.ApiKey)
	}

	offers := make([]Offer, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID == "" || !peer.AllowedModel(m.ID) {
			continue
		}
		cs := m.ContextLength
		if cs <= 0 {
			cs = fallbackCtx
		}
		offers = append(offers, Offer{
			PeerID:      peerID,
			ModelID:     m.ID,
			ContextSize: cs,
			Proxy:       base,
			ApiKey:      peer.ApiKey,
			Filters:     peer.Filters,
		})
	}
	return offers, nil
}

func fetchPropsContext(ctx context.Context, client *http.Client, base *url.URL, apiKey string) int {
	propsURL := base.JoinPath("/props").String()
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, propsURL, nil)
	if err != nil {
		return 0
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("x-api-key", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return 0
	}
	v := gjson.GetBytes(body, "default_generation_settings.n_ctx")
	if !v.Exists() {
		v = gjson.GetBytes(body, "n_ctx")
	}
	if v.Int() > 0 {
		return int(v.Int())
	}
	return 0
}
