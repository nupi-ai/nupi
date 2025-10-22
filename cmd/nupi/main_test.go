package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseManifest_Empty(t *testing.T) {
	raw, err := parseManifest("")
	if err != nil {
		t.Fatalf("parseManifest returned error: %v", err)
	}
	if raw != nil {
		t.Fatalf("expected nil manifest for empty input")
	}
}

func TestParseManifest_JSON(t *testing.T) {
	input := `{"apiVersion":"v1","metadata":{"name":"test"}}`
	raw, err := parseManifest(input)
	if err != nil {
		t.Fatalf("parseManifest returned error: %v", err)
	}
	if string(raw) != input {
		t.Fatalf("expected JSON to remain unchanged, got %s", raw)
	}
}

func TestParseManifest_YAML(t *testing.T) {
	input := `
apiVersion: v1
metadata:
  name: test
spec:
  moduleType: stt
`
	raw, err := parseManifest(input)
	if err != nil {
		t.Fatalf("parseManifest returned error: %v", err)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal converted manifest: %v", err)
	}
	if data["apiVersion"] != "v1" {
		t.Fatalf("expected apiVersion to be preserved, got %v", data["apiVersion"])
	}
	spec, ok := data["spec"].(map[string]interface{})
	if !ok || spec["moduleType"] != "stt" {
		t.Fatalf("expected spec.moduleType=stt, got %v", data["spec"])
	}
}

func TestParseManifest_Invalid(t *testing.T) {
	_, err := parseManifest("{invalid json")
	if err == nil {
		t.Fatalf("expected error for invalid manifest")
	}
	if !strings.Contains(err.Error(), "invalid YAML") {
		t.Fatalf("expected YAML error message, got %v", err)
	}
}

func TestParseManifest_NonStringKeys(t *testing.T) {
	input := `
1: numeric
true: boolean
`
	raw, err := parseManifest(input)
	if err != nil {
		t.Fatalf("parseManifest returned error: %v", err)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal converted manifest: %v", err)
	}
	if data["1"] != "numeric" {
		t.Fatalf("expected numeric key to be preserved as \"1\", got %v", data["1"])
	}
	if data["true"] != "boolean" {
		t.Fatalf("expected boolean key to be preserved as \"true\", got %v", data["true"])
	}
}

func TestParseManifest_DeepNesting(t *testing.T) {
	var builder strings.Builder
	depth := 1100
	for i := 0; i < depth; i++ {
		builder.WriteString(strings.Repeat("  ", i))
		builder.WriteString("node")
		builder.WriteString(strconv.Itoa(i))
		builder.WriteString(":\n")
	}
	builder.WriteString(strings.Repeat("  ", depth))
	builder.WriteString("leaf: value\n")

	if _, err := parseManifest(builder.String()); err != nil {
		t.Fatalf("parseManifest returned error for deep nesting: %v", err)
	}
}

func TestParseManifest_DeepNestingCutoff(t *testing.T) {
	var builder strings.Builder
	depth := 1100
	for i := 0; i < depth; i++ {
		builder.WriteString(strings.Repeat("  ", i))
		builder.WriteString("node")
		builder.WriteString(strconv.Itoa(i))
		builder.WriteString(":\n")
	}
	builder.WriteString(strings.Repeat("  ", depth))
	builder.WriteString("leaf: value\n")

	raw, err := parseManifest(builder.String())
	if err != nil {
		t.Fatalf("parseManifest returned error: %v", err)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal converted manifest: %v", err)
	}

	if maxDepth := maxJSONDepth(data); maxDepth > 1050 {
		t.Fatalf("expected json depth to be bounded, got %d", maxDepth)
	}
}

func maxJSONDepth(value interface{}) int {
	return maxJSONDepthInternal(value, 1)
}

func maxJSONDepthInternal(value interface{}, depth int) int {
	max := depth
	switch v := value.(type) {
	case map[string]interface{}:
		for _, val := range v {
			if d := maxJSONDepthInternal(val, depth+1); d > max {
				max = d
			}
		}
	case []interface{}:
		for _, val := range v {
			if d := maxJSONDepthInternal(val, depth+1); d > max {
				max = d
			}
		}
	}
	return max
}

func TestLoadModuleManifestFile(t *testing.T) {
	manifest := `apiVersion: nap.nupi.ai/v1alpha1
kind: ModuleManifest
metadata:
  name: Local STT
  slug: local-stt
spec:
  moduleType: stt
  entrypoint:
    command: ./module
    args: ["--foo", "bar"]
    transport: grpc
`

	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "module.yaml")
	if err := os.WriteFile(tmpFile, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	spec, raw, err := loadModuleManifestFile(tmpFile)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if spec.Metadata.Slug != "local-stt" {
		t.Fatalf("unexpected slug %q", spec.Metadata.Slug)
	}
	if spec.Spec.ModuleType != "stt" {
		t.Fatalf("unexpected module type %q", spec.Spec.ModuleType)
	}
	if spec.Spec.Entrypoint.Command != "./module" {
		t.Fatalf("unexpected command %q", spec.Spec.Entrypoint.Command)
	}
	if len(spec.Spec.Entrypoint.Args) != 2 {
		t.Fatalf("unexpected args %#v", spec.Spec.Entrypoint.Args)
	}
	if strings.TrimSpace(raw) == "" {
		t.Fatalf("expected raw manifest contents to be returned")
	}
}
