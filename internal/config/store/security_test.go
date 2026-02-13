package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	storecrypto "github.com/nupi-ai/nupi/internal/config/store/crypto"
)

func TestSecuritySettingsPersistence(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "config.db")
	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()

	// Save multiple secrets.
	secrets := map[string]string{
		"elevenlabs_api_key": "sk-test123",
		"openai_api_key":     "sk-openai-456",
	}
	if err := store.SaveSecuritySettings(ctx, secrets); err != nil {
		t.Fatalf("save security settings: %v", err)
	}

	// Load all secrets.
	loaded, err := store.LoadSecuritySettings(ctx)
	if err != nil {
		t.Fatalf("load security settings: %v", err)
	}
	if loaded["elevenlabs_api_key"] != "sk-test123" {
		t.Fatalf("expected elevenlabs_api_key=sk-test123, got %q", loaded["elevenlabs_api_key"])
	}
	if loaded["openai_api_key"] != "sk-openai-456" {
		t.Fatalf("expected openai_api_key=sk-openai-456, got %q", loaded["openai_api_key"])
	}

	// Load with key filter.
	filtered, err := store.LoadSecuritySettings(ctx, "elevenlabs_api_key")
	if err != nil {
		t.Fatalf("load filtered: %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("expected 1 filtered result, got %d", len(filtered))
	}
	if filtered["elevenlabs_api_key"] != "sk-test123" {
		t.Fatalf("filtered value mismatch: %q", filtered["elevenlabs_api_key"])
	}
}

func TestSecuritySettingsIsolatedByProfile(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "config.db")

	// Open store with profile A (default).
	storeA, err := Open(Options{DBPath: dbPath, ProfileName: "profile-a"})
	if err != nil {
		t.Fatalf("open store A: %v", err)
	}

	ctx := context.Background()

	if err := storeA.SaveSecuritySettings(ctx, map[string]string{
		"api_key": "secret-for-a",
	}); err != nil {
		t.Fatalf("save secret A: %v", err)
	}
	if err := storeA.Close(); err != nil {
		t.Fatalf("close store A before reopen: %v", err)
	}

	// Open store with profile B.
	storeB, err := Open(Options{DBPath: dbPath, ProfileName: "profile-b"})
	if err != nil {
		t.Fatalf("open store B: %v", err)
	}
	t.Cleanup(func() { storeB.Close() })

	loadedB, err := storeB.LoadSecuritySettings(ctx, "api_key")
	if err != nil {
		t.Fatalf("load from profile B: %v", err)
	}
	if val, ok := loadedB["api_key"]; ok {
		t.Fatalf("expected api_key to NOT be visible from profile B, got %q", val)
	}

	// Verify profile A still has it.
	storeA2, err := Open(Options{DBPath: dbPath, ProfileName: "profile-a"})
	if err != nil {
		t.Fatalf("reopen store A: %v", err)
	}
	t.Cleanup(func() { storeA2.Close() })

	loadedA, err := storeA2.LoadSecuritySettings(ctx, "api_key")
	if err != nil {
		t.Fatalf("load from profile A: %v", err)
	}
	if loadedA["api_key"] != "secret-for-a" {
		t.Fatalf("expected profile A secret, got %q", loadedA["api_key"])
	}
}

func TestSecuritySettingsEncryptedAtRest(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "config.db")
	ctx := context.Background()

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	secret := "super-secret-api-key-xyz"
	if err := store.SaveSecuritySettings(ctx, map[string]string{
		"test_key": secret,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	store.Close()

	// Query the database directly to verify the value is encrypted on disk.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()

	var rawValue string
	err = db.QueryRow(`SELECT value FROM security_settings WHERE key = 'test_key'`).Scan(&rawValue)
	if err != nil {
		t.Fatalf("raw query: %v", err)
	}

	// The raw value must NOT be the plaintext secret.
	if rawValue == secret {
		t.Fatal("secret is stored as plaintext in database — encryption is not working")
	}

	// The raw value must have the encryption prefix.
	if !strings.HasPrefix(rawValue, storecrypto.EncPrefix) {
		t.Fatalf("expected encrypted value with %q prefix, got %q", storecrypto.EncPrefix, rawValue[:20])
	}
}

func TestOpenRWFailsWhenKeyMissingButEncryptedValuesExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "config.db")
	ctx := context.Background()

	// Create a store, save an encrypted secret.
	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.SaveSecuritySettings(ctx, map[string]string{
		"api_key": "precious-secret",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	store.Close()

	// Delete the encryption key to simulate key loss.
	keyPath := storecrypto.KeyPath(dbPath)
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("remove key: %v", err)
	}

	// Re-open in RW mode — must fail (not silently create a new key).
	_, err = Open(Options{DBPath: dbPath})
	if err == nil {
		t.Fatal("expected Open to fail when key is missing but DB has encrypted values")
	}
	if !strings.Contains(err.Error(), "refusing to create a new key") {
		t.Fatalf("expected 'refusing to create a new key' error, got: %v", err)
	}
}

func TestOpenRWCreatesKeyWhenNoEncryptedValues(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "config.db")

	// Open fresh store — no encrypted values in DB, key creation should succeed.
	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	store.Close()

	// Key file must exist.
	keyPath := storecrypto.KeyPath(dbPath)
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("expected key file to exist: %v", err)
	}

	// Delete key and reopen — still no encrypted values, so should succeed.
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("remove key: %v", err)
	}
	store2, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("reopen without key (no encrypted values): %v", err)
	}
	store2.Close()
}

