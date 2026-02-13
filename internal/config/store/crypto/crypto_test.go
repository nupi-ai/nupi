package crypto_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	storecrypto "github.com/nupi-ai/nupi/internal/config/store/crypto"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Parallel()

	key := make([]byte, storecrypto.KeySize)
	for i := range key {
		key[i] = byte(i)
	}

	plaintext := "my-secret-api-key-12345"
	encrypted, err := storecrypto.EncryptValue(key, plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// Encrypted value must have the prefix.
	if len(encrypted) < len(storecrypto.EncPrefix) || encrypted[:len(storecrypto.EncPrefix)] != storecrypto.EncPrefix {
		t.Fatalf("expected enc:v1: prefix, got %q", encrypted[:20])
	}

	// Encrypted value must differ from plaintext.
	if encrypted == plaintext {
		t.Fatal("encrypted value must differ from plaintext")
	}

	decrypted, err := storecrypto.DecryptValue(key, encrypted)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if decrypted != plaintext {
		t.Fatalf("expected %q, got %q", plaintext, decrypted)
	}
}

func TestDecryptPlaintextRejectsUnprefixedValue(t *testing.T) {
	t.Parallel()

	key := make([]byte, storecrypto.KeySize)
	// Plaintext value without enc:v1: prefix must be rejected — all values
	// should have been migrated to encrypted form during Open().
	_, err := storecrypto.DecryptValue(key, "legacy-plaintext-secret")
	if err == nil {
		t.Fatal("expected error for plaintext value without encryption prefix")
	}
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	t.Parallel()

	keyA := make([]byte, storecrypto.KeySize)
	keyB := make([]byte, storecrypto.KeySize)
	for i := range keyB {
		keyB[i] = 0xFF
	}

	encrypted, err := storecrypto.EncryptValue(keyA, "secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	_, err = storecrypto.DecryptValue(keyB, encrypted)
	if err == nil {
		t.Fatal("expected decryption with wrong key to fail")
	}
}

func TestCreateEncryptionKeyCreatesFile(t *testing.T) {
	t.Parallel()

	keyPath := filepath.Join(t.TempDir(), ".secrets.key")

	key, err := storecrypto.CreateKey(keyPath)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if len(key) != storecrypto.KeySize {
		t.Fatalf("expected key size %d, got %d", storecrypto.KeySize, len(key))
	}

	// File must exist with correct permissions.
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 permissions, got %o", info.Mode().Perm())
	}

	// Load must return the same key.
	key2, err := storecrypto.LoadKey(keyPath)
	if err != nil {
		t.Fatalf("load existing key: %v", err)
	}
	if string(key) != string(key2) {
		t.Fatal("expected identical key on load")
	}
}

func TestLoadEncryptionKeyMissingFileReturnsNil(t *testing.T) {
	t.Parallel()

	keyPath := filepath.Join(t.TempDir(), "nonexistent.key")
	key, err := storecrypto.LoadKey(keyPath)
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if key != nil {
		t.Fatal("expected nil key for missing file")
	}
}

func TestLoadEncryptionKeyCorruptFile(t *testing.T) {
	t.Parallel()

	keyPath := filepath.Join(t.TempDir(), ".secrets.key")
	// Write a file with wrong size (not 32 bytes).
	if err := os.WriteFile(keyPath, []byte("too-short"), 0o600); err != nil {
		t.Fatalf("write corrupt key: %v", err)
	}

	_, err := storecrypto.LoadKey(keyPath)
	if err == nil {
		t.Fatal("expected error for corrupt key file")
	}
}

func TestCreateEncryptionKeyConcurrentRace(t *testing.T) {
	t.Parallel()

	keyPath := filepath.Join(t.TempDir(), ".secrets.key")
	const goroutines = 10

	var (
		wg   sync.WaitGroup
		keys [goroutines][]byte
		errs [goroutines]error
	)

	// Launch multiple goroutines that all try to create the key simultaneously.
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			keys[idx], errs[idx] = storecrypto.CreateKey(keyPath)
		}(i)
	}
	wg.Wait()

	// All must succeed and return the SAME key (the winner's key).
	var referenceKey []byte
	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d failed: %v", i, errs[i])
		}
		if len(keys[i]) != storecrypto.KeySize {
			t.Fatalf("goroutine %d returned key with size %d", i, len(keys[i]))
		}
		if referenceKey == nil {
			referenceKey = keys[i]
		} else if string(keys[i]) != string(referenceKey) {
			t.Fatalf("goroutine %d returned a different key — atomic link race protection failed", i)
		}
	}
}
