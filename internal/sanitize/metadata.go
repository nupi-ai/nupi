package sanitize

import (
	"fmt"
	"maps"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	DefaultMetadataMaxEntries    = 32
	DefaultMetadataMaxKeyRunes   = 64
	DefaultMetadataMaxValueRunes = 512
	DefaultMetadataMaxTotalBytes = 8192
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

// MetadataAccumulator applies lossy metadata normalization:
// keys/values are trimmed and rune-limited, entries over limits are dropped.
type MetadataAccumulator struct {
	limits  MetadataLimits
	entries map[string]string
	total   int
}

func NewMetadataAccumulator(base map[string]string, limits MetadataLimits) *MetadataAccumulator {
	acc := &MetadataAccumulator{
		limits: limits,
	}
	acc.Merge(base)
	return acc
}

func (m *MetadataAccumulator) Merge(src map[string]string) {
	if len(src) == 0 {
		return
	}
	keys := make([]string, 0, len(src))
	for k := range src {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		m.Add(key, src[key])
	}
}

func (m *MetadataAccumulator) Add(key, value string) {
	key = TrimToRunes(key, m.limits.MaxKeyRunes)
	if key == "" {
		return
	}
	value = TrimToRunes(value, m.limits.MaxValueRunes)

	if m.entries == nil {
		m.entries = make(map[string]string)
	}

	if m.limits.MaxEntries > 0 && len(m.entries) >= m.limits.MaxEntries {
		if _, exists := m.entries[key]; !exists {
			return
		}
	}

	addition := len(key) + len(value)
	if existing, ok := m.entries[key]; ok {
		m.total -= len(key) + len(existing)
	}

	if m.limits.MaxTotalBytes > 0 && m.total+addition > m.limits.MaxTotalBytes {
		if existing, ok := m.entries[key]; ok {
			m.total += len(key) + len(existing)
		}
		return
	}

	m.entries[key] = value
	m.total += addition

	if m.limits.MaxEntries > 0 && len(m.entries) > m.limits.MaxEntries {
		delete(m.entries, key)
		m.total -= addition
	}
}

func (m *MetadataAccumulator) Result() map[string]string {
	if len(m.entries) == 0 {
		return nil
	}
	return maps.Clone(m.entries)
}
