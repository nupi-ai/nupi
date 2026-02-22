package crypto

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// threadSafeMockKeyring is a concurrent-safe in-memory keyring for testing.
type threadSafeMockKeyring struct {
	mu   sync.RWMutex
	data map[string]string
}

// errNotFoundInKeyring uses the real keyring.ErrNotFound so that
// KeychainBackend.Get correctly translates it to ErrSecretNotFound.
var errNotFoundInKeyring = keyring.ErrNotFound

func newMockKeyring() *threadSafeMockKeyring {
	return &threadSafeMockKeyring{data: make(map[string]string)}
}

func (m *threadSafeMockKeyring) Set(service, user, password string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[service+"\x00"+user] = password
	return nil
}

func (m *threadSafeMockKeyring) Get(service, user string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[service+"\x00"+user]
	if !ok {
		return "", errNotFoundInKeyring
	}
	return v, nil
}

func (m *threadSafeMockKeyring) Delete(service, user string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := service + "\x00" + user
	if _, ok := m.data[key]; !ok {
		return errNotFoundInKeyring
	}
	delete(m.data, key)
	return nil
}

func TestKeychainBackendSetGetDelete(t *testing.T) {
	t.Parallel()
	mock := newMockKeyring()
	kb := newKeychainBackendWithProvider("default", "default", mock)
	ctx := context.Background()

	if err := kb.Set(ctx, "api_key", "sk-test-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	val, err := kb.Get(ctx, "api_key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "sk-test-123" {
		t.Fatalf("expected sk-test-123, got %q", val)
	}

	if err := kb.Delete(ctx, "api_key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err = kb.Get(ctx, "api_key")
	if err == nil {
		t.Fatal("expected error after delete")
	}
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound after delete, got: %v", err)
	}
}

func TestKeychainBackendCompositeKey(t *testing.T) {
	t.Parallel()
	mock := newMockKeyring()
	kb := newKeychainBackendWithProvider("myinst", "myprof", mock)
	ctx := context.Background()

	if err := kb.Set(ctx, "token", "abc"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Verify composite key format by reading directly from mock.
	// compositeKeySep is \x1f (ASCII Unit Separator).
	val, err := mock.Get("nupi", "myinst\x1fmyprof\x1ftoken")
	if err != nil {
		t.Fatalf("direct Get: %v", err)
	}
	if val != "abc" {
		t.Fatalf("expected abc, got %q", val)
	}
}

func TestKeychainBackendProfileIsolation(t *testing.T) {
	t.Parallel()
	mock := newMockKeyring()
	kbA := newKeychainBackendWithProvider("default", "profile-a", mock)
	kbB := newKeychainBackendWithProvider("default", "profile-b", mock)
	ctx := context.Background()

	if err := kbA.Set(ctx, "secret", "value-a"); err != nil {
		t.Fatalf("Set A: %v", err)
	}

	_, err := kbB.Get(ctx, "secret")
	if err == nil {
		t.Fatal("profile B should not see profile A's secret")
	}
}

func TestKeychainBackendAvailable(t *testing.T) {
	t.Parallel()
	mock := newMockKeyring()
	kb := newKeychainBackendWithProvider("default", "default", mock)
	if !kb.Available() {
		t.Fatal("expected Available() to return true with mock keyring")
	}
}

func TestKeychainBackendAvailableReturnsFalseOnError(t *testing.T) {
	t.Parallel()
	errKeyring := errors.New("keychain locked")
	kb := newKeychainBackendWithProvider("default", "default", &failingKeyring{err: errKeyring})
	if kb.Available() {
		t.Fatal("expected Available() to return false when keyring fails")
	}
}

func TestKeychainBackendName(t *testing.T) {
	t.Parallel()
	kb := NewKeychainBackend("default", "default")
	if kb.Name() != "keychain" {
		t.Fatalf("expected name 'keychain', got %q", kb.Name())
	}
}

func TestKeychainBackendGetNotFound(t *testing.T) {
	t.Parallel()
	mock := newMockKeyring()
	kb := newKeychainBackendWithProvider("default", "default", mock)
	ctx := context.Background()

	_, err := kb.Get(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent key")
	}
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got: %v", err)
	}
}

func TestKeychainBackendDeleteNotFound(t *testing.T) {
	t.Parallel()
	mock := newMockKeyring()
	kb := newKeychainBackendWithProvider("default", "default", mock)
	ctx := context.Background()

	err := kb.Delete(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error deleting nonexistent key")
	}
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got: %v", err)
	}
}

