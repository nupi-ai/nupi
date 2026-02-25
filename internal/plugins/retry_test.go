// Package plugins (whitebox) — tests unexported reloadWithRetry function.
// service_test.go uses plugins_test (blackbox) for public API; this file
// must be in package plugins to access the unexported function directly.
package plugins

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReloadWithRetrySucceedsOnFirstAttempt(t *testing.T) {
	t.Parallel()

	calls := 0
	err := reloadWithRetry(context.Background(), func() error {
		calls++
		return nil
	}, 3)

	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestReloadWithRetrySucceedsAfterTransientFailures(t *testing.T) {
	t.Parallel()

	calls := 0
	err := reloadWithRetry(context.Background(), func() error {
		calls++
		if calls < 3 {
			return errors.New("transient error")
		}
		return nil
	}, 3)

	if err != nil {
		t.Fatalf("expected nil error on third attempt, got: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestReloadWithRetryExhaustsAllAttempts(t *testing.T) {
	t.Parallel()

	calls := 0
	permanent := errors.New("permanent error")
	err := reloadWithRetry(context.Background(), func() error {
		calls++
		return permanent
	}, 3)

	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
	if !errors.Is(err, permanent) {
		t.Fatalf("expected wrapped permanent error, got: %v", err)
	}
}

func TestReloadWithRetryCancelledByContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	start := time.Now()
	err := reloadWithRetry(ctx, func() error {
		calls++
		// Cancel context after first failure — retry delay should abort.
		cancel()
		return errors.New("fail")
	}, 3)

	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call before cancellation, got %d", calls)
	}
	// Should return almost immediately — no 500ms delay.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("expected fast cancellation, took %v", elapsed)
	}
}

func TestReloadWithRetryRejectsInvalidMaxAttempts(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, -1, -100} {
		err := reloadWithRetry(context.Background(), func() error {
			t.Fatal("fn should not be called with invalid maxAttempts")
			return nil
		}, n)
		if err == nil {
			t.Fatalf("expected error for maxAttempts=%d, got nil", n)
		}
	}
}

func TestReloadWithRetryBackoffTiming(t *testing.T) {
	t.Parallel()

	calls := 0
	start := time.Now()
	err := reloadWithRetry(context.Background(), func() error {
		calls++
		if calls < 3 {
			return errors.New("fail")
		}
		return nil
	}, 3)

	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected nil error on third attempt, got: %v", err)
	}
	// Two delays: 500ms (attempt 1→2) + 1000ms (attempt 2→3) = 1500ms minimum.
	// Use 20% tolerance to avoid flakes under CI load.
	if elapsed < 1200*time.Millisecond {
		t.Fatalf("expected ~1500ms total backoff, got %v", elapsed)
	}
	// Upper bound sanity check: catch pathological backoff bugs.
	// Generous limit to avoid CI flakiness under load.
	if elapsed > 10*time.Second {
		t.Fatalf("backoff took too long, expected ~1500ms, got %v", elapsed)
	}
}
