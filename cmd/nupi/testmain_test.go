package main

import (
	"os"
	"testing"

	"github.com/nupi-ai/nupi/internal/config/store"
)

func TestMain(m *testing.M) {
	// Disable real OS keychain probing to prevent hangs in tests
	// that transitively call store.Open() via grpcclient.New().
	cleanup := store.DisableKeychainForTesting()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