func TestOpenRWMigratesPlaintextToEncrypted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "config.db")
	ctx := context.Background()

	// Open store to create schema and encryption key.
	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	store.Close()

	// Manually insert a plaintext (non-encrypted) secret directly into DB,
	// simulating a legacy value from before encryption was introduced.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO security_settings (instance_name, profile_name, key, value) VALUES (?, ?, ?, ?)`,
		"default", "default", "legacy_key", "plaintext-secret-value",
	)
	if err != nil {
		t.Fatalf("insert plaintext: %v", err)
	}
	db.Close()

	// Re-open the store — migration should encrypt the plaintext value.
	store2, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()

	// Verify value is readable via the normal API.
	loaded, err := store2.LoadSecuritySettings(ctx, "legacy_key")
	if err != nil {
		t.Fatalf("load migrated secret: %v", err)
	}
	if loaded["legacy_key"] != "plaintext-secret-value" {
		t.Fatalf("expected migrated value 'plaintext-secret-value', got %q", loaded["legacy_key"])
	}

	// Verify raw DB value is now encrypted (has enc:v1: prefix).
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db for verification: %v", err)
	}
	defer rawDB.Close()

	var rawValue string
	err = rawDB.QueryRow(
		`SELECT value FROM security_settings WHERE key = 'legacy_key' AND instance_name = 'default' AND profile_name = 'default'`,
	).Scan(&rawValue)
	if err != nil {
		t.Fatalf("raw query: %v", err)
	}
	if !strings.HasPrefix(rawValue, storecrypto.EncPrefix) {
		t.Fatalf("expected migrated value to have %q prefix, got %q", storecrypto.EncPrefix, rawValue[:min(20, len(rawValue))])
	}
	if rawValue == "plaintext-secret-value" {
		t.Fatal("migration did not encrypt the plaintext value")
	}
}

func TestOpenRWMigratesPrefixCollisionCorrectly(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "config.db")
	ctx := context.Background()

	// Open store to create schema and encryption key.
	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	store.Close()

	// Manually insert a plaintext value that starts with the enc:v1: prefix,
	// simulating a prefix collision. The migration must detect that this does
	// not decrypt successfully and re-encrypt the entire raw string.
	collisionValue := storecrypto.EncPrefix + "this-is-not-actually-encrypted"
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO security_settings (instance_name, profile_name, key, value) VALUES (?, ?, ?, ?)`,
		"default", "default", "collision_key", collisionValue,
	)
	if err != nil {
		t.Fatalf("insert collision value: %v", err)
	}
	db.Close()

	// Re-open the store — migration should detect the collision and encrypt it.
	store2, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()

	// The decrypted value must be the original raw string (including the prefix).
	loaded, err := store2.LoadSecuritySettings(ctx, "collision_key")
	if err != nil {
		t.Fatalf("load migrated collision value: %v", err)
	}
	if loaded["collision_key"] != collisionValue {
		t.Fatalf("expected %q, got %q", collisionValue, loaded["collision_key"])
	}

	// Verify raw DB value is now properly encrypted (double-prefixed).
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db for verification: %v", err)
	}
	defer rawDB.Close()

	var rawValue string
	err = rawDB.QueryRow(
		`SELECT value FROM security_settings WHERE key = 'collision_key' AND instance_name = 'default' AND profile_name = 'default'`,
	).Scan(&rawValue)
	if err != nil {
		t.Fatalf("raw query: %v", err)
	}
	if !strings.HasPrefix(rawValue, storecrypto.EncPrefix) {
		t.Fatalf("expected encrypted value with %q prefix, got %q", storecrypto.EncPrefix, rawValue[:min(20, len(rawValue))])
	}
	if rawValue == collisionValue {
		t.Fatal("migration did not re-encrypt the collision value")
	}
}

func TestSecuritySettingsReadOnlyWithMissingKeyReturnsError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "config.db")
	ctx := context.Background()

	// First, save an encrypted secret with a RW store.
	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open RW store: %v", err)
	}
	if err := store.SaveSecuritySettings(ctx, map[string]string{
		"api_key": "encrypted-secret",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	store.Close()

	// Delete the encryption key file to simulate missing key.
	keyPath := storecrypto.KeyPath(dbPath)
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("remove key file: %v", err)
	}

	// Open in read-only mode — should succeed (key missing is tolerated).
	roStore, err := Open(Options{DBPath: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatalf("open RO store: %v", err)
	}
	defer roStore.Close()

	// Loading encrypted secrets without key must return an error, not garbled data.
	_, err = roStore.LoadSecuritySettings(ctx, "api_key")
	if err == nil {
		t.Fatal("expected error when loading encrypted value without key")
	}
	if !strings.Contains(err.Error(), "no decryption key") {
		t.Fatalf("expected 'no decryption key' error, got: %v", err)
	}
}

func TestSecuritySettingsReadOnlyWithKeyDecryptsCorrectly(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "config.db")
	ctx := context.Background()

	// Save encrypted secret with RW store.
	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open RW store: %v", err)
	}
	if err := store.SaveSecuritySettings(ctx, map[string]string{
		"api_key": "ro-test-secret",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	store.Close()

	// Open in read-only mode — key file exists, so decryption should work.
	roStore, err := Open(Options{DBPath: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatalf("open RO store: %v", err)
	}
	defer roStore.Close()

	loaded, err := roStore.LoadSecuritySettings(ctx, "api_key")
	if err != nil {
		t.Fatalf("load from RO store: %v", err)
	}
	if loaded["api_key"] != "ro-test-secret" {
		t.Fatalf("expected ro-test-secret, got %q", loaded["api_key"])
	}
}
