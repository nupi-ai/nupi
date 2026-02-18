package pipelinecleaners_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nupi-ai/nupi/internal/jsrunner"
	"github.com/nupi-ai/nupi/internal/plugins"
	pipelinecleaners "github.com/nupi-ai/nupi/internal/plugins/pipeline_cleaners"
)

// skipIfNoBun skips the test if the JS runtime is not available.
// It first checks if the runtime is available via the standard resolver.
// If not, it checks if bun is in PATH and sets NUPI_JS_RUNTIME to use it.
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

func setHostScriptEnv(t *testing.T) {
	t.Helper()
	// Get the path to host.js relative to this test file
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to get test file path")
	}
	// Navigate from plugins_test.go to jsruntime/host.js
	hostScript := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filename))), "jsruntime", "host.js")
	if _, err := os.Stat(hostScript); err != nil {
		t.Skipf("host.js not found at %s", hostScript)
	}
	t.Setenv("NUPI_JS_HOST_SCRIPT", hostScript)
}

func TestLoadPipelinePluginBasic(t *testing.T) {
	// Test basic loading (without jsruntime validation)
	dir := t.TempDir()
	src := `
module.exports = {
  name: "sample",
  commands: ["foo", "BAR"],
  transform: function(input) { return input; }
};
`
	path := filepath.Join(dir, "sample.js")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write plugin: %v", err)
	}

	plugin, err := pipelinecleaners.LoadPipelinePlugin(path)
	if err != nil {
		t.Fatalf("LoadPipelinePlugin returned error: %v", err)
	}

	// Basic loading just reads the file, name defaults to filename
	if plugin.Name != "sample.js" {
		t.Fatalf("expected name sample.js, got %s", plugin.Name)
	}
	if plugin.FilePath != path {
		t.Fatalf("unexpected path: %s", plugin.FilePath)
	}
	if !strings.Contains(plugin.Source, "module.exports") {
		t.Fatalf("expected source to contain module exports, got %q", plugin.Source)
	}
}

func TestLoadPipelinePluginReadError(t *testing.T) {
	// Test error when file doesn't exist
	if _, err := pipelinecleaners.LoadPipelinePlugin("/nonexistent/plugin.js"); err == nil {
		t.Fatalf("expected error for missing file")
	}
}

func writeCleanerPlugin(t *testing.T, root, namespace, slug, script string) {
	t.Helper()
	dir := filepath.Join(root, "plugins", namespace, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	manifest := fmt.Sprintf(`apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: pipeline-cleaner
metadata:
  name: %s
  slug: %s
  namespace: %s
  version: 0.1.0
spec:
  main: main.js
`, slug, slug, namespace)
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.js"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
}

func TestServiceLoadPipelinePluginsBuildsIndex(t *testing.T) {
	skipIfNoBun(t)
	setHostScriptEnv(t)

	root := t.TempDir()
	const namespace = "test.namespace"

	writeCleanerPlugin(t, root, namespace, "default-cleaner", `module.exports = {
  name: "default",
  commands: ["alias"],
  transform: function(input) { return input; }
};`)
	writeCleanerPlugin(t, root, namespace, "skip-cleaner", `module.exports = { name: "skip", transform: "oops" };`)

	svc := plugins.NewService(root)

	// Start the service to initialize jsruntime
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	if plugin, ok := svc.PipelinePluginFor("alias"); !ok || plugin.Name != "default" {
		t.Fatalf("expected default plugin for alias, got %+v ok=%v", plugin, ok)
	}

	if plugin, ok := svc.PipelinePluginFor("unknown"); !ok || plugin.Name != "default" {
		t.Fatalf("expected fallback default plugin, got %+v ok=%v", plugin, ok)
	}
}

func TestServiceStartInitialisesPipeline(t *testing.T) {
	skipIfNoBun(t)
	setHostScriptEnv(t)

	root := t.TempDir()
	const namespace = "test.namespace"
	writeCleanerPlugin(t, root, namespace, "default-cleaner", `module.exports = {
  name: "default",
  transform: function(input) { return input; }
};`)

	svc := plugins.NewService(root)

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	if _, ok := svc.PipelinePluginFor("something"); !ok {
		t.Fatalf("expected fallback default plugin to be available")
	}
}
