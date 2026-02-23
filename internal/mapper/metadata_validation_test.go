package mapper

import (
	"strings"
	"testing"
)

func TestNormalizeAndValidateMetadata(t *testing.T) {
	limits := MetadataLimits{
		MaxEntries:    2,
		MaxKeyRunes:   3,
		MaxValueRunes: 4,
		MaxTotalBytes: 16,
	}

	got, err := NormalizeAndValidateMetadata(map[string]string{
		"  a  ": "  b  ",
		"":      "ignored",
	}, limits)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got["a"] != "b" {
		t.Fatalf("unexpected normalized map: %+v", got)
	}
}

func TestNormalizeAndValidateMetadataLimits(t *testing.T) {
	limits := MetadataLimits{
		MaxEntries:    2,
		MaxKeyRunes:   3,
		MaxValueRunes: 4,
		MaxTotalBytes: 10,
	}

	tests := []struct {
		name string
		in   map[string]string
	}{
		{
			name: "too_many_entries",
			in:   map[string]string{"a": "b", "c": "d", "e": "f"},
		},
		{
			name: "key_too_long",
			in:   map[string]string{"abcd": "v"},
		},
		{
			name: "value_too_long",
			in:   map[string]string{"a": strings.Repeat("x", 5)},
		},
		{
			name: "too_many_total_bytes",
			in:   map[string]string{"abc": "xxxx", "de": "yyyy"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NormalizeAndValidateMetadata(tc.in, limits); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
