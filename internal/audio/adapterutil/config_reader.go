package adapterutil

import "time"

// ConfigReader provides typed accessors for adapter mock configuration maps.
type ConfigReader struct {
	cfg map[string]any
}

func NewConfigReader(cfg map[string]any) ConfigReader {
	return ConfigReader{cfg: cfg}
}

func (r ConfigReader) Float64(key string, def float64) float64 {
	if r.cfg == nil {
		return def
	}
	value, ok := r.cfg[key]
	if !ok {
		return def
	}
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	}
	return def
}

func (r ConfigReader) Float32(key string, def float32) float32 {
	if r.cfg == nil {
		return def
	}
	value, ok := r.cfg[key]
	if !ok {
		return def
	}
	switch v := value.(type) {
	case float64:
		return float32(v)
	case float32:
		return v
	case int:
		return float32(v)
	}
	return def
}

func (r ConfigReader) Int(key string, def int) int {
	if r.cfg == nil {
		return def
	}
	value, ok := r.cfg[key]
	if !ok {
		return def
	}
	switch v := value.(type) {
	case int:
		return v
	case float64:
		return int(v)
	case float32:
		return int(v)
	}
	return def
}

func (r ConfigReader) String(key string) string {
	if r.cfg == nil {
		return ""
	}
	value, ok := r.cfg[key]
	if !ok {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	}
	return ""
}

func (r ConfigReader) Bool(key string, def bool) bool {
	if r.cfg == nil {
		return def
	}
	value, ok := r.cfg[key]
	if !ok {
		return def
	}
	switch v := value.(type) {
	case bool:
		return v
	}
	return def
}

func (r ConfigReader) StringSlice(key string) []string {
	if r.cfg == nil {
		return nil
	}
	val, ok := r.cfg[key]
	if !ok {
		return nil
	}

	switch raw := val.(type) {
	case []any:
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			switch v := item.(type) {
			case string:
				out = append(out, v)
			}
		}
		return out
	case []string:
		return append([]string(nil), raw...)
	default:
		return nil
	}
}

// Duration parses numeric config values as milliseconds.
func (r ConfigReader) Duration(key string, def time.Duration) time.Duration {
	if r.cfg == nil {
		return def
	}
	value, ok := r.cfg[key]
	if !ok {
		return def
	}
	switch v := value.(type) {
	case float64:
		return time.Duration(v) * time.Millisecond
	case float32:
		return time.Duration(v) * time.Millisecond
	case int:
		return time.Duration(v) * time.Millisecond
	}
	return def
}

func (r ConfigReader) StringMap(key string) map[string]string {
	if r.cfg == nil {
		return nil
	}
	val, ok := r.cfg[key]
	if !ok {
		return nil
	}

	out := make(map[string]string)
	switch raw := val.(type) {
	case map[string]any:
		for k, v := range raw {
			if str, ok := v.(string); ok {
				out[k] = str
			}
		}
	case map[string]string:
		for k, v := range raw {
			out[k] = v
		}
	}
	return out
}
