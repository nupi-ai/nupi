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
	if len(readiness.Issues) == 0 {
		t.Fatalf("expected issues to be reported")
	}

	var seenSTT, seenTTS bool
	for _, issue := range readiness.Issues {
		switch issue.Slot {
		case slots.STT:
			seenSTT = true
		case slots.TTS:
			seenTTS = true
		default:
			t.Fatalf("unexpected slot in issues: %+v", issue)
		}
		if issue.Code == "" || issue.Message == "" {
			t.Fatalf("issue missing details: %+v", issue)
		}
	}
	if !seenSTT || !seenTTS {
		t.Fatalf("expected issues for both stt and tts slots: %+v", readiness.Issues)
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
	if len(readiness.Issues) != 0 {
		t.Fatalf("expected no issues, got %+v", readiness.Issues)
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

	foundInactive := false
	for _, issue := range readiness.Issues {
		if issue.Slot == slots.TTS && issue.Code == voiceIssueAdapterInactive {
			foundInactive = true
		}
	}
	if !foundInactive {
		t.Fatalf("expected inactive adapter issue in %+v", readiness.Issues)
	}
}
