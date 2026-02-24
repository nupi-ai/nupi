package crypto

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS security_settings (
		instance_name TEXT NOT NULL,
		profile_name TEXT NOT NULL,
		key TEXT NOT NULL,
		value TEXT NOT NULL,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (instance_name, profile_name, key)
	)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func makeTestKey() []byte {
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func TestIntegrationKeychainBackendStoresAndRetrieves(t *testing.T) {
	t.Parallel()
	mock := newMockKeyring()
	kb := newKeychainBackendWithProvider("default", "default", mock)
	ctx := context.Background()

	secrets := map[string]string{
		"elevenlabs_api_key": "sk-el-test",
		"openai_api_key":     "sk-oai-test",
		"pairing_token":      "pair-abc-123",
	}

	for k, v := range secrets {
		if err := kb.Set(ctx, k, v); err != nil {
			t.Fatalf("Set %q: %v", k, err)
		}
	}

	for k, v := range secrets {
		got, err := kb.Get(ctx, k)
		if err != nil {
			t.Fatalf("Get %q: %v", k, err)
		}
		if got != v {
			t.Fatalf("Get %q: expected %q, got %q", k, v, got)
		}
	}
}

func TestIntegrationFallbackKeychainUnavailableUsesAES(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	encKey := makeTestKey()
	ctx := context.Background()

	aesBackend := NewAESBackend(db, encKey, "default", "default")
	unavailableKb := newKeychainBackendWithProvider("default", "default", &failingKeyring{err: errKeychainLocked})
	fb := NewFallbackBackend(unavailableKb, aesBackend)

	if err := fb.Set(ctx, "api_key", "secret-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	var rawValue string
	err := db.QueryRow(
		`SELECT ` + securitySettingsValueColumn + ` FROM security_settings WHERE key = 'api_key'`,
	).Scan(&rawValue)
	if err != nil {
		t.Fatalf("raw query: %v", err)
	}
	if rawValue == "secret-123" {
		t.Fatal("value stored as plaintext — encryption not working")
	}

	got, err := fb.Get(ctx, "api_key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "secret-123" {
		t.Fatalf("expected secret-123, got %q", got)
	}
}

var errKeychainLocked = &keychainLockedError{}

type keychainLockedError struct{}

func (e *keychainLockedError) Error() string { return "keychain locked" }

func TestIntegrationFallbackKeychainAvailableStoresInKeychain(t *testing.T) {
	t.Parallel()
	mock := newMockKeyring()

	db := setupTestDB(t)
	encKey := makeTestKey()
	ctx := context.Background()

	aesBackend := NewAESBackend(db, encKey, "default", "default")
	keychainBackend := newKeychainBackendWithProvider("default", "default", mock)
	fb := NewFallbackBackend(keychainBackend, aesBackend)

	if err := fb.Set(ctx, "api_key", "keychain-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Verify value is in keychain.
	val, err := keychainBackend.Get(ctx, "api_key")
	if err != nil {
		t.Fatalf("keychain Get: %v", err)
	}
	if val != "keychain-secret" {
		t.Fatalf("keychain: expected keychain-secret, got %q", val)
	}

	// Verify value is ALSO in SQLite (dual-write for enumeration).
	var rawValue string
	err = db.QueryRow(
		`SELECT ` + securitySettingsValueColumn + ` FROM security_settings WHERE key = 'api_key'`,
	).Scan(&rawValue)
	if err != nil {
		t.Fatalf("raw query: %v", err)
	}
	if rawValue == "keychain-secret" {
		t.Fatal("SQLite has plaintext — should be AES-encrypted")
	}

	// Read back via fallback — should come from keychain (primary).
	got, err := fb.Get(ctx, "api_key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "keychain-secret" {
		t.Fatalf("expected keychain-secret, got %q", got)
	}
}

func TestIntegrationProfileIsolationInKeychain(t *testing.T) {
	t.Parallel()
	mock := newMockKeyring()
	ctx := context.Background()

	kbA := newKeychainBackendWithProvider("default", "profile-a", mock)
	kbB := newKeychainBackendWithProvider("default", "profile-b", mock)

	if err := kbA.Set(ctx, "token", "token-a"); err != nil {
		t.Fatalf("Set A: %v", err)
	}
	if err := kbB.Set(ctx, "token", "token-b"); err != nil {
		t.Fatalf("Set B: %v", err)
	}

	gotA, _ := kbA.Get(ctx, "token")
	if gotA != "token-a" {
		t.Fatalf("expected token-a, got %q", gotA)
	}

	gotB, _ := kbB.Get(ctx, "token")
	if gotB != "token-b" {
		t.Fatalf("expected token-b, got %q", gotB)
	}
}

func TestIntegrationReadOnlyFallback(t *testing.T) {
	t.Parallel()
	mock := newMockKeyring()

	db := setupTestDB(t)
	encKey := makeTestKey()
	ctx := context.Background()

	kb := newKeychainBackendWithProvider("default", "default", mock)
	if err := kb.Set(ctx, "api_key", "keychain-value"); err != nil {
		t.Fatalf("keychain Set: %v", err)
	}

	enc, _ := EncryptValue(encKey, "aes-only-value")
	_, _ = db.ExecContext(ctx,
		`INSERT INTO security_settings (instance_name, profile_name, key, value) VALUES (?, ?, ?, ?)`,
		"default", "default", "other_key", enc,
	)

	aesBackend := NewAESBackend(db, encKey, "default", "default")
	fb := NewFallbackBackend(kb, aesBackend)

	got, err := fb.Get(ctx, "api_key")
	if err != nil {
		t.Fatalf("Get keychain key: %v", err)
	}
	if got != "keychain-value" {
		t.Fatalf("expected keychain-value, got %q", got)
	}

	got2, err := fb.Get(ctx, "other_key")
	if err != nil {
		t.Fatalf("Get AES key: %v", err)
	}
	if got2 != "aes-only-value" {
		t.Fatalf("expected aes-only-value, got %q", got2)
	}
}
