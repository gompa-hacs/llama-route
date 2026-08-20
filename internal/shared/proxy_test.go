package shared

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsHTTPS(t *testing.T) {
	trustLoopback := func(ip net.IP) bool {
		return ip != nil && ip.IsLoopback()
	}

	t.Run("direct TLS", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.TLS = &tls.ConnectionState{}
		if !IsHTTPS(r, trustLoopback) {
			t.Fatal("expected HTTPS from r.TLS")
		}
	})

	t.Run("forwarded proto from trusted proxy", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "127.0.0.1:1"
		r.Header.Set("X-Forwarded-Proto", "https")
		if !IsHTTPS(r, trustLoopback) {
			t.Fatal("expected HTTPS from X-Forwarded-Proto")
		}
	})

	t.Run("forwarded proto ignored from untrusted", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "8.8.8.8:1"
		r.Header.Set("X-Forwarded-Proto", "https")
		if IsHTTPS(r, trustLoopback) {
			t.Fatal("untrusted peer must not set HTTPS via header")
		}
	})
}

func TestClientIP_TrustedOnly(t *testing.T) {
	trustLoopback := func(ip net.IP) bool {
		return ip != nil && ip.IsLoopback()
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.1.1:9"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := ClientIP(r, trustLoopback); got != "10.1.1.1" {
		t.Fatalf("got %q, want direct peer", got)
	}

	r.RemoteAddr = "127.0.0.1:9"
	if got := ClientIP(r, trustLoopback); got != "1.2.3.4" {
		t.Fatalf("got %q, want forwarded client", got)
	}
}
