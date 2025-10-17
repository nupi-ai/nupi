package modules

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

var (
	// ErrRunnerBinaryUnset indicates the adapter-runner binary path was not configured.
	ErrRunnerBinaryUnset = errors.New("modules: adapter-runner binary path is empty")
	// ErrRunnerBinaryMissing indicates the adapter-runner binary does not exist.
	ErrRunnerBinaryMissing = errors.New("modules: adapter-runner binary not found")
)

type execLauncher struct{}

func (execLauncher) Launch(ctx context.Context, binary string, args []string, env []string) (ProcessHandle, error) {
	if strings.TrimSpace(binary) == "" {
		return nil, ErrRunnerBinaryUnset
	}
	if _, err := os.Stat(binary); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrRunnerBinaryMissing, binary)
		}
		return nil, fmt.Errorf("modules: stat adapter-runner: %w", err)
	}

	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, binary, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("modules: start adapter-runner: %w", err)
	}

	handle := &execHandle{
		cmd:    cmd,
		cancel: cancel,
		done:   make(chan error, 1),
	}
	go handle.wait()
	return handle, nil
}

type execHandle struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan error
}

func (h *execHandle) wait() {
	err := h.cmd.Wait()
	h.done <- err
	close(h.done)
}

func (h *execHandle) Stop(ctx context.Context) error {
	h.cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-h.done:
		if err == nil {
			return nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil
		}
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
}

func (h *execHandle) PID() int {
	if h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}
