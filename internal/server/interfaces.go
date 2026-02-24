package server

import (
	"context"
	"errors"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/recording"
	"github.com/nupi-ai/nupi/internal/session"
)

// ErrAudioStreamExists signals that an audio ingress stream already exists for the given identifiers.
var ErrAudioStreamExists = errors.New("audio stream already exists")

// AudioCaptureStream models an open audio ingress stream.
type AudioCaptureStream interface {
	Write([]byte) error
	Close() error
}

// AudioCaptureProvider exposes operations required by the gRPC layer to accept audio input.
type AudioCaptureProvider interface {
	OpenStream(sessionID, streamID string, format eventbus.AudioFormat, metadata map[string]string) (AudioCaptureStream, error)
}

// AudioPlaybackController exposes playback control operations used by API handlers.
type AudioPlaybackController interface {
	DefaultStreamID() string
	PlaybackFormat() eventbus.AudioFormat
	Interrupt(sessionID, streamID, reason string, metadata map[string]string)
}

// AdaptersController exposes adapter lifecycle operations required by the API.
type AdaptersController interface {
	Overview(ctx context.Context) ([]adapters.BindingStatus, error)
	StartSlot(ctx context.Context, slot adapters.Slot) (*adapters.BindingStatus, error)
	StopSlot(ctx context.Context, slot adapters.Slot) (*adapters.BindingStatus, error)
}

// PluginReloader reloads all plugin indices (pipeline cleaners, tool handlers, index).
type PluginReloader interface {
	Reload() error
}

// Compile-time interface satisfaction assertions.
var (
	_ SessionManager = (*session.Manager)(nil)
	_ ConfigStore    = (*configstore.Store)(nil)
)

// SessionManager abstracts session lifecycle operations used by gRPC services.
type SessionManager interface {
	ListSessions() []*session.Session
	CreateSession(opts pty.StartOptions, inspect bool) (*session.Session, error)
	GetSession(id string) (*session.Session, error)
	KillSession(id string) error
	WriteToSession(id string, data []byte) error
	ResizeSession(id string, rows, cols int) error
	GetRecordingStore() *recording.Store
	AddEventListener(listener session.SessionEventListener)
	AttachToSession(id string, sink pty.OutputSink, includeHistory bool) error
	DetachFromSession(id string, sink pty.OutputSink) error
}

// ConfigStore abstracts configuration storage operations used by API handlers and gRPC services.
type ConfigStore interface {
	GetTransportConfig(ctx context.Context) (configstore.TransportConfig, error)
	SaveTransportConfig(ctx context.Context, cfg configstore.TransportConfig) error
	ListAdapters(ctx context.Context) ([]configstore.Adapter, error)
	ListAdapterBindings(ctx context.Context) ([]configstore.AdapterBinding, error)
	UpsertAdapter(ctx context.Context, adapter configstore.Adapter) error
	UpsertAdapterEndpoint(ctx context.Context, endpoint configstore.AdapterEndpoint) error
	SetActiveAdapter(ctx context.Context, slot string, adapterID string, config map[string]any) error
	ClearAdapterBinding(ctx context.Context, slot string) error
	AdapterExists(ctx context.Context, adapterID string) (bool, error)
	EnsureRequiredAdapterSlots(ctx context.Context) (configstore.MigrationResult, error)
	EnsureAudioSettings(ctx context.Context) (bool, error)
	VoiceReadiness(ctx context.Context) (configstore.VoiceReadiness, error)
	LoadSecuritySettings(ctx context.Context, keys ...string) (map[string]string, error)
	SaveSecuritySettings(ctx context.Context, values map[string]string) error
	QuickstartStatus(ctx context.Context) (bool, *time.Time, error)
	PendingQuickstartSlots(ctx context.Context) ([]string, error)
	MarkQuickstartCompleted(ctx context.Context, complete bool) error
	SavePushToken(ctx context.Context, deviceID, token string, enabledEvents []string, authTokenID string) error
	SavePushTokenOwned(ctx context.Context, deviceID, token string, enabledEvents []string, authTokenID string) (bool, error)
	GetPushToken(ctx context.Context, deviceID string) (*configstore.PushToken, error)
	DeletePushToken(ctx context.Context, deviceID string) error
	DeletePushTokenOwned(ctx context.Context, deviceID, authTokenID string) (bool, error)
	DeletePushTokenIfMatch(ctx context.Context, deviceID, token string) (bool, error)
	DeletePushTokensByAuthToken(ctx context.Context, authTokenID string) error
	DeleteAllPushTokens(ctx context.Context) error
	ListPushTokens(ctx context.Context) ([]configstore.PushToken, error)
	ListPushTokensForEvent(ctx context.Context, eventType string) ([]configstore.PushToken, error)
}
