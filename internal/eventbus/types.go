package eventbus

import (
	"time"
)

// Topic identifies a logical channel on the bus.
type Topic string

// Standard topics emitted in phase 1.
const (
	TopicSessionsOutput     Topic = "sessions.output"
	TopicSessionsLifecycle  Topic = "sessions.lifecycle"
	TopicSessionsTool       Topic = "sessions.tool"
	TopicPipelineCleaned    Topic = "pipeline.cleaned"
	TopicPipelineError      Topic = "pipeline.error"
	TopicConversationPrompt Topic = "conversation.prompt"
	TopicConversationReply  Topic = "conversation.reply"
	TopicModulesStatus      Topic = "modules.status"
	TopicModulesLog         Topic = "modules.log"
)

// Source describes which component produced an event.
type Source string

const (
	SourceSessionManager  Source = "session_manager"
	SourceContentPipeline Source = "content_pipeline"
	SourceConversation    Source = "conversation"
	SourcePluginService   Source = "plugin_service"
	SourceModulesService  Source = "modules_service"
	SourceAdapterRunner   Source = "adapter_runner"
	SourceUnknown         Source = "unknown"
)

// Envelope wraps every message published on the bus.
type Envelope struct {
	Topic         Topic
	Timestamp     time.Time
	Source        Source
	CorrelationID string
	Payload       any
}

// ContentOrigin identifies who is the logical author of a message.
type ContentOrigin string

const (
	OriginUser   ContentOrigin = "user"
	OriginAI     ContentOrigin = "ai"
	OriginTool   ContentOrigin = "tool"
	OriginSystem ContentOrigin = "system"
)

// SessionState summarises lifecycle changes.
type SessionState string

const (
	SessionStateCreated  SessionState = "created"
	SessionStateRunning  SessionState = "running"
	SessionStateDetached SessionState = "detached"
	SessionStateStopped  SessionState = "stopped"
)

// SessionOutputEvent carries raw PTY chunks.
type SessionOutputEvent struct {
	SessionID string
	Sequence  uint64
	Data      []byte
	Origin    ContentOrigin
	Mode      string
}

// SessionLifecycleEvent notifies consumers about session state transitions.
type SessionLifecycleEvent struct {
	SessionID string
	State     SessionState
	ExitCode  *int
	Reason    string
}

// SessionToolEvent informs about detected tool metadata.
type SessionToolEvent struct {
	SessionID  string
	ToolName   string
	ToolID     string
	IconPath   string
	Confidence *float32
}

// PipelineMessageEvent is emitted after cleaners normalise output.
type PipelineMessageEvent struct {
	SessionID   string
	Origin      ContentOrigin
	Text        string
	Annotations map[string]string
	Sequence    uint64
}

// PipelineErrorEvent logs problems inside the content pipeline.
type PipelineErrorEvent struct {
	SessionID   string
	Stage       string
	Message     string
	Recoverable bool
}

// ConversationTurn stores previous dialogue entries for prompts.
type ConversationTurn struct {
	Origin ContentOrigin
	Text   string
	At     time.Time
	Meta   map[string]string
}

// ConversationMessage represents the new message that triggered the prompt.
type ConversationMessage struct {
	Origin ContentOrigin
	Text   string
	At     time.Time
	Meta   map[string]string
}

// ConversationPromptEvent encapsulates context sent to AI adapters.
type ConversationPromptEvent struct {
	SessionID  string
	PromptID   string
	Context    []ConversationTurn
	NewMessage ConversationMessage
	Metadata   map[string]string
}

// ConversationAction describes an operation that AI requests.
type ConversationAction struct {
	Type   string
	Target string
	Args   map[string]string
}

// ConversationReplyEvent delivers AI responses.
type ConversationReplyEvent struct {
	SessionID string
	PromptID  string
	Text      string
	Actions   []ConversationAction
	Metadata  map[string]string
}

// ModuleHealth indicates current module state.
type ModuleHealth string

const (
	ModuleHealthStarting ModuleHealth = "starting"
	ModuleHealthReady    ModuleHealth = "ready"
	ModuleHealthDegraded ModuleHealth = "degraded"
	ModuleHealthStopped  ModuleHealth = "stopped"
	ModuleHealthError    ModuleHealth = "error"
)

// ModuleStatusEvent informs about lifecycle status of adapter-runner modules.
type ModuleStatusEvent struct {
	ModuleID  string
	Slot      string
	Status    ModuleHealth
	Message   string
	StartedAt time.Time
	Extra     map[string]string
}

// LogLevel indicates severity for module log messages.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// ModuleLogEvent carries structured log entries from modules.
type ModuleLogEvent struct {
	ModuleID  string
	Level     LogLevel
	Message   string
	Fields    map[string]string
	Timestamp time.Time
}
