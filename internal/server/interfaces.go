package server

import (
	"context"
	"errors"

	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
)

// ErrAudioStreamExists signals that an audio ingress stream already exists for the given identifiers.
var ErrAudioStreamExists = errors.New("audio stream already exists")

// AudioCaptureStream models an open audio ingress stream.
type AudioCaptureStream interface {
	Write([]byte) error
	Close() error
}

// AudioCaptureProvider exposes operations required by the HTTP/gRPC layer to accept audio input.
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

// PluginDiscoveryWarning represents a plugin that was skipped during discovery.
type PluginDiscoveryWarning struct {
	Dir   string `json:"dir"`
	Error string `json:"error"`
}

// PluginWarningsProvider exposes plugin discovery warnings for observability.
type PluginWarningsProvider interface {
	GetDiscoveryWarnings() []PluginDiscoveryWarning
}
