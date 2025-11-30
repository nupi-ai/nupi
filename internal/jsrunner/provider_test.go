package jsrunner

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestGetRuntimePath_EnvOverride(t *testing.T) {
	// Create a temp file to act as the runtime
	tmpDir := t.TempDir()
	fakeBun := filepath.Join(tmpDir, "fake-bun")
	if err := os.WriteFile(fakeBun, []byte("#!/bin/sh\necho fake"), 0755); err != nil {
		t.Fatal(err)
	}

	// Set env and test
	t.Setenv("NUPI_JS_RUNTIME", fakeBun)

	path, err := GetRuntimePath()
	if err != nil {
		t.Errorf("GetRuntimePath() error = %v", err)
	}
	if path != fakeBun {
		t.Errorf("GetRuntimePath() = %v, want %v", path, fakeBun)
	}
}

func TestGetRuntimePath_EnvOverride_NotExists(t *testing.T) {
	// Set env to non-existent path - should return error
	t.Setenv("NUPI_JS_RUNTIME", "/nonexistent/path/to/bun")

	_, err := GetRuntimePath()
	if err == nil {
		t.Error("GetRuntimePath() should return error when NUPI_JS_RUNTIME points to non-existent file")
	}
	if !errors.Is(err, ErrRuntimeNotFound) {
		t.Errorf("GetRuntimePath() error should be ErrRuntimeNotFound, got %v", err)
	}
}

func TestGetRuntimePath_BundledLocation(t *testing.T) {
	// Create a temp NUPI_HOME with bundled bun
	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	binaryName := RuntimeName
	fakeBun := filepath.Join(binDir, binaryName)
	if err := os.WriteFile(fakeBun, []byte("#!/bin/sh\necho fake"), 0755); err != nil {
		t.Fatal(err)
	}

	// Clear NUPI_JS_RUNTIME and set NUPI_HOME
	t.Setenv("NUPI_JS_RUNTIME", "")
	t.Setenv("NUPI_HOME", tmpDir)

	path, err := GetRuntimePath()
	if err != nil {
		t.Errorf("GetRuntimePath() error = %v", err)
	}
	if path != fakeBun {
		t.Errorf("GetRuntimePath() = %v, want %v", path, fakeBun)
	}
}

func TestGetRuntimePath_NotFound(t *testing.T) {
	// With no runtime available, should return error (no PATH fallback)
	t.Setenv("NUPI_JS_RUNTIME", "")
	t.Setenv("NUPI_HOME", "/nonexistent/path")

	_, err := GetRuntimePath()
	if err == nil {
		t.Error("GetRuntimePath() should return error when runtime not found")
	}
	if !errors.Is(err, ErrRuntimeNotFound) {
		t.Errorf("GetRuntimePath() error should be ErrRuntimeNotFound, got %v", err)
	}
}

func TestIsAvailable(t *testing.T) {
	// Create a temp NUPI_HOME with bundled bun
	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	fakeBun := filepath.Join(binDir, RuntimeName)
	if err := os.WriteFile(fakeBun, []byte("#!/bin/sh\necho fake"), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("NUPI_JS_RUNTIME", "")
	t.Setenv("NUPI_HOME", tmpDir)

	if !IsAvailable() {
		t.Error("IsAvailable() should return true when runtime exists")
	}

	// Now test when not available
	t.Setenv("NUPI_HOME", "/nonexistent/path")
	if IsAvailable() {
		t.Error("IsAvailable() should return false when runtime not found")
	}
}

func TestRuntimeConstants(t *testing.T) {
	if RuntimeName == "" {
		t.Error("RuntimeName should not be empty")
	}
	if RuntimeVersion == "" {
		t.Error("RuntimeVersion should not be empty")
	}
}
