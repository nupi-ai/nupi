package mapper

// MergeStringMaps merges maps in order, where later maps override earlier
// values for the same key. It returns nil when all inputs are empty.
func MergeStringMaps(maps ...map[string]string) map[string]string {
	total := 0
	for _, m := range maps {
		total += len(m)
	}
	if total == 0 {
		return nil
	}

	out := make(map[string]string, total)
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
