package store

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Disable real OS keychain in store-level tests to prevent
	// non-deterministic behaviour from real keychain accessibility.
	// Keychain-specific tests live in the crypto package with mock providers.
	cleanup := DisableKeychainForTesting()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
