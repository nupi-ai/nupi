package sanitize

import (
	"strings"
	"unicode/utf8"
)

const (
	AdapterSlugMaxLength = 64
	defaultAdapterSlug   = "adapter"
)

// SafeSlug normalizes value to a filesystem-safe adapter slug.
func SafeSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.' || r == '/':
			b.WriteRune('-')
		}
	}
	res := strings.Trim(b.String(), "-_")
	if res == "" {
		return defaultAdapterSlug
	}
	if len(res) > AdapterSlugMaxLength {
		return res[:AdapterSlugMaxLength]
	}
	return res
}

// TruncateUTF8 truncates s to at most maxBytes bytes without splitting UTF-8 runes.
func TruncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	truncated := s[:maxBytes]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

// TrimToRunes trims surrounding whitespace and limits result to maxRunes.
func TrimToRunes(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if value == "" || maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes)
}
