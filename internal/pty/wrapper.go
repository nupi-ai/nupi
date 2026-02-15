package pty

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ptyDevice "github.com/creack/pty"
	"github.com/nupi-ai/nupi/internal/procutil"
	"golang.org/x/crypto/ssh/terminal"
)

// StartOptions contains options for starting a PTY session.
// This struct is missing in tty-share - added per requirements.
type StartOptions struct {
	Command    string   // Command to execute
	Args       []string // Command arguments
	WorkingDir string   // Working directory (NEW - not in tty-share)
	Env        []string // Environment variables
	Rows       uint16   // Terminal rows
	Cols       uint16   // Terminal columns
}

// OutputSink is a generic interface for output consumers.
// Based on PTY-WRAPPER-ANALYZE recommendation for extensibility.
type OutputSink interface {
	Write([]byte) error
}

// ResizableSink is an optional interface for sinks that need resize events.
type ResizableSink interface {
	OutputSink
	WriteResize(rows, cols int) error
}

// Event represents a PTY lifecycle event.
type Event struct {
	Type      string // "process_started", "process_exited", "error"
	Timestamp time.Time
	PID       int
	ExitCode  int
	Error     error
	Data      map[string]interface{}
}

// Wrapper is our enhanced PTY wrapper with buffering and events.
// Extends tty-share's ptyMaster with required Nupi features.
type Wrapper struct {
	ptyFile           *os.File
	command           *exec.Cmd
	terminalInitState *terminal.State

	headless    bool
	currentRows atomic.Int32
	currentCols atomic.Int32

	outputBuffer  *bytes.Buffer
	bufferMutex   sync.RWMutex
	maxBufferSize int

	outputSinks []OutputSink
	sinksMutex  sync.RWMutex

	events       chan Event
	eventsMutex  sync.RWMutex
	eventsClosed bool

	commandMu    sync.RWMutex
	ptyCloseOnce sync.Once

	isRunning atomic.Bool
	exitCode  atomic.Int32
	waitOnce  sync.Once
	startTime time.Time
	pid       int
}

// New creates a new PTY wrapper.
func New() *Wrapper {
	return &Wrapper{
		outputBuffer:  bytes.NewBuffer(nil),
		maxBufferSize: 1024 * 1024,
		outputSinks:   make([]OutputSink, 0),
		events:        make(chan Event, 100),
	}
}

func shellEscape(s string) string {
	if strings.ContainsAny(s, " \t\n'\"\\$`!*?#~&|;<>()[]{}") {
		return "'" + strings.Replace(s, "'", "'\\''", -1) + "'"
	}
	return s
}

// Start starts a command in a PTY with the given options.
func (w *Wrapper) Start(opts StartOptions) error {
	command := opts.Command
	args := opts.Args

	_, pathErr := exec.LookPath(command)
	if pathErr != nil {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/bash"
		}

		checkCmd := exec.Command(shell, "-i", "-c", "command -v "+command)
		if opts.WorkingDir != "" {
			checkCmd.Dir = opts.WorkingDir
		}
		if output, err := checkCmd.Output(); err == nil && len(output) > 0 {
			shellCmd := command
			if len(args) > 0 {
				for _, arg := range args {
					shellCmd += " " + shellEscape(arg)
				}
			}

			command = shell
			args = []string{"-i", "-l", "-c", shellCmd}
		} else {
			return fmt.Errorf("command not found: %s", opts.Command)
		}
	}

	w.command = exec.Command(command, args...)

	if opts.WorkingDir != "" {
		w.command.Dir = opts.WorkingDir
	}

	if len(opts.Env) > 0 {
		w.command.Env = opts.Env
	} else {
		w.command.Env = os.Environ()
	}

	termSet := false
	langSet := false
	for _, env := range w.command.Env {
		if strings.HasPrefix(env, "TERM=") {
			termSet = true
		}
		if strings.HasPrefix(env, "LANG=") || strings.HasPrefix(env, "LC_ALL=") {
			langSet = true
		}
	}

	if !termSet {
		w.command.Env = append(w.command.Env, "TERM=xterm-256color")
	}
	if !langSet {
		w.command.Env = append(w.command.Env, "LANG=C.UTF-8")
	}

	var err error
	w.ptyFile, err = ptyDevice.Start(w.command)
	if err != nil {
		return err
	}

	cols := int(opts.Cols)
	rows := int(opts.Rows)
	if cols == 0 || rows == 0 {
		if !w.headless && terminal.IsTerminal(0) {
			cols, rows, _ = terminal.GetSize(0)
		} else {
			if cols == 0 {
				cols = 80
			}
			if rows == 0 {
				rows = 24
			}
		}
	}
	w.SetWinSize(rows, cols)

	w.isRunning.Store(true)
	w.exitCode.Store(-1)
	w.waitOnce = sync.Once{}
	w.ptyCloseOnce = sync.Once{}
	w.startTime = time.Now()
	if w.command.Process != nil {
		w.pid = w.command.Process.Pid
	}

	w.emitEvent(Event{
		Type:      "process_started",
		Timestamp: time.Now(),
		PID:       w.pid,
		Data: map[string]interface{}{
			"command": opts.Command,
			"args":    opts.Args,
		},
	})

	go w.captureWithBuffer()

	return nil
}

