package manifest

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseAdapterOptionsSuccess(t *testing.T) {
	data := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: test
  slug: test
  catalog: ai.nupi
  version: 0.0.1
spec:
  slot: stt
  mode: local
  entrypoint:
    command: ./adapter
    transport: process
  options:
    use_gpu:
      type: boolean
      default: true
    threads:
      type: integer
      default: 4
    voice:
      type: enum
      default: en-US
      values: [en-US, en-GB]
`

	mf, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if mf.Adapter == nil {
		t.Fatalf("expected adapter spec")
	}

	opts := mf.Adapter.Options
	if len(opts) != 3 {
		t.Fatalf("expected 3 options, got %d", len(opts))
	}

	useGPU, ok := opts["use_gpu"]
	if !ok {
		t.Fatalf("missing use_gpu option")
	}
	if val, ok := useGPU.Default.(bool); !ok || !val {
		t.Fatalf("use_gpu default mismatch: %#v", useGPU.Default)
	}

	threads, ok := opts["threads"]
	if !ok {
		t.Fatalf("missing threads option")
	}
	if val, ok := threads.Default.(int); !ok || val != 4 {
		t.Fatalf("threads default mismatch: %#v", threads.Default)
	}

	voice, ok := opts["voice"]
	if !ok {
		t.Fatalf("missing voice option")
	}
	if val, ok := voice.Default.(string); !ok || val != "en-US" {
		t.Fatalf("voice default mismatch: %#v", voice.Default)
	}
	if len(voice.Values) != 2 {
		t.Fatalf("expected 2 voice values, got %d", len(voice.Values))
	}
	if voice.Values[0] != "en-US" || voice.Values[1] != "en-GB" {
		t.Fatalf("unexpected enum values: %#v", voice.Values)
	}
}

func TestParseAdapterOptionsInvalidType(t *testing.T) {
	data := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: test
  slug: test
  catalog: ai.nupi
  version: 0.0.1
spec:
  slot: stt
  mode: local
  entrypoint:
    command: ./adapter
  options:
    bad:
      type: unsupported
`
	if _, err := Parse([]byte(data)); err == nil {
		t.Fatalf("expected error for unsupported option type")
	}
}

