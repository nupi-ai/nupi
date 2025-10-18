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

	// remove ai.primary to simulate pre-migration database
	if _, err := store.DB().ExecContext(ctx, `
		DELETE FROM adapter_bindings WHERE instance_name = ? AND profile_name = ? AND slot = 'ai.primary'
	`, store.InstanceName(), store.ProfileName()); err != nil {
		t.Fatalf("delete slot: %v", err)
	}

	result, err := store.EnsureRequiredAdapterSlots(ctx)
	if err != nil {
		t.Fatalf("ensure required slots: %v", err)
	}

	found := false
	for _, slot := range result.UpdatedSlots {
		if slot == "ai.primary" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected ai.primary to be reported as updated, got %v", result.UpdatedSlots)
	}

	bindings, err := store.ListAdapterBindings(ctx)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}

	var restored bool
	for _, binding := range bindings {
		if binding.Slot != "ai.primary" {
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
		t.Fatalf("ai.primary slot not restored")
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

	slots := []string{"stt.primary", "stt.secondary", "vad.primary"}
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
