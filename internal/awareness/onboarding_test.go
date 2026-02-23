package awareness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

func TestIsOnboardingTrueWhenBootstrapExists(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	if !svc.isOnboarding() {
		t.Fatal("isOnboarding() = false, want true when BOOTSTRAP.md exists")
	}
}

func TestIsOnboardingFalseWhenBootstrapMissing(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	// Remove BOOTSTRAP.md.
	if err := os.Remove(filepath.Join(svc.awarenessDir, "BOOTSTRAP.md")); err != nil {
		t.Fatal(err)
	}

	if svc.isOnboarding() {
		t.Fatal("isOnboarding() = true, want false when BOOTSTRAP.md is missing")
	}
}

func TestIsOnboardingFalseOnFreshDirWithoutScaffold(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	// Do NOT call ensureDirectories or scaffoldCoreFiles — empty dir.
	if svc.isOnboarding() {
		t.Fatal("isOnboarding() = true, want false on empty dir without scaffolding")
	}
}

func TestBootstrapContentReturnsFileContent(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	content := svc.BootstrapContent()
	if content == "" {
		t.Fatal("BootstrapContent() = empty, want non-empty content")
	}
	if content != defaultBootstrapContent {
		t.Fatalf("BootstrapContent() does not match defaultBootstrapContent")
	}
}

func TestBootstrapContentEmptyWhenMissing(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	// No scaffolding — BOOTSTRAP.md doesn't exist.
	if got := svc.BootstrapContent(); got != "" {
		t.Fatalf("BootstrapContent() = %q, want empty when BOOTSTRAP.md is missing", got)
	}
}

func TestCompleteOnboardingDeletesBootstrap(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	removed, err := svc.CompleteOnboarding()
	if err != nil {
		t.Fatalf("CompleteOnboarding() error = %v", err)
	}
	if !removed {
		t.Fatal("CompleteOnboarding() returned removed=false, want true")
	}

	// BOOTSTRAP.md must be gone.
	if _, err := os.Stat(filepath.Join(svc.awarenessDir, "BOOTSTRAP.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("BOOTSTRAP.md still exists after CompleteOnboarding()")
	}

	// Other scaffolds must be untouched.
	for _, sf := range coreScaffolds {
		if sf.filename == "BOOTSTRAP.md" {
			continue
		}
		if _, err := os.Stat(filepath.Join(svc.awarenessDir, sf.filename)); err != nil {
			t.Fatalf("%s missing after CompleteOnboarding(): %v", sf.filename, err)
		}
	}
}

func TestCompleteOnboardingIdempotent(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	removed, err := svc.CompleteOnboarding()
	if err != nil {
		t.Fatalf("first CompleteOnboarding() error = %v", err)
	}
	if !removed {
		t.Fatal("first CompleteOnboarding() returned removed=false, want true")
	}

	// Second call should return (false, nil).
	removed, err = svc.CompleteOnboarding()
	if err != nil {
		t.Fatalf("second CompleteOnboarding() error = %v, want nil (idempotent)", err)
	}
	if removed {
		t.Fatal("second CompleteOnboarding() returned removed=true, want false (idempotent)")
	}
}

func TestCompleteOnboardingCreatesMarker(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.CompleteOnboarding(); err != nil {
		t.Fatalf("CompleteOnboarding() error = %v", err)
	}

	markerPath := filepath.Join(svc.awarenessDir, ".onboarding_done")
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf(".onboarding_done marker not created: %v", err)
	}
}

func TestIsOnboardingSessionClaimsFirst(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	if !svc.IsOnboardingSession("session-A") {
		t.Fatal("first session should claim onboarding lock")
	}
}

func TestIsOnboardingSessionRejectsSecond(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	svc.IsOnboardingSession("session-A") // claim

	if svc.IsOnboardingSession("session-B") {
		t.Fatal("second session should be rejected while session-A holds lock")
	}
}

func TestIsOnboardingSessionSameSessionReturnsTrue(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	svc.IsOnboardingSession("session-A") // claim

	if !svc.IsOnboardingSession("session-A") {
		t.Fatal("same session should still return true")
	}
}

func TestIsOnboardingSessionFalseWhenNotOnboarding(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	// No scaffolding — no BOOTSTRAP.md.

	if svc.IsOnboardingSession("session-A") {
		t.Fatal("should return false when not onboarding")
	}
}

func TestCompleteOnboardingClearsSessionLock(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	svc.IsOnboardingSession("session-A") // claim

	if _, err := svc.CompleteOnboarding(); err != nil {
		t.Fatalf("CompleteOnboarding() error = %v", err)
	}

	// Lock should be cleared — but IsOnboardingSession returns false
	// because BOOTSTRAP.md is gone (isOnboarding() == false).
	if svc.IsOnboardingSession("session-B") {
		t.Fatal("should return false after onboarding complete")
	}

	// Verify the internal lock was actually cleared.
	svc.onboardingMu.Lock()
	id := svc.onboardingSessionID
	svc.onboardingMu.Unlock()
	if id != "" {
		t.Fatalf("onboardingSessionID = %q, want empty after CompleteOnboarding()", id)
	}
}

func TestReleaseOnboardingSessionAllowsNewClaim(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	// Session A claims onboarding.
	if !svc.IsOnboardingSession("session-A") {
		t.Fatal("session-A should claim onboarding lock")
	}

	// Session B is rejected.
	if svc.IsOnboardingSession("session-B") {
		t.Fatal("session-B should be rejected while session-A holds lock")
	}

	// Session A disconnects — lock released.
	svc.ReleaseOnboardingSession("session-A")

	// Session B can now claim onboarding (BOOTSTRAP.md still exists).
	if !svc.IsOnboardingSession("session-B") {
		t.Fatal("session-B should claim onboarding lock after session-A released")
	}
}

