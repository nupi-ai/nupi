package store

import (
	"context"
	"fmt"
	"log"
)

const securityKeyColumns = "key"
const securitySettingsValueColumn = "value"

// SaveSecuritySettings upserts secret entries for the active profile.
func (s *Store) SaveSecuritySettings(ctx context.Context, values map[string]string) error {
	if err := s.ensureWritable("save security settings"); err != nil {
		return err
	}
	if s.secretBackend == nil {
		return fmt.Errorf("config: save security settings: secret backend not available")
	}
	if len(values) == 0 {
		return nil
	}

	if err := s.secretBackend.SetBatch(ctx, values); err != nil {
		return fmt.Errorf("config: save security settings: %w", err)
	}
	return nil
}

// LoadSecuritySettings returns secrets for the active profile.
func (s *Store) LoadSecuritySettings(ctx context.Context, keys ...string) (map[string]string, error) {
	if s.secretBackend == nil {
		return nil, fmt.Errorf("config: load security settings: secret backend not available")
	}

	if len(keys) == 0 {
		// Load all keys: query DB for key names, then batch-retrieve via backend.
		return s.loadAllSecuritySettings(ctx)
	}

	// Batch-retrieve requested keys. GetBatch silently skips keys that
	// don't exist (no ErrSecretNotFound for individual missing keys),
	// matching the legacy per-key skip behavior.
	result, err := s.secretBackend.GetBatch(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("config: load security settings: %w", err)
	}
	return result, nil
}

// loadAllSecuritySettings discovers all secret keys from the DB and loads
// them via the backend's batch operation (single SQL query for AES, with
// keychain overlay when available).
//
// Note: this issues two DB queries in the AES-only path (key enumeration +
// value retrieval). A single query would suffice for AES-only, but the
// two-step design lets FallbackBackend try the keychain first for values,
// avoiding the second DB round-trip when the keychain has all keys.
func (s *Store) loadAllSecuritySettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+securityKeyColumns+` FROM security_settings WHERE instance_name = ? AND profile_name = ?`,
		s.instanceName, s.profileName,
	)
	if err != nil {
		return nil, fmt.Errorf("config: list security keys: %w", err)
	}
	dbKeys, err := scanList(rows, scanString, "config: scan security key", "config: iterate security keys")
	if err != nil {
		return nil, err
	}

	if len(dbKeys) == 0 {
		return make(map[string]string), nil
	}

	result, err := s.secretBackend.GetBatch(ctx, dbKeys)
	if err != nil {
		return nil, fmt.Errorf("config: load all security settings: %w", err)
	}
	if len(result) < len(dbKeys) {
		var missing []string
		for _, k := range dbKeys {
			if _, ok := result[k]; !ok {
				missing = append(missing, k)
			}
		}
		log.Printf("[Config] WARNING: %d/%d security settings unavailable (degraded backend mode): %v", len(missing), len(dbKeys), missing)
	}
	return result, nil
}
