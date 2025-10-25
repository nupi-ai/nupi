package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPipelinePluginSuccess(t *testing.T) {
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

	plugin, err := LoadPipelinePlugin(path)
	if err != nil {
		t.Fatalf("LoadPipelinePlugin returned error: %v", err)
	}

	if plugin.Name != "sample" {
		t.Fatalf("expected name sample, got %s", plugin.Name)
	}
	if len(plugin.Commands) != 2 || plugin.Commands[0] != "foo" || plugin.Commands[1] != "BAR" {
		t.Fatalf("unexpected commands: %+v", plugin.Commands)
	}
	if plugin.FilePath != path {
		t.Fatalf("unexpected path: %s", plugin.FilePath)
	}
	if !strings.Contains(plugin.Source, "module.exports") {
		t.Fatalf("expected source to contain module exports, got %q", plugin.Source)
	}
}

func TestLoadPipelinePluginErrors(t *testing.T) {
	dir := t.TempDir()
	noTransform := `module.exports = { name: "broken" };`
	path := filepath.Join(dir, "broken.js")
	if err := os.WriteFile(path, []byte(noTransform), 0o644); err != nil {
		t.Fatalf("write plugin: %v", err)
	}
	if _, err := LoadPipelinePlugin(path); err == nil {
		t.Fatalf("expected error for missing transform")
	}

	badTransform := `module.exports = { transform: "not a function" };`
	path = filepath.Join(dir, "bad.js")
	if err := os.WriteFile(path, []byte(badTransform), 0o644); err != nil {
		t.Fatalf("write plugin: %v", err)
	}
	if _, err := LoadPipelinePlugin(path); err == nil {
		t.Fatalf("expected error for non-function transform")
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
	root := t.TempDir()
	const catalog = "test.catalog"

	writeCleanerPlugin(t, root, catalog, "default-cleaner", `module.exports = {
  name: "default",
  commands: ["alias"],
  transform: function(input) { return input; }
};`)
	writeCleanerPlugin(t, root, catalog, "skip-cleaner", `module.exports = { name: "skip", transform: "oops" };`)

	svc := NewService(root)
	if err := svc.LoadPipelinePlugins(); err != nil {
		t.Fatalf("LoadPipelinePlugins: %v", err)
	}

	if plugin, ok := svc.PipelinePluginFor("alias"); !ok || plugin.Name != "default" {
		t.Fatalf("expected default plugin for alias, got %+v ok=%v", plugin, ok)
	}

	if plugin, ok := svc.PipelinePluginFor("unknown"); !ok || plugin.Name != "default" {
		t.Fatalf("expected fallback default plugin, got %+v ok=%v", plugin, ok)
	}

	indexData, err := os.ReadFile(filepath.Join(root, "plugins", "pipeline_cleaners_index.json"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(indexData, &manifest); err != nil {
		t.Fatalf("unmarshal index: %v", err)
	}
	expectedRel := filepath.Join("test.catalog", "default-cleaner", "main.js")
	if manifest["alias"] != expectedRel {
		t.Fatalf("expected alias -> %s, got %+v", expectedRel, manifest)
	}
	if _, exists := manifest["skip"]; exists {
		t.Fatalf("unexpected skip entry in manifest: %+v", manifest)
	}
}

func TestServiceStartInitialisesPipeline(t *testing.T) {
	root := t.TempDir()
	const catalog = "test.catalog"
	writeCleanerPlugin(t, root, catalog, "default-cleaner", `module.exports = {
  name: "default",
  transform: function(input) { return input; }
};`)

	svc := NewService(root)

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := svc.PipelinePluginFor("something"); !ok {
		t.Fatalf("expected fallback default plugin to be available")
	}
}
