package crypto

import (
	"context"
	"errors"
	"fmt"
	"log"
)

// FallbackBackend writes to both primary (keychain) and secondary (AES/DB)
// backends on Set, ensuring the DB always has all keys for enumeration.
// On Get, it tries primary first and falls back to secondary.
type FallbackBackend struct {
	primary   SecretBackend
	secondary SecretBackend
}

// NewFallbackBackend creates a FallbackBackend with primary and secondary backends.
func NewFallbackBackend(primary, secondary SecretBackend) *FallbackBackend {
	return &FallbackBackend{
		primary:   primary,
		secondary: secondary,
	}
}

// SetBatch atomically stores multiple secrets. The secondary (AES/DB) write
// is transactional. Primary (keychain) writes are best-effort.
func (fb *FallbackBackend) SetBatch(ctx context.Context, values map[string]string) error {
	if err := fb.secondary.SetBatch(ctx, values); err != nil {
		return err
	}
	if fb.primary.Available() {
		if err := fb.primary.SetBatch(ctx, values); err != nil {
			log.Printf("[Config] %s SetBatch failed (AES fallback has the values): %v", fb.primary.Name(), err)
		}
	}
	return nil
}

// Set stores a secret in both backends. The secondary (AES/DB) is always
// written to maintain key enumeration. The primary (keychain) is written
// as an optimization for reads and security.
func (fb *FallbackBackend) Set(ctx context.Context, key, value string) error {
	// Always write to secondary (AES/DB) — this ensures key enumeration
	// works and provides a fallback if keychain becomes unavailable.
	if err := fb.secondary.Set(ctx, key, value); err != nil {
		return err
	}

	// Also write to primary (keychain) if available.
	if fb.primary.Available() {
		if err := fb.primary.Set(ctx, key, value); err != nil {
			log.Printf("[Config] %s Set(%q) failed (AES fallback has the value): %v", fb.primary.Name(), key, err)
		}
	}
	return nil
}

// GetBatch retrieves multiple secrets, trying primary first then filling
// gaps from secondary.
func (fb *FallbackBackend) GetBatch(ctx context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	var missingKeys []string

	if fb.primary.Available() {
		primaryResult, err := fb.primary.GetBatch(ctx, keys)
		if err != nil {
			log.Printf("[Config] %s GetBatch failed, falling back to %s: %v", fb.primary.Name(), fb.secondary.Name(), err)
			missingKeys = keys
		} else {
			for k, v := range primaryResult {
				result[k] = v
			}
			for _, k := range keys {
				if _, ok := result[k]; !ok {
					missingKeys = append(missingKeys, k)
				}
			}
		}
	} else {
		missingKeys = keys
	}

	if len(missingKeys) > 0 {
		secResult, err := fb.secondary.GetBatch(ctx, missingKeys)
		if err != nil {
			return nil, err
		}
		for k, v := range secResult {
			result[k] = v
		}
	}

	return result, nil
}

// Get retrieves a secret from the primary backend, falling back to secondary.
func (fb *FallbackBackend) Get(ctx context.Context, key string) (string, error) {
	if fb.primary.Available() {
		val, err := fb.primary.Get(ctx, key)
		if err == nil {
			return val, nil
		}
		// Only log non-"not found" errors — ErrSecretNotFound is normal
		// (key may exist in secondary but not primary).
		if !errors.Is(err, ErrSecretNotFound) {
			log.Printf("[Config] %s Get(%q) failed, falling back to %s: %v", fb.primary.Name(), key, fb.secondary.Name(), err)
		}
	}
	return fb.secondary.Get(ctx, key)
}

// Delete removes a secret from both backends. Primary (keychain) is deleted
// first: if it has the key but cannot delete it, the operation fails to
// prevent an inconsistent state where the key is gone from DB but still
// retrievable from keychain via Get (which checks primary first).
//
// Edge case: if primary.Delete times out (circuit breaker fires), the caller
// receives an error and both backends retain the key. If the caller retries,
// the circuit breaker makes Available() return false, so the retry deletes
// only from secondary (AES). The primary (keychain) may still hold a stale
// copy that resurfaces after a process restart when the circuit breaker
// resets. This is accepted because (a) Delete has no production callers
// today, (b) the stale value is identical to the pre-delete value, and
// (c) automatic cleanup would require tracking per-key circuit-breaker
// state, which adds complexity disproportionate to the risk.
func (fb *FallbackBackend) Delete(ctx context.Context, key string) error {
	// Delete from primary (keychain) first.
	if fb.primary.Available() {
		if err := fb.primary.Delete(ctx, key); err != nil {
			if !errors.Is(err, ErrSecretNotFound) {
				// Primary has the key but can't delete it — bail out so the
				// key stays consistent in both backends.
				return fmt.Errorf("delete from %s: %w", fb.primary.Name(), err)
			}
			// ErrSecretNotFound from primary is normal (key may only exist
			// in secondary, e.g. when keychain was unavailable during Set).
		}
	}

	// Delete from secondary (DB).
	return fb.secondary.Delete(ctx, key)
}

// Available returns true if either backend is available.
func (fb *FallbackBackend) Available() bool {
	return fb.primary.Available() || fb.secondary.Available()
}

// Name returns the combined backend name for logging.
func (fb *FallbackBackend) Name() string {
	return fb.primary.Name() + "+" + fb.secondary.Name()
}
