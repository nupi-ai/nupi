package mapper

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

const sqliteTimestampLayout = "2006-01-02 15:04:05"

// ParseTimestampStringToProto parses a timestamp string into a protobuf Timestamp.
// It supports RFC3339 and SQLite CURRENT_TIMESTAMP layout and returns nil for
// empty or unparseable values.
func ParseTimestampStringToProto(s string) *timestamppb.Timestamp {
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return ToProtoTimestamp(t.UTC())
	}
	if t, err := time.ParseInLocation(sqliteTimestampLayout, s, time.Local); err == nil {
		return ToProtoTimestamp(t.UTC())
	}
	return nil
}
