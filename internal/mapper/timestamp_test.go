package mapper

import (
	"testing"
	"time"
)

func TestToProtoTimestamp(t *testing.T) {
	if got := ToProtoTimestamp(time.Time{}); got != nil {
		t.Fatalf("expected nil for zero time, got %v", got)
	}

	now := time.Now()
	got := ToProtoTimestamp(now)
	if got == nil {
		t.Fatal("expected non-nil timestamp")
	}
	if !got.AsTime().Equal(now) {
		t.Fatalf("expected %v, got %v", now, got.AsTime())
	}
}

func TestToProtoTimestampPtr(t *testing.T) {
	if got := ToProtoTimestampPtr(nil); got != nil {
		t.Fatalf("expected nil for nil ptr, got %v", got)
	}

	ts := time.Now()
	got := ToProtoTimestampPtr(&ts)
	if got == nil {
		t.Fatal("expected non-nil timestamp")
	}
	if !got.AsTime().Equal(ts) {
		t.Fatalf("expected %v, got %v", ts, got.AsTime())
	}
}

func TestToProtoTimestampChecked(t *testing.T) {
	invalid := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := ToProtoTimestampChecked(invalid); got != nil {
		t.Fatalf("expected nil for invalid timestamp, got %v", got)
	}

	valid := time.Now()
	got := ToProtoTimestampChecked(valid)
	if got == nil {
		t.Fatal("expected non-nil timestamp")
	}
	if !got.AsTime().Equal(valid) {
		t.Fatalf("expected %v, got %v", valid, got.AsTime())
	}
}
