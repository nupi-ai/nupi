package adapterrunner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
)

func TestSyncSettings(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, ".nupi")

	m := NewManager(base)
	if err := m.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout failed: %v", err)
	}

	versionDir := m.VersionRoot("v0.9.0")
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		t.Fatalf("failed to create version directory: %v", err)
	}

	src := m.BinaryForVersion("v0.9.0")
	if err := os.WriteFile(src, []byte("runner"), 0o755); err != nil {
		t.Fatalf("failed to write runner binary: %v", err)
	}

	if err := m.ActivateVersion("v0.9.0"); err != nil {
		t.Fatalf("ActivateVersion failed: %v", err)
	}

	os.Setenv("HOME", tmp)
	defer os.Unsetenv("HOME")

	store, err := configstore.Open(configstore.Options{
		InstanceName: "default",
		ProfileName:  "default",
	})
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	if err := SyncSettings(context.Background(), store, m); err != nil {
		t.Fatalf("SyncSettings failed: %v", err)
	}

	settings, err := store.LoadSettings(context.Background(), SettingVersion, SettingBinaryPath)
	if err != nil {
		t.Fatalf("LoadSettings failed: %v", err)
	}

	if settings[SettingVersion] != "v0.9.0" {
		t.Fatalf("setting %s = %q; want v0.9.0", SettingVersion, settings[SettingVersion])
	}
	if settings[SettingBinaryPath] != m.BinaryPath() {
		t.Fatalf("setting %s = %q; want %q", SettingBinaryPath, settings[SettingBinaryPath], m.BinaryPath())
	}
}
