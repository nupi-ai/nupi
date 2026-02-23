package mapper

import (
	"testing"
	"time"
)

func TestParseTimestampStringToProto(t *testing.T) {
	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{name: "empty", input: "", valid: false},
		{name: "invalid", input: "not-a-timestamp", valid: false},
		{name: "rfc3339", input: "2026-02-23T12:34:56+02:00", valid: true},
		{name: "sqlite", input: "2026-02-23 12:34:56", valid: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseTimestampStringToProto(tc.input)
			if tc.valid && got == nil {
				t.Fatalf("expected non-nil for %q", tc.input)
			}
			if !tc.valid && got != nil {
				t.Fatalf("expected nil for %q, got %v", tc.input, got)
			}
		})
	}
}

func TestParseTimestampStringToProtoSQLiteUsesLocalLocation(t *testing.T) {
	input := "2026-02-23 12:34:56"
	got := ParseTimestampStringToProto(input)
	if got == nil {
		t.Fatalf("expected non-nil for %q", input)
	}

	expectedLocal, err := time.ParseInLocation(sqliteTimestampLayout, input, time.Local)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !got.AsTime().Equal(expectedLocal.UTC()) {
		t.Fatalf("expected %v, got %v", expectedLocal.UTC(), got.AsTime())
	}
}