func TestReleaseOnboardingSessionIgnoresWrongSession(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	svc.IsOnboardingSession("session-A") // claim

	// Releasing a different session should be a no-op.
	svc.ReleaseOnboardingSession("session-C")

	// Session A should still hold the lock.
	if !svc.IsOnboardingSession("session-A") {
		t.Fatal("session-A should still hold lock after releasing wrong session")
	}

	// Session B should still be rejected.
	if svc.IsOnboardingSession("session-B") {
		t.Fatal("session-B should be rejected — lock not actually released")
	}
}

func TestIsOnboardingSessionRejectsEmptySessionID(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	// Empty session ID must never claim the lock.
	if svc.IsOnboardingSession("") {
		t.Fatal("empty session ID should be rejected")
	}

	// A real session should still be able to claim after empty was rejected.
	if !svc.IsOnboardingSession("session-A") {
		t.Fatal("session-A should claim lock after empty ID was rejected")
	}
}

func TestReleaseOnboardingSessionNoopWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	// Should not panic when no lock is held.
	svc.ReleaseOnboardingSession("session-X")
}

func TestReleaseOnboardingSessionEmptyIDPreservesLock(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	// Session A claims onboarding lock.
	if !svc.IsOnboardingSession("session-A") {
		t.Fatal("session-A should claim onboarding lock")
	}

	// Releasing with empty session ID must be a no-op (guard in ReleaseOnboardingSession).
	svc.ReleaseOnboardingSession("")

	// Session A must still hold the lock.
	if !svc.IsOnboardingSession("session-A") {
		t.Fatal("session-A should still hold lock after ReleaseOnboardingSession(\"\")")
	}

	// Session B must still be rejected.
	if svc.IsOnboardingSession("session-B") {
		t.Fatal("session-B should be rejected — lock not released by empty ID")
	}
}

func TestIsOnboardingSessionDetectsManualDeletion(t *testing.T) {
	// Verifies that IsOnboardingSession() detects manual BOOTSTRAP.md deletion
	// without requiring BootstrapContent() to be called first. The stat check
	// inside IsOnboardingSession clears the stale cache immediately.
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	// Phase 1: Onboarding is active, session can claim lock.
	if !svc.IsOnboardingSession("session-A") {
		t.Fatal("phase 1: session-A should claim onboarding lock")
	}

	// Release session lock so we can re-test claiming after deletion.
	svc.ReleaseOnboardingSession("session-A")

	// Phase 2: Manually delete BOOTSTRAP.md (simulates user rm on disk).
	if err := os.Remove(filepath.Join(svc.awarenessDir, "BOOTSTRAP.md")); err != nil {
		t.Fatal(err)
	}

	// Phase 3: IsOnboardingSession should now return false immediately —
	// the stat check detects the missing file and clears the cache.
	if svc.IsOnboardingSession("session-B") {
		t.Fatal("phase 3: IsOnboardingSession should return false after manual BOOTSTRAP.md deletion")
	}

	// Phase 4: Confirm BootstrapContent also returns empty.
	content := svc.BootstrapContent()
	if content != "" {
		t.Fatalf("phase 4: BootstrapContent() = %q, want empty (file deleted)", content)
	}

	// Phase 5: Stays false for any new session.
	if svc.IsOnboardingSession("session-C") {
		t.Fatal("phase 5: IsOnboardingSession should remain false")
	}
}

func TestOnboardingLifecycleFullSequence(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	// Phase 1: onboarding active.
	if !svc.isOnboarding() {
		t.Fatal("phase 1: isOnboarding() = false, want true")
	}
	if content := svc.BootstrapContent(); content == "" {
		t.Fatal("phase 1: BootstrapContent() = empty, want non-empty")
	}

	// Phase 2: complete onboarding.
	if _, err := svc.CompleteOnboarding(); err != nil {
		t.Fatalf("phase 2: CompleteOnboarding() error = %v", err)
	}

	// Phase 3: verify all methods reflect completion.
	if svc.isOnboarding() {
		t.Fatal("phase 3: isOnboarding() = true, want false after CompleteOnboarding()")
	}
	if content := svc.BootstrapContent(); content != "" {
		t.Fatalf("phase 3: BootstrapContent() = %q, want empty after CompleteOnboarding()", content)
	}

	// Phase 4: simulate restart — scaffolding should NOT recreate BOOTSTRAP.md.
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatalf("phase 4: scaffoldCoreFiles() error = %v", err)
	}
	if svc.isOnboarding() {
		t.Fatal("phase 4: isOnboarding() = true after restart, want false (marker should prevent BOOTSTRAP.md recreation)")
	}
}

func TestConsumeLifecycleEventsReleasesLock(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	bus := eventbus.New()
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { svc.Shutdown(ctx) })

	// Session A claims onboarding lock.
	if !svc.IsOnboardingSession("session-A") {
		t.Fatal("session-A should claim onboarding lock")
	}

	// Session B is rejected.
	if svc.IsOnboardingSession("session-B") {
		t.Fatal("session-B should be rejected while session-A holds lock")
	}

	// Publish a lifecycle event: session-A stopped.
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager,
		eventbus.SessionLifecycleEvent{
			SessionID: "session-A",
			State:     eventbus.SessionStateStopped,
		},
	)

	// Wait for the consumer goroutine to process the event.
	// Use a ticker to avoid busy-spinning while keeping the wait bounded.
	// Generous timeout (5s) avoids flakes under -race detector overhead in CI.
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case <-timeout:
			t.Fatal("timeout: consumeLifecycleEvents did not release lock for session-A after lifecycle event")
		case <-ticker.C:
			if svc.IsOnboardingSession("session-B") {
				return // lock released and re-claimed by session-B
			}
		}
	}
}
