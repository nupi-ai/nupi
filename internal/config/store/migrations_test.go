package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureRequiredAdapterSlotsInsertsMissing(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		os.Remove(dbPath)
	})

	ctx := context.Background()

	// remove ai slot to simulate pre-migration database
	if _, err := store.DB().ExecContext(ctx, `
		DELETE FROM adapter_bindings WHERE instance_name = ? AND profile_name = ? AND slot = 'ai'
	`, store.InstanceName(), store.ProfileName()); err != nil {
		t.Fatalf("delete slot: %v", err)
	}

	result, err := store.EnsureRequiredAdapterSlots(ctx)
	if err != nil {
		t.Fatalf("ensure required slots: %v", err)
	}

	found := false
	for _, slot := range result.UpdatedSlots {
		if slot == "ai" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected ai to be reported as updated, got %v", result.UpdatedSlots)
	}

	bindings, err := store.ListAdapterBindings(ctx)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}

	var restored bool
	for _, binding := range bindings {
		if binding.Slot != "ai" {
			continue
		}
		restored = true
		if binding.Status != BindingStatusRequired {
			t.Fatalf("expected status required, got %s", binding.Status)
		}
		if binding.AdapterID != nil {
			t.Fatalf("expected adapter to remain nil, got %v", *binding.AdapterID)
		}
	}
	if !restored {
		t.Fatalf("ai slot not restored")
	}
}

func TestEnsureRequiredAdapterSlotsReconcilesStatus(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		os.Remove(dbPath)
	})

	ctx := context.Background()

	slots := []string{"stt", "tts", "vad"}
	for _, slot := range slots {
		if _, err := store.DB().ExecContext(ctx, `
			UPDATE adapter_bindings
			SET status = 'inactive', config = NULL, updated_at = CURRENT_TIMESTAMP
			WHERE instance_name = ? AND profile_name = ? AND slot = ?
		`, store.InstanceName(), store.ProfileName(), slot); err != nil {
			t.Fatalf("prepare slot %s: %v", slot, err)
		}
	}

	result, err := store.EnsureRequiredAdapterSlots(ctx)
	if err != nil {
		t.Fatalf("ensure required slots: %v", err)
	}

	found := make(map[string]bool)
	for _, slot := range slots {
		for _, repaired := range result.UpdatedSlots {
			if repaired == slot {
				found[slot] = true
			}
		}
	}
	for _, slot := range slots {
		if !found[slot] {
			t.Fatalf("expected %s to be repaired", slot)
		}
	}

	for _, slot := range slots {
		var status string
		var adapter sql.NullString
		var cfg sql.NullString
		if err := store.DB().QueryRowContext(ctx, `
			SELECT status, adapter_id, config FROM adapter_bindings
			WHERE instance_name = ? AND profile_name = ? AND slot = ?
		`, store.InstanceName(), store.ProfileName(), slot).Scan(&status, &adapter, &cfg); err != nil {
			t.Fatalf("query slot %s: %v", slot, err)
		}
		if status != BindingStatusRequired {
			t.Fatalf("expected status required after repair for %s, got %s", slot, status)
		}
		if adapter.Valid {
			t.Fatalf("expected adapter to remain nil for %s", slot)
		}
		if !cfg.Valid || strings.TrimSpace(cfg.String) == "" {
			t.Fatalf("expected required config to be set for %s, got %v", slot, cfg.String)
		}
	}
}

func TestEnsureAudioSettingsInsertsDefaults(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		os.Remove(dbPath)
	})

	ctx := context.Background()

	if _, err := store.DB().ExecContext(ctx, `
		DELETE FROM audio_settings WHERE instance_name = ? AND profile_name = ?
	`, store.InstanceName(), store.ProfileName()); err != nil {
		t.Fatalf("delete audio settings: %v", err)
	}

	updated, err := store.EnsureAudioSettings(ctx)
	if err != nil {
		t.Fatalf("ensure audio settings: %v", err)
	}
	if !updated {
		t.Fatalf("expected audio settings to be created")
	}

	settings, err := store.LoadAudioSettings(ctx)
	if err != nil {
		t.Fatalf("load audio settings: %v", err)
	}
	if settings.PreferredFormat != defaultAudioFormat {
		t.Fatalf("expected default format %s, got %s", defaultAudioFormat, settings.PreferredFormat)
	}
	if settings.VADThreshold != defaultVADThreshold {
		t.Fatalf("expected default VAD threshold %f, got %f", defaultVADThreshold, settings.VADThreshold)
	}
}

