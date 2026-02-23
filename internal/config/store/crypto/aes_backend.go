package crypto

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// upsertSecuritySQL is the SQL statement for inserting or updating a secret
// in security_settings. Shared by Set and SetBatch to prevent drift.
const upsertSecuritySQL = `
	INSERT INTO security_settings (instance_name, profile_name, key, value, updated_at)
	VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
	ON CONFLICT(instance_name, profile_name, key) DO UPDATE SET
		value = excluded.value,
		updated_at = CURRENT_TIMESTAMP
`

// AESBackend wraps the existing AES-256-GCM encryption with SQLite storage.
type AESBackend struct {
	db       *sql.DB
	key      []byte
	instance string
	profile  string
}

// NewAESBackend creates an AESBackend backed by the given DB and encryption key.
func NewAESBackend(db *sql.DB, key []byte, instance, profile string) *AESBackend {
	return &AESBackend{
		db:       db,
		key:      key,
		instance: instance,
		profile:  profile,
	}
}

// SetBatch atomically encrypts and upserts multiple secrets in a single transaction.
func (ab *AESBackend) SetBatch(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	tx, err := ab.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("aes-file: begin tx: %w", err)
	}
	defer tx.Rollback() // no-op after successful Commit

	stmt, err := tx.PrepareContext(ctx, upsertSecuritySQL)
	if err != nil {
		return fmt.Errorf("aes-file: prepare: %w", err)
	}
	defer stmt.Close()

	for key, value := range values {
		encrypted, err := EncryptValue(ab.key, value)
		if err != nil {
			return fmt.Errorf("aes-file: encrypt %q: %w", key, err)
		}
		if _, err := stmt.ExecContext(ctx, ab.instance, ab.profile, key, encrypted); err != nil {
			return fmt.Errorf("aes-file: set %q: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("aes-file: commit: %w", err)
	}
	return nil
}

// Set encrypts the value and upserts it into security_settings.
func (ab *AESBackend) Set(ctx context.Context, key, value string) error {
	encrypted, err := EncryptValue(ab.key, value)
	if err != nil {
		return fmt.Errorf("aes-file: encrypt %q: %w", key, err)
	}
	_, err = ab.db.ExecContext(ctx, upsertSecuritySQL, ab.instance, ab.profile, key, encrypted)
	if err != nil {
		return fmt.Errorf("aes-file: set %q: %w", key, err)
	}
	return nil
}

// GetBatch retrieves and decrypts multiple secrets in a single SQL query.
// Note: the IN clause uses one host parameter per key plus 2 (instance, profile).
// SQLite limits host parameters to SQLITE_MAX_VARIABLE_NUMBER (≥999). For the
// typical <100 secrets in security_settings, this is well within limits.
func (ab *AESBackend) GetBatch(ctx context.Context, keys []string) (map[string]string, error) {
	if len(keys) == 0 {
		return make(map[string]string), nil
	}

	placeholders := make([]string, len(keys))
	args := []any{ab.instance, ab.profile}
	for i, k := range keys {
		placeholders[i] = "?"
		args = append(args, k)
	}

	query := fmt.Sprintf(
		`SELECT key, value FROM security_settings WHERE instance_name = ? AND profile_name = ? AND key IN (%s)`,
		strings.Join(placeholders, ","),
	)

	rows, err := ab.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("aes-file: get batch: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string, len(keys))
	for rows.Next() {
		key, raw, err := scanStringPair(rows)
		if err != nil {
			return nil, fmt.Errorf("aes-file: scan batch: %w", err)
		}
		decrypted, err := DecryptValue(ab.key, raw)
		if err != nil {
			return nil, fmt.Errorf("aes-file: decrypt batch %q: %w", key, err)
		}
		result[key] = decrypted
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aes-file: iterate batch: %w", err)
	}
	return result, nil
}

// Get reads and decrypts a secret from security_settings.
func (ab *AESBackend) Get(ctx context.Context, key string) (string, error) {
	var raw string
	err := ab.db.QueryRowContext(ctx,
		`SELECT value FROM security_settings WHERE instance_name = ? AND profile_name = ? AND key = ?`,
		ab.instance, ab.profile, key,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("aes-file: get %q: %w", key, ErrSecretNotFound)
		}
		return "", fmt.Errorf("aes-file: get %q: %w", key, err)
	}
	decrypted, err := DecryptValue(ab.key, raw)
	if err != nil {
		return "", fmt.Errorf("aes-file: decrypt %q: %w", key, err)
	}
	return decrypted, nil
}

// Delete removes a secret from security_settings.
func (ab *AESBackend) Delete(ctx context.Context, key string) error {
	result, err := ab.db.ExecContext(ctx,
		`DELETE FROM security_settings WHERE instance_name = ? AND profile_name = ? AND key = ?`,
		ab.instance, ab.profile, key,
	)
	if err != nil {
		return fmt.Errorf("aes-file: delete %q: %w", key, err)
	}
	rows, raErr := result.RowsAffected()
	if raErr != nil {
		return fmt.Errorf("aes-file: delete %q rows affected: %w", key, raErr)
	}
	if rows == 0 {
		return fmt.Errorf("aes-file: delete %q: %w", key, ErrSecretNotFound)
	}
	return nil
}

// Available always returns true — AES encryption is always available when a key exists.
func (ab *AESBackend) Available() bool {
	return ab.key != nil
}

// Name returns the backend identifier for logging.
func (ab *AESBackend) Name() string { return "aes-file" }
