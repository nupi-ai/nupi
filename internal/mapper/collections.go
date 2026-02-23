package mapper

import "sort"

// MapSlice transforms each element from input using transform.
func MapSlice[In any, Out any](input []In, transform func(In) Out) []Out {
	out := make([]Out, 0, len(input))
	for _, item := range input {
		out = append(out, transform(item))
	}
	return out
}

// StringMapToSortedSlice converts a map into a slice sorted by map key.
// It returns nil when the input map is empty.
func StringMapToSortedSlice[Out any](input map[string]string, transform func(key, value string) Out) []Out {
	if len(input) == 0 {
		return nil
	}

	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]Out, 0, len(keys))
	for _, key := range keys {
		out = append(out, transform(key, input[key]))
	}
	return out
}

// StringMapFromSlice converts key/value entry slice into a map.
// It returns nil when the input slice is empty.
func StringMapFromSlice[In any](input []In, keyValue func(In) (key, value string)) map[string]string {
	if len(input) == 0 {
		return nil
	}

	out := make(map[string]string, len(input))
	for _, entry := range input {
		key, value := keyValue(entry)
		out[key] = value
	}
	return out
}
