package intentrouter

import (
	"fmt"
	"testing"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
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
		// TODO(epic-18.3): Update expected values below to real proto enums
		// (EVENT_TYPE_JOURNAL_COMPACTION, EVENT_TYPE_CONVERSATION_COMPACTION)
		// when proto enum values are added in ai.proto.
		{EventTypeJournalCompaction, napv1.EventType_EVENT_TYPE_UNSPECIFIED},
		{EventTypeConversationCompaction, napv1.EventType_EVENT_TYPE_UNSPECIFIED},
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
