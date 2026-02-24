// Package jsruntime provides a persistent JavaScript runtime using Bun.
// It manages a subprocess that executes JS code via JSON-RPC over an IPC
// channel with length-prefixed framing, enabling fast (<5ms) JS function
// calls without per-call process spawn overhead.
//
// On Unix the IPC channel is a Unix domain socket. On Windows it uses
// TCP on localhost (127.0.0.1) with a dynamic port.
package jsruntime

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nupi-ai/nupi/internal/jsrunner"
)

//go:embed host.js
var embeddedHostScript []byte

var (
	// ErrRuntimeNotStarted indicates the runtime subprocess is not running.
	ErrRuntimeNotStarted = errors.New("jsruntime: runtime not started")
	// ErrRuntimeStopped indicates the runtime subprocess has been stopped.
	ErrRuntimeStopped = errors.New("jsruntime: runtime stopped")
	// ErrTimeout indicates a call exceeded the specified timeout.
	ErrTimeout = errors.New("jsruntime: timeout")
	// ErrPluginNotLoaded indicates the plugin is not loaded in the runtime.
	// This can happen after a runtime restart.
	ErrPluginNotLoaded = errors.New("jsruntime: plugin not loaded")
)

// IsPluginNotLoadedError checks if an error indicates a plugin is not loaded.
// This can be used to trigger plugin reload and retry.
func IsPluginNotLoadedError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrPluginNotLoaded) {
		return true
	}
	// Also check for the error message from host.js
	return strings.Contains(err.Error(), "Plugin not loaded")
}

// Request represents a JSON-RPC request to the JS runtime.
type Request struct {
	ID     uint64         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
}

