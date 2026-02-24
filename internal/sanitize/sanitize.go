package sanitize

import (
	"strings"
	"unicode"
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

// StripControlChars removes ANSI escape sequences and non-printable control
// characters (except newline and tab) from s. Useful for sanitising terminal
// output before sending as push notification body text.
func StripControlChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		// Strip ANSI escape sequences: ESC [ ... final byte (0x40-0x7E).
		// L3 fix (Review 13): cap scan at 64 bytes to limit damage from crafted
		// malformed CSI sequences that never terminate.
		if i+1 < len(s) && s[i] == '\x1b' && s[i+1] == '[' {
			j := i + 2
			maxJ := j + 64
			if maxJ > len(s) {
				maxJ = len(s)
			}
			for j < maxJ && (s[j] < 0x40 || s[j] > 0x7E) {
				j++
			}
			if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7E {
				j++ // skip final byte
			}
			i = j
			continue
		}
		// Strip OSC sequences: ESC ] ... ST (ESC \ or BEL).
		if i+1 < len(s) && s[i] == '\x1b' && s[i+1] == ']' {
			j := i + 2
			for j < len(s) {
				if s[j] == '\x07' { // BEL terminator
					j++
					break
				}
				if j+1 < len(s) && s[j] == '\x1b' && s[j+1] == '\\' { // ST terminator
					j += 2
					break
				}
				j++
			}
			i = j
			continue
		}
		// Strip other ESC-initiated sequences (2-byte).
		if s[i] == '\x1b' {
			i += 2
			if i > len(s) {
				i = len(s)
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		// L2 fix (Review 14): use unicode.IsControl to catch all control chars
		// (form feed, vertical tab, etc.) instead of hardcoded range. Preserve
		// only newline and tab which are meaningful in notification body text.
		if r == '\n' || r == '\t' || (r >= ' ' && !unicode.IsControl(r)) {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
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
