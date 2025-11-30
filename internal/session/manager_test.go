package session

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/pty"
)

// skipIfNoPTY skips the test if PTY operations are not available
// (e.g., in sandboxed environments, containers without /dev/ptmx access).
func skipIfNoPTY(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests not supported on Windows")
	}
	// Try to start a minimal PTY using /bin/sh to match actual test commands
	p := pty.New()
	err := p.Start(pty.StartOptions{Command: "/bin/sh", Args: []string{"-c", "exit 0"}})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "operation not permitted") ||
			strings.Contains(msg, "permission denied") ||
			strings.Contains(msg, "no such file or directory") ||
			strings.Contains(msg, "failed to start") {
			t.Skipf("PTY not available in this environment: %v", err)
		}
		// Other errors might be real issues, don't skip
	}
	if p != nil {
		p.Stop(100 * time.Millisecond)
	}
}

// isPTYError checks if an error is PTY-related and should cause test skip
func isPTYError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "operation not permitted") ||
		strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "no such file or directory") ||
		strings.Contains(msg, "failed to start PTY")
}

type fakeSink struct{ data []byte }

func (f *fakeSink) Write(b []byte) error {
	f.data = append(f.data, b...)
	return nil
}

func TestManagerStatusTransitions(t *testing.T) {
	skipIfNoPTY(t)

	tmpHome := t.TempDir()
	oldHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", tmpHome); err != nil {
		t.Fatalf("failed to set HOME: %v", err)
	}
	t.Cleanup(func() {
		os.Setenv("HOME", oldHome)
	})

	m := NewManager()

	// Use a PTY wrapper that keeps running so detach can observe status
	sess, err := m.CreateSession(pty.StartOptions{Command: "/bin/sh", Args: []string{"-c", "sleep 1"}}, false)
	if err != nil {
		if isPTYError(err) {
			t.Skipf("PTY not available: %v", err)
		}
		t.Fatalf("CreateSession failed: %v", err)
	}

	if status := sess.CurrentStatus(); status != StatusRunning {
		t.Fatalf("expected status running, got %s", status)
	}

	sink := &fakeSink{}
	if err := m.AttachToSession(sess.ID, sink, true); err != nil {
		t.Fatalf("AttachToSession failed: %v", err)
	}

	if status := sess.CurrentStatus(); status != StatusRunning {
		t.Fatalf("expected status running after attach, got %s", status)
	}

	if err := m.DetachFromSession(sess.ID, sink); err != nil {
		t.Fatalf("DetachFromSession failed: %v", err)
	}

	if status := sess.CurrentStatus(); status != StatusDetached {
		t.Fatalf("expected status detached after detach, got %s", status)
	}

	if err := m.KillSession(sess.ID); err != nil {
		t.Fatalf("KillSession failed: %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		if sess.CurrentStatus() == StatusStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session did not reach stopped status")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
