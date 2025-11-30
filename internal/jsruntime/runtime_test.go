package jsruntime

import (
	"context"
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
	rt, err := New(ctx, "", hostScript)
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
  name: "test-detector",
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
	rt, err := New(ctx, "", hostScript)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer rt.Shutdown(context.Background())

	// Load the plugin
	meta, err := rt.LoadPlugin(ctx, pluginPath)
	if err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}

	if meta.Name != "test-detector" {
		t.Errorf("Name = %q, want %q", meta.Name, "test-detector")
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
	pluginPath := filepath.Join(tmpDir, "detector.js")
	pluginCode := `
module.exports = {
  name: "npm-detector",
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
	rt, err := New(ctx, "", hostScript)
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
	rt, err := New(ctx, "", hostScript)
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
	rt, err := New(ctx, "", hostScript)
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
	rt, err := New(ctx, "", hostScript)
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
	rt, err := New(ctx, "", hostScript)
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
