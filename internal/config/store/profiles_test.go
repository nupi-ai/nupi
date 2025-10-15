package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestActivateProfile(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	if _, err := store.DB().ExecContext(ctx, `
        INSERT INTO profiles (instance_name, name, is_default)
        VALUES (?, ?, 0)
    `, store.InstanceName(), "secondary"); err != nil {
		t.Fatalf("insert profile: %v", err)
	}

	if err := store.ActivateProfile(ctx, "secondary"); err != nil {
		t.Fatalf("activate profile: %v", err)
	}

	profiles, err := store.Profiles(ctx)
	if err != nil {
		t.Fatalf("list profiles: %v", err)
	}

	var foundDefault bool
	for _, profile := range profiles {
		if profile.Name == "secondary" {
			if !profile.IsDefault {
				t.Fatalf("expected secondary to be default")
			}
			foundDefault = true
		}
		if profile.Name == store.ProfileName() && profile.IsDefault {
			t.Fatalf("original default should be cleared")
		}
	}
	if !foundDefault {
		t.Fatalf("secondary profile not returned")
	}
}

func TestActivateProfileNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	err = store.ActivateProfile(ctx, "missing")
	if err == nil {
		t.Fatalf("expected not found error")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T", err)
	}
}
