package intentrouter

import (
	"fmt"
	"testing"
	"time"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/mapper"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
)

func TestEventTypeToProto(t *testing.T) {
	tests := []struct {
		input EventType
		want  napv1.EventType
	}{
		{EventTypeUserIntent, napv1.EventType_EVENT_TYPE_USER_INTENT},
		{EventTypeSessionOutput, napv1.EventType_EVENT_TYPE_SESSION_OUTPUT},
		{EventTypeClarification, napv1.EventType_EVENT_TYPE_CLARIFICATION},
		{EventTypeHeartbeat, napv1.EventType_EVENT_TYPE_SCHEDULED_TASK},
		{EventTypeOnboarding, napv1.EventType_EVENT_TYPE_ONBOARDING},
		{EventTypeJournalCompaction, napv1.EventType_EVENT_TYPE_JOURNAL_COMPACTION},
		{EventTypeConversationCompaction, napv1.EventType_EVENT_TYPE_CONVERSATION_COMPACTION},
		{"unknown_type", napv1.EventType_EVENT_TYPE_UNSPECIFIED},
		{"", napv1.EventType_EVENT_TYPE_UNSPECIFIED},
	}

	for _, tt := range tests {
		name := string(tt.input)
		if name == "" {
			name = "(empty)"
		}
		t.Run(fmt.Sprintf("eventType=%s", name), func(t *testing.T) {
			got := eventTypeToProto(tt.input, "test-adapter")
			if got != tt.want {
				t.Errorf("eventTypeToProto(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestSessionInfoIdleFieldsMappedToProto(t *testing.T) {
	now := time.Now().UTC()
	session := SessionInfo{
		ID:         "test-session",
		Command:    "bash",
		Status:     "running",
		IdleState:  "waiting-for-input",
		WaitingFor: "user-response",
		IdleSince:  now,
		Metadata:   map[string]string{"key": "val"},
	}

	proto := &napv1.SessionInfo{
		Id:         session.ID,
		Command:    session.Command,
		Status:     session.Status,
		StartTime:  mapper.ToProtoTimestampChecked(session.StartTime),
		Metadata:   maputil.Clone(session.Metadata),
		IdleState:  session.IdleState,
		WaitingFor: session.WaitingFor,
		IdleSince:  mapper.ToProtoTimestampChecked(session.IdleSince),
	}

	if proto.IdleState != "waiting-for-input" {
		t.Errorf("IdleState = %q, want %q", proto.IdleState, "waiting-for-input")
	}
	if proto.WaitingFor != "user-response" {
		t.Errorf("WaitingFor = %q, want %q", proto.WaitingFor, "user-response")
	}
	if proto.IdleSince == nil {
		t.Fatal("IdleSince should not be nil for non-zero time")
	}
	if proto.IdleSince.AsTime().Sub(now).Abs() > time.Second {
		t.Errorf("IdleSince time drift: got %v, want ~%v", proto.IdleSince.AsTime(), now)
	}
}

func TestEventTypeToProtoCoversAllPromptEventTypes(t *testing.T) {
	for _, et := range constants.PromptEventTypes {
		got := eventTypeToProto(EventType(et), "test-adapter")
		if got == napv1.EventType_EVENT_TYPE_UNSPECIFIED {
			t.Errorf("eventTypeToProto(%q) returned UNSPECIFIED — missing switch case", et)
		}
	}
}