func TestKeychainBackendSetBatch(t *testing.T) {
	t.Parallel()
	mock := newMockKeyring()
	kb := newKeychainBackendWithProvider("default", "default", mock)
	ctx := context.Background()

	values := map[string]string{
		"key1": "val1",
		"key2": "val2",
		"key3": "val3",
	}
	if err := kb.SetBatch(ctx, values); err != nil {
		t.Fatalf("SetBatch: %v", err)
	}

	for k, v := range values {
		got, err := kb.Get(ctx, k)
		if err != nil {
			t.Fatalf("Get %q after SetBatch: %v", k, err)
		}
		if got != v {
			t.Fatalf("Get %q: expected %q, got %q", k, v, got)
		}
	}
}

func TestKeychainBackendGetBatchSkipsMissing(t *testing.T) {
	t.Parallel()
	mock := newMockKeyring()
	kb := newKeychainBackendWithProvider("default", "default", mock)
	ctx := context.Background()

	if err := kb.Set(ctx, "exists", "val"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	result, err := kb.GetBatch(ctx, []string{"exists", "missing1", "missing2"})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d: %v", len(result), result)
	}
	if result["exists"] != "val" {
		t.Fatalf("expected val, got %q", result["exists"])
	}
}

func TestKeychainBackendForceAvailableOverridesProbe(t *testing.T) {
	t.Parallel()
	// A failing provider would normally make Available() return false.
	kb := newKeychainBackendWithProvider("default", "default", &failingKeyring{err: errors.New("locked")})
	kb.SetForceAvailable()
	if !kb.Available() {
		t.Fatal("expected Available() true with SetForceAvailable(), even with failing provider")
	}
}

func TestKeychainBackendCompositeKeyRejectsSeparator(t *testing.T) {
	t.Parallel()
	mock := newMockKeyring()
	ctx := context.Background()

	t.Run("key", func(t *testing.T) {
		kb := newKeychainBackendWithProvider("inst", "prof", mock)
		err := kb.Set(ctx, "bad\x1fkey", "val")
		if err == nil {
			t.Fatal("expected error for key containing \\x1f")
		}
		if !strings.Contains(err.Error(), "separator") {
			t.Fatalf("expected separator error, got: %v", err)
		}
	})

	t.Run("instance", func(t *testing.T) {
		kb := newKeychainBackendWithProvider("bad\x1finst", "prof", mock)
		err := kb.Set(ctx, "key", "val")
		if err == nil {
			t.Fatal("expected error for instance containing \\x1f")
		}
	})

	t.Run("profile", func(t *testing.T) {
		kb := newKeychainBackendWithProvider("inst", "bad\x1fprof", mock)
		err := kb.Set(ctx, "key", "val")
		if err == nil {
			t.Fatal("expected error for profile containing \\x1f")
		}
	})
}

func TestKeychainBackendSetTimesOutAndDisables(t *testing.T) {
	t.Parallel()
	kb := newKeychainBackendWithProvider("default", "default", &slowKeyring{delay: 500 * time.Millisecond})
	kb.opTimeout = 50 * time.Millisecond
	ctx := context.Background()

	// Set should timeout and return error.
	err := kb.Set(ctx, "key", "val")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got: %v", err)
	}

	// Circuit breaker: Available() should now return false.
	if kb.Available() {
		t.Fatal("expected Available() false after timeout (circuit breaker)")
	}
}

func TestKeychainBackendGetTimesOutAndDisables(t *testing.T) {
	t.Parallel()
	kb := newKeychainBackendWithProvider("default", "default", &slowKeyring{delay: 500 * time.Millisecond})
	kb.opTimeout = 50 * time.Millisecond
	ctx := context.Background()

	_, err := kb.Get(ctx, "key")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
	if kb.Available() {
		t.Fatal("expected Available() false after timeout (circuit breaker)")
	}
}

