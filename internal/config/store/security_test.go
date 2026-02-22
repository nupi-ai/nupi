package store

import (
	"bytes"
	"context"
	"database/sql"
	"log"
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
	if !strings.Contains(err.Error(), "secret backend not available") {
		t.Fatalf("expected 'secret backend not available' error, got: %v", err)
	}
}

func TestSaveSecuritySettingsNilBackendGuard(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "config.db")
	ctx := context.Background()

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Simulate a regression where secretBackend becomes nil after Open.
	store.secretBackend = nil

	err = store.SaveSecuritySettings(ctx, map[string]string{"api_key": "secret"})
	if err == nil {
		t.Fatal("expected error when saving with nil secret backend")
	}
	if !strings.Contains(err.Error(), "secret backend not available") {
		t.Fatalf("expected 'secret backend not available' error, got: %v", err)
	}
}

func TestSaveSecuritySettingsReadOnlyGuard(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "config.db")

	// Create schema via RW open, then close.
	s, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	s.Close()

	// Open read-only.
	roStore, err := Open(Options{DBPath: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatalf("open RO: %v", err)
	}
	defer roStore.Close()

	ctx := context.Background()
	err = roStore.SaveSecuritySettings(ctx, map[string]string{"api_key": "secret"})
	if err == nil {
		t.Fatal("expected error when saving in read-only mode")
	}
	if !strings.Contains(err.Error(), "store opened read-only") {
		t.Fatalf("expected 'store opened read-only' error, got: %v", err)
	}
}

func TestSaveSecuritySettingsEmptyMap(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "config.db")
	s, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// Empty map should return nil immediately (no backend call).
	if err := s.SaveSecuritySettings(ctx, map[string]string{}); err != nil {
		t.Fatalf("SaveSecuritySettings empty map: %v", err)
	}
}

// TestNupiKeychainEnvOff does not call t.Parallel() because t.Setenv
// panics when used with parallel tests.
func TestNupiKeychainEnvOff(t *testing.T) {
	t.Setenv("NUPI_KEYCHAIN", "off")
	kb, avail := defaultKeychainProber("default", "default")
	if kb != nil {
		t.Fatal("expected nil backend for NUPI_KEYCHAIN=off")
	}
	if avail {
		t.Fatal("expected false availability for NUPI_KEYCHAIN=off")
	}
}

// TestNupiKeychainEnvForce does not call t.Parallel() because t.Setenv
// panics when used with parallel tests.
func TestNupiKeychainEnvForce(t *testing.T) {
	t.Setenv("NUPI_KEYCHAIN", "force")
	kb, avail := defaultKeychainProber("default", "default")
	if kb == nil {
		t.Fatal("expected non-nil backend for NUPI_KEYCHAIN=force")
	}
	if !avail {
		t.Fatal("expected true availability for NUPI_KEYCHAIN=force")
	}
	// Available() must return true unconditionally (force mode bypasses platform check).
	if !kb.Available() {
		t.Fatal("expected Available() true for NUPI_KEYCHAIN=force — force mode should bypass platform check")
	}
}

// TestNupiKeychainEnvUnrecognized does not call t.Parallel() because
// t.Setenv panics when used with parallel tests.
func TestNupiKeychainEnvUnrecognized(t *testing.T) {
	t.Setenv("NUPI_KEYCHAIN", "yes")

	// Capture log output to verify the warning is emitted.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// Unrecognized value should log a warning and fall back to auto-detect.
	// The auto-detect path creates a real KeychainBackend and probes the OS
	// keychain (up to 3s timeout on macOS/Linux). This is acceptable because
	// the test's primary purpose is verifying the warning log, and the probe
	// has a bounded timeout that prevents indefinite hangs.
	_, _ = defaultKeychainProber("default", "default")

	if !strings.Contains(buf.String(), "unrecognized NUPI_KEYCHAIN") {
		t.Fatalf("expected warning log for unrecognized NUPI_KEYCHAIN, got log output: %q", buf.String())
	}
}

// mockKeychainForStore is a simple in-memory SecretBackend for testing the
// Store → FallbackBackend → keychain integration path.
type mockKeychainForStore struct {
	data map[string]string
}

func newMockKeychainForStore() *mockKeychainForStore {
	return &mockKeychainForStore{data: make(map[string]string)}
}

func (m *mockKeychainForStore) Set(_ context.Context, key, value string) error {
	m.data[key] = value
	return nil
}

func (m *mockKeychainForStore) SetBatch(_ context.Context, values map[string]string) error {
	for k, v := range values {
		m.data[k] = v
	}
	return nil
}

func (m *mockKeychainForStore) Get(_ context.Context, key string) (string, error) {
	v, ok := m.data[key]
	if !ok {
		return "", storecrypto.ErrSecretNotFound
	}
	return v, nil
}

func (m *mockKeychainForStore) GetBatch(_ context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := m.data[k]; ok {
			result[k] = v
		}
	}
	return result, nil
}

func (m *mockKeychainForStore) Delete(_ context.Context, key string) error {
	if _, ok := m.data[key]; !ok {
		return storecrypto.ErrSecretNotFound
	}
	delete(m.data, key)
	return nil
}

func (m *mockKeychainForStore) Available() bool { return true }
func (m *mockKeychainForStore) Name() string    { return "mock-keychain" }

