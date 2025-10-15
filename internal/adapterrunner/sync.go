package adapterrunner

import (
	"context"
	"errors"
	"fmt"
	"os"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
)

const (
	// SettingVersion stores the active adapter-runner version in config store.
	SettingVersion = "adapter_runner.version"
	// SettingBinaryPath stores the active adapter-runner binary path in config store.
	SettingBinaryPath = "adapter_runner.binary_path"
)

// SyncSettings persists adapter-runner metadata into the configuration store.
// Missing binaries are tolerated and logged by the caller.
func SyncSettings(ctx context.Context, store *configstore.Store, manager *Manager) error {
	if store == nil || manager == nil {
		return nil
	}

	if err := manager.EnsureLayout(); err != nil {
		return err
	}

	version, err := manager.InstalledVersion()
	if err != nil {
		if errors.Is(err, ErrNotInstalled) {
			return nil
		}
		return err
	}

	binary := manager.BinaryPath()
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf("adapter-runner active binary missing: %w", err)
	}

	values := map[string]string{
		SettingVersion:    version,
		SettingBinaryPath: binary,
	}
	return store.SaveSettings(ctx, values)
}
