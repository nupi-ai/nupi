package constants

import "fmt"

const (
	PromptEventUserIntent              = "user_intent"
	PromptEventSessionOutput           = "session_output"
	PromptEventClarification           = "clarification"
	PromptEventOnboarding              = "onboarding"
	PromptEventHeartbeat               = "heartbeat"
	PromptEventJournalCompaction       = "journal_compaction"
	PromptEventConversationCompaction  = "conversation_compaction"
)

// Heartbeat field length limits shared between awareness tool handlers and CLI.
const (
	MaxHeartbeatNameLen     = 255
	MaxHeartbeatPromptLen   = 10000
	MaxHeartbeatCronExprLen = 255
	MaxHeartbeatCount       = 100
)

// ValidateHeartbeatName rejects names containing control characters or double
// quotes. Control characters cause log and display corruption. Double quotes
// break the quoted context in heartbeat.md prompt template, creating a prompt
// injection vector (heartbeat names are AI-created via heartbeat_add tool).
func ValidateHeartbeatName(name string) error {
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("name contains control characters")
		}
		if r == '"' {
			return fmt.Errorf("name must not contain double quotes")
		}
	}
	return nil
}

// PromptEventTypes lists prompt event types accepted by configuration storage.
var PromptEventTypes = []string{
	PromptEventUserIntent,
	PromptEventSessionOutput,
	PromptEventClarification,
	PromptEventOnboarding,
	PromptEventHeartbeat,
	PromptEventJournalCompaction,
	PromptEventConversationCompaction,
}