// TestOpenWithMockKeychainDualWrites does not call t.Parallel() because
// it overrides the package-level keychain prober, which would race with
// other tests calling store.Open() concurrently.
func TestOpenWithMockKeychainDualWrites(t *testing.T) {
	mock := newMockKeychainForStore()
	cleanup := setKeychainProber(func(_, _ string) (storecrypto.SecretBackend, bool) {
		return mock, true
	})
	t.Cleanup(cleanup)

	dbPath := filepath.Join(t.TempDir(), "config.db")
	s, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// Save secrets — should dual-write to mock keychain + AES.
	secrets := map[string]string{
		"api_key":       "sk-test-123",
		"pairing_token": "pair-abc",
	}
	if err := s.SaveSecuritySettings(ctx, secrets); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Verify secrets are in mock keychain (plaintext, not AES-encrypted).
	for k, v := range secrets {
		if mock.data[k] != v {
			t.Fatalf("keychain %q: expected %q, got %q", k, v, mock.data[k])
		}
	}

	// Verify secrets are also in SQLite (AES-encrypted, not plaintext).
	for k, plaintext := range secrets {
		var rawValue string
		err = s.db.QueryRowContext(ctx,
			`SELECT value FROM security_settings WHERE key = ? AND instance_name = ? AND profile_name = ?`,
			k, s.instanceName, s.profileName,
		).Scan(&rawValue)
		if err != nil {
			t.Fatalf("raw query %q: %v", k, err)
		}
		if rawValue == plaintext {
			t.Fatalf("expected AES-encrypted value for %q in SQLite, got plaintext", k)
		}
	}

	// Load via Store — should read from keychain (primary).
	loaded, err := s.LoadSecuritySettings(ctx, "api_key", "pairing_token")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for k, v := range secrets {
		if loaded[k] != v {
			t.Fatalf("loaded %q: expected %q, got %q", k, v, loaded[k])
		}
	}

	// Delete from keychain mock only — Load should fall back to AES.
	delete(mock.data, "api_key")
	loadedFallback, err := s.LoadSecuritySettings(ctx, "api_key")
	if err != nil {
		t.Fatalf("load fallback: %v", err)
	}
	if loadedFallback["api_key"] != "sk-test-123" {
		t.Fatalf("fallback: expected sk-test-123, got %q", loadedFallback["api_key"])
	}
}

// TestReadOnlyKeychainOnlyFallback does not call t.Parallel() because
// it overrides the package-level keychain prober.
func TestReadOnlyKeychainOnlyFallback(t *testing.T) {
	mock := newMockKeychainForStore()
	mock.data["api_key"] = "keychain-secret"

	cleanup := setKeychainProber(func(_, _ string) (storecrypto.SecretBackend, bool) {
		return mock, true
	})
	t.Cleanup(cleanup)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "config.db")

	// Create schema and encryption key via RW open.
	s, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	s.Close()

	// Delete encryption key to simulate key loss.
	os.Remove(storecrypto.KeyPath(dbPath))

	// Open read-only — should use keychain-only backend.
	roStore, err := Open(Options{DBPath: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatalf("open RO: %v", err)
	}
	defer roStore.Close()

	ctx := context.Background()

	// Load specific key — should come from keychain.
	loaded, err := roStore.LoadSecuritySettings(ctx, "api_key")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded["api_key"] != "keychain-secret" {
		t.Fatalf("expected keychain-secret, got %q", loaded["api_key"])
	}
}

// TestReadOnlyKeychainOnlyLoadAll tests the zero-keys path of
// LoadSecuritySettings with a keychain-only backend. The DB has key
// names from a previous session, but the keychain may not have all
// values, resulting in a partial result with a warning log.
func TestReadOnlyKeychainOnlyLoadAll(t *testing.T) {
	mock := newMockKeychainForStore()
	// Keychain has only one of the two keys.
	mock.data["api_key"] = "keychain-secret"

	cleanup := setKeychainProber(func(_, _ string) (storecrypto.SecretBackend, bool) {
		return mock, true
	})
	t.Cleanup(cleanup)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "config.db")
	ctx := context.Background()

	// Create schema, key, and seed two secrets via RW store.
	s, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := s.SaveSecuritySettings(ctx, map[string]string{
		"api_key":       "rw-secret-1",
		"pairing_token": "rw-secret-2",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	s.Close()

	// Delete encryption key to force keychain-only mode.
	os.Remove(storecrypto.KeyPath(dbPath))

	// Clear mock — simulate keychain that only has one key.
	mock.data = map[string]string{"api_key": "keychain-secret"}

	// Open read-only — should use keychain-only backend.
	roStore, err := Open(Options{DBPath: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatalf("open RO: %v", err)
	}
	defer roStore.Close()

	// Load ALL keys (zero-keys path → loadAllSecuritySettings).
	// DB enumerates 2 keys, keychain only has 1 → partial result + warning.
	loaded, err := roStore.LoadSecuritySettings(ctx)
	if err != nil {
		t.Fatalf("load all: %v", err)
	}
	if loaded["api_key"] != "keychain-secret" {
		t.Fatalf("expected keychain-secret, got %q", loaded["api_key"])
	}
	if _, ok := loaded["pairing_token"]; ok {
		t.Fatal("expected pairing_token to be missing from keychain-only backend")
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 result (partial), got %d: %v", len(loaded), loaded)
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