// Response represents a JSON-RPC response from the JS runtime.
type Response struct {
	ID     uint64 `json:"id"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// PluginMetadata holds metadata extracted from a loaded JS plugin.
type PluginMetadata struct {
	Name         string   `json:"name"`
	Commands     []string `json:"commands"`
	Icon         string   `json:"icon,omitempty"`
	HasTransform bool     `json:"hasTransform"`
	HasDetect    bool     `json:"hasDetect"`
	// Tool processor capabilities (SESSION_OUTPUT flow)
	HasDetectIdleState bool `json:"hasDetectIdleState"`
	HasClean           bool `json:"hasClean"`
	HasExtractEvents   bool `json:"hasExtractEvents"`
}

// HasFunction checks if a function exists in the plugin metadata.
func (m *PluginMetadata) HasFunction(name string) bool {
	switch name {
	case "transform":
		return m.HasTransform
	case "detect":
		return m.HasDetect
	case "detectIdleState":
		return m.HasDetectIdleState
	case "clean":
		return m.HasClean
	case "extractEvents":
		return m.HasExtractEvents
	default:
		return false
	}
}

// LoadPluginOptions configures plugin loading behavior.
type LoadPluginOptions struct {
	// RequireFunctions specifies function names that must be present in the plugin.
	// If any are missing, LoadPlugin will return an error.
	RequireFunctions []string
}

// Runtime manages a persistent Bun subprocess for fast JS execution.
type Runtime struct {
	cmd    *exec.Cmd
	conn   net.Conn
	reader *bufio.Reader
	stderr io.ReadCloser
	stdout io.ReadCloser

	socketPath string // IPC address: UDS path (Unix) or TCP addr (Windows)

	mu        sync.Mutex
	requestID atomic.Uint64
	pending   map[uint64]chan Response
	pendingMu sync.Mutex

	stopped atomic.Bool
	done    chan struct{}
	errOnce sync.Once
	err     error

	logger        Logger
	stdoutWriter  io.Writer
	stderrWriter  io.Writer
	hostScript    string // path to host.js (temp file if embedded)
	cleanupScript bool   // whether to remove hostScript on shutdown
	runDir        string // directory for IPC sockets
}

// Logger is an optional interface for logging runtime events.
type Logger interface {
	Printf(format string, v ...any)
}

// Option configures the Runtime.
type Option func(*Runtime)

// WithLogger sets a logger for the runtime.
func WithLogger(logger Logger) Option {
	return func(r *Runtime) {
		r.logger = logger
	}
}

// WithStdoutWriter captures stdout output from the JS runtime process.
func WithStdoutWriter(w io.Writer) Option {
	return func(r *Runtime) {
		r.stdoutWriter = w
	}
}

// WithStderrWriter captures stderr output from the JS runtime process.
func WithStderrWriter(w io.Writer) Option {
	return func(r *Runtime) {
		r.stderrWriter = w
	}
}

// WithRunDir sets the base directory for IPC socket files (Unix only).
// On Windows this option is ignored since TCP localhost is used instead.
// If not set, os.TempDir() is used.
func WithRunDir(dir string) Option {
	return func(r *Runtime) {
		r.runDir = dir
	}
}

// New starts a persistent Bun subprocess running the host script.
// If hostScript is empty, uses the embedded host.js script.
// If bunPath is empty, resolves the JS runtime path from bundled location or PATH.
func New(ctx context.Context, bunPath, hostScript string, opts ...Option) (*Runtime, error) {
	if bunPath == "" {
		var err error
		bunPath, err = jsrunner.GetRuntimePath()
		if err != nil {
			return nil, fmt.Errorf("jsruntime: %w", err)
		}
	}

	cleanupScript := false
	if hostScript == "" {
		// Use embedded host.js - write to temp file
		tmpFile, err := os.CreateTemp("", "nupi-host-*.js")
		if err != nil {
			return nil, fmt.Errorf("jsruntime: create temp host script: %w", err)
		}
		if _, err := tmpFile.Write(embeddedHostScript); err != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
			return nil, fmt.Errorf("jsruntime: write temp host script: %w", err)
		}
		if err := tmpFile.Close(); err != nil {
			os.Remove(tmpFile.Name())
			return nil, fmt.Errorf("jsruntime: close temp host script: %w", err)
		}
		hostScript = tmpFile.Name()
		cleanupScript = true
	}

	r := &Runtime{
		pending:       make(map[uint64]chan Response),
		done:          make(chan struct{}),
		hostScript:    hostScript,
		cleanupScript: cleanupScript,
	}

	for _, opt := range opts {
		opt(r)
	}

	// Determine socket directory.
	runDir := r.runDir
	if runDir == "" {
		runDir = os.TempDir()
	}

	// Create IPC socket before starting the process.
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()%1e9)
	socketPath, listener, err := createIPCSocket(runDir, suffix)
	if err != nil {
		if cleanupScript {
			os.Remove(hostScript)
		}
		return nil, err
	}
	r.socketPath = socketPath

	cmd := exec.CommandContext(ctx, bunPath, "run", hostScript)
	cmd.Env = append(os.Environ(), "NUPI_IPC_SOCKET="+socketPath)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		listener.Close()
		cleanupSocket(socketPath)
		if cleanupScript {
			os.Remove(hostScript)
		}
		return nil, fmt.Errorf("jsruntime: stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		listener.Close()
		cleanupSocket(socketPath)
		stdout.Close()
		if cleanupScript {
			os.Remove(hostScript)
		}
		return nil, fmt.Errorf("jsruntime: stderr pipe: %w", err)
	}

	r.cmd = cmd
	r.stderr = stderr
	r.stdout = stdout

	if err := cmd.Start(); err != nil {
		listener.Close()
		cleanupSocket(socketPath)
		stdout.Close()
		stderr.Close()
		if cleanupScript {
			os.Remove(hostScript)
		}
		return nil, fmt.Errorf("jsruntime: start bun: %w", err)
	}

	// Wait for the JS process to connect to our socket.
	conn, err := acceptSingleConn(listener, acceptTimeout)
	if err != nil {
		cmd.Process.Kill()
		cleanupSocket(socketPath)
		if cleanupScript {
			os.Remove(hostScript)
		}
		return nil, err
	}
	r.conn = conn
	r.reader = bufio.NewReader(conn)

	go r.readResponses()
	go r.readStdout()
	go r.readStderr()
	go r.waitProcess()

	return r, nil
}

// readResponses reads length-prefixed frames from the IPC socket and dispatches
// to pending requests.
func (r *Runtime) readResponses() {
	for {
		data, err := readFrame(r.reader)
		if err != nil {
			if !r.stopped.Load() {
				r.setError(fmt.Errorf("jsruntime: read IPC frame: %w", err))
			}
			return
		}

		var resp Response
		if err := json.Unmarshal(data, &resp); err != nil {
			if r.logger != nil {
				r.logger.Printf("[jsruntime] invalid response: %s", string(data))
			}
			continue
		}

		r.pendingMu.Lock()
		ch, ok := r.pending[resp.ID]
		if ok {
			delete(r.pending, resp.ID)
		}
		r.pendingMu.Unlock()

		if ok {
			ch <- resp
			close(ch)
		}
	}
}

// readStdout forwards stdout output from the Bun process.
func (r *Runtime) readStdout() {
	if r.stdout == nil {
		return
	}
	buf := make([]byte, 4096)
	for {
		n, err := r.stdout.Read(buf)
		if n > 0 && r.stdoutWriter != nil {
			_, _ = r.stdoutWriter.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// readStderr logs stderr output from the Bun process.
func (r *Runtime) readStderr() {
	buf := make([]byte, 4096)
	for {
		n, err := r.stderr.Read(buf)
		if n > 0 {
			if r.stderrWriter != nil {
				_, _ = r.stderrWriter.Write(buf[:n])
			}
			if r.logger != nil {
				r.logger.Printf("[jsruntime:stderr] %s", string(buf[:n]))
			}
		}
		if err != nil {
			return
		}
	}
}

// waitProcess waits for the subprocess to exit.
func (r *Runtime) waitProcess() {
	err := r.cmd.Wait()
	r.stopped.Store(true)
	if err != nil {
		r.setError(fmt.Errorf("jsruntime: process exited: %w", err))
	}
	close(r.done)
}

func (r *Runtime) setError(err error) {
	r.errOnce.Do(func() {
		r.err = err
	})
}

// call sends a request and waits for response with timeout.
func (r *Runtime) call(ctx context.Context, method string, params map[string]any) (any, error) {
	if r.stopped.Load() {
		return nil, ErrRuntimeStopped
	}

	id := r.requestID.Add(1)
	req := Request{
		ID:     id,
		Method: method,
		Params: params,
	}

	respCh := make(chan Response, 1)
	r.pendingMu.Lock()
	r.pending[id] = respCh
	r.pendingMu.Unlock()

	// Cleanup on exit
	defer func() {
		r.pendingMu.Lock()
		delete(r.pending, id)
		r.pendingMu.Unlock()
	}()

	r.mu.Lock()
	data, err := json.Marshal(req)
	if err != nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("jsruntime: marshal request: %w", err)
	}

	if err := writeFrame(r.conn, data); err != nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("jsruntime: write request: %w", err)
	}
	r.mu.Unlock()

	select {
	case resp := <-respCh:
		if resp.Error != "" {
			return nil, errors.New(resp.Error)
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("jsruntime: %w: %w", ErrTimeout, ctx.Err())
	case <-r.done:
		if r.err != nil {
			return nil, r.err
		}
		return nil, ErrRuntimeStopped
	}
}

// LoadPlugin loads a JS plugin file and returns its metadata.
func (r *Runtime) LoadPlugin(ctx context.Context, path string) (*PluginMetadata, error) {
	return r.LoadPluginWithOptions(ctx, path, LoadPluginOptions{})
}

// LoadPluginWithOptions loads a JS plugin with validation options.
func (r *Runtime) LoadPluginWithOptions(ctx context.Context, path string, opts LoadPluginOptions) (*PluginMetadata, error) {
	params := map[string]any{"path": path}
	if len(opts.RequireFunctions) > 0 {
		params["requireFunctions"] = opts.RequireFunctions
	}

	result, err := r.call(ctx, "loadPlugin", params)
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("jsruntime: marshal metadata: %w", err)
	}

	var meta PluginMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("jsruntime: unmarshal metadata: %w", err)
	}

	return &meta, nil
}

// Call invokes a function on a loaded plugin.
func (r *Runtime) Call(ctx context.Context, pluginPath, fnName string, args ...any) (any, error) {
	return r.call(ctx, "call", map[string]any{
		"plugin": pluginPath,
		"fn":     fnName,
		"args":   args,
	})
}

// CallWithTimeout invokes a function with a specific timeout.
// The timeout is enforced both on Go side (context) and JS side (Promise.race).
func (r *Runtime) CallWithTimeout(pluginPath, fnName string, timeout time.Duration, args ...any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return r.call(ctx, "call", map[string]any{
		"plugin":  pluginPath,
		"fn":      fnName,
		"args":    args,
		"timeout": timeout.Milliseconds(), // Also tell JS side about the timeout
	})
}

// Shutdown gracefully stops the Bun subprocess.
func (r *Runtime) Shutdown(ctx context.Context) error {
	if !r.stopped.CompareAndSwap(false, true) {
		return nil // Already stopped
	}

	// Send shutdown command directly via the IPC frame (bypassing call()
	// which would short-circuit because stopped is already true).
	r.mu.Lock()
	if r.conn != nil {
		data, _ := json.Marshal(Request{ID: r.requestID.Add(1), Method: "shutdown"})
		writeFrame(r.conn, data)
	}
	r.mu.Unlock()

	// Close IPC connection to signal the JS process.
	if r.conn != nil {
		r.conn.Close()
	}

	// Wait for process to exit
	select {
	case <-r.done:
	case <-ctx.Done():
		// Force kill
		if r.cmd.Process != nil {
			r.cmd.Process.Kill()
		}
	}

	// Cleanup socket file
	cleanupSocket(r.socketPath)

	// Cleanup temp host script if we created it
	if r.cleanupScript && r.hostScript != "" {
		os.Remove(r.hostScript)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// Done returns a channel that's closed when the runtime stops.
func (r *Runtime) Done() <-chan struct{} {
	return r.done
}

// Err returns any error that caused the runtime to stop.
func (r *Runtime) Err() error {
	return r.err
}

// resolveJSRuntime returns path to the JS runtime.
// Delegates to jsrunner package for consistent resolution across the codebase.
// Returns error if runtime not found (no PATH fallback).
func resolveJSRuntime() (string, error) {
	return jsrunner.GetRuntimePath()
}