func TestParseAdapterOptionsInvalidEnumDefault(t *testing.T) {
	data := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: test
  slug: test
  catalog: ai.nupi
  version: 0.0.1
spec:
  slot: stt
  mode: local
  entrypoint:
    command: ./adapter
  options:
    voice:
      type: enum
      default: en-AU
      values: [en-US, en-GB]
`
	if _, err := Parse([]byte(data)); err == nil {
		t.Fatalf("expected error when enum default not in values")
	}
}

func TestInferOptionType(t *testing.T) {
	cases := map[string]struct {
		input  any
		expect string
	}{
		"nil":         {nil, ""},
		"bool":        {true, "boolean"},
		"int":         {int64(5), "integer"},
		"uint":        {uint32(10), "integer"},
		"float_int":   {float64(2.0), "integer"},
		"float":       {float64(2.5), "number"},
		"string":      {"value", "string"},
		"unsupported": {[]string{"a"}, ""},
	}

	for name, tc := range cases {
		if got := inferOptionType(tc.input); got != tc.expect {
			t.Fatalf("%s: expected %q, got %q", name, tc.expect, got)
		}
	}
}

func TestCoerceNumber(t *testing.T) {
	if val, err := coerceNumber(" 3.14 "); err != nil || val.(float64) != 3.14 {
		t.Fatalf("coerceNumber string failed: val=%v err=%v", val, err)
	}
	if val, err := coerceNumber(uint32(7)); err != nil || val.(float64) != 7 {
		t.Fatalf("coerceNumber uint failed: val=%v err=%v", val, err)
	}
	if val, err := coerceNumber(float32(1.5)); err != nil || val.(float64) != 1.5 {
		t.Fatalf("coerceNumber float32 failed: val=%v err=%v", val, err)
	}
	if _, err := coerceNumber("not-a-number"); err == nil {
		t.Fatalf("expected error for invalid number string")
	}
}

func TestCoerceInt(t *testing.T) {
	if val, err := coerceInt("42"); err != nil || val.(int) != 42 {
		t.Fatalf("expected string 42 -> 42, got %v err=%v", val, err)
	}
	if _, err := coerceInt(""); err == nil {
		t.Fatalf("expected error for empty string")
	}
	large := int64(math.MaxInt64)
	if val, err := coerceInt(large); err != nil || val.(int) != int(large) {
		t.Fatalf("expected MaxInt64 -> %d, got %v err=%v", large, val, err)
	}
	overflow := strconv.FormatUint(uint64(math.MaxInt64)+1, 10)
	if _, err := coerceInt(overflow); err == nil {
		t.Fatalf("expected overflow error")
	}
	if val, err := coerceInt(float32(8.0)); err != nil || val.(int) != 8 {
		t.Fatalf("expected float32 8.0 -> 8, got %v err=%v", val, err)
	}
	if _, err := coerceInt(3.14); err == nil {
		t.Fatalf("expected error for non-integral float")
	}
}

func TestIsFloatIntegral(t *testing.T) {
	if !isFloatIntegral(5.0) {
		t.Fatalf("expected 5.0 to be integral")
	}
	if isFloatIntegral(5.1) {
		t.Fatalf("expected 5.1 to be non-integral")
	}
}

func TestCoerceBool(t *testing.T) {
	cases := []struct {
		name   string
		input  any
		expect bool
	}{
		{"literal_true", true, true},
		{"string_true", "true", true},
		{"string_one", "1", true},
		{"literal_false", false, false},
		{"string_false", "false", false},
		{"string_zero", "0", false},
	}

	for _, tc := range cases {
		val, err := coerceBool(tc.input)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", tc.name, err)
		}
		if val.(bool) != tc.expect {
			t.Fatalf("%s: expected %v, got %v", tc.name, tc.expect, val)
		}
	}

	if _, err := coerceBool("maybe"); err == nil {
		t.Fatalf("expected error for invalid boolean string")
	}
}

type customStringer struct{}

func (customStringer) String() string { return "stringer" }

func TestCoerceString(t *testing.T) {
	val, err := coerceString(customStringer{})
	if err != nil {
		t.Fatalf("coerceString stringer error: %v", err)
	}
	if val.(string) != "stringer" {
		t.Fatalf("expected stringer output, got %v", val)
	}

	if val, err := coerceString(" direct "); err != nil || val.(string) != " direct " {
		t.Fatalf("coerceString literal failed: %v %v", val, err)
	}
}

func TestNormalizeAdapterOptionValueBoolean(t *testing.T) {
	opt := AdapterOption{Type: "boolean"}
	val, err := NormalizeAdapterOptionValue(opt, "true")
	if err != nil {
		t.Fatalf("expected bool coercion to succeed: %v", err)
	}
	if v, ok := val.(bool); !ok || !v {
		t.Fatalf("expected true boolean, got %#v", val)
	}

	if _, err := NormalizeAdapterOptionValue(opt, "not-bool"); err == nil {
		t.Fatalf("expected error for invalid boolean input")
	}
}

func TestNormalizeAdapterOptionValueInteger(t *testing.T) {
	opt := AdapterOption{Type: "integer"}
	val, err := NormalizeAdapterOptionValue(opt, " 42 ")
	if err != nil {
		t.Fatalf("expected integer coercion to succeed: %v", err)
	}
	if v, ok := val.(int); !ok || v != 42 {
		t.Fatalf("expected 42, got %#v", val)
	}

	if _, err := NormalizeAdapterOptionValue(opt, "abc"); err == nil {
		t.Fatalf("expected error for non-integer string")
	}
}

func TestNormalizeAdapterOptionValueEnum(t *testing.T) {
	opt := AdapterOption{Type: "enum", Values: []any{"base", "small"}}
	val, err := NormalizeAdapterOptionValue(opt, "small")
	if err != nil {
		t.Fatalf("expected enum coercion to succeed: %v", err)
	}
	if v, ok := val.(string); !ok || v != "small" {
		t.Fatalf("expected \"small\", got %#v", val)
	}

	if _, err := NormalizeAdapterOptionValue(opt, "medium"); err == nil {
		t.Fatalf("expected error for enum value outside allowed set")
	}
}

func TestNormalizeAdapterOptionValueNil(t *testing.T) {
	opt := AdapterOption{Type: "string"}
	val, err := NormalizeAdapterOptionValue(opt, nil)
	if err != nil {
		t.Fatalf("expected nil value to pass through: %v", err)
	}
	if val != nil {
		t.Fatalf("expected nil passthrough, got %#v", val)
	}
}

func TestParseAdapterRejectsEmptySlot(t *testing.T) {
	data := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: test
  slug: test
  catalog: ai.nupi
  version: 0.0.1
spec:
  entrypoint:
    command: ./adapter`

	_, err := Parse([]byte(data))
	if err == nil {
		t.Fatalf("expected error for adapter without slot")
	}
	if !strings.Contains(err.Error(), "missing required field: slot") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestParseAdapterRejectsMissingMode(t *testing.T) {
	data := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: test
  slug: test
  catalog: ai.nupi
  version: 0.0.1
spec:
  slot: ai
  entrypoint:
    command: ./adapter
    transport: process`

	_, err := Parse([]byte(data))
	if err == nil {
		t.Fatal("expected error for adapter without mode")
	}
	if !strings.Contains(err.Error(), "missing required field: mode") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestParseAdapterRejectsMissingTransport(t *testing.T) {
	data := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: test
  slug: test
  catalog: ai.nupi
  version: 0.0.1
spec:
  slot: ai
  mode: local
  entrypoint:
    command: ./adapter`

	_, err := Parse([]byte(data))
	if err == nil {
		t.Fatal("expected error for adapter without transport")
	}
	if !strings.Contains(err.Error(), "missing required field: entrypoint.transport") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestParseAdapterRejectsProcessWithoutCommand(t *testing.T) {
	data := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: test
  slug: test
  catalog: ai.nupi
  version: 0.0.1
spec:
  slot: stt
  mode: local
  entrypoint:
    transport: process`

	_, err := Parse([]byte(data))
	if err == nil {
		t.Fatalf("expected error for process adapter without command")
	}
	if !strings.Contains(err.Error(), "transport=process requires entrypoint.command") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestNormalizeAdapterOptionRejectsIntegerDefaultNotInValues(t *testing.T) {
	data := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: test
  slug: test
  catalog: ai.nupi
  version: 0.0.1
spec:
  slot: stt
  mode: local
  entrypoint:
    command: ./adapter
    transport: process
  options:
    threads:
      type: integer
      default: 3
      values: [1, 2, 4, 8]`

	_, err := Parse([]byte(data))
	if err == nil {
		t.Fatalf("expected error for integer default not in values")
	}
	if !strings.Contains(err.Error(), "not present in values") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestNormalizeAdapterOptionValueRejectsIntegerNotInValues(t *testing.T) {
	opt := AdapterOption{Type: "integer", Values: []any{1, 2, 4, 8}}
	_, err := NormalizeAdapterOptionValue(opt, 3)
	if err == nil {
		t.Fatalf("expected error for integer value not in values list")
	}
	if !strings.Contains(err.Error(), "expected one of") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestNormalizeAdapterOptionValueRejectsNumberNotInValues(t *testing.T) {
	opt := AdapterOption{Type: "number", Values: []any{1.0, 2.5, 5.0}}
	_, err := NormalizeAdapterOptionValue(opt, 3.14)
	if err == nil {
		t.Fatalf("expected error for number value not in values list")
	}
}

func TestNormalizeAdapterOptionValueRejectsBooleanNotInValues(t *testing.T) {
	opt := AdapterOption{Type: "boolean", Values: []any{true}}
	_, err := NormalizeAdapterOptionValue(opt, false)
	if err == nil {
		t.Fatalf("expected error for boolean value not in values list")
	}
}

func TestParseAdapterRejectsInvalidTransport(t *testing.T) {
	data := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: test
  slug: test
  catalog: ai.nupi
  version: 0.0.1
spec:
  slot: stt
  mode: local
  entrypoint:
    transport: invalid
    command: ./adapter`

	_, err := Parse([]byte(data))
	if err == nil {
		t.Fatalf("expected error for invalid transport")
	}
	if !strings.Contains(err.Error(), "invalid transport") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestParseAdapterRejectsInvalidRuntime(t *testing.T) {
	data := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: test
  slug: test
  catalog: ai.nupi
  version: 0.0.1
spec:
  slot: stt
  mode: local
  entrypoint:
    runtime: python
    transport: process
    command: ./adapter`

	_, err := Parse([]byte(data))
	if err == nil {
		t.Fatalf("expected error for invalid runtime")
	}
	if !strings.Contains(err.Error(), "invalid runtime") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestParseAdapterAcceptsValidRuntimes(t *testing.T) {
	tests := []struct {
		name    string
		runtime string
	}{
		{"empty (default binary)", ""},
		{"binary", "binary"},
		{"js", "js"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtimeLine := ""
			if tt.runtime != "" {
				runtimeLine = "    runtime: " + tt.runtime + "\n"
			}
			data := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: test
  slug: test
  catalog: ai.nupi
  version: 0.0.1
spec:
  slot: stt
  mode: local
  entrypoint:
` + runtimeLine + `    transport: process
    command: ./adapter`

			m, err := Parse([]byte(data))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			expectedRuntime := tt.runtime
			if m.Adapter.Entrypoint.Runtime != expectedRuntime {
				t.Errorf("runtime = %q, want %q", m.Adapter.Entrypoint.Runtime, expectedRuntime)
			}
		})
	}
}

func TestDiscoverWithWarningsReturnsSkippedPlugins(t *testing.T) {
	root := t.TempDir()

	// Valid manifest
	createPluginYAML(t, root, "valid/plugin1", adapterManifest("Valid", "plugin1", "valid", "stt"))

	// Invalid YAML
	createFile(t, root, "invalid/plugin2/plugin.yaml", "this is not valid yaml: [")

	// Missing slot
	createPluginYAML(t, root, "invalid/plugin3", `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: No Slot
  slug: no-slot
  catalog: invalid
spec:
  entrypoint:
    transport: process`)

	manifests, warnings := DiscoverWithWarnings(root)

	if len(manifests) != 1 {
		t.Fatalf("expected 1 valid manifest, got %d", len(manifests))
	}
	if manifests[0].Metadata.Slug != "plugin1" {
		t.Fatalf("expected valid manifest to be plugin1, got %s", manifests[0].Metadata.Slug)
	}

	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d", len(warnings))
	}

	// Verify warnings contain expected directories
	dirs := make(map[string]bool)
	for _, w := range warnings {
		dirs[filepath.Base(w.Dir)] = true
	}
	if !dirs["plugin2"] || !dirs["plugin3"] {
		t.Fatalf("expected warnings for plugin2 and plugin3, got: %v", warnings)
	}
}

func TestDiscoverFindsPluginsInCatalogStructure(t *testing.T) {
	root := t.TempDir()

	// Create catalog/slug directory structure with manifests
	createPluginYAML(t, root, "builtin/adapter.stt.mock", adapterManifest("Mock STT", "adapter.stt.mock", "builtin", "stt"))
	createPluginYAML(t, root, "ai/claude", adapterManifest("Claude AI", "claude", "ai", "ai"))
	createPluginYAML(t, root, "tools/detector.git", toolDetectorManifest("Git Detector", "detector.git", "tools"))

	manifests, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	if len(manifests) != 3 {
		t.Fatalf("expected 3 manifests, got %d", len(manifests))
	}

	// Verify manifests are sorted by directory path
	if manifests[0].Metadata.Slug != "claude" {
		t.Fatalf("expected first manifest to be claude, got %s", manifests[0].Metadata.Slug)
	}
	if manifests[1].Metadata.Slug != "adapter.stt.mock" {
		t.Fatalf("expected second manifest to be adapter.stt.mock, got %s", manifests[1].Metadata.Slug)
	}
	if manifests[2].Metadata.Slug != "detector.git" {
		t.Fatalf("expected third manifest to be detector.git, got %s", manifests[2].Metadata.Slug)
	}
}

func TestDiscoverReturnsNilForNonexistentRoot(t *testing.T) {
	manifests, err := Discover("/nonexistent/path/that/does/not/exist")
	if err != nil {
		t.Fatalf("expected no error for nonexistent root, got: %v", err)
	}
	if manifests != nil {
		t.Fatalf("expected nil manifests for nonexistent root, got %d items", len(manifests))
	}
}

func TestDiscoverSkipsInvalidManifests(t *testing.T) {
	root := t.TempDir()

	// Valid manifest
	createPluginYAML(t, root, "valid/plugin1", adapterManifest("Valid", "plugin1", "valid", "stt"))

	// Invalid YAML (will be logged and skipped)
	createFile(t, root, "invalid/plugin2/plugin.yaml", "this is not valid yaml: [")

	// Missing slot (will be logged and skipped)
	createPluginYAML(t, root, "invalid/plugin3", `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: No Slot
  slug: no-slot
  catalog: invalid
spec:
  entrypoint:
    command: ./adapter`)

	// Missing slug (will be logged and skipped)
	createPluginYAML(t, root, "invalid/plugin4", `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: No Slug
spec:
  slot: stt`)

	manifests, err := Discover(root)
	if err != nil {
		t.Fatalf("expected Discover to be resilient and skip invalid manifests, got error: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("expected 1 valid manifest, got %d", len(manifests))
	}
	if manifests[0].Metadata.Slug != "plugin1" {
		t.Fatalf("expected valid manifest to be plugin1, got %s", manifests[0].Metadata.Slug)
	}
}

func TestDiscoverSkipsFilesInRootAndCatalog(t *testing.T) {
	root := t.TempDir()

	// Create files that should be ignored
	createFile(t, root, "README.md", "# Plugins")
	createFile(t, root, "builtin/README.md", "# Builtin plugins")

	// Create valid plugin
	createPluginYAML(t, root, "builtin/plugin1", adapterManifest("Plugin", "plugin1", "builtin", "stt"))

	manifests, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	if len(manifests) != 1 {
		t.Fatalf("expected 1 manifest (ignoring files), got %d", len(manifests))
	}
}

func TestLoadFromDirLoadsManifest(t *testing.T) {
	root := t.TempDir()
	createPluginYAML(t, root, "builtin/test", adapterManifest("Test Adapter", "test", "builtin", "stt"))

	pluginDir := filepath.Join(root, "builtin", "test")
	mf, err := LoadFromDir(pluginDir)
	if err != nil {
		t.Fatalf("LoadFromDir failed: %v", err)
	}

	if mf.Metadata.Name != "Test Adapter" {
		t.Fatalf("expected name 'Test Adapter', got %s", mf.Metadata.Name)
	}
	if mf.Metadata.Slug != "test" {
		t.Fatalf("expected slug 'test', got %s", mf.Metadata.Slug)
	}
	if mf.Type != PluginTypeAdapter {
		t.Fatalf("expected type adapter, got %s", mf.Type)
	}
	if mf.Dir != pluginDir {
		t.Fatalf("expected Dir to be set to %s, got %s", pluginDir, mf.Dir)
	}
}

func TestLoadFromDirPrefersYAMLOverYMLAndJSON(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "test")

	// Create all three variants
	createFile(t, root, filepath.Join("test", "plugin.json"), `{"kind":"Plugin"}`)
	createFile(t, root, filepath.Join("test", "plugin.yml"), adapterManifest("YML Version", "test-yml", "test", "stt"))
	createFile(t, root, filepath.Join("test", "plugin.yaml"), adapterManifest("YAML Version", "test-yaml", "test", "stt"))

	mf, err := LoadFromDir(dir)
	if err != nil {
		t.Fatalf("LoadFromDir failed: %v", err)
	}

	// Should prefer plugin.yaml
	if mf.Metadata.Name != "YAML Version" {
		t.Fatalf("expected plugin.yaml to be preferred, got name: %s", mf.Metadata.Name)
	}
}

func TestLoadFromDirFallsBackToYMLAndJSON(t *testing.T) {
	root := t.TempDir()

	// Test .yml fallback
	ymlDir := filepath.Join(root, "yml-plugin")
	createFile(t, root, filepath.Join("yml-plugin", "plugin.yml"), adapterManifest("YML Plugin", "yml", "test", "stt"))
	mf, err := LoadFromDir(ymlDir)
	if err != nil {
		t.Fatalf("LoadFromDir .yml failed: %v", err)
	}
	if mf.Metadata.Name != "YML Plugin" {
		t.Fatalf("expected YML plugin to load, got: %s", mf.Metadata.Name)
	}

	// Test .json fallback
	jsonDir := filepath.Join(root, "json-plugin")
	createFile(t, root, filepath.Join("json-plugin", "plugin.json"), `{
		"apiVersion": "nap.nupi.ai/v1alpha1",
		"kind": "Plugin",
		"type": "adapter",
		"metadata": {"name": "JSON Plugin", "slug": "json", "catalog": "test"},
		"spec": {"slot": "stt", "mode": "local", "entrypoint": {"command": "./adapter", "transport": "process"}}
	}`)
	mf, err = LoadFromDir(jsonDir)
	if err != nil {
		t.Fatalf("LoadFromDir .json failed: %v", err)
	}
	if mf.Metadata.Name != "JSON Plugin" {
		t.Fatalf("expected JSON plugin to load, got: %s", mf.Metadata.Name)
	}
}

func TestLoadFromDirReturnsErrorForMissingManifest(t *testing.T) {
	root := t.TempDir()
	_, err := LoadFromDir(root)
	if err == nil {
		t.Fatalf("expected error for directory without manifest")
	}
}

func TestMainPathReturnsAbsolutePathForDetector(t *testing.T) {
	root := t.TempDir()
	createPluginYAML(t, root, "tools/test", toolDetectorManifest("Test Detector", "test", "tools"))

	pluginDir := filepath.Join(root, "tools", "test")
	mf, err := LoadFromDir(pluginDir)
	if err != nil {
		t.Fatalf("LoadFromDir failed: %v", err)
	}

	mainPath, err := mf.MainPath()
	if err != nil {
		t.Fatalf("MainPath failed: %v", err)
	}

	expected := filepath.Join(root, "tools", "test", "main.js")
	if mainPath != expected {
		t.Fatalf("expected MainPath %s, got %s", expected, mainPath)
	}
}

func TestMainPathReturnsAbsolutePathForPipelineCleaner(t *testing.T) {
	root := t.TempDir()
	createPluginYAML(t, root, "cleaners/test", pipelineCleanerManifest("Test Cleaner", "test", "cleaners", "cleaner.js"))

	pluginDir := filepath.Join(root, "cleaners", "test")
	mf, err := LoadFromDir(pluginDir)
	if err != nil {
		t.Fatalf("LoadFromDir failed: %v", err)
	}

	mainPath, err := mf.MainPath()
	if err != nil {
		t.Fatalf("MainPath failed: %v", err)
	}

	expected := filepath.Join(root, "cleaners", "test", "cleaner.js")
	if mainPath != expected {
		t.Fatalf("expected MainPath %s, got %s", expected, mainPath)
	}
}

func TestMainPathReturnsErrorForAdapter(t *testing.T) {
	root := t.TempDir()
	createPluginYAML(t, root, "builtin/test", adapterManifest("Test", "test", "builtin", "stt"))

	pluginDir := filepath.Join(root, "builtin", "test")
	mf, err := LoadFromDir(pluginDir)
	if err != nil {
		t.Fatalf("LoadFromDir failed: %v", err)
	}

	_, err = mf.MainPath()
	if err == nil {
		t.Fatalf("expected error for adapter MainPath, got success")
	}
}

func TestRelativeMainPathReturnsRelativePath(t *testing.T) {
	root := t.TempDir()
	createPluginYAML(t, root, "tools/test", toolDetectorManifest("Test", "test", "tools"))

	pluginDir := filepath.Join(root, "tools", "test")
	mf, err := LoadFromDir(pluginDir)
	if err != nil {
		t.Fatalf("LoadFromDir failed: %v", err)
	}

	relPath, err := mf.RelativeMainPath(root)
	if err != nil {
		t.Fatalf("RelativeMainPath failed: %v", err)
	}

	expected := filepath.Join("tools", "test", "main.js")
	if relPath != expected {
		t.Fatalf("expected RelativeMainPath %s, got %s", expected, relPath)
	}
}

// Test helpers

func createPluginYAML(t *testing.T, root, path, content string) {
	t.Helper()
	createFile(t, root, filepath.Join(path, "plugin.yaml"), content)
}

func createFile(t *testing.T, root, path, content string) {
	t.Helper()
	fullPath := filepath.Join(root, path)
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to create directory %s: %v", dir, err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write file %s: %v", fullPath, err)
	}
}

func adapterManifest(name, slug, catalog, slot string) string {
	return `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: ` + name + `
  slug: ` + slug + `
  catalog: ` + catalog + `
  version: 0.0.1
spec:
  slot: ` + slot + `
  mode: local
  entrypoint:
    command: ./adapter
    transport: process`
}

func toolDetectorManifest(name, slug, catalog string) string {
	return `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: tool-detector
metadata:
  name: ` + name + `
  slug: ` + slug + `
  catalog: ` + catalog + `
  version: 0.0.1
spec:
  main: main.js`
}

func pipelineCleanerManifest(name, slug, catalog, main string) string {
	return `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: pipeline-cleaner
metadata:
  name: ` + name + `
  slug: ` + slug + `
  catalog: ` + catalog + `
  version: 0.0.1
spec:
  main: ` + main
}
