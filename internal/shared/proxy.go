package shared

import (
	"net"
	"net/http"
	"strings"
)

// RemoteIP returns the direct peer IP from r.RemoteAddr.
func RemoteIP(r *http.Request) net.IP {
	if r == nil {
		return nil
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

// IsHTTPS reports whether the client-facing request was HTTPS. When the
// immediate peer is a trusted proxy, X-Forwarded-Proto is honored; otherwise
// only r.TLS is considered.
func IsHTTPS(r *http.Request, trustProxy func(net.IP) bool) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	ip := RemoteIP(r)
	if trustProxy == nil || !trustProxy(ip) {
		return false
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = r.Header.Get("X-Forwarded-Protocol")
	}
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// ClientIP returns the originating client address. Forwarded headers are only
// trusted when the immediate peer is a trusted proxy; otherwise RemoteAddr is
// used.
func ClientIP(r *http.Request, trustProxy func(net.IP) bool) string {
	if r == nil {
		return ""
	}
	ip := RemoteIP(r)
	if trustProxy != nil && trustProxy(ip) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, found := strings.Cut(xff, ","); found {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(xff)
		}
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			return strings.TrimSpace(xr)
		}
	}
	if ip != nil {
		return ip.String()
	}
	return r.RemoteAddr
}
