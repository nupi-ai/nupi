package validate

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// HTTPURL
// ---------------------------------------------------------------------------

func TestHTTPURL_Valid(t *testing.T) {
	for _, url := range []string{
		"http://example.com/index.yaml",
		"https://example.com/plugin.tar.gz",
	} {
		if err := HTTPURL(url); err != nil {
			t.Errorf("HTTPURL(%q) = %v, want nil", url, err)
		}
	}
}

func TestHTTPURL_DisallowedSchemes(t *testing.T) {
	tests := []struct {
		url    string
		errMsg string
	}{
		{"file:///etc/passwd", "not allowed"},
		{"ftp://example.com/file", "not allowed"},
		{"javascript:alert(1)", "not allowed"},
	}
	for _, tc := range tests {
		err := HTTPURL(tc.url)
		if err == nil {
			t.Fatalf("HTTPURL(%q): expected error, got nil", tc.url)
		}
		if !strings.Contains(err.Error(), tc.errMsg) {
			t.Errorf("HTTPURL(%q) error = %q, want it to contain %q", tc.url, err.Error(), tc.errMsg)
		}
	}
}

func TestHTTPURL_MissingScheme(t *testing.T) {
	err := HTTPURL("example.com/index.yaml")
	if err == nil {
		t.Fatal("expected error for URL with no scheme")
	}
	if !strings.Contains(err.Error(), "missing scheme") {
		t.Errorf("error = %q, want it to mention missing scheme", err.Error())
	}
}

func TestHTTPURL_EmptyString(t *testing.T) {
	if err := HTTPURL(""); err == nil {
		t.Fatal("expected error for empty string URL")
	}
}

func TestHTTPURL_MissingHost(t *testing.T) {
	tests := []string{
		"http://",
		"https://",
		"http:///path/only",
	}
	for _, url := range tests {
		err := HTTPURL(url)
		if err == nil {
			t.Fatalf("HTTPURL(%q): expected error for missing host, got nil", url)
		}
		if !strings.Contains(err.Error(), "missing host") {
			t.Errorf("HTTPURL(%q) error = %q, want it to mention missing host", url, err.Error())
		}
	}
}

// ---------------------------------------------------------------------------
// RejectPrivateURL
// ---------------------------------------------------------------------------

func TestRejectPrivateURL_PublicAddresses(t *testing.T) {
	for _, url := range []string{
		"https://example.com/index.yaml",
		"https://8.8.8.8/api",
		"https://1.2.3.4:443/path",
		"https://registry.nupi.ai/v1",
	} {
		if err := RejectPrivateURL(url); err != nil {
			t.Errorf("RejectPrivateURL(%q) = %v, want nil", url, err)
		}
	}
}

func TestRejectPrivateURL_PrivateAddresses(t *testing.T) {
	tests := []struct {
		url  string
		desc string
	}{
		{"http://127.0.0.1/path", "loopback IPv4"},
		{"http://127.0.0.42:8080/path", "loopback IPv4 non-.1"},
		{"http://10.0.0.1/secret", "RFC-1918 10.x"},
		{"http://172.16.0.1/internal", "RFC-1918 172.16.x"},
		{"http://172.31.255.255/internal", "RFC-1918 172.31.x"},
		{"http://192.168.1.1/admin", "RFC-1918 192.168.x"},
		{"http://169.254.169.254/latest/meta-data/", "AWS metadata (link-local)"},
		{"http://169.254.0.1/metadata", "link-local unicast"},
		{"http://0.0.0.0/path", "unspecified address"},
		{"http://localhost/path", "localhost hostname"},
		{"http://LOCALHOST:8080/path", "localhost uppercase"},
		{"http://[::1]/path", "loopback IPv6"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			err := RejectPrivateURL(tc.url)
			if err == nil {
				t.Fatalf("RejectPrivateURL(%q): expected error for %s, got nil", tc.url, tc.desc)
			}
			if !strings.Contains(err.Error(), "private/internal") {
				t.Errorf("error = %q, want it to mention private/internal", err.Error())
			}
		})
	}
}

func TestRejectPrivateURL_HostnameNotIP(t *testing.T) {
	// Non-IP hostnames (other than "localhost") are allowed through
	// since we can't resolve DNS here.
	for _, url := range []string{
		"https://my-internal-host.company.com/api",
		"https://metadata.google.internal/computeMetadata/v1/",
	} {
		if err := RejectPrivateURL(url); err != nil {
			t.Errorf("RejectPrivateURL(%q) = %v, want nil (non-literal IPs should pass)", url, err)
		}
	}
}

func TestRejectPrivateURL_EmptyHost(t *testing.T) {
	// Empty host should not error (HTTPURL handles that)
	if err := RejectPrivateURL("http://"); err != nil {
		t.Errorf("RejectPrivateURL with empty host: unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Ident
// ---------------------------------------------------------------------------

func TestIdent_Valid(t *testing.T) {
	for _, s := range []string{
		"hello", "my-plugin", "my.plugin", "my_plugin",
		"Plugin123", "a", "9start",
		strings.Repeat("a", MaxIdentLen),
	} {
		if !Ident(s) {
			t.Errorf("Ident(%q) = false, want true", s)
		}
	}
}

func TestIdent_Invalid(t *testing.T) {
	for _, s := range []string{
		"", "-start", ".start", "_start",
		"has space", "has/slash", "café",
		strings.Repeat("a", MaxIdentLen+1),
	} {
		if Ident(s) {
			t.Errorf("Ident(%q) = true, want false", s)
		}
	}
}

func TestIdentRe_Pattern(t *testing.T) {
	// Verify the pattern matches the documented format.
	if !IdentRe.MatchString("abc123") {
		t.Error("IdentRe should match alphanumeric strings")
	}
	if IdentRe.MatchString("-bad") {
		t.Error("IdentRe should not match strings starting with dash")
	}
}
