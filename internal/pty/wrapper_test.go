package pty_test

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/pty"
)

func requireEventually(t *testing.T, cond func() bool, timeout time.Duration, step time.Duration, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s", message)
		}
		time.Sleep(step)
	}
}

func TestWrapperCapturesOutputAndEmitsEvents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY wrapper tests rely on POSIX shell")
	}

	w := pty.New()
	opts := pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "printf foo"},
	}

	if err := w.Start(opts); err != nil {
		t.Fatalf("failed to start PTY: %v", err)
	}

	events := w.Events()

	startEvent := <-events
	if startEvent.Type != "process_started" {
		t.Fatalf("expected process_started event, got %q", startEvent.Type)
	}

	exitEvent := <-events
	if exitEvent.Type != "process_exited" {
		t.Fatalf("expected process_exited event, got %q", exitEvent.Type)
	}

	if _, ok := <-events; ok {
		t.Fatalf("expected events channel to be closed")
	}

	output := string(w.GetBuffer())
	if !strings.Contains(output, "foo") {
		t.Fatalf("expected output buffer to contain 'foo', got %q", output)
	}

	if _, err := w.GetExitCode(); err != nil {
		t.Fatalf("unexpected error retrieving exit code: %v", err)
	}
}

func TestWrapperStopTerminatesProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY wrapper tests rely on POSIX shell")
	}

	w := pty.New()
	opts := pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 5"},
	}

	if err := w.Start(opts); err != nil {
		t.Fatalf("failed to start PTY: %v", err)
	}

	if err := w.Stop(500 * time.Millisecond); err != nil {
		t.Fatalf("stop returned error: %v", err)
	}

	requireEventually(t, func() bool { return !w.IsRunning() }, time.Second, 50*time.Millisecond, "process did not stop")

	if _, err := w.GetExitCode(); err != nil {
		t.Fatalf("failed to get exit code: %v", err)
	}
}