func (w *Wrapper) captureWithBuffer() {
	buffer := make([]byte, 4096)

	for {
		n, err := w.ptyFile.Read(buffer)
		if err != nil {
			w.closePTY()
			w.isRunning.Store(false)
			_ = w.reapProcess()

			exitCode := int(w.exitCode.Load())

			w.emitEvent(Event{
				Type:      "process_exited",
				Timestamp: time.Now(),
				PID:       w.pid,
				ExitCode:  exitCode,
				Error:     err,
			})
			w.eventsMutex.Lock()
			if !w.eventsClosed {
				close(w.events)
				w.eventsClosed = true
			}
			w.eventsMutex.Unlock()
			return
		}

		w.bufferMutex.Lock()
		if w.outputBuffer.Len()+n > w.maxBufferSize {
			excess := w.outputBuffer.Len() + n - w.maxBufferSize
			w.outputBuffer.Next(excess)
		}
		w.outputBuffer.Write(buffer[:n])
		w.bufferMutex.Unlock()

		w.broadcastToSinks(buffer[:n])
	}
}

func (w *Wrapper) broadcastToSinks(data []byte) {
	w.sinksMutex.RLock()
	defer w.sinksMutex.RUnlock()

	for _, sink := range w.outputSinks {
		sink.Write(data)
	}
}

// AddSink adds an output sink.
func (w *Wrapper) AddSink(sink OutputSink) {
	w.sinksMutex.Lock()
	defer w.sinksMutex.Unlock()
	w.outputSinks = append(w.outputSinks, sink)
	log.Printf("[PTY] Added sink to session, total sinks: %d", len(w.outputSinks))
}

// RemoveSink removes an output sink.
func (w *Wrapper) RemoveSink(sink OutputSink) {
	w.sinksMutex.Lock()
	defer w.sinksMutex.Unlock()

	initialCount := len(w.outputSinks)
	for i, s := range w.outputSinks {
		if s == sink {
			w.outputSinks = append(w.outputSinks[:i], w.outputSinks[i+1:]...)
			log.Printf("[PTY] Removed sink from session, sinks: %d -> %d", initialCount, len(w.outputSinks))
			return
		}
	}
	log.Printf("[PTY] WARNING: Sink not found for removal, current sinks: %d", len(w.outputSinks))
}

// GetSinkCount returns the number of active output sinks.
func (w *Wrapper) GetSinkCount() int {
	w.sinksMutex.RLock()
	defer w.sinksMutex.RUnlock()
	return len(w.outputSinks)
}

// Write sends data to the PTY.
func (w *Wrapper) Write(data []byte) (int, error) {
	if w.ptyFile == nil {
		return 0, io.ErrClosedPipe
	}
	return w.ptyFile.Write(data)
}

// Read reads from the PTY (direct, not from buffer).
func (w *Wrapper) Read(p []byte) (int, error) {
	if w.ptyFile == nil {
		return 0, io.ErrClosedPipe
	}
	return w.ptyFile.Read(p)
}

// GetBuffer returns the current output buffer content.
func (w *Wrapper) GetBuffer() []byte {
	w.bufferMutex.RLock()
	defer w.bufferMutex.RUnlock()

	if w.outputBuffer.Len() == 0 {
		return nil
	}

	return append([]byte(nil), w.outputBuffer.Bytes()...)
}

// SetWinSize sets the PTY window size.
func (w *Wrapper) SetWinSize(rows, cols int) error {
	if w.ptyFile == nil {
		return io.ErrClosedPipe
	}

	winSize := ptyDevice.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	}

	if err := ptyDevice.Setsize(w.ptyFile, &winSize); err != nil {
		return err
	}

	w.currentRows.Store(int32(rows))
	w.currentCols.Store(int32(cols))

	w.notifyResize(rows, cols)

	return nil
}

