package crypto

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// memBackend is an in-memory SecretBackend for testing.
type memBackend struct {
	name      string
	available bool
	data      map[string]string
}

func newMemBackend(name string, available bool) *memBackend {
	return &memBackend{name: name, available: available, data: make(map[string]string)}
}

func (m *memBackend) Set(_ context.Context, key, value string) error {
	m.data[key] = value
	return nil
}

func (m *memBackend) Get(_ context.Context, key string) (string, error) {
	v, ok := m.data[key]
	if !ok {
		return "", ErrSecretNotFound
	}
	return v, nil
}

func (m *memBackend) Delete(_ context.Context, key string) error {
	if _, ok := m.data[key]; !ok {
		return ErrSecretNotFound
	}
	delete(m.data, key)
	return nil
}

func (m *memBackend) SetBatch(ctx context.Context, values map[string]string) error {
	for k, v := range values {
		if err := m.Set(ctx, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (m *memBackend) GetBatch(_ context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := m.data[k]; ok {
			result[k] = v
		}
	}
	return result, nil
}

func (m *memBackend) Available() bool { return m.available }
func (m *memBackend) Name() string    { return m.name }

// errBackend always returns errors for Set/Get/Delete but reports available.
type errBackend struct {
	name string
	err  error
}

func (e *errBackend) Set(_ context.Context, _, _ string) error              { return e.err }
func (e *errBackend) SetBatch(_ context.Context, _ map[string]string) error { return e.err }
func (e *errBackend) Get(_ context.Context, _ string) (string, error)       { return "", e.err }
func (e *errBackend) GetBatch(_ context.Context, _ []string) (map[string]string, error) {
	return nil, e.err
}
func (e *errBackend) Delete(_ context.Context, _ string) error { return e.err }
func (e *errBackend) Available() bool                          { return true }
func (e *errBackend) Name() string                             { return e.name }

func TestFallbackDualWritesBothBackends(t *testing.T) {
	t.Parallel()

	primary := newMemBackend("primary", true)
	secondary := newMemBackend("secondary", true)
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	if err := fb.Set(ctx, "key", "val"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Dual-write: both backends should have the value.
	if _, ok := primary.data["key"]; !ok {
		t.Fatal("expected key in primary")
	}
	if _, ok := secondary.data["key"]; !ok {
		t.Fatal("expected key in secondary (dual-write for enumeration)")
	}

	// Get reads from primary first.
	got, err := fb.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "val" {
		t.Fatalf("expected val, got %q", got)
	}
}

func TestFallbackUsesSecondaryWhenPrimaryUnavailable(t *testing.T) {
	t.Parallel()

	primary := newMemBackend("primary", false) // unavailable
	secondary := newMemBackend("secondary", true)
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	if err := fb.Set(ctx, "key", "val"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Only secondary should have the value.
	if _, ok := secondary.data["key"]; !ok {
		t.Fatal("expected key in secondary")
	}
	if _, ok := primary.data["key"]; ok {
		t.Fatal("did not expect key in primary (unavailable)")
	}

	got, err := fb.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "val" {
		t.Fatalf("expected val, got %q", got)
	}
}

func TestFallbackPrimarySetErrorDoesNotFail(t *testing.T) {
	t.Parallel()

	primary := &errBackend{name: "primary", err: errors.New("keychain locked")}
	secondary := newMemBackend("secondary", true)
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	// Set should succeed — secondary write succeeds, primary error is logged.
	if err := fb.Set(ctx, "key", "val"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, ok := secondary.data["key"]; !ok {
		t.Fatal("expected key in secondary")
	}

	// Get falls back to secondary.
	got, err := fb.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "val" {
		t.Fatalf("expected val, got %q", got)
	}
}

func TestFallbackGetFallsBackOnPrimaryError(t *testing.T) {
	t.Parallel()

	primary := &errBackend{name: "primary", err: errors.New("keychain locked")}
	secondary := newMemBackend("secondary", true)
	secondary.data["key"] = "val"
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	got, err := fb.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "val" {
		t.Fatalf("expected val, got %q", got)
	}
}

func TestFallbackAvailable(t *testing.T) {
	t.Parallel()

	fb1 := NewFallbackBackend(newMemBackend("a", true), newMemBackend("b", true))
	if !fb1.Available() {
		t.Fatal("expected available when both backends available")
	}

	fb2 := NewFallbackBackend(newMemBackend("a", false), newMemBackend("b", true))
	if !fb2.Available() {
		t.Fatal("expected available when secondary available")
	}

	fb3 := NewFallbackBackend(newMemBackend("a", false), newMemBackend("b", false))
	if fb3.Available() {
		t.Fatal("expected unavailable when both backends unavailable")
	}
}

func TestFallbackName(t *testing.T) {
	t.Parallel()

	fb := NewFallbackBackend(newMemBackend("keychain", true), newMemBackend("aes-file", true))
	if fb.Name() != "keychain+aes-file" {
		t.Fatalf("expected keychain+aes-file, got %q", fb.Name())
	}
}

func TestFallbackSetBatchWritesBothBackends(t *testing.T) {
	t.Parallel()

	primary := newMemBackend("primary", true)
	secondary := newMemBackend("secondary", true)
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	values := map[string]string{"k1": "v1", "k2": "v2"}
	if err := fb.SetBatch(ctx, values); err != nil {
		t.Fatalf("SetBatch: %v", err)
	}

	for k, v := range values {
		if secondary.data[k] != v {
			t.Fatalf("expected %q=%q in secondary, got %q", k, v, secondary.data[k])
		}
		if primary.data[k] != v {
			t.Fatalf("expected %q=%q in primary, got %q", k, v, primary.data[k])
		}
	}
}

func TestFallbackSetBatchPrimaryErrorDoesNotFail(t *testing.T) {
	t.Parallel()

	primary := &errBackend{name: "primary", err: errors.New("keychain locked")}
	secondary := newMemBackend("secondary", true)
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	values := map[string]string{"k1": "v1", "k2": "v2"}
	if err := fb.SetBatch(ctx, values); err != nil {
		t.Fatalf("SetBatch: %v", err)
	}

	for k, v := range values {
		if secondary.data[k] != v {
			t.Fatalf("expected %q=%q in secondary, got %q", k, v, secondary.data[k])
		}
	}
}

func TestFallbackGetSilentlyFallsBackOnErrSecretNotFound(t *testing.T) {
	t.Parallel()

	// Primary is available but does NOT have the key → returns ErrSecretNotFound.
	// FallbackBackend should silently (no log) fall back to secondary.
	primary := newMemBackend("primary", true)
	secondary := newMemBackend("secondary", true)
	secondary.data["key"] = "from-secondary"
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	got, err := fb.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "from-secondary" {
		t.Fatalf("expected from-secondary, got %q", got)
	}
}

func TestFallbackDeleteReturnsErrSecretNotFound(t *testing.T) {
	t.Parallel()

	primary := newMemBackend("primary", true)
	secondary := newMemBackend("secondary", true)
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	err := fb.Delete(ctx, "nonexistent")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got: %v", err)
	}
}

func TestFallbackDeleteRemovesFromBoth(t *testing.T) {
	t.Parallel()

	primary := newMemBackend("primary", true)
	secondary := newMemBackend("secondary", true)
	primary.data["key"] = "val"
	secondary.data["key"] = "val"
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	if err := fb.Delete(ctx, "key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := secondary.data["key"]; ok {
		t.Fatal("expected key deleted from secondary")
	}
	if _, ok := primary.data["key"]; ok {
		t.Fatal("expected key deleted from primary")
	}
}

func TestFallbackGetBatchFillsGapsFromSecondary(t *testing.T) {
	t.Parallel()

	primary := newMemBackend("primary", true)
	secondary := newMemBackend("secondary", true)
	primary.data["k1"] = "from-primary"
	secondary.data["k1"] = "from-secondary"
	secondary.data["k2"] = "only-in-secondary"
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	result, err := fb.GetBatch(ctx, []string{"k1", "k2"})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if result["k1"] != "from-primary" {
		t.Fatalf("k1: expected from-primary, got %q", result["k1"])
	}
	if result["k2"] != "only-in-secondary" {
		t.Fatalf("k2: expected only-in-secondary, got %q", result["k2"])
	}
}

func TestFallbackGetBatchPrimaryUnavailableUsesSecondary(t *testing.T) {
	t.Parallel()

	primary := newMemBackend("primary", false) // unavailable
	secondary := newMemBackend("secondary", true)
	secondary.data["k1"] = "val1"
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	result, err := fb.GetBatch(ctx, []string{"k1"})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if result["k1"] != "val1" {
		t.Fatalf("k1: expected val1, got %q", result["k1"])
	}
}

func TestFallbackGetBatchPrimaryErrorFallsBackToSecondary(t *testing.T) {
	t.Parallel()

	// Primary is available but GetBatch returns an error.
	// FallbackBackend should log the error and fall back to secondary.
	primary := &errBackend{name: "primary", err: errors.New("keychain locked")}
	secondary := newMemBackend("secondary", true)
	secondary.data["k1"] = "val1"
	secondary.data["k2"] = "val2"
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	result, err := fb.GetBatch(ctx, []string{"k1", "k2"})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if result["k1"] != "val1" {
		t.Fatalf("k1: expected val1, got %q", result["k1"])
	}
	if result["k2"] != "val2" {
		t.Fatalf("k2: expected val2, got %q", result["k2"])
	}
}

func TestFallbackDeleteSilentlyIgnoresPrimaryErrSecretNotFound(t *testing.T) {
	t.Parallel()

	// Primary is available but does NOT have the key → returns ErrSecretNotFound.
	// FallbackBackend.Delete should silently ignore this (no log), matching
	// the pattern in Get() for keys that exist in secondary but not primary.
	primary := newMemBackend("primary", true)
	secondary := newMemBackend("secondary", true)
	secondary.data["key"] = "val"
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	if err := fb.Delete(ctx, "key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := secondary.data["key"]; ok {
		t.Fatal("expected key deleted from secondary")
	}
}

func TestFallbackSetBatchSecondaryErrorReturnsError(t *testing.T) {
	t.Parallel()

	primary := newMemBackend("primary", true)
	secondary := &errBackend{name: "secondary", err: errors.New("db write failed")}
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	err := fb.SetBatch(ctx, map[string]string{"k1": "v1"})
	if err == nil {
		t.Fatal("expected error when secondary SetBatch fails")
	}
	// Primary must NOT be written to when secondary fails.
	if len(primary.data) > 0 {
		t.Fatalf("expected empty primary after secondary failure, got %v", primary.data)
	}
}

func TestFallbackDeletePrimaryErrorPreventsSecondaryDelete(t *testing.T) {
	t.Parallel()

	// Primary errors (not ErrSecretNotFound) should cause Delete to bail
	// before touching secondary — prevents orphaned keychain entries that
	// would still be retrievable via Get (which checks primary first).
	primary := &errBackend{name: "primary", err: errors.New("keychain locked")}
	secondary := newMemBackend("secondary", true)
	secondary.data["key"] = "val"
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	err := fb.Delete(ctx, "key")
	if err == nil {
		t.Fatal("expected error when primary delete fails with real error")
	}
	// Secondary must NOT be deleted — key stays consistent in both backends.
	if _, ok := secondary.data["key"]; !ok {
		t.Fatal("expected key still in secondary after primary failure")
	}
}

func TestFallbackDeletePrimaryUnavailableDeletesFromSecondary(t *testing.T) {
	t.Parallel()

	// When primary is unavailable, Delete should skip primary entirely
	// and delete from secondary only — matching the Set/Get pattern.
	primary := newMemBackend("primary", false) // unavailable
	secondary := newMemBackend("secondary", true)
	secondary.data["key"] = "val"
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	if err := fb.Delete(ctx, "key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := secondary.data["key"]; ok {
		t.Fatal("expected key deleted from secondary")
	}
}

func TestFallbackSetBatchPrimaryUnavailableWritesOnlySecondary(t *testing.T) {
	t.Parallel()

	// When primary is unavailable, SetBatch should write only to secondary
	// without attempting primary writes — matching the Set pattern.
	primary := newMemBackend("primary", false) // unavailable
	secondary := newMemBackend("secondary", true)
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	values := map[string]string{"k1": "v1", "k2": "v2"}
	if err := fb.SetBatch(ctx, values); err != nil {
		t.Fatalf("SetBatch: %v", err)
	}

	for k, v := range values {
		if secondary.data[k] != v {
			t.Fatalf("expected %q=%q in secondary, got %q", k, v, secondary.data[k])
		}
	}
	if len(primary.data) > 0 {
		t.Fatalf("expected empty primary (unavailable), got %v", primary.data)
	}
}

func TestFallbackGetBatchSecondaryErrorDiscardsPartialPrimary(t *testing.T) {
	t.Parallel()

	// Primary returns some keys successfully, but secondary fails when
	// queried for the remaining (missing) keys. The method should return
	// an error — partial results from primary are discarded because
	// returning an incomplete map would be misleading to callers.
	primary := newMemBackend("primary", true)
	primary.data["k1"] = "from-primary"
	secondary := &errBackend{name: "secondary", err: errors.New("db connection lost")}
	fb := NewFallbackBackend(primary, secondary)
	ctx := context.Background()

	_, err := fb.GetBatch(ctx, []string{"k1", "k2"})
	if err == nil {
		t.Fatal("expected error when secondary GetBatch fails")
	}
	if !strings.Contains(err.Error(), "db connection lost") {
		t.Fatalf("expected secondary error to propagate, got: %v", err)
	}
}
