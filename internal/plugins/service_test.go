package plugins_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/jsrunner"
	"github.com/nupi-ai/nupi/internal/plugins"
	pipelinecleaners "github.com/nupi-ai/nupi/internal/plugins/pipeline_cleaners"
)

var (
	bunOnce      sync.Once
	bunAvailable bool
)

// skipIfNoBun skips the test if the JS runtime is not available.
// Uses sync.Once + os.Setenv (not t.Setenv) so tests can use t.Parallel().
func skipIfNoBun(t *testing.T) {
	t.Helper()
	bunOnce.Do(func() {
		if jsrunner.IsAvailable() {
			bunAvailable = true
			return
		}
		bunPath, err := exec.LookPath("bun")
		if err != nil {
			return
		}
		os.Setenv("NUPI_JS_RUNTIME", bunPath)
		bunAvailable = true
	})
	if !bunAvailable {
		t.Skip("JS runtime not available: not bundled and bun not in PATH")
	}
}

func writePlugin(t *testing.T, root, catalog, slug, pluginType, script string) {
	t.Helper()
	dir := filepath.Join(root, "plugins", catalog, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	manifest := fmt.Sprintf(`apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: %s
metadata:
  name: %s
  slug: %s
  catalog: %s
  version: 0.1.0
spec:
  main: main.js
`, pluginType, slug, slug, catalog)
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.js"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
}

// --- Plugin Service Lifecycle ---

func TestServiceStartCreatesPluginDir(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()
	pluginDir := filepath.Join(root, "plugins")

	// Verify plugin dir does not exist yet.
	if _, err := os.Stat(pluginDir); !os.IsNotExist(err) {
		t.Fatalf("expected plugin dir to not exist before Start, stat err: %v", err)
	}

	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	info, err := os.Stat(pluginDir)
	if err != nil {
		t.Fatalf("expected plugin dir to exist after Start: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected plugin dir to be a directory")
	}
}

func TestServiceStartInitializesRuntime(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()
	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	rt := svc.JSRuntime()
	if rt == nil {
		t.Fatal("expected JSRuntime to be non-nil after Start")
	}
}

func TestServiceShutdownStopsRuntime(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()
	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	rt := svc.JSRuntime()
	if rt != nil {
		t.Fatal("expected JSRuntime to be nil after Shutdown")
	}
}

func TestServiceStartEmptyDirSucceeds(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()
	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start with empty dir: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// No plugins loaded, but service is healthy.
	if _, ok := svc.ToolHandlerPluginFor("nonexistent"); ok {
		t.Fatal("expected no tool handler for nonexistent command")
	}
	if _, ok := svc.PipelinePluginFor("nonexistent"); ok {
		t.Fatal("expected no pipeline plugin for nonexistent command")
	}
}

func TestServiceStartLogsWarningsForInvalidPlugins(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()

	// Create a valid plugin alongside an invalid one.
	writePlugin(t, root, "test", "valid-handler", "tool-handler", `module.exports = {
  name: "valid",
  commands: ["mycommand"],
  detect: function(output) { return output.includes("mycommand"); }
};`)

	// Invalid manifest: missing slug.
	invalidDir := filepath.Join(root, "plugins", "test", "bad-plugin")
	if err := os.MkdirAll(invalidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	badManifest := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: tool-handler
metadata:
  name: Bad Plugin
spec:
  main: main.js`
	if err := os.WriteFile(filepath.Join(invalidDir, "plugin.yaml"), []byte(badManifest), 0o644); err != nil {
		t.Fatalf("write bad manifest: %v", err)
	}

	svc := plugins.NewService(root)
	// Start should NOT fail even with an invalid plugin.
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start should succeed despite invalid plugins: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// Valid plugin should be loaded.
	if _, ok := svc.ToolHandlerPluginFor("mycommand"); !ok {
		t.Fatal("expected valid tool handler to be loaded despite invalid sibling plugin")
	}

	// Warnings should be recorded.
	warnings := svc.GetDiscoveryWarnings()
	if len(warnings) == 0 {
		t.Fatal("expected discovery warnings for invalid plugin")
	}
}

// --- Tool Handler Plugin Loading and Execution ---

func TestServiceLoadsToolHandlerWithAllFunctions(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()
	writePlugin(t, root, "test", "full-handler", "tool-handler", `module.exports = {
  name: "full-handler",
  commands: ["fullcmd"],
  detect: function(output) { return output.includes("fullcmd"); },
  clean: function(output) { return output.replace(/noise/g, ""); },
  detectIdleState: function(buffer) {
    if (buffer.includes("$ ")) return { is_idle: true, reason: "prompt", prompt_text: "$ " };
    return null;
  },
  extractEvents: function(output) {
    if (/error/i.test(output)) return [{ severity: "error", title: "Error" }];
    return [];
  }
};`)

	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	plugin, ok := svc.ToolHandlerPluginFor("fullcmd")
	if !ok {
		t.Fatal("expected tool handler for 'fullcmd'")
	}
	if plugin.Name != "full-handler" {
		t.Fatalf("expected name 'full-handler', got %q", plugin.Name)
	}
	if !plugin.HasClean {
		t.Error("expected HasClean to be true")
	}
	if !plugin.HasDetectIdleState {
		t.Error("expected HasDetectIdleState to be true")
	}
	if !plugin.HasExtractEvents {
		t.Error("expected HasExtractEvents to be true")
	}
}

func TestServiceLoadsMinimalToolHandler(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()
	writePlugin(t, root, "test", "minimal-handler", "tool-handler", `module.exports = {
  name: "minimal",
  commands: ["mincmd"],
  detect: function(output) { return output.includes("mincmd"); }
};`)

	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	plugin, ok := svc.ToolHandlerPluginFor("mincmd")
	if !ok {
		t.Fatal("expected tool handler for 'mincmd'")
	}
	if plugin.Name != "minimal" {
		t.Fatalf("expected name 'minimal', got %q", plugin.Name)
	}
	if plugin.HasClean {
		t.Error("expected HasClean to be false for minimal plugin")
	}
	if plugin.HasDetectIdleState {
		t.Error("expected HasDetectIdleState to be false for minimal plugin")
	}
	if plugin.HasExtractEvents {
		t.Error("expected HasExtractEvents to be false for minimal plugin")
	}
}

func TestServiceSkipsToolHandlerMissingDetect(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()
	writePlugin(t, root, "test", "no-detect", "tool-handler", `module.exports = {
  name: "no-detect",
  commands: ["badcmd"],
  clean: function(output) { return output; }
};`)

	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	if _, ok := svc.ToolHandlerPluginFor("badcmd"); ok {
		t.Fatal("expected tool handler without detect() to NOT be loaded")
	}
}

func TestServiceToolHandlerForReturnsCorrectPlugin(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()
	writePlugin(t, root, "test", "handler-a", "tool-handler", `module.exports = {
  name: "handler-a",
  commands: ["alpha"],
  detect: function(output) { return output.includes("alpha"); }
};`)
	writePlugin(t, root, "test", "handler-b", "tool-handler", `module.exports = {
  name: "handler-b",
  commands: ["beta"],
  detect: function(output) { return output.includes("beta"); }
};`)

	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	pluginA, ok := svc.ToolHandlerPluginFor("alpha")
	if !ok {
		t.Fatal("expected tool handler for 'alpha'")
	}
	if pluginA.Name != "handler-a" {
		t.Fatalf("expected handler-a for alpha, got %q", pluginA.Name)
	}

	pluginB, ok := svc.ToolHandlerPluginFor("beta")
	if !ok {
		t.Fatal("expected tool handler for 'beta'")
	}
	if pluginB.Name != "handler-b" {
		t.Fatalf("expected handler-b for beta, got %q", pluginB.Name)
	}

	if _, ok := svc.ToolHandlerPluginFor("gamma"); ok {
		t.Fatal("expected no tool handler for unknown command 'gamma'")
	}
}

// --- Pipeline Cleaner Plugin Loading and Execution ---

func TestServiceLoadsPipelineCleanerWithTransform(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()
	writePlugin(t, root, "test", "my-cleaner", "pipeline-cleaner", `module.exports = {
  name: "my-cleaner",
  commands: ["cleancmd"],
  transform: function(input) {
    return { text: input.text.toUpperCase(), annotations: input.annotations };
  }
};`)

	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	plugin, ok := svc.PipelinePluginFor("cleancmd")
	if !ok {
		t.Fatal("expected pipeline plugin for 'cleancmd'")
	}
	if plugin.Name != "my-cleaner" {
		t.Fatalf("expected name 'my-cleaner', got %q", plugin.Name)
	}
}

func TestServiceSkipsPipelineCleanerMissingTransform(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()
	writePlugin(t, root, "test", "no-transform", "pipeline-cleaner", `module.exports = {
  name: "no-transform",
  commands: ["notransform"]
};`)

	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// The "no-transform" plugin should be skipped, and since there's no default
	// cleaner loaded, PipelinePluginFor should return false.
	if _, ok := svc.PipelinePluginFor("notransform"); ok {
		t.Fatal("expected pipeline plugin without transform() to NOT be available")
	}
}

func TestServicePipelinePluginForFallsBackToDefault(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()
	writePlugin(t, root, "test", "default-cleaner", "pipeline-cleaner", `module.exports = {
  name: "default",
  transform: function(input) { return input; }
};`)

	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// Querying an unknown command should fall back to the "default" named plugin.
	plugin, ok := svc.PipelinePluginFor("unknown-command")
	if !ok {
		t.Fatal("expected fallback to default pipeline plugin")
	}
	if plugin.Name != "default" {
		t.Fatalf("expected fallback to 'default', got %q", plugin.Name)
	}
}

func TestServicePipelineTransformExecution(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()
	writePlugin(t, root, "test", "upper-cleaner", "pipeline-cleaner", `module.exports = {
  name: "upper",
  commands: ["upcmd"],
  transform: function(input) {
    return { text: input.text.toUpperCase() };
  }
};`)

	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	plugin, ok := svc.PipelinePluginFor("upcmd")
	if !ok {
		t.Fatal("expected pipeline plugin for 'upcmd'")
	}

	rt := svc.JSRuntime()
	if rt == nil {
		t.Fatal("expected non-nil JSRuntime")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := plugin.Transform(ctx, rt, pipelinecleaners.TransformInput{
		Text: "hello world",
	})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if result.Text != "HELLO WORLD" {
		t.Fatalf("expected 'HELLO WORLD', got %q", result.Text)
	}
}

// --- Discovery warnings via Service ---

func TestServiceLastDiscoveryWarnings(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()

	// Create a valid plugin.
	writePlugin(t, root, "test", "ok-handler", "tool-handler", `module.exports = {
  name: "ok",
  commands: ["okcmd"],
  detect: function(output) { return true; }
};`)

	// Create an invalid plugin (missing slug in manifest).
	invalidDir := filepath.Join(root, "plugins", "test", "bad-handler")
	if err := os.MkdirAll(invalidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	badYAML := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: tool-handler
metadata:
  name: Bad Handler
spec:
  main: main.js`
	if err := os.WriteFile(filepath.Join(invalidDir, "plugin.yaml"), []byte(badYAML), 0o644); err != nil {
		t.Fatalf("write bad manifest: %v", err)
	}

	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	warnings := svc.GetDiscoveryWarnings()
	if len(warnings) == 0 {
		t.Fatal("expected at least one discovery warning")
	}

	foundBad := false
	for _, w := range warnings {
		if filepath.Base(w.Dir) == "bad-handler" {
			foundBad = true
			break
		}
	}
	if !foundBad {
		t.Fatalf("expected warning for bad-handler, got warnings: %v", warnings)
	}
}

func TestServiceDiscoverMultipleCatalogs(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()

	writePlugin(t, root, "catalog-a", "handler-x", "tool-handler", `module.exports = {
  name: "handler-x",
  commands: ["xcmd"],
  detect: function(output) { return output.includes("xcmd"); }
};`)
	writePlugin(t, root, "catalog-b", "handler-y", "tool-handler", `module.exports = {
  name: "handler-y",
  commands: ["ycmd"],
  detect: function(output) { return output.includes("ycmd"); }
};`)

	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	if _, ok := svc.ToolHandlerPluginFor("xcmd"); !ok {
		t.Fatal("expected tool handler for xcmd from catalog-a")
	}
	if _, ok := svc.ToolHandlerPluginFor("ycmd"); !ok {
		t.Fatal("expected tool handler for ycmd from catalog-b")
	}
}

// --- JS Runtime Integration ---

func TestServiceRuntimeAvailableForPluginCalls(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()
	writePlugin(t, root, "test", "detect-handler", "tool-handler", `module.exports = {
  name: "detect-handler",
  commands: ["detectcmd"],
  detect: function(output) { return output.includes("magic-token"); }
};`)

	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	plugin, ok := svc.ToolHandlerPluginFor("detectcmd")
	if !ok {
		t.Fatal("expected tool handler for detectcmd")
	}

	rt := svc.JSRuntime()
	if rt == nil {
		t.Fatal("expected non-nil JSRuntime")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Test detect returns true.
	detected, err := plugin.Detect(ctx, rt, "output with magic-token here")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !detected {
		t.Fatal("expected Detect to return true for matching output")
	}

	// Test detect returns false.
	detected, err = plugin.Detect(ctx, rt, "output without the token")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if detected {
		t.Fatal("expected Detect to return false for non-matching output")
	}
}

func TestServiceRuntimeHandlesMultipleReturnTypes(t *testing.T) {
	skipIfNoBun(t)
	t.Parallel()

	root := t.TempDir()
	writePlugin(t, root, "test", "multi-return", "tool-handler", `module.exports = {
  name: "multi-return",
  commands: ["multicmd"],
  detect: function(output) { return true; },
  clean: function(output) { return "cleaned:" + output; },
  detectIdleState: function(buffer) {
    if (buffer.includes("idle")) return { is_idle: true, reason: "idle", prompt_text: "> " };
    return null;
  },
  extractEvents: function(output) {
    return [{ severity: "info", title: "event1" }, { severity: "warning", title: "event2" }];
  }
};`)

	svc := plugins.NewService(root)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	plugin, ok := svc.ToolHandlerPluginFor("multicmd")
	if !ok {
		t.Fatal("expected tool handler for multicmd")
	}

	rt := svc.JSRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("boolean_detect", func(t *testing.T) {
		detected, err := plugin.Detect(ctx, rt, "anything")
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if !detected {
			t.Fatal("expected true from detect")
		}
	})

	t.Run("string_clean", func(t *testing.T) {
		cleaned, err := plugin.Clean(ctx, rt, "raw")
		if err != nil {
			t.Fatalf("Clean: %v", err)
		}
		if cleaned != "cleaned:raw" {
			t.Fatalf("expected 'cleaned:raw', got %q", cleaned)
		}
	})

	t.Run("object_detectIdleState", func(t *testing.T) {
		state, err := plugin.DetectIdleState(ctx, rt, "idle state")
		if err != nil {
			t.Fatalf("DetectIdleState: %v", err)
		}
		if state == nil {
			t.Fatal("expected non-nil idle state")
		}
		if !state.IsIdle {
			t.Error("expected IsIdle to be true")
		}
		if state.Reason != "idle" {
			t.Errorf("expected reason 'idle', got %q", state.Reason)
		}
	})

	t.Run("null_detectIdleState", func(t *testing.T) {
		state, err := plugin.DetectIdleState(ctx, rt, "some other output")
		if err != nil {
			t.Fatalf("DetectIdleState: %v", err)
		}
		if state != nil {
			t.Errorf("expected nil for non-matching buffer, got %+v", state)
		}
	})

	t.Run("array_extractEvents", func(t *testing.T) {
		events, err := plugin.ExtractEvents(ctx, rt, "test output")
		if err != nil {
			t.Fatalf("ExtractEvents: %v", err)
		}
		if len(events) != 2 {
			t.Fatalf("expected 2 events, got %d", len(events))
		}
		if events[0].Severity != "info" || events[0].Title != "event1" {
			t.Errorf("unexpected event[0]: %+v", events[0])
		}
		if events[1].Severity != "warning" || events[1].Title != "event2" {
			t.Errorf("unexpected event[1]: %+v", events[1])
		}
	})
}
