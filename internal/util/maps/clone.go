package maps

import stdmaps "maps"

// Clone returns a shallow clone of the input map.
// It returns nil for nil or empty input to preserve metadata handling semantics
// used across the codebase.
func Clone[K comparable, V any](m map[K]V) map[K]V {
	if len(m) == 0 {
		return nil
	}
	return stdmaps.Clone(m)
}
