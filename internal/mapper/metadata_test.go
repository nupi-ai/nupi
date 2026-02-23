package mapper

import "testing"

func TestMergeStringMaps(t *testing.T) {
	tests := []struct {
		name string
		in   []map[string]string
		out  map[string]string
		nil  bool
	}{
		{
			name: "all empty",
			in:   []map[string]string{nil, {}, nil},
			nil:  true,
		},
		{
			name: "single map",
			in: []map[string]string{
				{"a": "1"},
			},
			out: map[string]string{"a": "1"},
		},
		{
			name: "later overrides earlier",
			in: []map[string]string{
				{"a": "1", "b": "1"},
				{"b": "2"},
				{"c": "3"},
			},
			out: map[string]string{"a": "1", "b": "2", "c": "3"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MergeStringMaps(tc.in...)
			if tc.nil {
				if got != nil {
					t.Fatalf("expected nil map, got %v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil map")
			}
			if len(got) != len(tc.out) {
				t.Fatalf("expected len %d, got %d", len(tc.out), len(got))
			}
			for k, want := range tc.out {
				if got[k] != want {
					t.Fatalf("expected key %q=%q, got %q", k, want, got[k])
				}
			}
		})
	}
}
