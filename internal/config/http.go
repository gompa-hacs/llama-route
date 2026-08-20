package config

import (
	"fmt"
	"net"
	"strings"
)

// defaultTrustedProxies are used when http.trustedProxies is omitted. They
// cover a same-host reverse proxy (nginx on 127.0.0.1 / ::1), which is the
// recommended deployment for internet-facing setups.
var defaultTrustedProxies = []string{
	"127.0.0.0/8",
	"::1/128",
}

// HTTPConfig controls how llama-swap interprets reverse-proxy headers.
type HTTPConfig struct {
	// TrustedProxies is a list of IP addresses or CIDR ranges whose
	// X-Forwarded-Proto / X-Forwarded-For / X-Real-IP headers are honored.
	// Empty means the defaults (loopback only).
	TrustedProxies []string `yaml:"trustedProxies"`

	trustedNets []*net.IPNet `yaml:"-"`
}

// Trusts reports whether ip belongs to a configured trusted proxy network.
// When trustedProxies was never parsed (e.g. an empty Config literal), the
// loopback defaults apply so same-host nginx still works.
func (h *HTTPConfig) Trusts(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range h.trustedOrDefault() {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (h *HTTPConfig) trustedOrDefault() []*net.IPNet {
	if h != nil && len(h.trustedNets) > 0 {
		return h.trustedNets
	}
	nets := make([]*net.IPNet, 0, len(defaultTrustedProxies))
	for _, raw := range defaultTrustedProxies {
		n, err := parseIPOrCIDR(raw)
		if err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}

func validateHTTPConfig(h *HTTPConfig) error {
	if h == nil {
		return nil
	}
	if len(h.TrustedProxies) == 0 {
		// Leave trustedNets nil so Trusts() applies loopback defaults lazily.
		h.trustedNets = nil
		return nil
	}

	nets := make([]*net.IPNet, 0, len(h.TrustedProxies))
	for _, raw := range h.TrustedProxies {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return fmt.Errorf("http.trustedProxies: empty entry")
		}
		n, err := parseIPOrCIDR(raw)
		if err != nil {
			return fmt.Errorf("http.trustedProxies: invalid entry %q: %w", raw, err)
		}
		nets = append(nets, n)
	}
	h.trustedNets = nets
	return nil
}

func parseIPOrCIDR(s string) (*net.IPNet, error) {
	if strings.Contains(s, "/") {
		_, n, err := net.ParseCIDR(s)
		return n, err
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("not an IP or CIDR")
	}
	if v4 := ip.To4(); v4 != nil {
		return &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}, nil
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}, nil
}
