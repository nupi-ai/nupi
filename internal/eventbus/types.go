package eventbus

import (
	"fmt"
	"strings"
	"time"
)

// Topic identifies a logical channel on the bus.
type Topic string

// Standard topics emitted in phase 1.
const (
	TopicSessionsOutput          Topic = "sessions.output"
	TopicSessionsLifecycle       Topic = "sessions.lifecycle"
	TopicSessionsTool            Topic = "sessions.tool"
	TopicSessionsToolChanged     Topic = "sessions.tool_changed"
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
	TopicConversationTurn        Topic = "conversation.turn"
	TopicPairingCreated          Topic = "pairing.created"
	TopicPairingClaimed          Topic = "pairing.claimed"
	TopicAwarenessSync           Topic = "awareness.sync"
)

// Source describes which component produced an event.
type Source string

const (
	SourceSessionManager     Source = "session_manager"
	SourceContentPipeline    Source = "content_pipeline"
	SourceConversation       Source = "conversation"
	SourceIntentRouter       Source = "intent_router"
	SourceIntentRouterBridge Source = "intent_router_bridge"
	SourcePluginService      Source = "plugin_service"
	SourceAdaptersService    Source = "adapters_service"
	SourceAdapterProcess     Source = "adapter_process"
	SourceAudioIngress       Source = "audio_ingress"
	SourceAudioEgress        Source = "audio_egress"
	SourceAudioSTT           Source = "audio_stt"
	SourceSpeechBarge        Source = "speech_barge"
	SourceSpeechVAD          Source = "speech_vad"
	SourceClient             Source = "client"
	SourcePairing            Source = "pairing"
	SourceAwareness          Source = "awareness"
	SourceUnknown            Source = "unknown"
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

// Label returns the human-readable label for a ContentOrigin.
// Used for formatting conversation turns in exports, prompts, and serialization.
func (o ContentOrigin) Label() string {
	switch o {
	case OriginAI:
		return "assistant"
	case OriginTool:
		return "tool"
	case OriginSystem:
		return "system"
	default:
		return "user"
	}
}

// SessionState summarises lifecycle changes.
type SessionState string

const (
	SessionStateCreated  SessionState = "created"
	SessionStateRunning  SessionState = "running"
	SessionStateDetached SessionState = "detached"
	SessionStateStopped  SessionState = "stopped"
)

// SessionReasonKilled is the Reason value set on lifecycle events published
// by KillSession. Consumers can use this to suppress notifications for
// user-initiated kills (the user already knows they killed the session).
const SessionReasonKilled = "session_killed"

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
	Label     string // Human-readable session label (e.g., command name).
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

// SessionToolChangedEvent is emitted when the detected tool changes in a session.
type SessionToolChangedEvent struct {
	SessionID    string
	PreviousTool string
	NewTool      string
	Timestamp    time.Time
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

// SerializeTurns formats conversation turns as readable text for AI prompts.
// Each turn is formatted as "[origin] text" on its own line.
func SerializeTurns(turns []ConversationTurn) string {
	var sb strings.Builder
	for _, t := range turns {
		fmt.Fprintf(&sb, "[%s] %s\n", t.Origin.Label(), t.Text)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// ConversationMessage represents the new message that triggered the prompt.
type ConversationMessage struct {
	Origin ContentOrigin
	Text   string
	At     time.Time
	Meta   map[string]string
}

// ConversationTurnEvent carries a single conversation turn for downstream consumers.
// Publisher: conversation service (wired in Epic 19.2).
type ConversationTurnEvent struct {
	SessionID string
	Turn      ConversationTurn
}

// ConversationPromptEvent encapsulates context sent to AI adapters.
type ConversationPromptEvent struct {
	SessionID  string
	PromptID   string
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

// PairingCreatedEvent is published when a new pairing code is generated via CreatePairing RPC.
type PairingCreatedEvent struct {
	Code       string
	DeviceName string
	Role       string
	ConnectURL string
	ExpiresAt  time.Time
}

// PairingClaimedEvent is published when a pairing code is claimed via ClaimPairing RPC.
type PairingClaimedEvent struct {
	Code       string
	DeviceName string
	Role       string
	ClaimedAt  time.Time
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

// AdapterStatusEvent informs about lifecycle status of adapters.
type AdapterStatusEvent struct {
	AdapterID string
	Slot      string
	Status    AdapterHealth
	Message   string
	StartedAt time.Time
	Extra     map[string]string
}

// LogLevel indicates severity for plugin log messages.
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

// ---------------------------------------------------------------------------
// Typed topic descriptors
// ---------------------------------------------------------------------------
// Each TopicDef binds a Topic constant to its payload type, enabling
// compile-time enforcement via Publish[T] and SubscribeTo[T].

// Sessions groups session-related topic descriptors.
var Sessions = struct {
	Output      TopicDef[SessionOutputEvent]
	Lifecycle   TopicDef[SessionLifecycleEvent]
	Tool        TopicDef[SessionToolEvent]
	ToolChanged TopicDef[SessionToolChangedEvent]
}{
	Output:      NewTopicDef[SessionOutputEvent](TopicSessionsOutput),
	Lifecycle:   NewTopicDef[SessionLifecycleEvent](TopicSessionsLifecycle),
	Tool:        NewTopicDef[SessionToolEvent](TopicSessionsTool),
	ToolChanged: NewTopicDef[SessionToolChangedEvent](TopicSessionsToolChanged),
}

// Pipeline groups content-pipeline topic descriptors.
var Pipeline = struct {
	Cleaned TopicDef[PipelineMessageEvent]
	Error   TopicDef[PipelineErrorEvent]
}{
	Cleaned: NewTopicDef[PipelineMessageEvent](TopicPipelineCleaned),
	Error:   NewTopicDef[PipelineErrorEvent](TopicPipelineError),
}

// Conversation groups conversation topic descriptors.
var Conversation = struct {
	Prompt TopicDef[ConversationPromptEvent]
	Reply  TopicDef[ConversationReplyEvent]
	Speak  TopicDef[ConversationSpeakEvent]
	Turn   TopicDef[ConversationTurnEvent]
}{
	Prompt: NewTopicDef[ConversationPromptEvent](TopicConversationPrompt),
	Reply:  NewTopicDef[ConversationReplyEvent](TopicConversationReply),
	Speak:  NewTopicDef[ConversationSpeakEvent](TopicConversationSpeak),
	Turn:   NewTopicDef[ConversationTurnEvent](TopicConversationTurn),
}

// Audio groups audio topic descriptors.
var Audio = struct {
	IngressRaw     TopicDef[AudioIngressRawEvent]
	IngressSegment TopicDef[AudioIngressSegmentEvent]
	EgressPlayback TopicDef[AudioEgressPlaybackEvent]
	Interrupt      TopicDef[AudioInterruptEvent]
}{
	IngressRaw:     NewTopicDef[AudioIngressRawEvent](TopicAudioIngressRaw),
	IngressSegment: NewTopicDef[AudioIngressSegmentEvent](TopicAudioIngressSegment),
	EgressPlayback: NewTopicDef[AudioEgressPlaybackEvent](TopicAudioEgressPlayback),
	Interrupt:      NewTopicDef[AudioInterruptEvent](TopicAudioInterrupt),
}

// Speech groups speech-related topic descriptors.
var Speech = struct {
	TranscriptPartial TopicDef[SpeechTranscriptEvent]
	TranscriptFinal   TopicDef[SpeechTranscriptEvent]
	VADDetected       TopicDef[SpeechVADEvent]
	BargeIn           TopicDef[SpeechBargeInEvent]
}{
	TranscriptPartial: NewTopicDef[SpeechTranscriptEvent](TopicSpeechTranscriptPartial),
	TranscriptFinal:   NewTopicDef[SpeechTranscriptEvent](TopicSpeechTranscriptFinal),
	VADDetected:       NewTopicDef[SpeechVADEvent](TopicSpeechVADDetected),
	BargeIn:           NewTopicDef[SpeechBargeInEvent](TopicSpeechBargeIn),
}

// Adapters groups adapter topic descriptors.
var Adapters = struct {
	Status TopicDef[AdapterStatusEvent]
	Log    TopicDef[AdapterLogEvent]
}{
	Status: NewTopicDef[AdapterStatusEvent](TopicAdaptersStatus),
	Log:    NewTopicDef[AdapterLogEvent](TopicAdaptersLog),
}

// Pairing groups pairing topic descriptors.
var Pairing = struct {
	Created TopicDef[PairingCreatedEvent]
	Claimed TopicDef[PairingClaimedEvent]
}{
	Created: NewTopicDef[PairingCreatedEvent](TopicPairingCreated),
	Claimed: NewTopicDef[PairingClaimedEvent](TopicPairingClaimed),
}

// AwarenessSyncEvent notifies that an awareness file was created or updated.
type AwarenessSyncEvent struct {
	FilePath string
	SyncType string // "created", "updated", "deleted"
}

// Memory groups awareness/memory topic descriptors.
var Memory = struct {
	Sync TopicDef[AwarenessSyncEvent]
}{
	Sync: NewTopicDef[AwarenessSyncEvent](TopicAwarenessSync),
}
