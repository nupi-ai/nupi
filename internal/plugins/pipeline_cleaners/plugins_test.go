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

	"github.com/nupi-ai/nupi/internal/plugins"
	pipelinecleaners "github.com/nupi-ai/nupi/internal/plugins/pipeline_cleaners"
)

func bunAvailable() bool {
	_, err := exec.LookPath("bun")
	return err == nil
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

func writeCleanerPlugin(t *testing.T, root, catalog, slug, script string) {
	t.Helper()
	dir := filepath.Join(root, "plugins", catalog, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	manifest := fmt.Sprintf(`apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: pipeline-cleaner
metadata:
  name: %s
  slug: %s
  catalog: %s
  version: 0.1.0
spec:
  main: main.js
`, slug, slug, catalog)
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.js"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
}

func TestServiceLoadPipelinePluginsBuildsIndex(t *testing.T) {
	if !bunAvailable() {
		t.Skip("bun not available")
	}
	setHostScriptEnv(t)

	root := t.TempDir()
	const catalog = "test.catalog"

	writeCleanerPlugin(t, root, catalog, "default-cleaner", `module.exports = {
  name: "default",
  commands: ["alias"],
  transform: function(input) { return input; }
};`)
	writeCleanerPlugin(t, root, catalog, "skip-cleaner", `module.exports = { name: "skip", transform: "oops" };`)

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
	if !bunAvailable() {
		t.Skip("bun not available")
	}
	setHostScriptEnv(t)

	root := t.TempDir()
	const catalog = "test.catalog"
	writeCleanerPlugin(t, root, catalog, "default-cleaner", `module.exports = {
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
