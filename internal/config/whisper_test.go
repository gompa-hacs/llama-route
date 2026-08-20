package config

import "testing"

func TestIsWhisperCmd(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"whisper-server --port 8080", true},
		{"/opt/bin/whisper-server -m model.bin", true},
		{"/tools/whisper-server.exe -m m.bin", true},
		{"llama-server --port 8080", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsWhisperCmd(tt.cmd); got != tt.want {
			t.Errorf("IsWhisperCmd(%q) = %v, want %v", tt.cmd, got, tt.want)
		}
	}
}

func TestResolveWhisperCompat(t *testing.T) {
	tests := []struct {
		name   string
		compat string
		cmd    string
		want   bool
	}{
		{"auto whisper-server", "", "whisper-server --port ${PORT}", true},
		{"explicit compat", "whisper", "http proxy only", true},
		{"explicit case", "Whisper", "", true},
		{"request-path disables auto", "", "whisper-server --request-path /v1/audio/transcriptions", false},
		{"request-path disables explicit", "whisper", "whisper-server --request-path /v1/audio/transcriptions --inference-path \"\"", false},
		{"llama-server", "", "llama-server --port 1", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveWhisperCompat(tt.compat, tt.cmd); got != tt.want {
				t.Fatalf("ResolveWhisperCompat(%q, %q) = %v, want %v", tt.compat, tt.cmd, got, tt.want)
			}
		})
	}
}
