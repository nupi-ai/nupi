package adapterutil

import (
	"testing"
	"time"
)

func TestConfigReaderTypedAccessors(t *testing.T) {
	cfg := map[string]any{
		"f64":         float64(1.25),
		"f32":         float32(2.5),
		"i":           int(3),
		"s":           "text",
		"b":           true,
		"dur_ms":      float64(150),
		"slice_any":   []any{"a", 1, "b"},
		"slice_str":   []string{"x", "y"},
		"map_any":     map[string]any{"a": "1", "b": 2},
		"map_string":  map[string]string{"x": "1", "y": "2"},
		"unsupported": struct{}{},
	}
	r := NewConfigReader(cfg)

	if got := r.Float64("f64", 0); got != 1.25 {
		t.Fatalf("Float64(f64) = %v, want 1.25", got)
	}
	if got := r.Float64("f32", 0); got != 2.5 {
		t.Fatalf("Float64(f32) = %v, want 2.5", got)
	}
	if got := r.Float64("i", 0); got != 3 {
		t.Fatalf("Float64(i) = %v, want 3", got)
	}

	if got := r.Float32("f64", 0); got != 1.25 {
		t.Fatalf("Float32(f64) = %v, want 1.25", got)
	}
	if got := r.Float32("f32", 0); got != 2.5 {
		t.Fatalf("Float32(f32) = %v, want 2.5", got)
	}
	if got := r.Float32("i", 0); got != 3 {
		t.Fatalf("Float32(i) = %v, want 3", got)
	}

	if got := r.Int("i", 0); got != 3 {
		t.Fatalf("Int(i) = %v, want 3", got)
	}
	if got := r.Int("f64", 0); got != 1 {
		t.Fatalf("Int(f64) = %v, want 1", got)
	}
	if got := r.Int("f32", 0); got != 2 {
		t.Fatalf("Int(f32) = %v, want 2", got)
	}

	if got := r.String("s"); got != "text" {
		t.Fatalf("String(s) = %q, want text", got)
	}
	if got := r.Bool("b", false); !got {
		t.Fatalf("Bool(b) = %v, want true", got)
	}

	if got := r.Duration("dur_ms", 0); got != 150*time.Millisecond {
		t.Fatalf("Duration(dur_ms) = %v, want 150ms", got)
	}

	sliceAny := r.StringSlice("slice_any")
	if len(sliceAny) != 2 || sliceAny[0] != "a" || sliceAny[1] != "b" {
		t.Fatalf("StringSlice(slice_any) = %#v, want [a b]", sliceAny)
	}

	sliceStr := r.StringSlice("slice_str")
	if len(sliceStr) != 2 || sliceStr[0] != "x" || sliceStr[1] != "y" {
		t.Fatalf("StringSlice(slice_str) = %#v, want [x y]", sliceStr)
	}
	sliceStr[0] = "changed"
	if cfg["slice_str"].([]string)[0] != "x" {
		t.Fatal("StringSlice(slice_str) should return a copy")
	}

	mapAny := r.StringMap("map_any")
	if len(mapAny) != 1 || mapAny["a"] != "1" {
		t.Fatalf("StringMap(map_any) = %#v, want map[a:1]", mapAny)
	}

	mapString := r.StringMap("map_string")
	if len(mapString) != 2 || mapString["x"] != "1" || mapString["y"] != "2" {
		t.Fatalf("StringMap(map_string) = %#v, want map[x:1 y:2]", mapString)
	}
}

func TestConfigReaderDefaultsAndEdgeCases(t *testing.T) {
	var nilCfg map[string]any
	nilReader := NewConfigReader(nilCfg)

	if got := nilReader.Float64("missing", 1); got != 1 {
		t.Fatalf("Float64 nil cfg = %v, want 1", got)
	}
	if got := nilReader.Float32("missing", 2); got != 2 {
		t.Fatalf("Float32 nil cfg = %v, want 2", got)
	}
	if got := nilReader.Int("missing", 3); got != 3 {
		t.Fatalf("Int nil cfg = %v, want 3", got)
	}
	if got := nilReader.String("missing"); got != "" {
		t.Fatalf("String nil cfg = %q, want empty", got)
	}
	if got := nilReader.Bool("missing", true); !got {
		t.Fatalf("Bool nil cfg = %v, want true", got)
	}
	if got := nilReader.Duration("missing", 4*time.Millisecond); got != 4*time.Millisecond {
		t.Fatalf("Duration nil cfg = %v, want 4ms", got)
	}
	if got := nilReader.StringSlice("missing"); got != nil {
		t.Fatalf("StringSlice nil cfg = %#v, want nil", got)
	}
	if got := nilReader.StringMap("missing"); got != nil {
		t.Fatalf("StringMap nil cfg = %#v, want nil", got)
	}

	r := NewConfigReader(map[string]any{
		"unsupported": struct{}{},
		"slice_any":   []any{1, 2},
		"slice_bad":   123,
		"map_any":     map[string]any{"n": 1},
		"map_bad":     123,
	})

	if got := r.Float64("unsupported", 1); got != 1 {
		t.Fatalf("Float64 unsupported = %v, want 1", got)
	}
	if got := r.Float32("unsupported", 2); got != 2 {
		t.Fatalf("Float32 unsupported = %v, want 2", got)
	}
	if got := r.Int("unsupported", 3); got != 3 {
		t.Fatalf("Int unsupported = %v, want 3", got)
	}
	if got := r.Bool("unsupported", true); !got {
		t.Fatalf("Bool unsupported = %v, want true", got)
	}
	if got := r.Duration("unsupported", 4*time.Millisecond); got != 4*time.Millisecond {
		t.Fatalf("Duration unsupported = %v, want 4ms", got)
	}

	// Preserve old behavior: []any returns an empty (non-nil) slice
	// when no entries are strings.
	if got := r.StringSlice("slice_any"); got == nil || len(got) != 0 {
		t.Fatalf("StringSlice(slice_any) = %#v, want empty non-nil slice", got)
	}
	if got := r.StringSlice("slice_bad"); got != nil {
		t.Fatalf("StringSlice(slice_bad) = %#v, want nil", got)
	}

	// Preserve old behavior: map keys with unsupported types return an
	// empty (non-nil) map when the key exists.
	if got := r.StringMap("map_any"); got == nil || len(got) != 0 {
		t.Fatalf("StringMap(map_any) = %#v, want empty non-nil map", got)
	}
	if got := r.StringMap("map_bad"); got == nil || len(got) != 0 {
		t.Fatalf("StringMap(map_bad) = %#v, want empty non-nil map", got)
	}
}
