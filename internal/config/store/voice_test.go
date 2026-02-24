package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nupi-ai/nupi/internal/voice/slots"
)

func TestVoiceReadinessDefaults(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")
	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	readiness, err := store.VoiceReadiness(ctx)
	if err != nil {
		t.Fatalf("voice readiness: %v", err)
	}

	if readiness.CaptureEnabled {
		t.Fatalf("expected capture to be disabled by default")
	}
	if readiness.PlaybackEnabled {
		t.Fatalf("expected playback to be disabled by default")
	}
}

func TestVoiceReadinessActiveAdapters(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")
	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	if err := store.UpsertAdapter(ctx, Adapter{
		ID:     "adapter.stt.mock",
		Source: "builtin",
		Type:   "stt",
		Name:   "Mock STT",
	}); err != nil {
		t.Fatalf("upsert stt adapter: %v", err)
	}
	if err := store.UpsertAdapter(ctx, Adapter{
		ID:     "adapter.tts.mock",
		Source: "builtin",
		Type:   "tts",
		Name:   "Mock TTS",
	}); err != nil {
		t.Fatalf("upsert tts adapter: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, slots.STT, "adapter.stt.mock", nil); err != nil {
		t.Fatalf("activate stt: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, slots.TTS, "adapter.tts.mock", nil); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	readiness, err := store.VoiceReadiness(ctx)
	if err != nil {
		t.Fatalf("voice readiness: %v", err)
	}
	if !readiness.CaptureEnabled {
		t.Fatalf("expected capture to be enabled")
	}
	if !readiness.PlaybackEnabled {
		t.Fatalf("expected playback to be enabled")
	}
}

// TestVoiceReadinessPartialConfig verifies VoiceReadiness when only STT is
// configured (no TTS). Capture should be enabled, playback disabled, and
// issues should only report TTS-related problems.
func TestVoiceReadinessPartialConfig(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")
	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Only configure STT — no TTS.
	if err := store.UpsertAdapter(ctx, Adapter{
		ID:     "adapter.stt.mock",
		Source: "builtin",
		Type:   "stt",
		Name:   "Mock STT",
	}); err != nil {
		t.Fatalf("upsert stt adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, slots.STT, "adapter.stt.mock", nil); err != nil {
		t.Fatalf("activate stt: %v", err)
	}

	readiness, err := store.VoiceReadiness(ctx)
	if err != nil {
		t.Fatalf("voice readiness: %v", err)
	}
	if !readiness.CaptureEnabled {
		t.Fatalf("expected capture to be enabled (STT active)")
	}
	if readiness.PlaybackEnabled {
		t.Fatalf("expected playback to be disabled (no TTS)")
	}
}

func TestVoiceReadinessInactiveBinding(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")
	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	if err := store.UpsertAdapter(ctx, Adapter{
		ID:     "adapter.tts.mock",
		Source: "builtin",
		Type:   "tts",
		Name:   "Mock TTS",
	}); err != nil {
		t.Fatalf("upsert tts adapter: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, slots.TTS, "adapter.tts.mock", nil); err != nil {
		t.Fatalf("activate tts: %v", err)
	}
	if err := store.UpdateAdapterBindingStatus(ctx, slots.TTS, BindingStatusInactive); err != nil {
		t.Fatalf("set binding inactive: %v", err)
	}

	readiness, err := store.VoiceReadiness(ctx)
	if err != nil {
		t.Fatalf("voice readiness: %v", err)
	}
	if readiness.PlaybackEnabled {
		t.Fatalf("expected playback disabled due to inactive adapter")
	}
}
