package runtime

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func withEnv(t *testing.T, key, value string) {
	t.Helper()
	original, present := os.LookupEnv(key)
	if value == "" {
		_ = os.Unsetenv(key)
	} else {
		if err := os.Setenv(key, value); err != nil {
			t.Fatalf("setenv %s: %v", key, err)
		}
	}
	t.Cleanup(func() {
		if !present {
			_ = os.Unsetenv(key)
			return
		}
		_ = os.Setenv(key, original)
	})
}

func TestLoadConfigFromEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script helper not supported on Windows")
	}
	tempDir := t.TempDir()
	command := filepath.Join(tempDir, "bin", "module")
	if err := os.MkdirAll(filepath.Dir(command), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}

	withEnv(t, "NUPI_MODULE_COMMAND", command)
	withEnv(t, "NUPI_MODULE_SLOT", "stt.primary")
	withEnv(t, "NUPI_MODULE_ADAPTER", "adapter.test")
	withEnv(t, "NUPI_MODULE_ARGS", `["--foo","bar"]`)
	withEnv(t, "NUPI_MODULE_HOME", filepath.Join(tempDir, "home"))
	withEnv(t, "NUPI_MODULE_DATA_DIR", "cache")
	withEnv(t, "NUPI_MODULE_SHUTDOWN_GRACE", "5s")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Command != command {
		t.Fatalf("expected command %q, got %q", command, cfg.Command)
	}
	if cfg.ListenAddrEnv != "NUPI_ADAPTER_LISTEN_ADDR" {
		t.Fatalf("expected default listen env, got %s", cfg.ListenAddrEnv)
	}
	if len(cfg.Args) != 2 || cfg.Args[0] != "--foo" || cfg.Args[1] != "bar" {
		t.Fatalf("unexpected args: %#v", cfg.Args)
	}
	if !strings.HasSuffix(cfg.ModuleDataDir, "cache") {
		t.Fatalf("expected data dir resolved, got %s", cfg.ModuleDataDir)
	}
	if cfg.GracePeriod != 5*time.Second {
		t.Fatalf("expected grace period 5s, got %s", cfg.GracePeriod)
	}
}

func TestLoadConfigFromEnvMissingCommand(t *testing.T) {
	_, err := LoadConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "NUPI_MODULE_COMMAND") {
		t.Fatalf("expected missing command error, got %v", err)
	}
}
