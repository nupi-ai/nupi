package procutil

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestIsProcessAlive_Self(t *testing.T) {
	if !IsProcessAlive(os.Getpid()) {
		t.Fatal("IsProcessAlive should return true for own process")
	}
}

func TestIsProcessAlive_InvalidPID(t *testing.T) {
	// Use a very large PID that is well beyond any realistic pid_max on any OS.
	if IsProcessAlive(1<<30 - 1) {
		t.Fatal("IsProcessAlive should return false for non-existent PID")
	}
}

// longRunningCmd returns a cross-platform exec.Cmd that blocks until killed.
func longRunningCmd() *exec.Cmd {
	if runtime.GOOS == "windows" {
		// "waitfor" blocks indefinitely (signal name will never arrive).
		return exec.Command("waitfor", "NupiTestSignalNeverSent", "/T", "300")
	}
	return exec.Command("sleep", "300")
}

func TestGracefulTerminate(t *testing.T) {
	cmd := longRunningCmd()
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start subprocess: %v", err)
	}

	if err := GracefulTerminate(cmd.Process); err != nil {
		t.Fatalf("GracefulTerminate returned error: %v", err)
	}

	// Wait for the process to exit so we don't leave zombies.
	_ = cmd.Wait()

	// Give OS a moment to reap the process.
	time.Sleep(50 * time.Millisecond)

	if IsProcessAlive(cmd.Process.Pid) {
		t.Fatal("process should not be alive after GracefulTerminate")
	}
}

func TestTerminateByPID(t *testing.T) {
	cmd := longRunningCmd()
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start subprocess: %v", err)
	}
	pid := cmd.Process.Pid

	if err := TerminateByPID(pid); err != nil {
		t.Fatalf("TerminateByPID returned error: %v", err)
	}

	_ = cmd.Wait()

	// Give OS a moment to reap the process.
	time.Sleep(50 * time.Millisecond)

	if IsProcessAlive(pid) {
		t.Fatal("process should not be alive after TerminateByPID")
	}
}
