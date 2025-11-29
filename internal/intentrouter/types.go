package intentrouter

import (
	"context"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// ActionType describes the type of action AI requests.
type ActionType string

const (
	// ActionCommand requests executing a command in a session.
	ActionCommand ActionType = "command"
	// ActionSpeak requests speaking a response via TTS without executing anything.
	ActionSpeak ActionType = "speak"
	// ActionClarify requests additional information from the user.
	ActionClarify ActionType = "clarify"
	// ActionNoop indicates no action is needed.
	ActionNoop ActionType = "noop"
)

// SessionInfo provides context about an available session.
type SessionInfo struct {
	ID         string            `json:"id"`
	Command    string            `json:"command"`
	Args       []string          `json:"args,omitempty"`
	WorkDir    string            `json:"work_dir,omitempty"`
	Tool       string            `json:"tool,omitempty"`
	Status     string            `json:"status"`
	StartTime  time.Time         `json:"start_time"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// EventType categorizes the type of prompt being sent to the AI.
type EventType string

const (
	// EventTypeUserIntent is the default - user voice/text requiring interpretation.
	EventTypeUserIntent EventType = "user_intent"
	// EventTypeSessionOutput is for notable terminal output needing AI analysis.
	EventTypeSessionOutput EventType = "session_output"
	// EventTypeHistorySummary requests summarizing conversation history.
	EventTypeHistorySummary EventType = "history_summary"
	// EventTypeClarification follows a previous clarification request.
	EventTypeClarification EventType = "clarification"
)

// IntentRequest contains the context sent to the AI adapter for intent resolution.
type IntentRequest struct {
	// PromptID identifies the conversation prompt that triggered this request.
	PromptID string `json:"prompt_id"`

	// SessionID is the session context (may be empty if multiple sessions exist).
	SessionID string `json:"session_id,omitempty"`

	// EventType categorizes the prompt (user_intent, session_output, etc.).
	EventType EventType `json:"event_type"`

	// Transcript is the user's speech or text input.
	Transcript string `json:"transcript"`

	// ConversationHistory contains recent conversation turns for context.
	ConversationHistory []eventbus.ConversationTurn `json:"conversation_history,omitempty"`

	// AvailableSessions lists all sessions the user can interact with.
	AvailableSessions []SessionInfo `json:"available_sessions"`

	// CurrentTool is the detected tool/command in the current session.
	CurrentTool string `json:"current_tool,omitempty"`

	// SessionOutput contains terminal output for session_output events.
	SessionOutput string `json:"session_output,omitempty"`

	// ClarificationQuestion contains the original question for clarification events.
	ClarificationQuestion string `json:"clarification_question,omitempty"`

	// SystemPrompt is the pre-built system prompt from Prompts Engine.
	SystemPrompt string `json:"system_prompt,omitempty"`

	// UserPrompt is the pre-built user prompt from Prompts Engine.
	UserPrompt string `json:"user_prompt,omitempty"`

	// Metadata contains additional context (e.g., detected tool, confidence).
	Metadata map[string]string `json:"metadata,omitempty"`
}

// IntentAction represents a single action the AI wants to perform.
type IntentAction struct {
	// Type is the action type (command, speak, clarify, noop).
	Type ActionType `json:"type"`

	// SessionRef identifies the target session for command actions.
	SessionRef string `json:"session_ref,omitempty"`

	// Command is the shell command to execute (for ActionCommand).
	Command string `json:"command,omitempty"`

	// Text is the message to speak or clarification question.
	Text string `json:"text,omitempty"`

	// Metadata contains additional action-specific data.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// IntentResponse contains the AI adapter's decision.
type IntentResponse struct {
	// PromptID echoes the request's prompt ID.
	PromptID string `json:"prompt_id"`

	// Actions is the list of actions to perform (usually one, but can be multiple).
	Actions []IntentAction `json:"actions"`

	// Reasoning explains the AI's decision (for debugging/logging).
	Reasoning string `json:"reasoning,omitempty"`

	// Confidence indicates how confident the AI is (0.0-1.0).
	Confidence float32 `json:"confidence,omitempty"`

	// Metadata contains additional response data.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// IntentAdapter defines the interface for AI adapters that resolve user intents.
type IntentAdapter interface {
	// ResolveIntent processes the user's input and returns the intended action(s).
	ResolveIntent(ctx context.Context, req IntentRequest) (*IntentResponse, error)

	// Name returns the adapter's identifier.
	Name() string

	// Ready returns true if the adapter is ready to process requests.
	Ready() bool
}

// SessionProvider provides access to session information.
type SessionProvider interface {
	// ListSessionInfos returns information about all available sessions.
	ListSessionInfos() []SessionInfo

	// GetSessionInfo returns information about a specific session.
	GetSessionInfo(sessionID string) (SessionInfo, bool)

	// ValidateSession checks if a session exists and is accessible.
	ValidateSession(sessionID string) error
}

// CommandExecutor handles command execution in sessions.
type CommandExecutor interface {
	// QueueCommand queues a command for execution in the specified session.
	// The origin parameter indicates who issued the command (user/ai).
	QueueCommand(sessionID string, command string, origin eventbus.ContentOrigin) error
}

// Option configures the Service.
type Option func(*Service)

// WithAdapter sets the AI adapter for intent resolution.
func WithAdapter(adapter IntentAdapter) Option {
	return func(s *Service) {
		s.adapter = adapter
	}
}

// WithSessionProvider sets the session provider.
func WithSessionProvider(provider SessionProvider) Option {
	return func(s *Service) {
		s.sessionProvider = provider
	}
}

// WithCommandExecutor sets the command executor.
func WithCommandExecutor(executor CommandExecutor) Option {
	return func(s *Service) {
		s.commandExecutor = executor
	}
}

// PromptEngine builds system and user prompts for AI adapters.
type PromptEngine interface {
	// Build generates prompts for the given request context.
	Build(req PromptBuildRequest) (*PromptBuildResponse, error)
}

// PromptBuildRequest contains the context for building prompts.
type PromptBuildRequest struct {
	EventType             EventType
	SessionID             string
	Transcript            string
	History               []eventbus.ConversationTurn
	AvailableSessions     []SessionInfo
	CurrentTool           string
	SessionOutput         string
	ClarificationQuestion string
	Metadata              map[string]string
}

// PromptBuildResponse contains the generated prompts.
type PromptBuildResponse struct {
	SystemPrompt string
	UserPrompt   string
	Context      map[string]any
}

// WithPromptEngine sets the prompt engine for generating AI prompts.
func WithPromptEngine(engine PromptEngine) Option {
	return func(s *Service) {
		s.promptEngine = engine
	}
}
