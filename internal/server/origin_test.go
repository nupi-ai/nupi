package server

import (
	"net/url"
	"testing"
)

func TestIsBuiltinOrigin(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		want   bool
	}{
		// Tauri origins
		{"tauri localhost", "tauri://localhost", true},
		{"tauri localhost with path", "tauri://localhost/", true},
		{"https tauri.localhost", "https://tauri.localhost", true},
		{"https tauri.local no port", "https://tauri.local", true},
		{"https tauri.local with port", "https://tauri.local:8080", true},

		// Localhost origins
		{"http localhost no port", "http://localhost", true},
		{"http localhost with port", "http://localhost:3000", true},
		{"http localhost high port", "http://localhost:51234", true},

		// 127.0.0.1 origins
		{"http 127.0.0.1 no port", "http://127.0.0.1", true},
		{"http 127.0.0.1 with port", "http://127.0.0.1:8080", true},

		// Attack vectors — must be rejected
		{"evil localhost subdomain", "http://localhost.evil.com", false},
		{"evil tauri.local subdomain", "https://tauri.local.evil.com", false},
		{"evil 127.0.0.1 subdomain", "http://127.0.0.1.evil.com", false},

		// Origins with paths (should still match, path is irrelevant for Origin)
		{"https tauri.localhost with path", "https://tauri.localhost/foo", true},
		{"http localhost with path", "http://localhost/bar", true},

		// Wrong scheme
		{"https localhost", "https://localhost", false},
		{"http tauri.localhost", "http://tauri.localhost", false},
		{"http tauri.local", "http://tauri.local", false},
		{"tauri with port", "tauri://localhost:1234", false},

		// https tauri.localhost must NOT accept port (portAny=false)
		{"https tauri.localhost with port", "https://tauri.localhost:8080", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.origin)
			if err != nil {
				t.Fatalf("parse %q: %v", tt.origin, err)
			}
			got := isBuiltinOrigin(u)
			if got != tt.want {
				t.Errorf("isBuiltinOrigin(%q) = %v, want %v", tt.origin, got, tt.want)
			}
		})
	}
}

func TestIsBuiltinOriginNil(t *testing.T) {
	if isBuiltinOrigin(nil) {
		t.Error("isBuiltinOrigin(nil) = true, want false")
	}
}

func TestOriginAllowed(t *testing.T) {
	tests := []struct {
		name           string
		origin         string
		allowedOrigins []string
		want           bool
	}{
		// Empty origin always rejected
		{"empty origin", "", nil, false},

		// Builtin origins
		{"builtin tauri", "tauri://localhost", nil, true},
		{"builtin localhost", "http://localhost:3000", nil, true},
		{"builtin 127.0.0.1", "http://127.0.0.1:8080", nil, true},

		// Configured origins — exact match
		{"configured match", "https://example.com", []string{"https://example.com"}, true},
		{"configured no match", "https://other.com", []string{"https://example.com"}, false},
		{"configured multiple", "https://b.com", []string{"https://a.com", "https://b.com"}, true},

		// Malformed inputs
		{"no scheme", "localhost", nil, false},
		{"invalid url", "://", nil, false},
		{"only scheme", "http://", nil, false},

		// Attack vectors
		{"evil localhost", "http://localhost.evil.com", nil, false},
		{"evil tauri", "https://tauri.local.evil.com", nil, false},
		{"evil port injection", "http://localhost:evil.com", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := &transportConfig{
				allowedOrigins: tt.allowedOrigins,
			}
			got := tc.originAllowed(tt.origin)
			if got != tt.want {
				t.Errorf("originAllowed(%q) = %v, want %v", tt.origin, got, tt.want)
			}
		})
	}
}
