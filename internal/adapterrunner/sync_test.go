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
	src := buildStubRunner(t, tmp, "v0.9.0")
	if err := m.InstallFromFile(src); err != nil {
		t.Fatalf("InstallFromFile failed: %v", err)
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
