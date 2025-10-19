package store

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAudioSettingsDefaults(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
	})

	ctx := context.Background()

	settings, err := store.LoadAudioSettings(ctx)
	if err != nil {
		t.Fatalf("load audio settings: %v", err)
	}

	if settings.CaptureDevice != "" {
		t.Fatalf("expected empty capture device, got %q", settings.CaptureDevice)
	}
	if settings.PlaybackDevice != "" {
		t.Fatalf("expected empty playback device, got %q", settings.PlaybackDevice)
	}
	if settings.PreferredFormat != defaultAudioFormat {
		t.Fatalf("expected default format %q, got %q", defaultAudioFormat, settings.PreferredFormat)
	}
	if settings.VADThreshold != defaultVADThreshold {
		t.Fatalf("expected default VAD threshold %f, got %f", defaultVADThreshold, settings.VADThreshold)
	}
}

func TestAudioSettingsSaveAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	updated := AudioSettings{
		CaptureDevice:   "BuiltInMic",
		PlaybackDevice:  "ExternalSpeakers",
		PreferredFormat: "pcm_float32",
		VADThreshold:    float32(0.65),
		Metadata:        sql.NullString{String: `{"noise_gate":true}`, Valid: true},
	}

	if err := store.SaveAudioSettings(ctx, updated); err != nil {
		t.Fatalf("save audio settings: %v", err)
	}

	reloaded, err := store.LoadAudioSettings(ctx)
	if err != nil {
		t.Fatalf("reload audio settings: %v", err)
	}

	if reloaded.CaptureDevice != updated.CaptureDevice {
		t.Fatalf("capture device mismatch: want %q got %q", updated.CaptureDevice, reloaded.CaptureDevice)
	}
	if reloaded.PlaybackDevice != updated.PlaybackDevice {
		t.Fatalf("playback device mismatch: want %q got %q", updated.PlaybackDevice, reloaded.PlaybackDevice)
	}
	if reloaded.PreferredFormat != updated.PreferredFormat {
		t.Fatalf("preferred format mismatch: want %q got %q", updated.PreferredFormat, reloaded.PreferredFormat)
	}
	if diff := float64(reloaded.VADThreshold) - float64(updated.VADThreshold); math.Abs(diff) > 1e-6 {
		t.Fatalf("vad threshold mismatch: want %f got %f", updated.VADThreshold, reloaded.VADThreshold)
	}
	if !reloaded.Metadata.Valid || reloaded.Metadata.String != updated.Metadata.String {
		t.Fatalf("metadata mismatch: want %q got %v", updated.Metadata.String, reloaded.Metadata)
	}
	if strings.TrimSpace(reloaded.UpdatedAt) == "" {
		t.Fatalf("expected updated_at to be populated")
	}
}

func TestWatchDetectsAudioSettingsChanges(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	watchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := store.Watch(watchCtx, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("start watch: %v", err)
	}

	if err := store.SaveAudioSettings(ctx, AudioSettings{PreferredFormat: "pcm_float32"}); err != nil {
		t.Fatalf("save audio settings: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("watch channel closed unexpectedly")
			}
			if ev.AudioSettingsChanged {
				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for audio settings change event")
		}
	}
}
