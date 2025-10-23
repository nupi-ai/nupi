package plugins

import (
	"context"
	"encoding/json"
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

func TestServiceLoadPipelinePluginsBuildsIndex(t *testing.T) {
	root := t.TempDir()
	pipelineDir := filepath.Join(root, "pipeline")
	if err := os.MkdirAll(pipelineDir, 0o755); err != nil {
		t.Fatalf("mkdir pipeline: %v", err)
	}

	validScript := `
module.exports = {
  name: "default",
  commands: ["alias"],
  transform: function(input) { return input; }
};
`
	otherScript := `module.exports = { name: "skip", transform: "oops" };`

	if err := os.WriteFile(filepath.Join(pipelineDir, "default.js"), []byte(validScript), 0o644); err != nil {
		t.Fatalf("write default plugin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pipelineDir, "skip.js"), []byte(otherScript), 0o644); err != nil {
		t.Fatalf("write skip plugin: %v", err)
	}

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

	indexData, err := os.ReadFile(filepath.Join(pipelineDir, "index.json"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(indexData, &manifest); err != nil {
		t.Fatalf("unmarshal index: %v", err)
	}
	if manifest["alias"] != "default.js" {
		t.Fatalf("expected alias -> default.js, got %+v", manifest)
	}
	if _, exists := manifest["skip"]; exists {
		t.Fatalf("unexpected skip entry in manifest: %+v", manifest)
	}
}

func TestServiceSyncEmbeddedUsesExtractor(t *testing.T) {
	root := t.TempDir()
	var called bool
	var received string
	svc := NewService(root, WithExtractor(func(target string) error {
		called = true
		received = target
		return nil
	}))

	if err := svc.SyncEmbedded(); err != nil {
		t.Fatalf("SyncEmbedded: %v", err)
	}
	if !called {
		t.Fatalf("expected custom extractor to be called")
	}
	if received != filepath.Join(root, "plugins") {
		t.Fatalf("unexpected extractor target %s", received)
	}
}

func TestServiceStartInitialisesPipeline(t *testing.T) {
	root := t.TempDir()
	pipelineDir := filepath.Join(root, "pipeline")
	if err := os.MkdirAll(pipelineDir, 0o755); err != nil {
		t.Fatalf("mkdir pipeline: %v", err)
	}
	defaultScript := `
module.exports = {
  name: "default",
  transform: function(input) { return input; }
};
`
	if err := os.WriteFile(filepath.Join(pipelineDir, "default.js"), []byte(defaultScript), 0o644); err != nil {
		t.Fatalf("write default plugin: %v", err)
	}

	var extractorCalled bool
	svc := NewService(root, WithExtractor(func(target string) error {
		extractorCalled = true
		return nil
	}))

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !extractorCalled {
		t.Fatalf("expected extractor call during Start")
	}
	if _, ok := svc.PipelinePluginFor("something"); !ok {
		t.Fatalf("expected fallback default plugin to be available")
	}
}
