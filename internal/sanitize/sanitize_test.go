package sanitize

import (
	"strings"
	"testing"
)

func TestSafeSlug(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Local STT":         "local-stt",
		"UPPER_case--Slug":  "upper_case--slug",
		"   spaced slug   ": "spaced-slug",
		"../dangerous/path": "dangerous-path",
		"@@@":               "adapter",
		"slug-with-extremely-long-name-that-should-be-trimmed-because-it-exceeds-sixty-four-characters": "slug-with-extremely-long-name-that-should-be-trimmed-because-it-",
	}

	for input, expected := range cases {
		if actual := SafeSlug(input); actual != expected {
			t.Fatalf("SafeSlug(%q) = %q, expected %q", input, actual, expected)
		}
	}
}

func TestTruncateUTF8(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		maxBytes int
		want     string
	}{
		{"ascii short", "hello", 10, "hello"},
		{"ascii exact", "hello", 5, "hello"},
		{"ascii truncate", "hello world", 5, "hello"},
		{"utf8 no split", "héllo", 6, "héllo"},
		{"utf8 mid-char", "héllo", 2, "h"},
		{"emoji no split", "hi🎉x", 4, "hi"},
		{"empty", "", 10, ""},
		{"zero max", "hello", 0, ""},
		{"negative max", "hello", -1, ""},
		{"invalid utf8 prefix", string([]byte{0xff, 'a', 'b'}), 2, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateUTF8(tt.input, tt.maxBytes)
			if got != tt.want {
				t.Errorf("TruncateUTF8(%q, %d) = %q, want %q", tt.input, tt.maxBytes, got, tt.want)
			}
		})
	}
}

func TestTrimToRunes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		maxRunes int
		want     string
	}{
		{"trim and keep", "  hello  ", 10, "hello"},
		{"truncate ascii", "hello world", 5, "hello"},
		{"truncate utf8", "żółć", 3, "żół"},
		{"empty after trim", "   ", 10, ""},
		{"zero max", "hello", 0, ""},
		{"negative max", "hello", -1, ""},
		{"long unchanged", strings.Repeat("a", 8), 8, strings.Repeat("a", 8)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TrimToRunes(tt.input, tt.maxRunes)
			if got != tt.want {
				t.Errorf("TrimToRunes(%q, %d) = %q, want %q", tt.input, tt.maxRunes, got, tt.want)
			}
		})
	}
}