func TestKeychainBackendDeleteTimesOutAndDisables(t *testing.T) {
	t.Parallel()
	kb := newKeychainBackendWithProvider("default", "default", &slowKeyring{delay: 500 * time.Millisecond})
	kb.opTimeout = 50 * time.Millisecond
	ctx := context.Background()

	err := kb.Delete(ctx, "key")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
	// Timeout error must NOT be translated to ErrSecretNotFound.
	if errors.Is(err, ErrSecretNotFound) {
		t.Fatal("timeout error should not be ErrSecretNotFound")
	}
	// Circuit breaker: Available() should now return false.
	if kb.Available() {
		t.Fatal("expected Available() false after timeout (circuit breaker)")
	}
}

func TestKeychainBackendSetBatchCircuitBreakerStopsCascade(t *testing.T) {
	t.Parallel()
	kb := newKeychainBackendWithProvider("default", "default", &slowKeyring{delay: 500 * time.Millisecond})
	kb.opTimeout = 50 * time.Millisecond
	ctx := context.Background()

	// First Set will timeout and fire circuit breaker. Subsequent keys
	// in the batch must fail immediately (no additional 5s waits).
	start := time.Now()
	err := kb.SetBatch(ctx, map[string]string{"k1": "v1", "k2": "v2", "k3": "v3"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error from SetBatch")
	}
	// With 3 keys and no guard, total would be 3×50ms = 150ms.
	// With guard, only 1 timeout + immediate returns = ~50ms.
	// Use generous threshold (500ms) to avoid flakiness on loaded CI runners.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("expected fast abort after circuit breaker, but took %v", elapsed)
	}
}

func TestKeychainBackendGetBatchCircuitBreakerStopsCascade(t *testing.T) {
	t.Parallel()
	kb := newKeychainBackendWithProvider("default", "default", &slowKeyring{delay: 500 * time.Millisecond})
	kb.opTimeout = 50 * time.Millisecond
	ctx := context.Background()

	start := time.Now()
	_, err := kb.GetBatch(ctx, []string{"k1", "k2", "k3"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error from GetBatch")
	}
	// Use generous threshold (500ms) to avoid flakiness on loaded CI runners.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("expected fast abort after circuit breaker, but took %v", elapsed)
	}
}

func TestKeychainBackendSetAfterCircuitBreakerReturnsImmediately(t *testing.T) {
	t.Parallel()
	kb := newKeychainBackendWithProvider("default", "default", &slowKeyring{delay: 500 * time.Millisecond})
	kb.opTimeout = 50 * time.Millisecond
	ctx := context.Background()

	// Fire the circuit breaker via a timeout.
	_ = kb.Set(ctx, "trigger", "val")
	if kb.Available() {
		t.Fatal("expected circuit breaker to fire")
	}

	// Subsequent direct Set call must return immediately (no goroutine
	// spawn, no 5s wait). Without the disabled guard in withTimeout,
	// this would take another 50ms+ for the timeout.
	start := time.Now()
	err := kb.Set(ctx, "key", "val")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error from Set after circuit breaker")
	}
	if !strings.Contains(err.Error(), "circuit breaker") {
		t.Fatalf("expected circuit breaker error, got: %v", err)
	}
	if elapsed > 10*time.Millisecond {
		t.Fatalf("expected immediate return after circuit breaker, but took %v", elapsed)
	}
}

// slowKeyring simulates a keychain that hangs on operations.
type slowKeyring struct{ delay time.Duration }

func (s *slowKeyring) Set(_, _, _ string) error {
	time.Sleep(s.delay)
	return nil
}
func (s *slowKeyring) Get(_, _ string) (string, error) {
	time.Sleep(s.delay)
	return "val", nil
}
func (s *slowKeyring) Delete(_, _ string) error {
	time.Sleep(s.delay)
	return nil
}

// failingKeyring always returns the configured error.
type failingKeyring struct{ err error }

func (f *failingKeyring) Set(_, _, _ string) error        { return f.err }
func (f *failingKeyring) Get(_, _ string) (string, error) { return "", f.err }
func (f *failingKeyring) Delete(_, _ string) error        { return f.err }
