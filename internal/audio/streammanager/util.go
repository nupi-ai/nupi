package streammanager

import (
	"fmt"
	"strings"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// SessionParams describes context for establishing an audio stream.
// Identical across STT, VAD and TTS/Egress services.
type SessionParams struct {
	SessionID string
	StreamID  string
	Format    eventbus.AudioFormat
	Metadata  map[string]string
	AdapterID string
	Config    map[string]any
}

// StreamKey builds a composite map key from session and stream identifiers.
func StreamKey(sessionID, streamID string) string {
	return sessionID + "::" + streamID
}

// SplitStreamKey splits a composite key back into session and stream identifiers.
func SplitStreamKey(key string) (string, string) {
	const sep = "::"
	if idx := strings.Index(key, sep); idx >= 0 {
		return key[:idx], key[idx+len(sep):]
	}
	return key, ""
}

// CopyMetadata returns a shallow copy of the metadata map.
// Returns nil for empty/nil input.
func CopyMetadata(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// RetryConfig holds backoff parameters for pending stream retries.
type RetryConfig struct {
	Initial time.Duration
	Max     time.Duration
}

// DefaultRetryConfig returns sensible retry defaults.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		Initial: 200 * time.Millisecond,
		Max:     5 * time.Second,
	}
}

// ValidateRetryConfig normalises the config, ensuring non-zero Initial
// and Max >= Initial.
func ValidateRetryConfig(cfg *RetryConfig) {
	if cfg.Initial <= 0 {
		cfg.Initial = DefaultRetryConfig().Initial
	}
	if cfg.Max < cfg.Initial {
		cfg.Max = cfg.Initial
	}
}

// FormatStreamLog returns a formatted string for log messages.
func FormatStreamLog(prefix, key string) string {
	sid, strid := SplitStreamKey(key)
	return fmt.Sprintf("[%s] session=%s stream=%s", prefix, sid, strid)
}
