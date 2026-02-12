package jsruntime

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/jsrunner"
)

// skipIfNoBun skips the test if the JS runtime is not available.
// It first checks if the runtime is available via the standard resolver.
// If not, it checks if bun is in PATH and sets NUPI_JS_RUNTIME to use it.
// This allows tests to run in dev environments where bun is installed globally.
func skipIfNoBun(t *testing.T) {
	t.Helper()

	// First, check if runtime is available via standard resolution
	if jsrunner.IsAvailable() {
		return
	}

	// Fallback: check if bun is in PATH and use it via NUPI_JS_RUNTIME
	bunPath, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("JS runtime not available: not bundled and bun not in PATH")
	}

	// Set NUPI_JS_RUNTIME so the resolver finds it
	t.Setenv("NUPI_JS_RUNTIME", bunPath)
}

func hostScriptPath(t *testing.T) string {
	t.Helper()
	// Get the path to host.js relative to this test file
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to get test file path")
	}
	return filepath.Join(filepath.Dir(filename), "host.js")
}

func TestNewRuntime(t *testing.T) {
	skipIfNoBun(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hostScript := hostScriptPath(t)
	rt, err := New(ctx, "", hostScript, WithRunDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer rt.Shutdown(context.Background())

	// Verify runtime is running
	select {
	case <-rt.Done():
		t.Fatal("runtime stopped unexpectedly")
	default:
		// OK, still running
	}
}

func TestLoadPlugin(t *testing.T) {
	skipIfNoBun(t)

	// Create a test plugin
	tmpDir := t.TempDir()
	pluginPath := filepath.Join(tmpDir, "test-plugin.js")
	pluginCode := `
module.exports = {
  name: "test-handler",
  commands: ["npm", "node"],
  icon: "nodejs",
  detect: function(output) {
    return output.includes("npm") || output.includes("node");
  }
};
`
	if err := os.WriteFile(pluginPath, []byte(pluginCode), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hostScript := hostScriptPath(t)
	rt, err := New(ctx, "", hostScript, WithRunDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer rt.Shutdown(context.Background())

	// Load the plugin
	meta, err := rt.LoadPlugin(ctx, pluginPath)
	if err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}

	if meta.Name != "test-handler" {
		t.Errorf("Name = %q, want %q", meta.Name, "test-handler")
	}
	if len(meta.Commands) != 2 || meta.Commands[0] != "npm" {
		t.Errorf("Commands = %v, want [npm node]", meta.Commands)
	}
	if meta.Icon != "nodejs" {
		t.Errorf("Icon = %q, want %q", meta.Icon, "nodejs")
	}
}

func TestCallFunction(t *testing.T) {
	skipIfNoBun(t)

	// Create a test plugin with detect function
	tmpDir := t.TempDir()
	pluginPath := filepath.Join(tmpDir, "handler.js")
	pluginCode := `
module.exports = {
  name: "npm-handler",
  commands: ["npm"],
  detect: function(output) {
    return output.includes("npm");
  }
};
`
	if err := os.WriteFile(pluginPath, []byte(pluginCode), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hostScript := hostScriptPath(t)
	rt, err := New(ctx, "", hostScript, WithRunDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer rt.Shutdown(context.Background())

	// Load plugin first
	if _, err := rt.LoadPlugin(ctx, pluginPath); err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}

	// Test detect function - should return true
	result, err := rt.Call(ctx, pluginPath, "detect", "npm install completed")
	if err != nil {
		t.Fatalf("Call(detect) error = %v", err)
	}
	if result != true {
		t.Errorf("detect('npm install completed') = %v, want true", result)
	}

	// Test detect function - should return false
	result, err = rt.Call(ctx, pluginPath, "detect", "go build ./...")
	if err != nil {
		t.Fatalf("Call(detect) error = %v", err)
	}
	if result != false {
		t.Errorf("detect('go build ./...') = %v, want false", result)
	}
}

func TestCallTransform(t *testing.T) {
	skipIfNoBun(t)

	// Create a test pipeline cleaner plugin
	tmpDir := t.TempDir()
	pluginPath := filepath.Join(tmpDir, "cleaner.js")
	pluginCode := `
module.exports = {
  name: "test-cleaner",
  commands: ["test"],
  transform: function(input) {
    // Simple transform: uppercase the text
    return {
      text: input.text.toUpperCase(),
      annotations: input.annotations
    };
  }
};
`
	if err := os.WriteFile(pluginPath, []byte(pluginCode), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hostScript := hostScriptPath(t)
	rt, err := New(ctx, "", hostScript, WithRunDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer rt.Shutdown(context.Background())

	// Load plugin first
	if _, err := rt.LoadPlugin(ctx, pluginPath); err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}

	// Test transform function
	input := map[string]any{
		"text":        "hello world",
		"annotations": map[string]any{"source": "test"},
	}
	result, err := rt.Call(ctx, pluginPath, "transform", input)
	if err != nil {
		t.Fatalf("Call(transform) error = %v", err)
	}

	resultMap, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map[string]any", result)
	}
	if resultMap["text"] != "HELLO WORLD" {
		t.Errorf("transform result text = %v, want HELLO WORLD", resultMap["text"])
	}
}

func TestShutdown(t *testing.T) {
	skipIfNoBun(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hostScript := hostScriptPath(t)
	rt, err := New(ctx, "", hostScript, WithRunDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()

	if err := rt.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	// Verify runtime is stopped
	select {
	case <-rt.Done():
		// OK, stopped
	case <-time.After(1 * time.Second):
		t.Fatal("runtime did not stop after Shutdown")
	}
}

func TestCallTimeout(t *testing.T) {
	skipIfNoBun(t)

	// Create a plugin with a slow function
	tmpDir := t.TempDir()
	pluginPath := filepath.Join(tmpDir, "slow.js")
	pluginCode := `
module.exports = {
  name: "slow-plugin",
  commands: [],
  slowFn: async function() {
    await new Promise(resolve => setTimeout(resolve, 5000));
    return "done";
  }
};
`
	if err := os.WriteFile(pluginPath, []byte(pluginCode), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hostScript := hostScriptPath(t)
	rt, err := New(ctx, "", hostScript, WithRunDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer rt.Shutdown(context.Background())

	// Load plugin
	if _, err := rt.LoadPlugin(ctx, pluginPath); err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}

	// Call with short timeout should fail
	_, err = rt.CallWithTimeout(pluginPath, "slowFn", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestCallUnloadedPlugin(t *testing.T) {
	skipIfNoBun(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hostScript := hostScriptPath(t)
	rt, err := New(ctx, "", hostScript, WithRunDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer rt.Shutdown(context.Background())

	// Call without loading should fail
	_, err = rt.Call(ctx, "/nonexistent/plugin.js", "detect", "test")
	if err == nil {
		t.Fatal("expected error for unloaded plugin")
	}
}

// TestSocketCleanup verifies the socket file is removed after shutdown.
func TestSocketCleanup(t *testing.T) {
	skipIfNoBun(t)

	runDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hostScript := hostScriptPath(t)
	rt, err := New(ctx, "", hostScript, WithRunDir(runDir))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	socketPath := rt.socketPath
	// Socket file should exist while runtime is up.
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("socket file should exist: %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	if err := rt.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	// Socket file should be removed after shutdown.
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file should be removed after shutdown, got err = %v", err)
	}
}

// TestStdoutDoesNotCorruptProtocol verifies that a plugin writing to
// process.stdout does not break the IPC protocol (since we use a socket now).
func TestStdoutDoesNotCorruptProtocol(t *testing.T) {
	skipIfNoBun(t)

	tmpDir := t.TempDir()
	pluginPath := filepath.Join(tmpDir, "stdout-writer.js")
	pluginCode := `
module.exports = {
  name: "stdout-writer",
  commands: [],
  doWork: function() {
    // This would have broken the old stdin/stdout protocol.
    process.stdout.write("GARBAGE ON STDOUT\n");
    return "ok";
  }
};
`
	if err := os.WriteFile(pluginPath, []byte(pluginCode), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hostScript := hostScriptPath(t)
	rt, err := New(ctx, "", hostScript, WithRunDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer rt.Shutdown(context.Background())

	if _, err := rt.LoadPlugin(ctx, pluginPath); err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}

	// Call the function that writes to stdout - should not break the protocol.
	result, err := rt.Call(ctx, pluginPath, "doWork")
	if err != nil {
		t.Fatalf("Call(doWork) error = %v", err)
	}
	if result != "ok" {
		t.Errorf("doWork() = %v, want %q", result, "ok")
	}

	// Verify further calls still work.
	result, err = rt.Call(ctx, pluginPath, "doWork")
	if err != nil {
		t.Fatalf("second Call(doWork) error = %v", err)
	}
	if result != "ok" {
		t.Errorf("second doWork() = %v, want %q", result, "ok")
	}
}

// ---------------------------------------------------------------------------
// Unit tests for frame framing (writeFrame / readFrame)
// ---------------------------------------------------------------------------

func TestWriteReadFrame(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	payload := []byte(`{"id":1,"method":"ping"}`)

	// Write in a goroutine.
	go func() {
		if err := writeFrame(client, payload); err != nil {
			t.Errorf("writeFrame error: %v", err)
		}
	}()

	got, err := readFrame(server)
	if err != nil {
		t.Fatalf("readFrame error: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("readFrame = %q, want %q", got, payload)
	}
}

func TestWriteReadFrameEmpty(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- writeFrame(client, []byte{})
		client.Close()
	}()

	got, err := readFrame(server)
	if err != nil {
		t.Fatalf("readFrame error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("readFrame = %q, want empty", got)
	}
	if err := <-errCh; err != nil {
		t.Errorf("writeFrame error: %v", err)
	}
}

func TestReadFrameTooLarge(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Write a header claiming a payload larger than max.
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], maxFramePayload+1)
		client.Write(header[:])
		client.Close()
	}()

	_, err := readFrame(server)
	if err == nil {
		t.Fatal("expected error for oversized frame")
	}
	<-done
}

func TestReadFrameMultiple(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	messages := []string{
		`{"id":1,"method":"ping"}`,
		`{"id":2,"method":"loadPlugin","params":{"path":"/foo"}}`,
		`{"id":3,"method":"shutdown"}`,
	}

	go func() {
		for _, msg := range messages {
			if err := writeFrame(client, []byte(msg)); err != nil {
				t.Errorf("writeFrame error: %v", err)
			}
		}
	}()

	reader := io.Reader(server)
	for i, want := range messages {
		got, err := readFrame(reader)
		if err != nil {
			t.Fatalf("readFrame[%d] error: %v", i, err)
		}
		if string(got) != want {
			t.Errorf("readFrame[%d] = %q, want %q", i, got, want)
		}
	}
}
