package session

import (
	"os"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/pty"
)

type fakeSink struct{ data []byte }

func (f *fakeSink) Write(b []byte) error {
	f.data = append(f.data, b...)
	return nil
}

func TestManagerStatusTransitions(t *testing.T) {
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
