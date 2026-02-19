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
		{EventTypeHistorySummary, napv1.EventType_EVENT_TYPE_HISTORY_SUMMARY},
		{EventTypeClarification, napv1.EventType_EVENT_TYPE_CLARIFICATION},
		{EventTypeMemoryFlush, napv1.EventType_EVENT_TYPE_MEMORY_FLUSH},
		{EventTypeScheduledTask, napv1.EventType_EVENT_TYPE_SCHEDULED_TASK},
		{EventTypeSessionSlug, napv1.EventType_EVENT_TYPE_SESSION_SLUG},
		{EventTypeOnboarding, napv1.EventType_EVENT_TYPE_ONBOARDING},
		{"unknown_type", napv1.EventType_EVENT_TYPE_UNSPECIFIED},
		{"", napv1.EventType_EVENT_TYPE_UNSPECIFIED},
	}

	for _, tt := range tests {
		name := string(tt.input)
		if name == "" {
			name = "(empty)"
		}
		t.Run(fmt.Sprintf("eventType=%s", name), func(t *testing.T) {
			got := eventTypeToProto(tt.input)
			if got != tt.want {
				t.Errorf("eventTypeToProto(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