func TestEnsureAudioSettingsNormalises(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		os.Remove(dbPath)
	})

	ctx := context.Background()

	if _, err := store.DB().ExecContext(ctx, `
		UPDATE audio_settings
		SET capture_device = '  mic0  ',
		    playback_device = '  speakers  ',
		    preferred_format = '',
		    vad_threshold = -0.5,
		    metadata = '  {"foo":true}  '
		WHERE instance_name = ? AND profile_name = ?
	`, store.InstanceName(), store.ProfileName()); err != nil {
		t.Fatalf("prepare audio settings: %v", err)
	}

	updated, err := store.EnsureAudioSettings(ctx)
	if err != nil {
		t.Fatalf("ensure audio settings: %v", err)
	}
	if !updated {
		t.Fatalf("expected audio settings normalisation to be reported")
	}

	settings, err := store.LoadAudioSettings(ctx)
	if err != nil {
		t.Fatalf("load audio settings: %v", err)
	}
	if settings.CaptureDevice != "mic0" {
		t.Fatalf("unexpected capture device: %q", settings.CaptureDevice)
	}
	if settings.PlaybackDevice != "speakers" {
		t.Fatalf("unexpected playback device: %q", settings.PlaybackDevice)
	}
	if settings.PreferredFormat != defaultAudioFormat {
		t.Fatalf("expected format to be default, got %s", settings.PreferredFormat)
	}
	if settings.VADThreshold != defaultVADThreshold {
		t.Fatalf("expected VAD threshold to be default, got %f", settings.VADThreshold)
	}
	if !settings.Metadata.Valid || strings.TrimSpace(settings.Metadata.String) != `{"foo":true}` {
		t.Fatalf("unexpected metadata: %+v", settings.Metadata)
	}
}

func TestEnsureAudioSettingsHandlesNaN(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		os.Remove(dbPath)
	})

	ctx := context.Background()

	if _, err := store.DB().ExecContext(ctx, `
		UPDATE audio_settings
		SET vad_threshold = 'NaN'
		WHERE instance_name = ? AND profile_name = ?
	`, store.InstanceName(), store.ProfileName()); err != nil {
		t.Fatalf("prepare NaN threshold: %v", err)
	}

	updated, err := store.EnsureAudioSettings(ctx)
	if err != nil {
		t.Fatalf("ensure audio settings: %v", err)
	}
	if !updated {
		t.Fatalf("expected NaN threshold to be normalised")
	}

	settings, err := store.LoadAudioSettings(ctx)
	if err != nil {
		t.Fatalf("load audio settings: %v", err)
	}
	if settings.VADThreshold != defaultVADThreshold {
		t.Fatalf("expected default VAD threshold, got %f", settings.VADThreshold)
	}
}

func TestEnsureAudioSettingsMetadataWhitespace(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		os.Remove(dbPath)
	})

	ctx := context.Background()

	if _, err := store.DB().ExecContext(ctx, `
		UPDATE audio_settings
		SET metadata = '   '
		WHERE instance_name = ? AND profile_name = ?
	`, store.InstanceName(), store.ProfileName()); err != nil {
		t.Fatalf("prepare metadata whitespace: %v", err)
	}

	updated, err := store.EnsureAudioSettings(ctx)
	if err != nil {
		t.Fatalf("ensure audio settings: %v", err)
	}
	if !updated {
		t.Fatalf("expected metadata whitespace to be cleared")
	}

	settings, err := store.LoadAudioSettings(ctx)
	if err != nil {
		t.Fatalf("load audio settings: %v", err)
	}
	if settings.Metadata.Valid {
		t.Fatalf("expected metadata to be empty, got %+v", settings.Metadata)
	}
}

func TestEnsureAudioSettingsBoundaryThresholds(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		os.Remove(dbPath)
	})

	ctx := context.Background()

	for _, threshold := range []float32{0, 1} {
		if _, err := store.DB().ExecContext(ctx, `
			UPDATE audio_settings
			SET vad_threshold = ?, metadata = '{"keep":true}'
			WHERE instance_name = ? AND profile_name = ?
		`, threshold, store.InstanceName(), store.ProfileName()); err != nil {
			t.Fatalf("prepare boundary threshold %f: %v", threshold, err)
		}

		updated, err := store.EnsureAudioSettings(ctx)
		if err != nil {
			t.Fatalf("ensure audio settings: %v", err)
		}
		if updated {
			t.Fatalf("expected boundary threshold %f to remain unchanged", threshold)
		}

		settings, err := store.LoadAudioSettings(ctx)
		if err != nil {
			t.Fatalf("load audio settings: %v", err)
		}
		if settings.VADThreshold != threshold {
			t.Fatalf("expected threshold %f, got %f", threshold, settings.VADThreshold)
		}
		if !settings.Metadata.Valid || strings.TrimSpace(settings.Metadata.String) != `{"keep":true}` {
			t.Fatalf("expected metadata to be preserved, got %+v", settings.Metadata)
		}
	}
}
