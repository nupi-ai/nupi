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