// GetWinSize returns the current PTY window size.
func (w *Wrapper) GetWinSize() (rows, cols int) {
	return int(w.currentRows.Load()), int(w.currentCols.Load())
}

func (w *Wrapper) notifyResize(rows, cols int) {
	w.sinksMutex.RLock()
	defer w.sinksMutex.RUnlock()

	for _, sink := range w.outputSinks {
		if resizable, ok := sink.(ResizableSink); ok {
			resizable.WriteResize(rows, cols)
		}
	}
}

// closePTY closes the PTY file descriptor exactly once.
// This unblocks any goroutine blocked in ptyFile.Read() and releases the fd.
func (w *Wrapper) closePTY() {
	w.ptyCloseOnce.Do(func() {
		if w.ptyFile != nil {
			w.ptyFile.Close()
		}
	})
}

// isExpectedTerminationError reports whether err is a normal process exit
// caused by graceful termination (SIGTERM on Unix, TerminateProcess on Windows).
// This is called only after GracefulTerminate succeeded, so any ExitError is expected.
func isExpectedTerminationError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// Stop stops the PTY process gracefully with timeout.
func (w *Wrapper) Stop(timeout time.Duration) error {
	if !w.isRunning.Load() {
		return nil
	}

	// Close the PTY fd after process cleanup to unblock any Read()
	// blocked in captureWithBuffer and release the file descriptor.
	defer w.closePTY()

	w.commandMu.RLock()
	cmd := w.command
	w.commandMu.RUnlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	proc := cmd.Process
	if proc == nil {
		return nil
	}

	if err := procutil.GracefulTerminate(proc); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- w.reapProcess()
	}()

	select {
	case err := <-done:
		w.isRunning.Store(false)
		w.eventsMutex.Lock()
		if !w.eventsClosed {
			close(w.events)
			w.eventsClosed = true
		}
		w.eventsMutex.Unlock()
		if err != nil && isExpectedTerminationError(err) {
			return nil
		}
		return err
	case <-time.After(timeout):
		w.commandMu.RLock()
		cmd := w.command
		w.commandMu.RUnlock()
		if cmd == nil || cmd.Process == nil {
			return nil
		}
		proc := cmd.Process
		if proc == nil {
			return nil
		}
		if err := proc.Kill(); err != nil {
			return err
		}
		w.isRunning.Store(false)
		w.eventsMutex.Lock()
		if !w.eventsClosed {
			close(w.events)
			w.eventsClosed = true
		}
		w.eventsMutex.Unlock()
		err := <-done
		if err != nil && isExpectedTerminationError(err) {
			return nil
		}
		return err
	}
}

func (w *Wrapper) reapProcess() error {
	var waitErr error
	w.waitOnce.Do(func() {
		w.commandMu.Lock()
		defer w.commandMu.Unlock()

		if w.command == nil {
			w.exitCode.Store(-1)
			return
		}

		waitErr = w.command.Wait()

		if state := w.command.ProcessState; state != nil {
			w.exitCode.Store(int32(state.ExitCode()))
		} else {
			w.exitCode.Store(-1)
		}
	})
	return waitErr
}

// IsRunning returns whether the PTY process is running.
func (w *Wrapper) IsRunning() bool {
	return w.isRunning.Load()
}

// GetPID returns the process ID.
func (w *Wrapper) GetPID() int {
	return w.pid
}

// GetExitCode returns the exit code (or -1 if still running).
func (w *Wrapper) GetExitCode() (int, error) {
	if w.isRunning.Load() {
		return -1, nil
	}
	return int(w.exitCode.Load()), nil
}

// Events returns the event channel.
func (w *Wrapper) Events() <-chan Event {
	w.eventsMutex.RLock()
	defer w.eventsMutex.RUnlock()
	return w.events
}

// emitEvent sends an event to the channel.
func (w *Wrapper) emitEvent(event Event) {
	w.eventsMutex.RLock()
	defer w.eventsMutex.RUnlock()

	if w.eventsClosed {
		return
	}

	select {
	case w.events <- event:
	default:
	}
}

// MakeRaw sets the terminal to raw mode.
func (w *Wrapper) MakeRaw() error {
	if w.headless || !terminal.IsTerminal(0) {
		return nil
	}

	var err error
	w.terminalInitState, err = terminal.MakeRaw(0)
	return err
}

// Restore restores the terminal from raw mode.
func (w *Wrapper) Restore() {
	if w.terminalInitState != nil {
		terminal.Restore(0, w.terminalInitState)
		w.terminalInitState = nil
	}
}
