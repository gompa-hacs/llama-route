package config

import (
	"net"
	"strings"
	"testing"
)

func TestHTTPConfig_DefaultTrustsLoopback(t *testing.T) {
	h := &HTTPConfig{}
	if !h.Trusts(net.ParseIP("127.0.0.1")) {
		t.Fatal("expected default trust for 127.0.0.1")
	}
	if !h.Trusts(net.ParseIP("::1")) {
		t.Fatal("expected default trust for ::1")
	}
	if h.Trusts(net.ParseIP("8.8.8.8")) {
		t.Fatal("must not trust public IPs by default")
	}
}

func TestValidateHTTPConfig_CustomCIDR(t *testing.T) {
	h := &HTTPConfig{TrustedProxies: []string{"10.0.0.0/8", "192.168.1.1"}}
	if err := validateHTTPConfig(h); err != nil {
		t.Fatal(err)
	}
	if !h.Trusts(net.ParseIP("10.1.2.3")) {
		t.Fatal("expected 10.1.2.3 trusted")
	}
	if !h.Trusts(net.ParseIP("192.168.1.1")) {
		t.Fatal("expected single IP trusted")
	}
	if h.Trusts(net.ParseIP("127.0.0.1")) {
		t.Fatal("custom list should replace defaults")
	}
}

func TestValidateHTTPConfig_Invalid(t *testing.T) {
	h := &HTTPConfig{TrustedProxies: []string{"not-an-ip"}}
	err := validateHTTPConfig(h)
	if err == nil || !strings.Contains(err.Error(), "trustedProxies") {
		t.Fatalf("err = %v", err)
	}
}
