package mapper

import (
	"reflect"
	"testing"
)

func TestMapSlice(t *testing.T) {
	input := []int{1, 2, 3}
	got := MapSlice(input, func(v int) string { return string(rune('a' + v - 1)) })
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestStringMapToSortedSlice(t *testing.T) {
	input := map[string]string{"b": "2", "a": "1"}
	got := StringMapToSortedSlice(input, func(key, value string) string { return key + "=" + value })
	want := []string{"a=1", "b=2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestStringMapToSortedSliceEmpty(t *testing.T) {
	if got := StringMapToSortedSlice(map[string]string{}, func(key, value string) string { return key + value }); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

type kvEntry struct {
	Key   string
	Value string
}

func TestStringMapFromSlice(t *testing.T) {
	input := []kvEntry{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}}
	got := StringMapFromSlice(input, func(e kvEntry) (string, string) { return e.Key, e.Value })
	want := map[string]string{"a": "1", "b": "2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestStringMapFromSliceEmpty(t *testing.T) {
	if got := StringMapFromSlice([]kvEntry{}, func(e kvEntry) (string, string) { return e.Key, e.Value }); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}
