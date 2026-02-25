package intentrouter

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nupi-ai/nupi/internal/constants"
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
	// ActionToolUse indicates the AI wants to invoke one or more tools.
	ActionToolUse ActionType = "tool_use"
)

// SessionInfo provides context about an available session.
type SessionInfo struct {
	ID        string            `json:"id"`
	Command   string            `json:"command"`
	Args      []string          `json:"args,omitempty"`
	WorkDir   string            `json:"work_dir,omitempty"`
	Tool      string            `json:"tool,omitempty"`
	Status    string            `json:"status"`
	StartTime time.Time         `json:"start_time"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// ToolDefinition describes a tool available to the AI for invocation.
type ToolDefinition struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	ParametersJSON string `json:"parameters_json"`
}

// ToolCall represents the AI's request to invoke a specific tool.
type ToolCall struct {
	CallID        string `json:"call_id"`
	ToolName      string `json:"tool_name"`
	ArgumentsJSON string `json:"arguments_json"`
}

// ToolResult contains the outcome of a tool invocation.
type ToolResult struct {
	CallID     string `json:"call_id"`
	ResultJSON string `json:"result_json"`
	IsError    bool   `json:"is_error"`
}

// ToolInteraction pairs a tool call with its result for history tracking.
type ToolInteraction struct {
	Call   ToolCall   `json:"call"`
	Result ToolResult `json:"result"`
}

// ToolHandler executes a tool call and returns the result.
type ToolHandler interface {
	// Execute runs the tool with the given arguments and returns a JSON result.
	Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error)

	// Definition returns the tool's metadata (name, description, parameters schema).
	Definition() ToolDefinition
}

// ToolRegistry manages available tools and controls which tools are offered per event type.
type ToolRegistry interface {
	// Register adds a tool handler to the registry, keyed by its Definition().Name.
	Register(handler ToolHandler)

	// GetToolsForEventType returns definitions for tools allowed for the given event type,
	// filtered to only include tools that have been registered.
	GetToolsForEventType(eventType EventType) []ToolDefinition

	// Execute looks up a tool by name and calls its handler with the given arguments.
	Execute(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
}

// EventType categorizes the type of prompt being sent to the AI.
type EventType string

const (
	// EventTypeUserIntent is the default - user voice/text requiring interpretation.
	EventTypeUserIntent EventType = constants.PromptEventUserIntent
	// EventTypeSessionOutput is for notable terminal output needing AI analysis.
	EventTypeSessionOutput EventType = constants.PromptEventSessionOutput
	// EventTypeHistorySummary requests summarizing conversation history.
	EventTypeHistorySummary EventType = constants.PromptEventHistorySummary
	// EventTypeClarification follows a previous clarification request.
	EventTypeClarification EventType = constants.PromptEventClarification
	// EventTypeMemoryFlush saves memories before conversation compaction.
	EventTypeMemoryFlush EventType = constants.PromptEventMemoryFlush
	// EventTypeHeartbeat is for periodic background heartbeat tasks.
	EventTypeHeartbeat EventType = constants.PromptEventHeartbeat
	// EventTypeSessionSlug generates session file names.
	EventTypeSessionSlug EventType = constants.PromptEventSessionSlug
	// EventTypeOnboarding is for first-time user setup.
	EventTypeOnboarding EventType = constants.PromptEventOnboarding
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

	// AvailableTools lists tool definitions the AI may invoke during this request.
	AvailableTools []ToolDefinition `json:"available_tools,omitempty"`

	// ToolHistory contains prior tool call/result pairs from earlier iterations
	// of the same prompt resolution.
	ToolHistory []ToolInteraction `json:"tool_history,omitempty"`
}

// IntentAction represents a single action the AI wants to perform.
type IntentAction struct {
	// Type is the action type (command, speak, clarify, noop, tool_use).
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

	// ToolCalls contains tool invocations requested by the AI.
	// Non-empty when an action has type ActionToolUse.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
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
	// The origin parameter indicates who issued the command (user/ai/tool/system)
	// and determines execution priority: user > tool > ai > system.
	QueueCommand(sessionID string, command string, origin eventbus.ContentOrigin) error

	// Stop signals the executor to stop its background goroutine.
	// Queued commands that have not yet been executed may be dropped.
	Stop()
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

// CoreMemoryProvider supplies core memory content for system prompt injection.
type CoreMemoryProvider interface {
	// CoreMemory returns the combined content of all core memory files.
	// Returns empty string if no core memory is available.
	CoreMemory() string
}

// WithCoreMemoryProvider sets the core memory provider for awareness injection.
func WithCoreMemoryProvider(provider CoreMemoryProvider) Option {
	return func(s *Service) {
		s.coreMemoryProvider = provider
	}
}

// WithToolRegistry sets the tool registry for tool-use support.
func WithToolRegistry(registry ToolRegistry) Option {
	return func(s *Service) {
		s.toolRegistry = registry
	}
}

// OnboardingProvider checks onboarding state and provides bootstrap content.
type OnboardingProvider interface {
	IsOnboardingSession(sessionID string) bool
	BootstrapContent() string
}

// WithOnboardingProvider sets the onboarding provider for first-time setup detection.
func WithOnboardingProvider(p OnboardingProvider) Option {
	return func(s *Service) {
		s.onboardingProvider = p
	}
}
