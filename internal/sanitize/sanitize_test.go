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

// M3 fix (Review 13): dedicated tests for StripControlChars.
func TestStripControlChars(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain text", "hello world", "hello world"},
		{"csi color sequence", "\x1b[31mred\x1b[0m", "red"},
		{"unterminated csi at end", "hello\x1b[31", "hello"},
		{"osc bel terminator", "\x1b]0;window title\x07body text", "body text"},
		{"osc st terminator", "\x1b]0;window title\x1b\\body text", "body text"},
		{"bare esc at end", "text\x1b", "text"},
		{"bare esc mid-string", "a\x1bb", "a"}, // ESC consumes next byte as 2-byte sequence
		{"nul byte stripped", "hello\x00world", "helloworld"},
		{"carriage return stripped", "hello\rworld", "helloworld"},
		{"preserves newline", "hello\nworld", "hello\nworld"},
		{"preserves tab", "hello\tworld", "hello\tworld"},
		{"multiple csi sequences", "\x1b[1m\x1b[32mbold green\x1b[0m", "bold green"},
		{"csi with params", "\x1b[38;5;196mcolor256\x1b[m", "color256"},
		{"empty string", "", ""},
		{"only escape", "\x1b", ""},
		{"only csi", "\x1b[m", ""},
		{"mixed utf8 and ansi", "héllo \x1b[31mwörld\x1b[0m", "héllo wörld"},
		{"bell char stripped", "alert\x07!", "alert!"},
		{"backspace stripped", "abc\x08d", "abcd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripControlChars(tt.input)
			if got != tt.want {
				t.Errorf("StripControlChars(%q) = %q, want %q", tt.input, got, tt.want)
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
