package toolhandlers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/jsrunner"
	"github.com/nupi-ai/nupi/internal/jsruntime"
)

// skipIfNoBun skips the test if the JS runtime is not available.
func skipIfNoBun(t *testing.T) {
	t.Helper()
	if jsrunner.IsAvailable() {
		return
	}
	bunPath, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("JS runtime not available: not bundled and bun not in PATH")
	}
	t.Setenv("NUPI_JS_RUNTIME", bunPath)
}

func newRuntime(t *testing.T) *jsruntime.Runtime {
	t.Helper()

	// Use empty hostScript to use embedded host.js
	rt, err := jsruntime.New(context.Background(), "", "", jsruntime.WithRunDir(t.TempDir()))
	if err != nil {
		t.Fatalf("jsruntime.New() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rt.Shutdown(ctx)
	})
	return rt
}

func TestJSPlugin_DetectIdleState(t *testing.T) {
	skipIfNoBun(t)

	tmpDir := t.TempDir()
	pluginPath := filepath.Join(tmpDir, "idle-handler.js")
	pluginCode := fmt.Sprintf(`
module.exports = {
  name: "idle-handler",
  commands: ["test"],
  detect: function(output) {
    return output.includes("test");
  },
  detectIdleState: function(buffer) {
    if (buffer.includes(">>> ")) {
      return {
        is_idle: true,
        reason: "prompt",
        prompt_text: ">>> ",
        waiting_for: "%s"
      };
    }
    if (buffer.includes("[y/n]")) {
      return {
        is_idle: true,
        reason: "question",
        waiting_for: "%s"
      };
    }
    return null;
  }
};
`, constants.PipelineWaitingForUserInput, constants.PipelineWaitingForConfirmation)
	if err := os.WriteFile(pluginPath, []byte(pluginCode), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	rt := newRuntime(t)
	ctx := context.Background()

	plugin, err := LoadPluginWithRuntime(ctx, rt, pluginPath)
	if err != nil {
		t.Fatalf("LoadPluginWithRuntime() error = %v", err)
	}

	// Verify capability flag
	if !plugin.HasDetectIdleState {
		t.Error("expected HasDetectIdleState to be true")
	}

	// Test prompt detection
	state, err := plugin.DetectIdleState(ctx, rt, "Python 3.10\n>>> ")
	if err != nil {
		t.Fatalf("DetectIdleState() error = %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil IdleState for prompt")
	}
	if !state.IsIdle {
		t.Error("expected IsIdle=true")
	}
	if state.Reason != "prompt" {
		t.Errorf("Reason = %q, want %q", state.Reason, "prompt")
	}
	if state.PromptText != ">>> " {
		t.Errorf("PromptText = %q, want %q", state.PromptText, ">>> ")
	}
	if state.WaitingFor != constants.PipelineWaitingForUserInput {
		t.Errorf("WaitingFor = %q, want %q", state.WaitingFor, constants.PipelineWaitingForUserInput)
	}

	// Test question detection
	state, err = plugin.DetectIdleState(ctx, rt, "Continue? [y/n]")
	if err != nil {
		t.Fatalf("DetectIdleState() error = %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil IdleState for question")
	}
	if state.Reason != "question" {
		t.Errorf("Reason = %q, want %q", state.Reason, "question")
	}
	if state.WaitingFor != constants.PipelineWaitingForConfirmation {
		t.Errorf("WaitingFor = %q, want %q", state.WaitingFor, constants.PipelineWaitingForConfirmation)
	}

	// Test no match
	state, err = plugin.DetectIdleState(ctx, rt, "just some output")
	if err != nil {
		t.Fatalf("DetectIdleState() error = %v", err)
	}
	if state != nil {
		t.Errorf("expected nil IdleState for non-idle output, got %+v", state)
	}
}

func TestJSPlugin_DetectIdleState_Fallback(t *testing.T) {
	skipIfNoBun(t)

	// Plugin without detectIdleState function
	tmpDir := t.TempDir()
	pluginPath := filepath.Join(tmpDir, "no-idle.js")
	pluginCode := `
module.exports = {
  name: "no-idle-handler",
  commands: ["test"],
  detect: function(output) {
    return true;
  }
};
`
	if err := os.WriteFile(pluginPath, []byte(pluginCode), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	rt := newRuntime(t)
	ctx := context.Background()

	plugin, err := LoadPluginWithRuntime(ctx, rt, pluginPath)
	if err != nil {
		t.Fatalf("LoadPluginWithRuntime() error = %v", err)
	}

	// Verify capability flag is false
	if plugin.HasDetectIdleState {
		t.Error("expected HasDetectIdleState to be false")
	}

	// Should return nil without error when function not implemented
	state, err := plugin.DetectIdleState(ctx, rt, ">>> ")
	if err != nil {
		t.Fatalf("DetectIdleState() error = %v", err)
	}
	if state != nil {
		t.Errorf("expected nil when detectIdleState not implemented, got %+v", state)
	}
}

func TestJSPlugin_Clean(t *testing.T) {
	skipIfNoBun(t)

	tmpDir := t.TempDir()
	pluginPath := filepath.Join(tmpDir, "cleaner.js")
	pluginCode := `
module.exports = {
  name: "cleaner",
  commands: ["test"],
  detect: function(output) {
    return true;
  },
  clean: function(output) {
    // Remove ANSI escape codes and spinner characters
    return output
      .replace(/\x1b\[[0-9;]*[a-zA-Z]/g, '')
      .replace(/[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]/g, '')
      .trim();
  }
};
`
	if err := os.WriteFile(pluginPath, []byte(pluginCode), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	rt := newRuntime(t)
	ctx := context.Background()

	plugin, err := LoadPluginWithRuntime(ctx, rt, pluginPath)
	if err != nil {
		t.Fatalf("LoadPluginWithRuntime() error = %v", err)
	}

	// Verify capability flag
	if !plugin.HasClean {
		t.Error("expected HasClean to be true")
	}

	// Test cleaning ANSI codes
	input := "\x1b[32mSuccess\x1b[0m: Build completed"
	cleaned, err := plugin.Clean(ctx, rt, input)
	if err != nil {
		t.Fatalf("Clean() error = %v", err)
	}
	expected := "Success: Build completed"
	if cleaned != expected {
		t.Errorf("Clean() = %q, want %q", cleaned, expected)
	}

	// Test cleaning spinner
	input = "⠋ Loading..."
	cleaned, err = plugin.Clean(ctx, rt, input)
	if err != nil {
		t.Fatalf("Clean() error = %v", err)
	}
	expected = "Loading..."
	if cleaned != expected {
		t.Errorf("Clean() = %q, want %q", cleaned, expected)
	}
}

func TestJSPlugin_Clean_Fallback(t *testing.T) {
	skipIfNoBun(t)

	// Plugin without clean function
	tmpDir := t.TempDir()
	pluginPath := filepath.Join(tmpDir, "no-clean.js")
	pluginCode := `
module.exports = {
  name: "no-clean",
  commands: ["test"],
  detect: function(output) {
    return true;
  }
};
`
	if err := os.WriteFile(pluginPath, []byte(pluginCode), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	rt := newRuntime(t)
	ctx := context.Background()

	plugin, err := LoadPluginWithRuntime(ctx, rt, pluginPath)
	if err != nil {
		t.Fatalf("LoadPluginWithRuntime() error = %v", err)
	}

	// Verify capability flag is false
	if plugin.HasClean {
		t.Error("expected HasClean to be false")
	}

	// Should return original output when clean not implemented
	input := "\x1b[32mNot cleaned\x1b[0m"
	cleaned, err := plugin.Clean(ctx, rt, input)
	if err != nil {
		t.Fatalf("Clean() error = %v", err)
	}
	if cleaned != input {
		t.Errorf("Clean() = %q, want original %q", cleaned, input)
	}
}

func TestJSPlugin_ExtractEvents(t *testing.T) {
	skipIfNoBun(t)

	tmpDir := t.TempDir()
	pluginPath := filepath.Join(tmpDir, "event-extractor.js")
	pluginCode := `
module.exports = {
  name: "event-extractor",
  commands: ["test"],
  detect: function(output) {
    return true;
  },
  extractEvents: function(output) {
    var events = [];
    if (/error:/i.test(output)) {
      var match = output.match(/error:\s*(.+)/i);
      events.push({
        severity: "error",
        title: "Error detected",
        details: match ? match[1] : "",
        action_suggestion: "Fix the error"
      });
    }
    if (/warning:/i.test(output)) {
      events.push({
        severity: "warning",
        title: "Warning detected"
      });
    }
    if (/success/i.test(output)) {
      events.push({
        severity: "success",
        title: "Operation succeeded"
      });
    }
    return events;
  }
};
`
	if err := os.WriteFile(pluginPath, []byte(pluginCode), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	rt := newRuntime(t)
	ctx := context.Background()

	plugin, err := LoadPluginWithRuntime(ctx, rt, pluginPath)
	if err != nil {
		t.Fatalf("LoadPluginWithRuntime() error = %v", err)
	}

	// Verify capability flag
	if !plugin.HasExtractEvents {
		t.Error("expected HasExtractEvents to be true")
	}

	// Test error extraction
	events, err := plugin.ExtractEvents(ctx, rt, "Error: cannot find module 'foo'")
	if err != nil {
		t.Fatalf("ExtractEvents() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Severity != "error" {
		t.Errorf("Severity = %q, want %q", events[0].Severity, "error")
	}
	if events[0].Title != "Error detected" {
		t.Errorf("Title = %q, want %q", events[0].Title, "Error detected")
	}
	if events[0].Details != "cannot find module 'foo'" {
		t.Errorf("Details = %q, want %q", events[0].Details, "cannot find module 'foo'")
	}
	if events[0].ActionSuggestion != "Fix the error" {
		t.Errorf("ActionSuggestion = %q, want %q", events[0].ActionSuggestion, "Fix the error")
	}

	// Test multiple events
	events, err = plugin.ExtractEvents(ctx, rt, "Warning: deprecated\nError: failed\nSuccess!")
	if err != nil {
		t.Fatalf("ExtractEvents() error = %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	// Test no events
	events, err = plugin.ExtractEvents(ctx, rt, "normal output")
	if err != nil {
		t.Fatalf("ExtractEvents() error = %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestJSPlugin_ExtractEvents_Fallback(t *testing.T) {
	skipIfNoBun(t)

	// Plugin without extractEvents function
	tmpDir := t.TempDir()
	pluginPath := filepath.Join(tmpDir, "no-events.js")
	pluginCode := `
module.exports = {
  name: "no-events",
  commands: ["test"],
  detect: function(output) {
    return true;
  }
};
`
	if err := os.WriteFile(pluginPath, []byte(pluginCode), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	rt := newRuntime(t)
	ctx := context.Background()

	plugin, err := LoadPluginWithRuntime(ctx, rt, pluginPath)
	if err != nil {
		t.Fatalf("LoadPluginWithRuntime() error = %v", err)
	}

	// Verify capability flag is false
	if plugin.HasExtractEvents {
		t.Error("expected HasExtractEvents to be false")
	}

	// Should return nil when extractEvents not implemented
	events, err := plugin.ExtractEvents(ctx, rt, "Error: something")
	if err != nil {
		t.Fatalf("ExtractEvents() error = %v", err)
	}
	if events != nil {
		t.Errorf("expected nil events when extractEvents not implemented, got %+v", events)
	}
}

func TestJSPlugin_Process(t *testing.T) {
	skipIfNoBun(t)

	tmpDir := t.TempDir()
	pluginPath := filepath.Join(tmpDir, "full-processor.js")
	pluginCode := `
module.exports = {
  name: "full-processor",
  commands: ["test"],
  detect: function(output) {
    return true;
  },
  detectIdleState: function(buffer) {
    if (buffer.includes("$ ")) {
      return { is_idle: true, reason: "prompt", prompt_text: "$ " };
    }
    return null;
  },
  clean: function(output) {
    return output.replace(/\x1b\[[0-9;]*[a-zA-Z]/g, '');
  },
  extractEvents: function(output) {
    if (/error/i.test(output)) {
      return [{ severity: "error", title: "Error" }];
    }
    return [];
  }
};
`
	if err := os.WriteFile(pluginPath, []byte(pluginCode), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	rt := newRuntime(t)
	ctx := context.Background()

	plugin, err := LoadPluginWithRuntime(ctx, rt, pluginPath)
	if err != nil {
		t.Fatalf("LoadPluginWithRuntime() error = %v", err)
	}

	// Test full processing pipeline
	result, err := plugin.Process(ctx, rt, "\x1b[31mError: failed\x1b[0m\n$ ")
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	if result.IdleState == nil {
		t.Error("expected non-nil IdleState")
	} else if result.IdleState.Reason != "prompt" {
		t.Errorf("IdleState.Reason = %q, want %q", result.IdleState.Reason, "prompt")
	}

	expectedCleaned := "Error: failed\n$ "
	if result.Cleaned != expectedCleaned {
		t.Errorf("Cleaned = %q, want %q", result.Cleaned, expectedCleaned)
	}

	if len(result.Events) != 1 {
		t.Errorf("expected 1 event, got %d", len(result.Events))
	} else if result.Events[0].Severity != "error" {
		t.Errorf("Event.Severity = %q, want %q", result.Events[0].Severity, "error")
	}
}
