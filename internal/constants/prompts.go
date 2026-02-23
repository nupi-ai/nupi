package constants

const (
	PromptEventUserIntent     = "user_intent"
	PromptEventSessionOutput  = "session_output"
	PromptEventHistorySummary = "history_summary"
	PromptEventClarification  = "clarification"
	PromptEventMemoryFlush    = "memory_flush"
	PromptEventSessionSlug    = "session_slug"
	PromptEventOnboarding     = "onboarding"
)

// PromptEventTypes lists prompt event types accepted by configuration storage.
var PromptEventTypes = []string{
	PromptEventUserIntent,
	PromptEventSessionOutput,
	PromptEventHistorySummary,
	PromptEventClarification,
	PromptEventMemoryFlush,
	PromptEventSessionSlug,
	PromptEventOnboarding,
}
