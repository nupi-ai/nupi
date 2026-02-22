package egress

import (
	"os"
	"testing"

	"github.com/nupi-ai/nupi/internal/config/store"
)

func TestMain(m *testing.M) {
	// Disable real OS keychain probing to prevent hangs in egress tests
	// that transitively call store.Open().
	cleanup := store.DisableKeychainForTesting()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
