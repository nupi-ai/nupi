package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAdapterExists(t *testing.T) {
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

	exists, err := store.AdapterExists(ctx, "adapter.ai.demo")
	if err != nil {
		t.Fatalf("adapter exists query failed: %v", err)
	}
	if exists {
		t.Fatalf("expected adapter to be absent by default")
	}

	adapter := Adapter{
		ID:      "adapter.ai.demo",
		Source:  "builtin",
		Type:    "ai",
		Name:    "Demo AI",
		Version: "1.0.0",
	}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	exists, err = store.AdapterExists(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("adapter exists query failed: %v", err)
	}
	if !exists {
		t.Fatalf("expected adapter to exist after upsert")
	}
}
