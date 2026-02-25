// Package plugins (whitebox) — tests unexported cachedChecker method.
// Must be in package plugins to access the unexported method directly.
package plugins

import (
	"errors"
	"testing"
)

func TestCachedCheckerReturnsNilWhenNoIntegrityChecker(t *testing.T) {
	t.Parallel()

	svc := &Service{}
	// integrityChecker is nil by default — cachedChecker must return nil.
	if checker := svc.cachedChecker(); checker != nil {
		t.Fatal("expected nil cachedChecker when integrityChecker is nil")
	}
}

func TestCachedCheckerDistinguishesDifferentPluginDirs(t *testing.T) {
	t.Parallel()

	calls := 0
	svc := &Service{}
	svc.integrityChecker = func(namespace, slug, pluginDir string) error {
		calls++
		if pluginDir == "/bad" {
			return errors.New("tampered")
		}
		return nil
	}

	checker := svc.cachedChecker()

	// First call for dir /good — should pass and cache.
	if err := checker("ns", "slug", "/good"); err != nil {
		t.Fatalf("expected nil for /good, got: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call after first check, got %d", calls)
	}

	// Second call for same namespace/slug but different dir — must NOT use cache.
	if err := checker("ns", "slug", "/bad"); err == nil {
		t.Fatal("expected error for /bad, got nil (cache key didn't include pluginDir)")
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (different dirs are separate cache entries), got %d", calls)
	}

	// Third call for /good — should use cache (no new call).
	if err := checker("ns", "slug", "/good"); err != nil {
		t.Fatalf("expected cached nil for /good, got: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected still 2 calls (cache hit for /good), got %d", calls)
	}

	// Fourth call for /bad — should use cache (cached error).
	if err := checker("ns", "slug", "/bad"); err == nil {
		t.Fatal("expected cached error for /bad, got nil")
	}
	if calls != 2 {
		t.Fatalf("expected still 2 calls (cache hit for /bad), got %d", calls)
	}
}
