package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	storecrypto "github.com/nupi-ai/nupi/internal/config/store/crypto"
)

// SaveSecuritySettings upserts secret entries for the active profile.
func (s *Store) SaveSecuritySettings(ctx context.Context, values map[string]string) error {
	if s.readOnly {
		return fmt.Errorf("config: save security settings: store opened read-only")
	}
	if len(values) == 0 {
		return nil
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
            INSERT INTO security_settings (instance_name, profile_name, key, value, updated_at)
            VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
            ON CONFLICT(instance_name, profile_name, key) DO UPDATE SET
                value = excluded.value,
                updated_at = CURRENT_TIMESTAMP
        `)
		if err != nil {
			return fmt.Errorf("config: prepare save security: %w", err)
		}
		defer stmt.Close()

		for key, value := range values {
			storedValue := value
			if s.encryptionKey != nil {
				encrypted, err := storecrypto.EncryptValue(s.encryptionKey, value)
				if err != nil {
					return fmt.Errorf("config: encrypt security %q: %w", key, err)
				}
				storedValue = encrypted
			}
			if _, err := stmt.ExecContext(ctx, s.instanceName, s.profileName, key, storedValue); err != nil {
				return fmt.Errorf("config: exec save security %q: %w", key, err)
			}
		}
		return nil
	})
}

// LoadSecuritySettings returns secrets for the active profile.
func (s *Store) LoadSecuritySettings(ctx context.Context, keys ...string) (map[string]string, error) {
	query := `SELECT key, value FROM security_settings WHERE instance_name = ? AND profile_name = ?`
	args := []any{s.instanceName, s.profileName}

	if len(keys) > 0 {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
		query += fmt.Sprintf(" AND key IN (%s)", placeholders)
		for _, key := range keys {
			args = append(args, key)
		}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("config: load security: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("config: scan security row: %w", err)
		}
		if s.encryptionKey == nil {
			return nil, fmt.Errorf("config: security %q is encrypted but no decryption key is available", key)
		}
		decrypted, err := storecrypto.DecryptValue(s.encryptionKey, value)
		if err != nil {
			return nil, fmt.Errorf("config: decrypt security %q: %w", key, err)
		}
		value = decrypted
		result[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("config: iterate security rows: %w", err)
	}
	return result, nil
}
