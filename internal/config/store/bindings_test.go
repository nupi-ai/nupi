package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSetActiveAdapter(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer os.Remove(dbPath)
	defer store.Close()

	ctx := context.Background()

	// ensure adapter exists
	adapter := Adapter{ID: "adapter.test", Source: "builtin", Type: "ai", Name: "Test"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, "ai.primary", adapter.ID, map[string]any{"foo": "bar"}); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	bindings, err := store.ListAdapterBindings(ctx)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}

	var found bool
	for _, b := range bindings {
		if b.Slot == "ai.primary" {
			found = true
			if b.AdapterID == nil || *b.AdapterID != adapter.ID {
				t.Fatalf("expected adapter %q, got %v", adapter.ID, b.AdapterID)
			}
		}
	}
	if !found {
		t.Fatalf("binding for ai.primary not found")
	}
}

func TestUpdateAdapterBindingStatus(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer os.Remove(dbPath)
	defer store.Close()

	ctx := context.Background()

	adapter := Adapter{ID: "adapter.test2", Source: "builtin", Type: "ai", Name: "Test2"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, "ai.primary", adapter.ID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	if err := store.UpdateAdapterBindingStatus(ctx, "ai.primary", BindingStatusInactive); err != nil {
		t.Fatalf("update binding status: %v", err)
	}

	bindings, err := store.ListAdapterBindings(ctx)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}

	for _, binding := range bindings {
		if binding.Slot != "ai.primary" {
			continue
		}
		if binding.Status != BindingStatusInactive {
			t.Fatalf("expected status inactive, got %s", binding.Status)
		}
		if binding.AdapterID == nil || *binding.AdapterID != adapter.ID {
			t.Fatalf("expected adapter to remain %s, got %v", adapter.ID, binding.AdapterID)
		}
	}

	if err := store.UpdateAdapterBindingStatus(ctx, "ai.primary", "invalid"); err == nil {
		t.Fatalf("expected error for invalid status")
	}
}
