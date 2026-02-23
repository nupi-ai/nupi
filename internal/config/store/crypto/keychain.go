package crypto

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/zalando/go-keyring"
)

const (
	keychainService = "nupi"
	// compositeKeySep is the ASCII Unit Separator (0x1F), used to join
	// instance/profile/key into a single keyring user string. Using a
	// non-printable control character avoids ambiguity if any component
	// contains "/" or other common separators.
	compositeKeySep = "\x1f"
	// keychainOpTimeout limits how long a single keychain Set/Get/Delete
	// operation may block. If exceeded, the keychain is disabled for the
	// rest of the process lifetime (circuit breaker) and FallbackBackend
	// falls back to AES for all subsequent operations.
	keychainOpTimeout = constants.Duration5Seconds
)

// keyringProvider abstracts go-keyring calls for testing.
type keyringProvider interface {
	Set(service, user, password string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

// osKeyring delegates to the real go-keyring package.
type osKeyring struct{}

func (osKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (osKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (osKeyring) Delete(service, user string) error        { return keyring.Delete(service, user) }

// KeychainBackend stores secrets in the OS keychain via go-keyring.
type KeychainBackend struct {
	instance   string
	profile    string
	provider   keyringProvider
	mockProbe  bool          // when true, Available() uses Set+Delete probe instead of OS-level check
	forceAvail atomic.Bool   // when true, Available() always returns true (NUPI_KEYCHAIN=force)
	disabled   atomic.Bool   // circuit breaker: set to true on operation timeout
	opTimeout  time.Duration // 0 means keychainOpTimeout; tests can override

	availableOnce sync.Once
	cachedAvail   bool
}

// NewKeychainBackend creates a KeychainBackend for the given instance/profile.
func NewKeychainBackend(instance, profile string) *KeychainBackend {
	return &KeychainBackend{
		instance: instance,
		profile:  profile,
		provider: osKeyring{},
	}
}

// newKeychainBackendWithProvider creates a KeychainBackend with a custom provider (for testing).
// Uses Set+Delete probe for Available() instead of OS-level checks.
func newKeychainBackendWithProvider(instance, profile string, p keyringProvider) *KeychainBackend {
	return &KeychainBackend{
		instance:  instance,
		profile:   profile,
		provider:  p,
		mockProbe: true,
	}
}

// SetForceAvailable makes Available() always return true without running
// any platform check. Used when NUPI_KEYCHAIN=force to bypass probing.
// Thread-safe: may be called before concurrent Available() calls.
func (kb *KeychainBackend) SetForceAvailable() {
	kb.forceAvail.Store(true)
}

// withTimeout runs fn in a goroutine with a timeout. If the operation
// exceeds the timeout, the keychain is disabled for the rest of the
// session (circuit breaker) and an error is returned.
//
// Note: on timeout the goroutine running the keyring call will leak
// until the underlying operation completes. This is the same accepted
// limitation as availableProbe — go-keyring does not support context
// cancellation.
func (kb *KeychainBackend) withTimeout(op string, fn func() error) error {
	if kb.disabled.Load() {
		return fmt.Errorf("keychain %s: disabled by circuit breaker", op)
	}
	timeout := kb.opTimeout
	if timeout == 0 {
		timeout = keychainOpTimeout
	}
	ch := make(chan error, 1)
	go func() { ch <- fn() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-ch:
		return err
	case <-timer.C:
		kb.disabled.Store(true)
		log.Printf("[Config] Keychain %s timed out after %v — disabling keychain for this session", op, timeout)
		return fmt.Errorf("keychain %s timed out after %v", op, timeout)
	}
}

// compositeKey builds the keyring user field: "<instance>\x1f<profile>\x1f<key>".
// Returns an error if any component is empty or contains the separator — this
// guards against ambiguous keyring entries from corrupted or misconfigured data.
func (kb *KeychainBackend) compositeKey(key string) (string, error) {
	if kb.instance == "" || kb.profile == "" || key == "" {
		return "", fmt.Errorf("keychain: composite key component is empty: instance=%q profile=%q key=%q",
			kb.instance, kb.profile, key)
	}
	if strings.Contains(kb.instance, compositeKeySep) ||
		strings.Contains(kb.profile, compositeKeySep) ||
		strings.Contains(key, compositeKeySep) {
		return "", fmt.Errorf("keychain: composite key component contains separator 0x1F: instance=%q profile=%q key=%q",
			kb.instance, kb.profile, key)
	}
	return kb.instance + compositeKeySep + kb.profile + compositeKeySep + key, nil
}

// SetBatch stores multiple secrets in the OS keychain (non-transactional).
// On partial failure, previously-set keys in this batch are NOT rolled back:
// cleanup via Delete could be worse than leaving them, because Delete would
// remove legitimate previous values (not just the new ones). The partial
// state is harmless in practice — FallbackBackend always writes to AES/DB
// first, so all keys are recoverable from the secondary backend.
func (kb *KeychainBackend) SetBatch(ctx context.Context, values map[string]string) error {
	for key, value := range values {
		if kb.disabled.Load() {
			return fmt.Errorf("keychain SetBatch: disabled by circuit breaker")
		}
		if err := kb.Set(ctx, key, value); err != nil {
			return err
		}
	}
	return nil
}

// GetBatch retrieves multiple secrets from the OS keychain.
func (kb *KeychainBackend) GetBatch(ctx context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if kb.disabled.Load() {
			return nil, fmt.Errorf("keychain GetBatch: disabled by circuit breaker")
		}
		val, err := kb.Get(ctx, key)
		if err != nil {
			if errors.Is(err, ErrSecretNotFound) {
				continue
			}
			return nil, err
		}
		result[key] = val
	}
	return result, nil
}

// Set stores a secret in the OS keychain with timeout protection.
// Context is accepted for SecretBackend interface compliance but not used
// directly: go-keyring's API does not support context-based cancellation.
// Instead, a goroutine + timeout wrapper provides hang protection.
func (kb *KeychainBackend) Set(_ context.Context, key, value string) error {
	ckey, err := kb.compositeKey(key)
	if err != nil {
		return err
	}
	if err := kb.withTimeout("Set", func() error {
		return kb.provider.Set(keychainService, ckey, value)
	}); err != nil {
		return fmt.Errorf("keychain: set %q: %w", key, err)
	}
	return nil
}

// Get retrieves a secret from the OS keychain with timeout protection.
// Context is accepted for SecretBackend interface compliance but not used
// directly: go-keyring's API does not support context-based cancellation.
// Instead, a goroutine + timeout wrapper provides hang protection.
func (kb *KeychainBackend) Get(_ context.Context, key string) (string, error) {
	ckey, err := kb.compositeKey(key)
	if err != nil {
		return "", err
	}
	var val string
	if err := kb.withTimeout("Get", func() error {
		var getErr error
		val, getErr = kb.provider.Get(keychainService, ckey)
		return getErr
	}); err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", fmt.Errorf("keychain: get %q: %w", key, ErrSecretNotFound)
		}
		return "", fmt.Errorf("keychain: get %q: %w", key, err)
	}
	return val, nil
}

// Delete removes a secret from the OS keychain with timeout protection.
// Context is accepted for SecretBackend interface compliance but not used
// directly: go-keyring's API does not support context-based cancellation.
// Instead, a goroutine + timeout wrapper provides hang protection.
func (kb *KeychainBackend) Delete(_ context.Context, key string) error {
	ckey, err := kb.compositeKey(key)
	if err != nil {
		return err
	}
	if err := kb.withTimeout("Delete", func() error {
		return kb.provider.Delete(keychainService, ckey)
	}); err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return fmt.Errorf("keychain: delete %q: %w", key, ErrSecretNotFound)
		}
		return fmt.Errorf("keychain: delete %q: %w", key, err)
	}
	return nil
}

