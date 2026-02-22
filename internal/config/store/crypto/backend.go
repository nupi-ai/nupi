package crypto

import (
	"context"
	"errors"
)

// ErrSecretNotFound is returned when a secret key does not exist in the backend.
var ErrSecretNotFound = errors.New("secret not found")

// SecretBackend abstracts secret storage (keychain vs AES-file).
// Note: DeleteBatch is intentionally omitted — secret deletion is a rare
// one-at-a-time operation, unlike batch writes (SetBatch) and reads (GetBatch)
// which are needed for saving/loading adapter configurations.
type SecretBackend interface {
	Set(ctx context.Context, key, value string) error
	// SetBatch atomically stores multiple secrets. Implementations that
	// support transactions (e.g., AESBackend) commit all-or-nothing.
	SetBatch(ctx context.Context, values map[string]string) error
	Get(ctx context.Context, key string) (string, error)
	// GetBatch retrieves multiple secrets. Implementations should skip keys
	// that don't exist (no ErrSecretNotFound for individual missing keys).
	GetBatch(ctx context.Context, keys []string) (map[string]string, error)
	Delete(ctx context.Context, key string) error
	Available() bool
	Name() string // "keychain" or "aes-file" — for logging
}
