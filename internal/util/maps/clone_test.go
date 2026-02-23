package maps

import "testing"

func TestCloneNil(t *testing.T) {
	if got := Clone[string, string](nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestCloneEmpty(t *testing.T) {
	if got := Clone(map[string]string{}); got != nil {
		t.Fatalf("expected nil for empty map, got %v", got)
	}
}

func TestCloneIsolation(t *testing.T) {
	src := map[string]string{"a": "1", "b": "2"}
	dst := Clone(src)
	dst["c"] = "3"
	if _, ok := src["c"]; ok {
		t.Fatalf("mutation leaked to source")
	}
}

func TestCloneGenericType(t *testing.T) {
	src := map[int]bool{1: true}
	dst := Clone(src)
	if len(dst) != 1 || !dst[1] {
		t.Fatalf("unexpected clone content: %v", dst)
	}
}