// Available checks whether the OS keychain is usable without triggering
// system dialogs. The result is probed once and cached for the lifetime
// of the backend — keychain availability does not change mid-process in
// practice, and FallbackBackend already handles transient errors by
// falling back to the secondary (AES) backend on each operation.
//
// On macOS this uses the `security` CLI to verify that a default keychain
// exists (read-only, no popups). On other platforms a Set+Delete probe
// with a timeout is used as a fallback.
//
// Note: the circuit breaker (disabled) takes precedence over forceAvail.
// Even with NUPI_KEYCHAIN=force, a single operation timeout permanently
// disables the keychain for the session. This is intentional — force mode
// bypasses the *initial probe*, but cannot override a runtime hang that
// would block the daemon.
func (kb *KeychainBackend) Available() bool {
	if kb.disabled.Load() {
		return false
	}
	if kb.forceAvail.Load() {
		return true
	}
	kb.availableOnce.Do(func() {
		if kb.mockProbe {
			kb.cachedAvail = kb.availableProbe()
		} else if runtime.GOOS == "darwin" {
			kb.cachedAvail = kb.availableDarwin()
		} else {
			kb.cachedAvail = kb.availableProbe()
		}
	})
	return kb.cachedAvail
}

// availableDarwin checks macOS keychain via `security default-keychain`
// which never triggers UI dialogs. A 3-second timeout prevents hangs.
func (kb *KeychainBackend) availableDarwin() bool {
	ctx, cancel := context.WithTimeout(context.Background(), constants.Duration3Seconds)
	defer cancel()
	if err := exec.CommandContext(ctx, "security", "default-keychain", "-d", "user").Run(); err != nil {
		log.Printf("[Config] Keychain not available (no default keychain): %v", err)
		return false
	}
	return true
}

// availableProbe uses a Set+Delete cycle with a timeout for non-macOS
// platforms where there is no equivalent dialog-free check.
//
// Note: if the probe times out, the goroutine running the keyring call
// will leak until the underlying operation completes. This is acceptable
// because (a) go-keyring does not accept context for cancellation,
// (b) this runs at most once during Open(), and (c) a timeout means the
// keychain is unusable so the process will use the AES fallback.
//
// The initial Delete is a proactive cleanup: if a previous process crashed
// after Set but before Delete, the __probe__ key would remain in the
// keychain permanently. This idempotent Delete removes any orphaned entry.
func (kb *KeychainBackend) availableProbe() bool {
	probeKey, err := kb.compositeKey("__probe__")
	if err != nil {
		log.Printf("[Config] Keychain probe key error: %v", err)
		return false
	}

	type probeResult struct {
		ok  bool
		err error
	}
	ch := make(chan probeResult, 1)

	go func() {
		// Clean up any orphaned probe key from a previous crashed probe.
		_ = kb.provider.Delete(keychainService, probeKey)

		if err := kb.provider.Set(keychainService, probeKey, "probe"); err != nil {
			ch <- probeResult{ok: false, err: err}
			return
		}
		_ = kb.provider.Delete(keychainService, probeKey)
		ch <- probeResult{ok: true}
	}()

	timer := time.NewTimer(constants.Duration3Seconds)
	defer timer.Stop()

	select {
	case res := <-ch:
		if !res.ok {
			log.Printf("[Config] Keychain not available: %v", res.err)
		}
		return res.ok
	case <-timer.C:
		log.Printf("[Config] Keychain probe timed out (possible OS dialog blocking)")
		return false
	}
}

// Name returns the backend identifier for logging.
func (kb *KeychainBackend) Name() string { return "keychain" }
