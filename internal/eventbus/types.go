package eventbus

import (
	"time"
)

// Topic identifies a logical channel on the bus.
type Topic string

// Standard topics emitted in phase 1.
const (
	TopicSessionsOutput          Topic = "sessions.output"
	TopicSessionsLifecycle       Topic = "sessions.lifecycle"
	TopicSessionsTool            Topic = "sessions.tool"
	TopicPipelineCleaned         Topic = "pipeline.cleaned"
	TopicPipelineError           Topic = "pipeline.error"
	TopicConversationPrompt      Topic = "conversation.prompt"
	TopicConversationReply       Topic = "conversation.reply"
	TopicAdaptersStatus          Topic = "adapters.status"
	TopicAdaptersLog             Topic = "adapters.log"
	TopicAudioIngressRaw         Topic = "audio.ingress.raw"
	TopicAudioIngressSegment     Topic = "audio.ingress.segment"
	TopicAudioEgressPlayback     Topic = "audio.egress.playback"
	TopicAudioInterrupt          Topic = "audio.interrupt"
	TopicSpeechTranscriptPartial Topic = "speech.transcript.partial"
	TopicSpeechTranscriptFinal   Topic = "speech.transcript.final"
	TopicSpeechVADDetected       Topic = "speech.vad.detected"
	TopicSpeechBargeIn           Topic = "speech.barge_in"
	TopicConversationSpeak       Topic = "conversation.speak"
)

// Source describes which component produced an event.
type Source string

const (
	SourceSessionManager  Source = "session_manager"
	SourceContentPipeline Source = "content_pipeline"
	SourceConversation    Source = "conversation"
	SourcePluginService   Source = "plugin_service"
	SourceAdaptersService Source = "adapters_service"
	SourceAdapterRunner   Source = "adapter_runner"
	SourceAudioIngress    Source = "audio_ingress"
	SourceAudioEgress     Source = "audio_egress"
	SourceAudioSTT        Source = "audio_stt"
	SourceSpeechBarge     Source = "speech_barge"
	SourceSpeechVAD       Source = "speech_vad"
	SourceClient          Source = "client"
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

// AudioEncoding identifies the codec of an audio stream.
type AudioEncoding string

const (
	AudioEncodingPCM16 AudioEncoding = "pcm_s16le"
)

// AudioFormat describes the characteristics of an audio buffer.
type AudioFormat struct {
	Encoding      AudioEncoding
	SampleRate    int
	Channels      int
	BitDepth      int
	FrameDuration time.Duration
}

// AudioIngressRawEvent carries raw audio frames received from clients.
type AudioIngressRawEvent struct {
	SessionID string
	StreamID  string
	Sequence  uint64
	Format    AudioFormat
	Data      []byte
	Received  time.Time
	Metadata  map[string]string
}

// AudioIngressSegmentEvent represents a processed chunk prepared for STT adapters.
type AudioIngressSegmentEvent struct {
	SessionID string
	StreamID  string
	Sequence  uint64
	Format    AudioFormat
	Data      []byte
	Duration  time.Duration
	First     bool
	Last      bool
	StartedAt time.Time
	EndedAt   time.Time
	Metadata  map[string]string
}

// AudioEgressPlaybackEvent broadcasts audio generated by TTS adapters.
type AudioEgressPlaybackEvent struct {
	SessionID string
	StreamID  string
	Sequence  uint64
	Format    AudioFormat
	Data      []byte
	Duration  time.Duration
	Final     bool
	Metadata  map[string]string
}

// AudioInterruptEvent is emitted when a client requests manual TTS interruption.
type AudioInterruptEvent struct {
	SessionID string
	StreamID  string
	Reason    string
	Timestamp time.Time
	Metadata  map[string]string
}

// SpeechTranscriptEvent delivers recognised speech segments produced by STT adapters.
type SpeechTranscriptEvent struct {
	SessionID  string
	StreamID   string
	Sequence   uint64
	Text       string
	Confidence float32
	Final      bool
	StartedAt  time.Time
	EndedAt    time.Time
	Metadata   map[string]string
}

// SpeechVADEvent captures voice activity detection changes.
type SpeechVADEvent struct {
	SessionID   string
	StreamID    string
	Active      bool
	Confidence  float32
	EnergyLevel float32
	Timestamp   time.Time
	Metadata    map[string]string
}

// SpeechBargeInEvent signals that playback should be interrupted.
type SpeechBargeInEvent struct {
	SessionID string
	StreamID  string
	Reason    string
	Timestamp time.Time
	Metadata  map[string]string
}

// ConversationSpeakEvent instructs audio services to render a spoken response.
type ConversationSpeakEvent struct {
	SessionID string
	PromptID  string
	Text      string
	Metadata  map[string]string
}

// AdapterHealth indicates current adapter state.
type AdapterHealth string

const (
	AdapterHealthStarting AdapterHealth = "starting"
	AdapterHealthReady    AdapterHealth = "ready"
	AdapterHealthDegraded AdapterHealth = "degraded"
	AdapterHealthStopped  AdapterHealth = "stopped"
	AdapterHealthError    AdapterHealth = "error"
)

// AdapterStatusEvent informs about lifecycle status of adapter-runner adapters.
type AdapterStatusEvent struct {
	AdapterID string
	Slot      string
	Status    AdapterHealth
	Message   string
	StartedAt time.Time
	Extra     map[string]string
}

// LogLevel indicates severity for adapter log messages.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// AdapterLogEvent carries structured log entries from adapters.
type AdapterLogEvent struct {
	AdapterID string
	Level     LogLevel
	Message   string
	Fields    map[string]string
	Timestamp time.Time
}
