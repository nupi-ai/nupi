package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// StartOptions customises how the module process is launched.
type StartOptions struct {
	Stdout io.Writer
	Stderr io.Writer
	Env    []string // optional override. When empty, parent environment is used.
}

// ModuleProcess exposes control primitives for the launched module.
type ModuleProcess struct {
	cmd         *exec.Cmd
	stdoutDone  chan struct{}
	stderrDone  chan struct{}
	waitOnce    sync.Once
	waitErr     error
	waitCh      chan error
	terminateMu sync.Mutex
}

// Start launches the module command described by cfg.
func Start(ctx context.Context, cfg Config, opts *StartOptions) (*ModuleProcess, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	if err := ensureDir(cfg.ModuleHome, 0o755); err != nil {
		return nil, fmt.Errorf("adapter-runner: ensure module home: %w", err)
	}
	if err := ensureDir(cfg.ModuleDataDir, 0o755); err != nil {
		return nil, fmt.Errorf("adapter-runner: ensure module data dir: %w", err)
	}

	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	if cfg.ModuleHome != "" {
		cmd.Dir = cfg.ModuleHome
	}

	env := cfg.RawEnvironment
	if opts != nil && len(opts.Env) > 0 {
		env = opts.Env
	}
	cmd.Env = env

	var stdout io.Writer = os.Stdout
	var stderr io.Writer = os.Stderr
	if opts != nil {
		if opts.Stdout != nil {
			stdout = opts.Stdout
		}
		if opts.Stderr != nil {
			stderr = opts.Stderr
		}
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("adapter-runner: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("adapter-runner: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("adapter-runner: start module: %w", err)
	}

	proc := &ModuleProcess{
		cmd:        cmd,
		stdoutDone: make(chan struct{}),
		stderrDone: make(chan struct{}),
		waitCh:     make(chan error, 1),
	}

	go func() {
		_, _ = io.Copy(stdout, stdoutPipe)
		close(proc.stdoutDone)
	}()

	go func() {
		_, _ = io.Copy(stderr, stderrPipe)
		close(proc.stderrDone)
	}()

	go func() {
		err := cmd.Wait()
		proc.waitOnce.Do(func() {
			proc.waitErr = err
		})
		<-proc.stdoutDone
		<-proc.stderrDone
		proc.waitCh <- err
		close(proc.waitCh)
	}()

	return proc, nil
}

// Wait blocks until the module exits and returns the underlying error.
func (p *ModuleProcess) Wait() error {
	if p == nil {
		return errors.New("adapter-runner: nil process")
	}
	err, ok := <-p.waitCh
	if !ok {
		return p.waitErr
	}
	return err
}

// Terminate sends the provided signal to the module and waits for graceful
// shutdown. If the process does not exit within grace, it is killed forcibly.
func (p *ModuleProcess) Terminate(sig os.Signal, grace time.Duration) error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}

	p.terminateMu.Lock()
	defer p.terminateMu.Unlock()

	// Process already finished.
	select {
	case <-p.waitCh:
		return p.waitErr
	default:
	}

	if sig != nil {
		_ = p.cmd.Process.Signal(sig)
	}

	if grace <= 0 {
		grace = defaultGracePeriod
	}

	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-p.waitCh:
		return p.waitErr
	case <-timer.C:
		// Force kill.
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("adapter-runner: kill module: %w", err)
		}
		<-p.waitCh
		return p.waitErr
	}
}

// Pid returns the OS process identifier.
func (p *ModuleProcess) Pid() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// ExitCode extracts the exit code from a Wait error.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
		return exitErr.ExitCode()
	}
	return 1
}

func ensureDir(path string, perm os.FileMode) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s exists but is not a directory", path)
		}
		return nil
	}
	return os.MkdirAll(path, perm)
}
