package shared

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"testing"
)

func TestRewriteWhisperOpenAIPath(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"/v1/audio/transcriptions", "/inference"},
		{"/v1/audio/transcriptions/", "/inference"},
		{"/inference", "/inference"},
		{"/v1/chat/completions", "/v1/chat/completions"},
		{"/health", "/health"},
	}
	for _, tt := range tests {
		r, _ := http.NewRequest(http.MethodPost, tt.in, nil)
		RewriteWhisperOpenAIPath(r)
		if r.URL.Path != tt.want {
			t.Errorf("path %q -> %q, want %q", tt.in, r.URL.Path, tt.want)
		}
	}
}

func TestNewSingleHostReverseProxyWhisper(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:9")
	rp := NewSingleHostReverseProxy(target, true)
	if rp.Rewrite == nil {
		t.Fatal("expected Rewrite")
	}

	in, _ := http.NewRequest(http.MethodPost, "http://gateway/v1/audio/transcriptions", nil)
	out := in.Clone(in.Context())
	pr := &httputil.ProxyRequest{In: in, Out: out}
	rp.Rewrite(pr)
	if pr.Out.URL.Path != "/inference" {
		t.Fatalf("rewritten path = %q, want /inference", pr.Out.URL.Path)
	}
	if pr.Out.Host != "gateway" {
		t.Fatalf("Host = %q, want gateway (preserved)", pr.Out.Host)
	}

	in2, _ := http.NewRequest(http.MethodPost, "http://gateway/inference", nil)
	out2 := in2.Clone(in2.Context())
	pr2 := &httputil.ProxyRequest{In: in2, Out: out2}
	rp.Rewrite(pr2)
	if pr2.Out.URL.Path != "/inference" {
		t.Fatalf("native path = %q, want /inference", pr2.Out.URL.Path)
	}
}
