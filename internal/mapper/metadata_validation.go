package mapper

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

type MetadataLimits struct {
	MaxEntries    int
	MaxKeyRunes   int
	MaxValueRunes int
	MaxTotalBytes int
}

// NormalizeAndValidateMetadata trims keys and values, drops empty keys,
// and validates limits while preserving deterministic key handling.
func NormalizeAndValidateMetadata(raw map[string]string, limits MetadataLimits) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	normalized := make(map[string]string, len(raw))
	totalBytes := 0

	for _, rawKey := range keys {
		key := strings.TrimSpace(rawKey)
		if key == "" {
			continue
		}

		value := strings.TrimSpace(raw[rawKey])

		if limits.MaxKeyRunes > 0 && utf8.RuneCountInString(key) > limits.MaxKeyRunes {
			return nil, fmt.Errorf("metadata key %q exceeds maximum length of %d characters", key, limits.MaxKeyRunes)
		}
		if limits.MaxValueRunes > 0 && utf8.RuneCountInString(value) > limits.MaxValueRunes {
			return nil, fmt.Errorf("metadata value for key %q exceeds maximum length of %d characters", key, limits.MaxValueRunes)
		}

		if _, exists := normalized[key]; !exists && limits.MaxEntries > 0 && len(normalized) >= limits.MaxEntries {
			return nil, fmt.Errorf("metadata exceeds maximum of %d entries", limits.MaxEntries)
		}

		entryBytes := len(key) + len(value)
		if previous, exists := normalized[key]; exists {
			totalBytes -= len(key) + len(previous)
		}
		if limits.MaxTotalBytes > 0 && totalBytes+entryBytes > limits.MaxTotalBytes {
			return nil, fmt.Errorf("metadata total size exceeds %d bytes", limits.MaxTotalBytes)
		}

		normalized[key] = value
		totalBytes += entryBytes
	}

	if len(normalized) == 0 {
		return nil, nil
	}

	return normalized, nil
}
