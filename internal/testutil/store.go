package testutil

import (
	"path/filepath"
	"testing"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
)

// OpenStore creates a temporary config store and returns a cleanup function.
func OpenStore(t *testing.T) (*configstore.Store, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "config.db")
	store, err := configstore.Open(configstore.Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store, func() { store.Close() }
}
