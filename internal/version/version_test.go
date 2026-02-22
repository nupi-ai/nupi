package version

import (
	"strings"
	"testing"
)

func TestStringReflectsBuildVersion(t *testing.T) {
	cleanup := ForTesting("1.2.3-test")
	t.Cleanup(cleanup)

	if got := String(); got != "1.2.3-test" {
		t.Fatalf("expected version 1.2.3-test, got %s", got)
	}
}

func TestCheckVersionMismatch(t *testing.T) {
	tests := []struct {
		name          string
		clientVersion string
		daemonVersion string
		wantWarning   bool
	}{
		{
			name:          "same version no warning",
			clientVersion: "0.3.0",
			daemonVersion: "0.3.0",
			wantWarning:   false,
		},
		{
			name:          "different version warning",
			clientVersion: "0.3.0",
			daemonVersion: "0.2.0",
			wantWarning:   true,
		},
		{
			name:          "daemon dev skip",
			clientVersion: "0.3.0",
			daemonVersion: "dev",
			wantWarning:   false,
		},
		{
			name:          "client dev skip",
			clientVersion: "dev",
			daemonVersion: "0.3.0",
			wantWarning:   false,
		},
		{
			name:          "both dev skip",
			clientVersion: "dev",
			daemonVersion: "dev",
			wantWarning:   false,
		},
		{
			name:          "empty daemon version skip",
			clientVersion: "0.3.0",
			daemonVersion: "",
			wantWarning:   false,
		},
		{
			name:          "empty client version skip",
			clientVersion: "",
			daemonVersion: "0.3.0",
			wantWarning:   false,
		},
		{
			name:          "git describe suffix stripped same base",
			clientVersion: "0.3.0-5-gabcdef",
			daemonVersion: "0.3.0",
			wantWarning:   false,
		},
		{
			name:          "git describe suffix stripped different base",
			clientVersion: "0.3.0-5-gabcdef",
			daemonVersion: "0.2.0",
			wantWarning:   true,
		},
		{
			name:          "git describe suffix on both sides same",
			clientVersion: "0.3.0-5-gabcdef",
			daemonVersion: "v0.3.0-10-g1234567",
			wantWarning:   false,
		},
		{
			name:          "v prefix normalized same version",
			clientVersion: "v0.3.0",
			daemonVersion: "0.3.0",
			wantWarning:   false,
		},
		{
			name:          "v prefix normalized different version",
			clientVersion: "v0.3.0",
			daemonVersion: "v0.2.0",
			wantWarning:   true,
		},
		{
			name:          "cargo sentinel 0.0.0 client skip",
			clientVersion: "0.0.0",
			daemonVersion: "0.3.0",
			wantWarning:   false,
		},
		{
			name:          "cargo sentinel 0.0.0 daemon skip",
			clientVersion: "0.3.0",
			daemonVersion: "0.0.0",
			wantWarning:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanup := ForTesting(tt.clientVersion)
			t.Cleanup(cleanup)

			got := CheckVersionMismatch(tt.daemonVersion)
			if tt.wantWarning && got == "" {
				t.Error("expected warning string, got empty")
			}
			if !tt.wantWarning && got != "" {
				t.Errorf("expected no warning, got %q", got)
			}
			if tt.wantWarning {
				// Use literal substrings — NOT FormatVersion() — to avoid tautological assertion.
				if !strings.HasPrefix(got, "WARNING: nupi ") {
					t.Errorf("warning %q missing expected prefix", got)
				}
				if !strings.Contains(got, "nupid ") {
					t.Errorf("warning %q missing daemon version reference", got)
				}
				if !strings.Contains(got, "please restart the daemon or reinstall") {
					t.Errorf("warning %q missing remediation suffix", got)
				}
			}
		})
	}
}

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"v0.3.0", "0.3.0"},
		{"0.3.0", "0.3.0"},
		{"0.3.0-5-gabcdef", "0.3.0"},
		{"v0.3.0-10-g1234567", "0.3.0"},
		{"0.3.0-rc1", "0.3.0-rc1"},
		{"0.3.0-beta-5-gabcdef", "0.3.0-beta"},
		{"dev", "dev"},
		{"", ""},
		{"abcdef1", "abcdef1"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := normalizeVersion(tt.input); got != tt.want {
				t.Errorf("normalizeVersion(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"0.3.0", "v0.3.0"},
		{"v0.3.0", "v0.3.0"},
		{"dev", "dev"},
		{"", ""},
		{"1.0.0-rc1", "v1.0.0-rc1"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := FormatVersion(tt.input); got != tt.want {
				t.Errorf("FormatVersion(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
