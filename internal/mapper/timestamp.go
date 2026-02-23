package mapper

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// ToProtoTimestamp converts a non-zero time to protobuf timestamp.
// It returns nil for zero time values.
func ToProtoTimestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// ToProtoTimestampPtr converts a non-nil, non-zero time pointer to protobuf timestamp.
func ToProtoTimestampPtr(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return ToProtoTimestamp(*t)
}

// ToProtoTimestampChecked converts time to protobuf timestamp and returns nil
// when the resulting timestamp is invalid.
func ToProtoTimestampChecked(t time.Time) *timestamppb.Timestamp {
	ts := ToProtoTimestamp(t)
	if ts == nil {
		return nil
	}
	if err := ts.CheckValid(); err != nil {
		return nil
	}
	return ts
}
