package tooldetectors

// IdleState describes whether a tool is waiting for user input.
type IdleState struct {
	IsIdle     bool   `json:"is_idle"`
	Reason     string `json:"reason,omitempty"`      // "prompt", "question", "timeout", "marker"
	PromptText string `json:"prompt_text,omitempty"` // detected prompt if any
	WaitingFor string `json:"waiting_for,omitempty"` // "user_input", "confirmation", "choice"
}

// Event represents a notable occurrence in tool output.
type Event struct {
	Severity         string `json:"severity"`                     // error, warning, info, success
	Title            string `json:"title"`
	Details          string `json:"details,omitempty"`
	ActionSuggestion string `json:"action_suggestion,omitempty"`
}

// ProcessorResult contains all results from tool processing.
type ProcessorResult struct {
	Cleaned   string     `json:"cleaned"`
	Events    []Event    `json:"events,omitempty"`
	Summary   string     `json:"summary,omitempty"`
	IdleState *IdleState `json:"idle_state,omitempty"`
}
