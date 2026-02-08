package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSecuritySettingsPersistence(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "config.db")
	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()

	// Save multiple secrets.
	secrets := map[string]string{
		"elevenlabs_api_key": "sk-test123",
		"openai_api_key":     "sk-openai-456",
	}
	if err := store.SaveSecuritySettings(ctx, secrets); err != nil {
		t.Fatalf("save security settings: %v", err)
	}

	// Load all secrets.
	loaded, err := store.LoadSecuritySettings(ctx)
	if err != nil {
		t.Fatalf("load security settings: %v", err)
	}
	if loaded["elevenlabs_api_key"] != "sk-test123" {
		t.Fatalf("expected elevenlabs_api_key=sk-test123, got %q", loaded["elevenlabs_api_key"])
	}
	if loaded["openai_api_key"] != "sk-openai-456" {
		t.Fatalf("expected openai_api_key=sk-openai-456, got %q", loaded["openai_api_key"])
	}

	// Load with key filter.
	filtered, err := store.LoadSecuritySettings(ctx, "elevenlabs_api_key")
	if err != nil {
		t.Fatalf("load filtered: %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("expected 1 filtered result, got %d", len(filtered))
	}
	if filtered["elevenlabs_api_key"] != "sk-test123" {
		t.Fatalf("filtered value mismatch: %q", filtered["elevenlabs_api_key"])
	}
}

func TestSecuritySettingsIsolatedByProfile(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "config.db")

	// Open store with profile A (default).
	storeA, err := Open(Options{DBPath: dbPath, ProfileName: "profile-a"})
	if err != nil {
		t.Fatalf("open store A: %v", err)
	}

	ctx := context.Background()

	if err := storeA.SaveSecuritySettings(ctx, map[string]string{
		"api_key": "secret-for-a",
	}); err != nil {
		t.Fatalf("save secret A: %v", err)
	}
	if err := storeA.Close(); err != nil {
		t.Fatalf("close store A before reopen: %v", err)
	}

	// Open store with profile B.
	storeB, err := Open(Options{DBPath: dbPath, ProfileName: "profile-b"})
	if err != nil {
		t.Fatalf("open store B: %v", err)
	}
	t.Cleanup(func() { storeB.Close() })

	loadedB, err := storeB.LoadSecuritySettings(ctx, "api_key")
	if err != nil {
		t.Fatalf("load from profile B: %v", err)
	}
	if val, ok := loadedB["api_key"]; ok {
		t.Fatalf("expected api_key to NOT be visible from profile B, got %q", val)
	}

	// Verify profile A still has it.
	storeA2, err := Open(Options{DBPath: dbPath, ProfileName: "profile-a"})
	if err != nil {
		t.Fatalf("reopen store A: %v", err)
	}
	t.Cleanup(func() { storeA2.Close() })

	loadedA, err := storeA2.LoadSecuritySettings(ctx, "api_key")
	if err != nil {
		t.Fatalf("load from profile A: %v", err)
	}
	if loadedA["api_key"] != "secret-for-a" {
		t.Fatalf("expected profile A secret, got %q", loadedA["api_key"])
	}
}
