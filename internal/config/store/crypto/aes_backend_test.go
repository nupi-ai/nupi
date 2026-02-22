package crypto

import (
	"context"
	"errors"
	"testing"
)

func newTestAESBackend(t *testing.T) *AESBackend {
	t.Helper()
	return NewAESBackend(setupTestDB(t), makeTestKey(), "default", "default")
}

func TestAESBackendSetAndGet(t *testing.T) {
	t.Parallel()
	ab := newTestAESBackend(t)
	ctx := context.Background()

	if err := ab.Set(ctx, "api_key", "sk-test-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	val, err := ab.Get(ctx, "api_key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "sk-test-123" {
		t.Fatalf("expected sk-test-123, got %q", val)
	}
}

func TestAESBackendGetNotFound(t *testing.T) {
	t.Parallel()
	ab := newTestAESBackend(t)
	ctx := context.Background()

	_, err := ab.Get(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent key")
	}
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got: %v", err)
	}
}

func TestAESBackendDelete(t *testing.T) {
	t.Parallel()
	ab := newTestAESBackend(t)
	ctx := context.Background()

	if err := ab.Set(ctx, "api_key", "to-delete"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := ab.Delete(ctx, "api_key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := ab.Get(ctx, "api_key")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound after delete, got: %v", err)
	}
}

func TestAESBackendDeleteNotFound(t *testing.T) {
	t.Parallel()
	ab := newTestAESBackend(t)
	ctx := context.Background()

	err := ab.Delete(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error deleting nonexistent key")
	}
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got: %v", err)
	}
}

func TestAESBackendSetBatchAndGetBatch(t *testing.T) {
	t.Parallel()
	ab := newTestAESBackend(t)
	ctx := context.Background()

	values := map[string]string{
		"key1": "val1",
		"key2": "val2",
		"key3": "val3",
	}
	if err := ab.SetBatch(ctx, values); err != nil {
		t.Fatalf("SetBatch: %v", err)
	}

	got, err := ab.GetBatch(ctx, []string{"key1", "key2", "key3"})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	for k, v := range values {
		if got[k] != v {
			t.Fatalf("GetBatch %q: expected %q, got %q", k, v, got[k])
		}
	}
}

func TestAESBackendSetBatchEmpty(t *testing.T) {
	t.Parallel()
	ab := newTestAESBackend(t)
	ctx := context.Background()

	// Empty map should return nil immediately (no transaction created).
	if err := ab.SetBatch(ctx, map[string]string{}); err != nil {
		t.Fatalf("SetBatch empty: %v", err)
	}
}

