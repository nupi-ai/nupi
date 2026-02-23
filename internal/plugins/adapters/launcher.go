package adapters

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/procutil"
)

// DefaultGracefulShutdownTimeout is the maximum time to wait for adapter process
// to exit after graceful termination before force-killing.
const DefaultGracefulShutdownTimeout = constants.AdapterGracefulShutdownTimeout

var (
	// ErrAdapterBinaryUnset indicates the adapter binary path was not configured.
	ErrAdapterBinaryUnset = errors.New("adapters: adapter binary path is empty")
	// ErrAdapterBinaryMissing indicates the adapter binary does not exist.
	ErrAdapterBinaryMissing = errors.New("adapters: adapter binary not found")
	// ErrAdapterKilled indicates the adapter was forcefully killed after timeout.
	ErrAdapterKilled = errors.New("adapters: adapter killed after graceful shutdown timeout")
)

type execLauncher struct{}

func (execLauncher) Launch(ctx context.Context, binary string, args []string, env []string, stdout io.Writer, stderr io.Writer, workingDir string) (ProcessHandle, error) {
	if strings.TrimSpace(binary) == "" {
		return nil, ErrAdapterBinaryUnset
	}
	if _, err := os.Stat(binary); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrAdapterBinaryMissing, binary)
		}
		return nil, fmt.Errorf("adapters: stat adapter binary: %w", err)
	}

	// Note: We don't use exec.CommandContext here because we need to control
	// signal delivery for graceful shutdown (SIGTERM → SIGKILL).
	cmd := exec.Command(binary, args...)
	if workingDir != "" {
		cmd.Dir = workingDir
	}
	if stdout == nil {
		cmd.Stdout = io.Discard
	} else {
		cmd.Stdout = stdout
	}
	if stderr == nil {
		cmd.Stderr = io.Discard
	} else {
		cmd.Stderr = stderr
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("adapters: start adapter: %w", err)
	}

	handle := &execHandle{
		cmd:  cmd,
		done: make(chan error, 1),
	}
	go handle.wait()
	return handle, nil
}

type execHandle struct {
	cmd  *exec.Cmd
	done chan error
}

func (h *execHandle) wait() {
	err := h.cmd.Wait()
	h.done <- err
	close(h.done)
}

func (h *execHandle) Stop(ctx context.Context) error {
	return h.StopWithTimeout(ctx, DefaultGracefulShutdownTimeout)
}

// StopWithTimeout stops the adapter process with a custom graceful shutdown timeout.
func (h *execHandle) StopWithTimeout(ctx context.Context, gracefulTimeout time.Duration) error {
	if h.cmd.Process == nil {
		return nil
	}

	pid := h.cmd.Process.Pid

	// Check if process has already exited
	select {
	case err := <-h.done:
		return normalizeExitError(err, false)
	default:
	}

	// Request graceful shutdown (SIGTERM on Unix, TerminateProcess on Windows)
	if err := procutil.GracefulTerminate(h.cmd.Process); err != nil {
		// Process may have already exited
		if errors.Is(err, os.ErrProcessDone) {
			select {
			case err := <-h.done:
				return normalizeExitError(err, false)
			default:
				return nil
			}
		}
		// Fall through to force-kill if graceful termination fails
	}

	// Respect context deadline if it's shorter than our timeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < gracefulTimeout {
			gracefulTimeout = remaining
		}
	}

	gracefulTimer := time.NewTimer(gracefulTimeout)
	defer gracefulTimer.Stop()

	select {
	case err := <-h.done:
		return normalizeExitError(err, false)
	case <-gracefulTimer.C:
		log.Printf("[Adapters] adapter pid=%d did not exit within %v after graceful termination, force-killing", pid, gracefulTimeout)
	case <-ctx.Done():
		log.Printf("[Adapters] context cancelled while stopping adapter pid=%d, force-killing", pid)
	}

	// Force-kill the process
	if err := h.cmd.Process.Kill(); err != nil {
		if !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("adapters: kill adapter: %w", err)
		}
	}

	// Wait for process to finish after force-kill
	select {
	case err := <-h.done:
		return normalizeExitError(err, true)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// normalizeExitError converts expected exit errors to appropriate return values.
// If forceKilled is true, it returns ErrAdapterKilled to indicate the adapter
// did not shut down gracefully and had to be killed.
func normalizeExitError(err error, forceKilled bool) error {
	if err == nil {
		if forceKilled {
			return ErrAdapterKilled
		}
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if forceKilled {
			return ErrAdapterKilled
		}
		// Normal exit (even with non-zero status) during shutdown is ok
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (h *execHandle) PID() int {
	if h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}
