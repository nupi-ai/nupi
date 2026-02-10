package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	keySize     = 32 // AES-256
	keyFileName = ".secrets.key"
	// encPrefix marks encrypted values in the database.
	// Plaintext values (pre-encryption migration) lack this prefix.
	encPrefix = "enc:v1:"
)

// loadEncryptionKey reads an existing encryption key from keyPath.
// Returns nil, nil if the file doesn't exist (key not yet created).
func loadEncryptionKey(keyPath string) ([]byte, error) {
	data, err := os.ReadFile(keyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: read encryption key: %w", err)
	}
	if len(data) != keySize {
		return nil, fmt.Errorf("config: encryption key at %s has invalid size %d (expected %d)", keyPath, len(data), keySize)
	}
	return data, nil
}

// createEncryptionKey generates a new 32-byte AES key and writes it to keyPath.
// Uses a temp-file + hard-link pattern for atomic creation to prevent race
// conditions when multiple processes open the store concurrently.
//
// The key is first written to a temporary file, then atomically linked to the
// final path. os.Link fails with EEXIST if another process already created
// the file, guaranteeing exactly one key wins and the file is never partially
// written at keyPath.
//
// Callers must verify that creating a new key is safe (i.e. no existing
// encrypted values in the DB) before calling this function.
func createEncryptionKey(keyPath string) ([]byte, error) {
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("config: generate encryption key: %w", err)
	}

	// Write key to a unique temp file first (fully written before visible at keyPath).
	tmpFile, err := os.CreateTemp(filepath.Dir(keyPath), ".secrets.key.tmp.*")
	if err != nil {
		return nil, fmt.Errorf("config: create encryption key temp: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(key); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("config: write encryption key temp: %w", err)
	}
	if err := tmpFile.Chmod(0o600); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("config: chmod encryption key temp: %w", err)
	}
	tmpFile.Close()

	// Atomic link: creates keyPath pointing to the fully-written temp file.
	// Fails with EEXIST if another process/goroutine already created keyPath.
	if err := os.Link(tmpPath, keyPath); err != nil {
		os.Remove(tmpPath)
		if os.IsExist(err) {
			// Another process won the race — read the key it created.
			return loadEncryptionKey(keyPath)
		}
		return nil, fmt.Errorf("config: link encryption key: %w", err)
	}
	os.Remove(tmpPath)

	return key, nil
}

// encryptionKeyPath returns the path for the encryption key relative to the DB.
func encryptionKeyPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), keyFileName)
}

// hasEncryptedValues checks whether the security_settings table contains any
// values with the enc:v1: prefix. Used to prevent creating a new encryption
// key when existing encrypted data would become permanently unreadable.
func hasEncryptedValues(ctx context.Context, db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM security_settings WHERE value LIKE ?`,
		encPrefix+"%",
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("config: check encrypted values: %w", err)
	}
	return count > 0, nil
}

// migrateEncryptPlaintext ensures every row in security_settings is properly
// encrypted. Called during Open() in RW mode.
//
// For each row the function decides what to do:
//   - No enc:v1: prefix → plaintext, encrypt it.
//   - Has enc:v1: prefix AND decrypts successfully → already migrated, skip.
//   - Has enc:v1: prefix but decryption fails → the raw string is treated as
//     plaintext that coincidentally starts with enc:v1: (prefix collision);
//     encrypt the entire raw value.
//
// Returns the number of rows migrated.
func migrateEncryptPlaintext(ctx context.Context, db *sql.DB, key []byte) (int, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT rowid, value FROM security_settings`,
	)
	if err != nil {
		return 0, fmt.Errorf("config: query secrets for migration: %w", err)
	}
	defer rows.Close()

	type pendingUpdate struct {
		rowid int64
		enc   string
	}
	var updates []pendingUpdate

	for rows.Next() {
		var rowid int64
		var raw string
		if err := rows.Scan(&rowid, &raw); err != nil {
			return 0, fmt.Errorf("config: scan secret for migration: %w", err)
		}

		if strings.HasPrefix(raw, encPrefix) {
			// Try to decrypt — if it succeeds the value is already properly
			// encrypted and we leave it alone.
			if _, err := decryptValue(key, raw); err == nil {
				continue
			}
			// Decryption failed: the raw string is plaintext that happens to
			// start with the enc:v1: prefix. Fall through to encrypt it.
		}

		encrypted, err := encryptValue(key, raw)
		if err != nil {
			return 0, fmt.Errorf("config: encrypt during migration: %w", err)
		}
		updates = append(updates, pendingUpdate{rowid: rowid, enc: encrypted})
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("config: iterate secrets for migration: %w", err)
	}

	if len(updates) == 0 {
		return 0, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("config: begin migration tx: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`UPDATE security_settings SET value = ?, updated_at = CURRENT_TIMESTAMP WHERE rowid = ?`,
	)
	if err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("config: prepare migration update: %w", err)
	}
	defer stmt.Close()

	for _, u := range updates {
		if _, err := stmt.ExecContext(ctx, u.enc, u.rowid); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("config: update row %d during migration: %w", u.rowid, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("config: commit migration tx: %w", err)
	}

	return len(updates), nil
}

// encryptValue encrypts plaintext using AES-256-GCM and returns a prefixed base64 string.
func encryptValue(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decryptValue decrypts an encrypted value. The value must have the enc:v1:
// prefix; values without it are rejected as invalid (plaintext values should
// have been migrated during Open).
func decryptValue(key []byte, stored string) (string, error) {
	if !strings.HasPrefix(stored, encPrefix) {
		return "", fmt.Errorf("config: value is not encrypted (missing %s prefix)", encPrefix)
	}

	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", fmt.Errorf("config: decode encrypted value: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("config: encrypted value too short")
	}

	plaintext, err := gcm.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("config: decrypt value: %w", err)
	}

	return string(plaintext), nil
}
