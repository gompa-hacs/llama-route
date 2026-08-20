package shared

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

const (
	WhisperInferencePath       = "/inference"
	WhisperOpenAITranscription = "/v1/audio/transcriptions"
)

// RewriteWhisperOpenAIPath maps OpenAI transcription URLs to whisper.cpp's
// stock /inference path. Native /inference is left unchanged.
func RewriteWhisperOpenAIPath(r *http.Request) {
	if r == nil {
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == WhisperOpenAITranscription {
		r.URL.Path = WhisperInferencePath
		r.URL.RawPath = ""
	}
}

// NewSingleHostReverseProxy is like httputil.NewSingleHostReverseProxy but uses
// Rewrite (not the deprecated Director) and optionally rewrites whisper OpenAI
// transcription paths to /inference. Host is preserved like the stdlib helper.
func NewSingleHostReverseProxy(target *url.URL, whisperCompat bool) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			if whisperCompat {
				RewriteWhisperOpenAIPath(pr.Out)
			}
		},
	}
}