func TestAESBackendGetBatchEmpty(t *testing.T) {
	t.Parallel()
	ab := newTestAESBackend(t)
	ctx := context.Background()

	got, err := ab.GetBatch(ctx, []string{})
	if err != nil {
		t.Fatalf("GetBatch empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}

func TestAESBackendGetBatchSkipsMissingKeys(t *testing.T) {
	t.Parallel()
	ab := newTestAESBackend(t)
	ctx := context.Background()

	if err := ab.Set(ctx, "exists", "val"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := ab.GetBatch(ctx, []string{"exists", "missing1", "missing2"})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d: %v", len(got), got)
	}
	if got["exists"] != "val" {
		t.Fatalf("expected val, got %q", got["exists"])
	}
}

func TestAESBackendSetBatchEncryptionFailureRollsBack(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	key := makeTestKey()
	ctx := context.Background()

	// Nil encryption key makes EncryptValue fail immediately — no SQL
	// Exec is ever reached. This verifies the pre-Exec failure path.
	nilKeyAB := NewAESBackend(db, nil, "default", "default")

	err := nilKeyAB.SetBatch(ctx, map[string]string{"k1": "v1", "k2": "v2"})
	if err == nil {
		t.Fatal("expected error with nil encryption key")
	}

	var count int
	if qErr := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM security_settings`).Scan(&count); qErr != nil {
		t.Fatalf("count query: %v", qErr)
	}
	if count != 0 {
		t.Fatalf("expected 0 rows after failed SetBatch, got %d", count)
	}

	// Verify that a successful SetBatch followed by a failed one
	// does not corrupt earlier data.
	goodAB := NewAESBackend(db, key, "default", "default")
	if err := goodAB.SetBatch(ctx, map[string]string{"existing": "safe"}); err != nil {
		t.Fatalf("seed SetBatch: %v", err)
	}

	err = nilKeyAB.SetBatch(ctx, map[string]string{"existing": "overwritten", "new": "val"})
	if err == nil {
		t.Fatal("expected error with nil encryption key")
	}

	got, err := goodAB.Get(ctx, "existing")
	if err != nil {
		t.Fatalf("Get after failed batch: %v", err)
	}
	if got != "safe" {
		t.Fatalf("expected 'safe' after failed batch, got %q", got)
	}
}

func TestAESBackendSetBatchMidBatchSQLErrorRollsBack(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	key := makeTestKey()
	ctx := context.Background()

	// Create a BEFORE INSERT trigger that causes a SQL error for a
	// specific key name. This lets some keys succeed their ExecContext
	// call before the trigger fires, exercising the defer tx.Rollback()
	// path with actually-executed statements in the transaction.
	_, err := db.ExecContext(ctx, `
		CREATE TRIGGER fail_on_bomb BEFORE INSERT ON security_settings
		WHEN NEW.key = 'bomb'
		BEGIN
			SELECT RAISE(ABORT, 'intentional mid-batch failure');
		END
	`)
	if err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	ab := NewAESBackend(db, key, "default", "default")

	// With 20 keys, Go's non-deterministic map iteration makes it
	// statistically near-certain (>99.99%) that some good keys are
	// inserted before "bomb" triggers the SQL error. The defer
	// tx.Rollback() must undo ALL previously-executed inserts.
	values := map[string]string{
		"key_01": "v01", "key_02": "v02", "key_03": "v03", "key_04": "v04",
		"key_05": "v05", "key_06": "v06", "key_07": "v07", "key_08": "v08",
		"key_09": "v09", "key_10": "v10", "key_11": "v11", "key_12": "v12",
		"key_13": "v13", "key_14": "v14", "key_15": "v15", "key_16": "v16",
		"key_17": "v17", "key_18": "v18", "key_19": "v19",
		"bomb": "kaboom",
	}
	err = ab.SetBatch(ctx, values)
	if err == nil {
		t.Fatal("expected error from trigger on 'bomb' key")
	}

	// Verify ALL rows were rolled back — including any that were
	// successfully inserted before the trigger fired.
	var count int
	if qErr := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM security_settings`).Scan(&count); qErr != nil {
		t.Fatalf("count query: %v", qErr)
	}
	if count != 0 {
		t.Fatalf("expected 0 rows after mid-batch SQL failure (transaction not rolled back), got %d", count)
	}
}

func TestAESBackendAvailableWithKey(t *testing.T) {
	t.Parallel()
	ab := newTestAESBackend(t)
	if !ab.Available() {
		t.Fatal("expected Available() true with valid key")
	}
}

func TestAESBackendAvailableWithNilKey(t *testing.T) {
	t.Parallel()
	ab := NewAESBackend(nil, nil, "default", "default")
	if ab.Available() {
		t.Fatal("expected Available() false with nil key")
	}
}

func TestAESBackendName(t *testing.T) {
	t.Parallel()
	ab := newTestAESBackend(t)
	if ab.Name() != "aes-file" {
		t.Fatalf("expected name 'aes-file', got %q", ab.Name())
	}
}

func TestAESBackendSetOverwrites(t *testing.T) {
	t.Parallel()
	ab := newTestAESBackend(t)
	ctx := context.Background()

	if err := ab.Set(ctx, "key", "original"); err != nil {
		t.Fatalf("Set original: %v", err)
	}
	if err := ab.Set(ctx, "key", "updated"); err != nil {
		t.Fatalf("Set updated: %v", err)
	}

	val, err := ab.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "updated" {
		t.Fatalf("expected updated, got %q", val)
	}
}

func TestAESBackendProfileIsolation(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	key := makeTestKey()
	ctx := context.Background()

	abA := NewAESBackend(db, key, "default", "profile-a")
	abB := NewAESBackend(db, key, "default", "profile-b")

	if err := abA.Set(ctx, "secret", "val-a"); err != nil {
		t.Fatalf("Set A: %v", err)
	}

	_, err := abB.Get(ctx, "secret")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound from profile B, got: %v", err)
	}

	valA, err := abA.Get(ctx, "secret")
	if err != nil {
		t.Fatalf("Get A: %v", err)
	}
	if valA != "val-a" {
		t.Fatalf("expected val-a, got %q", valA)
	}
}
