package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// StartOptions customises how the adapter process is launched.
type StartOptions struct {
	Stdout io.Writer
	Stderr io.Writer
	Env    []string // optional override. When empty, parent environment is used.
}

// AdapterProcess exposes control primitives for the launched adapter.
type AdapterProcess struct {
	cmd         *exec.Cmd
	stdoutDone  chan struct{}
	stderrDone  chan struct{}
	waitOnce    sync.Once
	waitErr     error
	waitCh      chan error
	terminateMu sync.Mutex
}

// Start launches the adapter command described by cfg.
func Start(ctx context.Context, cfg Config, opts *StartOptions) (*AdapterProcess, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	if err := ensureDir(cfg.AdapterHome, 0o755); err != nil {
		return nil, fmt.Errorf("adapter-runner: ensure adapter home: %w", err)
	}
	if err := ensureDir(cfg.AdapterDataDir, 0o755); err != nil {
		return nil, fmt.Errorf("adapter-runner: ensure adapter data dir: %w", err)
	}

	// Determine actual command and args based on runtime
	command := cfg.Command
	args := cfg.Args
	if cfg.Runtime == "js" {
		// Use Nupi-provided JS runtime (currently Bun)
		jsRuntime := resolveJSRuntime()
		args = append([]string{"run", command}, args...)
		command = jsRuntime
	}

	cmd := exec.CommandContext(ctx, command, args...)
	if cfg.AdapterHome != "" {
		cmd.Dir = cfg.AdapterHome
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
		return nil, fmt.Errorf("adapter-runner: start adapter: %w", err)
	}

	proc := &AdapterProcess{
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

// Wait blocks until the adapter exits and returns the underlying error.
func (p *AdapterProcess) Wait() error {
	if p == nil {
		return errors.New("adapter-runner: nil process")
	}
	err, ok := <-p.waitCh
	if !ok {
		return p.waitErr
	}
	return err
}

// Terminate sends the provided signal to the adapter and waits for graceful
// shutdown. If the process does not exit within grace, it is killed forcibly.
func (p *AdapterProcess) Terminate(sig os.Signal, grace time.Duration) error {
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
			return fmt.Errorf("adapter-runner: kill adapter: %w", err)
		}
		<-p.waitCh
		return p.waitErr
	}
}

// Pid returns the OS process identifier.
func (p *AdapterProcess) Pid() int {
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

// resolveJSRuntime returns path to the JS runtime.
// Currently uses Bun, but this is an implementation detail hidden from plugins.
// Plugins declare "runtime: js" and Nupi decides which runtime to use.
func resolveJSRuntime() string {
	// 1. Check NUPI_JS_RUNTIME env (for testing/override)
	if path := os.Getenv("NUPI_JS_RUNTIME"); path != "" {
		return path
	}
	// 2. Check bundled location in NUPI_HOME
	if home := os.Getenv("NUPI_HOME"); home != "" {
		bundled := filepath.Join(home, "bin", "bun")
		if _, err := os.Stat(bundled); err == nil {
			return bundled
		}
	}
	// 3. Fall back to system bun (assumed in PATH)
	return "bun"
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
