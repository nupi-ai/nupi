package constants

const (
	MetadataKeyEventType             = "event_type"
	MetadataKeyCurrentTool           = "current_tool"
	MetadataKeySessionOutput         = "session_output"
	MetadataKeyClarificationQuestion = "clarification_question"
	MetadataKeyWaitingFor            = "waiting_for"
	MetadataKeyNotable               = "notable"
	MetadataKeyIdleState             = "idle_state"
	MetadataKeyPromptText            = "prompt_text"
	MetadataKeyTool                  = "tool"
	MetadataKeyToolID                = "tool_id"
	MetadataKeyToolChanged           = "tool_changed"
	MetadataKeyConfidence            = "confidence"
	MetadataKeyStreamID              = "stream_id"
	MetadataKeySeverity              = "severity"
	MetadataKeyInputSource           = "input_source"
	MetadataKeyMode                  = "mode"
	MetadataKeySessionless           = "sessionless"
	MetadataKeyAdapter               = "adapter"
	MetadataKeyLanguage              = "language"
	MetadataKeyModel                 = "model"
	MetadataKeyProvider              = "provider"
	MetadataKeyPriority              = "priority"
	MetadataKeyContextWindow         = "context_window"
	MetadataKeyEventCount            = "event_count"
	MetadataKeyEventTitle            = "event_title"
	MetadataKeyEventDetails          = "event_details"
	MetadataKeyEventAction           = "event_action"
	MetadataKeySummarized            = "summarized"
	MetadataKeyOriginalLength        = "original_length"
	MetadataKeyBufferTruncated       = "buffer_truncated"
	MetadataKeyBufferMaxSize         = "buffer_max_size"
	MetadataKeyPromptID              = "prompt_id"
	MetadataKeyBargeIn               = "barge_in"
	MetadataKeyBargeInReason         = "barge_in_reason"
	MetadataKeyBargeInTimestamp      = "barge_in_timestamp"
	MetadataKeyTrigger               = "trigger"
	MetadataKeyStatus                = "status"
	MetadataKeyError                 = "error"
	MetadataKeyErrorType             = "error_type"
	MetadataKeyRecoverable           = "recoverable"
	MetadataKeySessionID             = "session_id"
	MetadataKeySuggestedSession      = "suggested_session"
	MetadataKeyRoutingHint           = "routing_hint"
	MetadataKeySource                = "source"
	MetadataKeyOrigin                = "origin"
	MetadataKeyReason                = "reason"
	MetadataKeyLocale                = "locale"
	MetadataKeyCleaned               = "cleaned"
	MetadataKeyClientType            = "client_type"
	MetadataKeyVADStreamID           = "vad_stream_id"
	MetadataKeyMockText              = "mock_text"
	MetadataKeyPhase                 = "phase"
	MetadataKeyTextLength            = "text_length"
	MetadataKeyDevice                = "device"
	MetadataKeyPart                  = "part"
	MetadataKeyVoice                 = "voice"
	MetadataKeyChunkID               = "chunk_id"
	MetadataKeySynthesisTimeMS       = "synthesis_time_ms"
	MetadataKeyVoiceID               = "voice_id"
	MetadataKeyLatency               = "latency"
	MetadataKeyProgress              = "progress"
	MetadataKeyLanguagePrefix        = "nupi.lang."
	MetadataKeyBargePrefix           = "barge_"

	// Template data keys for awareness context injection (Epic 22)
	TemplateKeyJournalSummaries      = "journal_summaries"
	TemplateKeyJournalRaw            = "journal_raw"
	TemplateKeyConversationSummaries = "conversation_summaries"
	TemplateKeyConversationRaw       = "conversation_raw"
)

// IsValidWaitingForValue reports whether v is a recognized waiting_for annotation value.
func IsValidWaitingForValue(v string) bool {
	switch v {
	case PipelineWaitingForUserInput, PipelineWaitingForConfirmation, PipelineWaitingForChoice:
		return true
	default:
		return false
	}
}

const (
	MetadataValueTrue = "true"
)
