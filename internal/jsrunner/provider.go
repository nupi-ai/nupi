// Package jsrunner provides JS runtime resolution for Nupi plugins.
//
// This package is the single source of truth for JS runtime configuration.
// Currently uses Bun, but the API is generic to allow future changes.
//
// Resolution order:
//  1. NUPI_JS_RUNTIME environment variable (explicit path override)
//  2. Bundled runtime next to the executable (release package layout)
//  3. NUPI_HOME/bin/ or ~/.nupi/bin/ (manual install location)
//
// Note: Bun is bundled in release packages. There is NO fallback to system PATH.
// If the runtime is not found, an error is returned. Users can override with
// NUPI_JS_RUNTIME environment variable if needed.
package jsrunner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// RuntimeName is the name of the JS runtime (currently Bun).
// This may change in future versions (e.g., to Node or Deno).
const RuntimeName = "bun"

// RuntimeVersion is the version of the bundled JS runtime.
// This should match the version downloaded by scripts/download-bun.sh
const RuntimeVersion = "1.1.34"

// ErrRuntimeNotFound indicates the JS runtime could not be located.
var ErrRuntimeNotFound = errors.New("jsrunner: runtime not found")

// GetRuntimePath returns the path to the JS runtime binary.
// It checks (in order):
//  1. NUPI_JS_RUNTIME env var (explicit override)
//  2. Next to the current executable (release package layout)
//  3. NUPI_HOME/bin/ or ~/.nupi/bin/
//
// Returns ErrRuntimeNotFound if the runtime cannot be located.
// There is NO fallback to system PATH - bundled runtime is required.
func GetRuntimePath() (string, error) {
	binaryName := getRuntimeBinaryName()

	// 1. Check NUPI_JS_RUNTIME env (explicit override)
	if envPath := os.Getenv("NUPI_JS_RUNTIME"); envPath != "" {
		if _, err := os.Stat(envPath); err == nil {
			return envPath, nil
		}
		// Env var set but file doesn't exist - this is an error
		return "", fmt.Errorf("%w: NUPI_JS_RUNTIME=%q does not exist", ErrRuntimeNotFound, envPath)
	}

	// 2. Check next to executable (release package layout: bin/nupid + bin/bun)
	if exePath, err := os.Executable(); err == nil {
		bundledPath := filepath.Join(filepath.Dir(exePath), binaryName)
		if _, err := os.Stat(bundledPath); err == nil {
			return bundledPath, nil
		}
	}

	// 3. Check NUPI_HOME/bin/ or ~/.nupi/bin/
	nupiHomePath := getNupiHomeBundledPath(binaryName)
	if _, err := os.Stat(nupiHomePath); err == nil {
		return nupiHomePath, nil
	}

	// Not found - return error with helpful message
	return "", fmt.Errorf("%w: %s not found. Install via 'make download-bun install' or set NUPI_JS_RUNTIME env var",
		ErrRuntimeNotFound, binaryName)
}

// IsRuntimeNotFound returns true if the error indicates the JS runtime was not found.
func IsRuntimeNotFound(err error) bool {
	return errors.Is(err, ErrRuntimeNotFound)
}

// getRuntimeBinaryName returns the platform-specific binary name.
func getRuntimeBinaryName() string {
	if runtime.GOOS == "windows" {
		return RuntimeName + ".exe"
	}
	return RuntimeName
}

// getNupiHomeBundledPath returns the path in NUPI_HOME/bin/.
func getNupiHomeBundledPath(binaryName string) string {
	home := os.Getenv("NUPI_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			userHome = "."
		}
		home = filepath.Join(userHome, ".nupi")
	}
	return filepath.Join(home, "bin", binaryName)
}

// IsAvailable checks if the JS runtime is available.
func IsAvailable() bool {
	_, err := GetRuntimePath()
	return err == nil
}
