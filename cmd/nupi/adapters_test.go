package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	apihttp "github.com/nupi-ai/nupi/internal/api/http"
	"github.com/nupi-ai/nupi/internal/sanitize"
	"github.com/spf13/cobra"
)

func TestParseManifest_Empty(t *testing.T) {
	t.Parallel()
	raw, err := parseManifest("")
	if err != nil {
		t.Fatalf("parseManifest returned error: %v", err)
	}
	if raw != nil {
		t.Fatalf("expected nil manifest for empty input")
	}
}

func TestParseManifest_JSON(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	input := `
apiVersion: v1
metadata:
  name: test
spec:
  slot: stt
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
	if !ok || spec["slot"] != "stt" {
		t.Fatalf("expected spec.slot=stt, got %v", data["spec"])
	}
}

func TestParseManifest_Invalid(t *testing.T) {
	t.Parallel()
	_, err := parseManifest("{invalid json")
	if err == nil {
		t.Fatalf("expected error for invalid manifest")
	}
	if !strings.Contains(err.Error(), "invalid YAML") {
		t.Fatalf("expected YAML error message, got %v", err)
	}
}

func TestParseManifest_NonStringKeys(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestLoadAdapterManifestFile(t *testing.T) {
	t.Parallel()
	manifest := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Local STT
  slug: local-stt
spec:
  slot: stt
  mode: local
  entrypoint:
    command: ./adapter
    args: ["--foo", "bar"]
    transport: grpc
`

	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "plugin.yaml")
	if err := os.WriteFile(tmpFile, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	parsed, raw, err := loadAdapterManifestFile(tmpFile)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if parsed.Metadata.Slug != "local-stt" {
		t.Fatalf("unexpected slug %q", parsed.Metadata.Slug)
	}
	if parsed.Adapter == nil || parsed.Adapter.Slot != "stt" {
		t.Fatalf("unexpected adapter slot")
	}
	if parsed.Adapter.Entrypoint.Command != "./adapter" {
		t.Fatalf("unexpected command %q", parsed.Adapter.Entrypoint.Command)
	}
	if len(parsed.Adapter.Entrypoint.Args) != 2 {
		t.Fatalf("unexpected args %#v", parsed.Adapter.Entrypoint.Args)
	}
	if strings.TrimSpace(raw) == "" {
		t.Fatalf("expected raw manifest contents to be returned")
	}
}

func TestSanitizeAdapterSlug(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Local STT":         "local-stt",
		"UPPER_case--Slug":  "upper_case--slug",
		"   spaced slug   ": "spaced-slug",
		"../dangerous/path": "dangerous-path",
		"@@@":               "adapter",
		"slug-with-extremely-long-name-that-should-be-trimmed-because-it-exceeds-sixty-four-characters": "slug-with-extremely-long-name-that-should-be-trimmed-because-it-",
	}

	for input, expected := range cases {
		if actual := sanitize.SafeSlug(input); actual != expected {
			t.Fatalf("sanitize.SafeSlug(%q) = %q, expected %q", input, actual, expected)
		}
	}
}

func TestCopyFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix-style permission check not supported on Windows")
	}
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	srcPath := filepath.Join(srcDir, "source.txt")
	content := []byte("hello world")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	dstPath := filepath.Join(dstDir, "copied.txt")
	if err := copyFile(srcPath, dstPath, 0o711); err != nil {
		t.Fatalf("copyFile error: %v", err)
	}

	data, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(data) != string(content) {
		t.Fatalf("expected copied contents %q, got %q", content, data)
	}

	info, err := os.Stat(dstPath)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode().Perm() != 0o711 {
		t.Fatalf("expected permissions 0711, got %v", info.Mode().Perm())
	}
}

func TestCopyFile_SameSourceAndDestination(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "file.txt")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	err := copyFile(path, path, 0o644)
	if err == nil || !strings.Contains(err.Error(), "source and destination are the same") {
		t.Fatalf("expected same-path error, got %v", err)
	}
}

func TestCopyFile_DestinationExists(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	src := filepath.Join(tmpDir, "src.txt")
	dst := filepath.Join(tmpDir, "dst.txt")

	if err := os.WriteFile(src, []byte("src"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("dst"), 0o644); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	err := copyFile(src, dst, 0o644)
	if err == nil || !strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("expected destination exists error, got %v", err)
	}
}

func TestCopyFile_SourceIsSymlink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}

	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "target.txt")
	link := filepath.Join(tmpDir, "link.txt")
	dst := filepath.Join(tmpDir, "dst.txt")

	if err := os.WriteFile(target, []byte("content"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	err := copyFile(link, dst, 0o644)
	if err == nil || !strings.Contains(err.Error(), "source is a symlink") {
		t.Fatalf("expected symlink error, got %v", err)
	}
}

func TestCopyFile_SourceMissing(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "dst.txt")

	err := copyFile(filepath.Join(tmpDir, "missing.txt"), dst, 0o644)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected not exist error, got %v", err)
	}
}

// --- Build timeout flag tests ---

func TestBuildTimeoutFlag_InvalidDurations(t *testing.T) {
	t.Parallel()

	// Create a minimal manifest so we reach the build branch.
	dir := t.TempDir()
	manifest := "apiVersion: nap.nupi.ai/v1alpha1\nkind: Plugin\ntype: adapter\nmetadata:\n  name: test\n  slug: test-timeout\nspec:\n  slot: stt\n  mode: local\n  entrypoint:\n    command: ./test\n    transport: process\n"
	manifestPath := filepath.Join(dir, "plugin.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		value   string
		wantErr string
	}{
		{"invalid string", "abc", "invalid --build-timeout: must be a Go duration like 30s, 2m, 5m"},
		{"negative", "-5s", "invalid --build-timeout: must be positive"},
		{"zero with unit", "0s", "invalid --build-timeout: must be positive"},
		{"zero no unit", "0", "invalid --build-timeout: must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := newAdaptersCommand()
			cmd.SetArgs([]string{"install-local", "--build", "--adapter-dir", dir, "--build-timeout", tc.value, "--manifest-file", manifestPath})
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

func TestBuildTimeoutFlag_ValidDurations(t *testing.T) {
	t.Parallel()

	// Create a minimal manifest so we reach the build branch.
	dir := t.TempDir()
	manifest := "apiVersion: nap.nupi.ai/v1alpha1\nkind: Plugin\ntype: adapter\nmetadata:\n  name: test\n  slug: test-valid\nspec:\n  slot: stt\n  mode: local\n  entrypoint:\n    command: ./test\n    transport: process\n"
	manifestPath := filepath.Join(dir, "plugin.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	validDurations := []string{"30s", "5m", "1h"}
	for _, dur := range validDurations {
		t.Run(dur, func(t *testing.T) {
			t.Parallel()
			cmd := newAdaptersCommand()
			// Valid timeout passes validation, then fails on build (expected — no Go module in dir)
			cmd.SetArgs([]string{"install-local", "--build", "--adapter-dir", dir, "--build-timeout", dur, "--manifest-file", manifestPath})
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error (build failure), got nil")
			}
			if strings.Contains(err.Error(), "invalid --build-timeout") {
				t.Fatalf("valid duration %q was rejected: %v", dur, err)
			}
		})
	}
}

func TestBuildTimeoutFlag_Default(t *testing.T) {
	t.Parallel()
	cmd := newAdaptersCommand()
	for _, c := range cmd.Commands() {
		if c.Name() == "install-local" {
			val, _ := c.Flags().GetString("build-timeout")
			if val != "5m" {
				t.Fatalf("expected default build-timeout to be %q, got %q", "5m", val)
			}
			return
		}
	}
	t.Fatal("install-local subcommand not found")
}

func TestBuildTimeout_ErrorMessageIncludesDuration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create a minimal Go module
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cmd", "adapter"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmd", "adapter", "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create manifest
	manifest := "apiVersion: nap.nupi.ai/v1alpha1\nkind: Plugin\ntype: adapter\nmetadata:\n  name: test\n  slug: test-timeout\nspec:\n  slot: stt\n  mode: local\n  entrypoint:\n    command: ./test\n    transport: process\n"
	manifestPath := filepath.Join(dir, "plugin.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newAdaptersCommand()
	cmd.SetArgs([]string{
		"install-local",
		"--manifest-file", manifestPath,
		"--build",
		"--adapter-dir", dir,
		"--build-timeout", "1ns",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "build timed out after") {
		t.Fatalf("expected 'build timed out after' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "1ns") {
		t.Fatalf("expected duration '1ns' in error, got: %v", err)
	}
}

// --- dirExists helper tests ---

func TestDirExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if !dirExists(dir) {
		t.Fatal("expected existing directory to return true")
	}
	if dirExists(filepath.Join(dir, "nonexistent")) {
		t.Fatal("expected non-existent path to return false")
	}
	filePath := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(filePath, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirExists(filePath) {
		t.Fatal("expected file path to return false (not a directory)")
	}
}

// --- Build artifact cleanup tests ---

func TestCleanupBuildArtifacts_NewDist(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	distDir := filepath.Join(dir, "dist")
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(distDir, "my-adapter")
	if err := os.WriteFile(outputPath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	cleanupBuildArtifacts(io.Discard, distDir, outputPath, false, false)

	if _, err := os.Stat(distDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expected entire dist/ to be removed when newly created")
	}
}

func TestCleanupBuildArtifacts_ExistingDist(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	distDir := filepath.Join(dir, "dist")
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatal(err)
	}

	otherFile := filepath.Join(distDir, "other-file")
	if err := os.WriteFile(otherFile, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}

	outputPath := filepath.Join(distDir, "my-adapter")
	if err := os.WriteFile(outputPath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	cleanupBuildArtifacts(io.Discard, distDir, outputPath, true, false)

	if _, err := os.Stat(distDir); err != nil {
		t.Fatalf("dist dir should still exist: %v", err)
	}
	if _, err := os.Stat(otherFile); err != nil {
		t.Fatalf("pre-existing file should be preserved: %v", err)
	}
	if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("output file should be removed")
	}
}

// --- Adapter directory cleanup tests ---

func TestCleanupAdapterDir_NewDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	adapterHome := filepath.Join(dir, "plugins", "my-adapter")
	binDir := filepath.Join(adapterHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(binDir, "my-adapter")
	if err := os.WriteFile(destPath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	cleanupAdapterDir(io.Discard, adapterHome, destPath, false, false)

	if _, err := os.Stat(adapterHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expected entire adapter dir to be removed when newly created")
	}
}

func TestCleanupAdapterDir_ExistingDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	adapterHome := filepath.Join(dir, "plugins", "my-adapter")
	binDir := filepath.Join(adapterHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configFile := filepath.Join(adapterHome, "config.yaml")
	if err := os.WriteFile(configFile, []byte("config"), 0o644); err != nil {
		t.Fatal(err)
	}

	destPath := filepath.Join(binDir, "my-adapter")
	if err := os.WriteFile(destPath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	cleanupAdapterDir(io.Discard, adapterHome, destPath, true, false)

	if _, err := os.Stat(adapterHome); err != nil {
		t.Fatalf("adapter dir should still exist: %v", err)
	}
	if _, err := os.Stat(configFile); err != nil {
		t.Fatalf("pre-existing config file should be preserved: %v", err)
	}
	if _, err := os.Stat(destPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("binary should be removed")
	}
}

func TestCleanupAdapterDir_RegistrationFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	adapterHome := filepath.Join(dir, "plugins", "my-adapter")
	binDir := filepath.Join(adapterHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(binDir, "my-adapter")
	if err := os.WriteFile(destPath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	cleanupAdapterDir(io.Discard, adapterHome, destPath, false, false)

	if _, err := os.Stat(adapterHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("adapter dir should be removed on registration failure (newly created)")
	}
}

func TestCleanupAdapterDir_ContrastBindingVsRegistrationFailure(t *testing.T) {
	t.Parallel()

	// Registration failure: cleanup IS called → directory removed.
	t.Run("registration_failure_cleans_up", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		adapterHome := filepath.Join(dir, "plugins", "my-adapter")
		binDir := filepath.Join(adapterHome, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		destPath := filepath.Join(binDir, "my-adapter")
		if err := os.WriteFile(destPath, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}

		cleanupAdapterDir(io.Discard, adapterHome, destPath, false, false)

		if _, err := os.Stat(adapterHome); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("registration failure should clean up adapter directory")
		}
	})

	// Binding failure: cleanupAdapterDir is called with both "existed" flags
	// true, so it must be a no-op — all pre-existing artifacts survive.
	t.Run("binding_failure_preserves_artifacts", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		adapterHome := filepath.Join(dir, "plugins", "my-adapter")
		binDir := filepath.Join(adapterHome, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		destPath := filepath.Join(binDir, "my-adapter")
		if err := os.WriteFile(destPath, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}

		// Both existed flags true → cleanup must preserve everything.
		cleanupAdapterDir(io.Discard, adapterHome, destPath, true, true)

		if _, err := os.Stat(destPath); err != nil {
			t.Fatalf("binary should be preserved: %v", err)
		}
		if _, err := os.Stat(adapterHome); err != nil {
			t.Fatalf("adapter dir should be preserved: %v", err)
		}
	})
}

func TestCleanupBuildArtifacts_PreExistingOutputFile_Preserved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	distDir := filepath.Join(dir, "dist")
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatal(err)
	}

	outputPath := filepath.Join(distDir, "my-adapter")
	if err := os.WriteFile(outputPath, []byte("existing-output"), 0o755); err != nil {
		t.Fatal(err)
	}

	// dist existed, output file existed — cleanup must NOT remove the pre-existing output
	cleanupBuildArtifacts(io.Discard, distDir, outputPath, true, true)

	if _, err := os.Stat(outputPath); err != nil {
		t.Fatal("pre-existing output file should be preserved")
	}
	content, _ := os.ReadFile(outputPath)
	if string(content) != "existing-output" {
		t.Fatal("output file content should be unchanged")
	}
}

func TestCleanupAdapterDir_PreExistingDestBinary_Preserved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	adapterHome := filepath.Join(dir, "plugins", "my-adapter")
	binDir := filepath.Join(adapterHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	destPath := filepath.Join(binDir, "my-adapter")
	if err := os.WriteFile(destPath, []byte("existing-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	// dir existed, dest file existed — cleanup must NOT remove the pre-existing binary
	cleanupAdapterDir(io.Discard, adapterHome, destPath, true, true)

	if _, err := os.Stat(destPath); err != nil {
		t.Fatal("pre-existing binary should be preserved")
	}
	content, _ := os.ReadFile(destPath)
	if string(content) != "existing-binary" {
		t.Fatal("binary content should be unchanged")
	}
}

func TestCleanupAdapterDir_ExistingDir_EmptyBinRemoved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	adapterHome := filepath.Join(dir, "plugins", "my-adapter")
	binDir := filepath.Join(adapterHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-existing config in adapter dir, but bin/ was newly created by install
	configFile := filepath.Join(adapterHome, "config.yaml")
	if err := os.WriteFile(configFile, []byte("config"), 0o644); err != nil {
		t.Fatal(err)
	}

	destPath := filepath.Join(binDir, "my-adapter")
	if err := os.WriteFile(destPath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	cleanupAdapterDir(io.Discard, adapterHome, destPath, true, false)

	// Binary should be removed
	if _, err := os.Stat(destPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("binary should be removed")
	}
	// Empty bin/ should be removed (was likely created by this install)
	if _, err := os.Stat(binDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty bin/ directory should be removed")
	}
	// Adapter dir with pre-existing config should be preserved
	if _, err := os.Stat(adapterHome); err != nil {
		t.Fatalf("adapter dir should still exist: %v", err)
	}
	if _, err := os.Stat(configFile); err != nil {
		t.Fatalf("config file should be preserved: %v", err)
	}
}

func TestCleanupAdapterDir_ExistingDir_NonEmptyBinPreserved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	adapterHome := filepath.Join(dir, "plugins", "my-adapter")
	binDir := filepath.Join(adapterHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-existing binary from previous install
	otherBinary := filepath.Join(binDir, "other-adapter")
	if err := os.WriteFile(otherBinary, []byte("other"), 0o755); err != nil {
		t.Fatal(err)
	}

	destPath := filepath.Join(binDir, "my-adapter")
	if err := os.WriteFile(destPath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	cleanupAdapterDir(io.Discard, adapterHome, destPath, true, false)

	// Our binary should be removed
	if _, err := os.Stat(destPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("binary should be removed")
	}
	// bin/ should be preserved because it still has other files
	if _, err := os.Stat(binDir); err != nil {
		t.Fatalf("bin/ with other files should be preserved: %v", err)
	}
	if _, err := os.Stat(otherBinary); err != nil {
		t.Fatalf("other binary should be preserved: %v", err)
	}
}

func TestCleanupBuildArtifacts_WritesToWriter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	distDir := filepath.Join(dir, "dist")
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(distDir, "my-adapter")
	if err := os.WriteFile(outputPath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	// With io.Discard (simulates JSON mode), no output should be written anywhere
	cleanupBuildArtifacts(io.Discard, distDir, outputPath, true, false)

	// Cleanup should still work (file removed) — only output is suppressed
	if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("output file should still be removed even with discarded writer")
	}
}

func TestCleanup_PreExistingStateIsNoOp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Set up build artifacts with pre-existing state.
	distDir := filepath.Join(dir, "dist")
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatal(err)
	}
	buildOutput := filepath.Join(distDir, "my-adapter")
	if err := os.WriteFile(buildOutput, []byte("build-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Set up adapter directory with pre-existing state.
	adapterHome := filepath.Join(dir, "plugins", "my-adapter")
	binDir := filepath.Join(adapterHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(binDir, "my-adapter")
	if err := os.WriteFile(destPath, []byte("adapter-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Calling cleanup with both "existed" flags true should be a no-op:
	// all pre-existing artifacts must survive.
	cleanupBuildArtifacts(io.Discard, distDir, buildOutput, true, true)
	cleanupAdapterDir(io.Discard, adapterHome, destPath, true, true)

	if _, err := os.Stat(buildOutput); err != nil {
		t.Fatalf("build output should be preserved: %v", err)
	}
	buildContent, _ := os.ReadFile(buildOutput)
	if string(buildContent) != "build-binary" {
		t.Fatal("build output content should be unchanged")
	}
	if _, err := os.Stat(destPath); err != nil {
		t.Fatalf("adapter binary should be preserved: %v", err)
	}
	destContent, _ := os.ReadFile(destPath)
	if string(destContent) != "adapter-binary" {
		t.Fatal("adapter binary content should be unchanged")
	}
}

// --- Sanity check tests ---

func TestSanityCheck_EmptyBinaryPath(t *testing.T) {
	t.Parallel()
	result := sanityCheck("")
	if result.Passed {
		t.Fatal("expected sanity check to fail for empty binary path")
	}
	if len(result.Checks) != 1 {
		t.Fatalf("expected 1 check for empty path, got %d", len(result.Checks))
	}
	c := result.Checks[0]
	if c.Name != "binary_exists" {
		t.Fatalf("expected check name binary_exists, got %q", c.Name)
	}
	if c.Passed {
		t.Fatal("expected binary_exists to fail for empty path")
	}
	if !strings.Contains(c.Message, "empty") {
		t.Fatalf("expected message to mention 'empty', got %q", c.Message)
	}
	if !strings.Contains(c.Remediation, "--binary") {
		t.Fatalf("expected remediation to mention --binary flag, got %q", c.Remediation)
	}
	if result.BinaryPath != "" {
		t.Fatalf("expected empty BinaryPath for empty input, got %q", result.BinaryPath)
	}
}

func TestSanityCheck_BinaryExistsAndExecutable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "adapter")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	result := sanityCheck(bin)
	if !result.Passed {
		t.Fatalf("expected sanity check to pass, got failed: %+v", result.Checks)
	}
	if !result.Checks[0].Passed || result.Checks[0].Name != "binary_exists" {
		t.Fatalf("expected binary_exists to pass, got %+v", result.Checks[0])
	}
	if result.Checks[0].Message == "" {
		t.Fatal("expected binary_exists check to have a non-empty Message")
	}
	if len(result.Checks) != 2 {
		t.Fatalf("expected 2 checks (exists + executable), got %d", len(result.Checks))
	}
	if !result.Checks[1].Passed || result.Checks[1].Name != "binary_executable" {
		t.Fatalf("expected binary_executable to pass, got %+v", result.Checks[1])
	}
	if result.Checks[1].Message == "" {
		t.Fatal("expected binary_executable check to have a non-empty Message")
	}
}

func TestSanityCheck_BinaryMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "nonexistent")

	result := sanityCheck(missing)
	if result.Passed {
		t.Fatal("expected sanity check to fail for missing binary")
	}
	if len(result.Checks) != 1 {
		t.Fatalf("expected 1 check (binary_exists fail, early return), got %d", len(result.Checks))
	}
	c := result.Checks[0]
	if c.Passed {
		t.Fatal("expected binary_exists to fail")
	}
	if !strings.Contains(c.Remediation, missing) {
		t.Fatalf("remediation should include path %q, got %q", missing, c.Remediation)
	}
	if !strings.Contains(c.Remediation, "check --binary flag or build output") {
		t.Fatalf("remediation should include hint, got %q", c.Remediation)
	}
}

func TestSanityCheck_BinaryNotExecutable(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("executable permission check not applicable on Windows")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "adapter")
	if err := os.WriteFile(bin, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := sanityCheck(bin)
	if result.Passed {
		t.Fatal("expected sanity check to fail for non-executable binary")
	}
	if len(result.Checks) != 2 {
		t.Fatalf("expected 2 checks, got %d", len(result.Checks))
	}
	c := result.Checks[1]
	if c.Passed || c.Name != "binary_executable" {
		t.Fatalf("expected binary_executable to fail, got %+v", c)
	}
	if !strings.Contains(c.Remediation, "chmod +x") {
		t.Fatalf("remediation should include chmod hint, got %q", c.Remediation)
	}
	if !strings.Contains(c.Remediation, bin) {
		t.Fatalf("remediation should include path, got %q", c.Remediation)
	}
}

func TestSanityCheck_DirectoryNotRegularFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	subdir := filepath.Join(dir, "not-a-binary")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	result := sanityCheck(subdir)
	if result.Passed {
		t.Fatal("expected sanity check to fail for directory")
	}
	if len(result.Checks) != 1 {
		t.Fatalf("expected 1 check (early return), got %d", len(result.Checks))
	}
	c := result.Checks[0]
	if c.Passed || c.Name != "binary_exists" {
		t.Fatalf("expected binary_exists to fail, got %+v", c)
	}
	if !strings.Contains(c.Remediation, "not a regular file") {
		t.Fatalf("remediation should mention 'not a regular file', got %q", c.Remediation)
	}
	if !strings.Contains(c.Remediation, subdir) {
		t.Fatalf("remediation should include path, got %q", c.Remediation)
	}
}

func TestSanityCheck_SymlinkToExecutable(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real-adapter")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "adapter-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	result := sanityCheck(link)
	if !result.Passed {
		t.Fatalf("expected sanity check to pass for symlink to executable, got: %+v", result.Checks)
	}
	if result.BinaryPath != link {
		t.Fatalf("expected BinaryPath=%q (symlink), got %q", link, result.BinaryPath)
	}
}

func TestSanityCheck_SymlinkToNonExecutable(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real-adapter")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "adapter-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	result := sanityCheck(link)
	if result.Passed {
		t.Fatalf("expected sanity check to fail for symlink to non-executable, got passed")
	}
	if len(result.Checks) < 2 {
		t.Fatalf("expected at least 2 checks, got %d", len(result.Checks))
	}
	execCheck := result.Checks[1]
	if execCheck.Passed {
		t.Fatalf("expected binary_executable check to fail")
	}
	if !strings.Contains(execCheck.Remediation, "chmod +x") {
		t.Fatalf("expected chmod remediation hint, got %q", execCheck.Remediation)
	}
}

func TestSanityCheck_PermissionDenied(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission test not applicable on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("permission test not applicable when running as root")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "adapter")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Remove read+execute on parent directory so os.Stat fails with EACCES.
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	result := sanityCheck(bin)
	if result.Passed {
		t.Fatal("expected sanity check to fail for inaccessible binary")
	}
	c := result.Checks[0]
	if !strings.Contains(c.Message, "cannot access binary") {
		t.Fatalf("expected 'cannot access binary' message for permission error, got %q", c.Message)
	}
	if !strings.Contains(c.Remediation, "check file permissions") {
		t.Fatalf("expected permission hint in remediation, got %q", c.Remediation)
	}
}

func TestSanityCheck_PathWithSpecialChars(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("single-quote in path not testable on Windows (NTFS disallows quotes)")
	}
	// Create a directory with spaces and a single quote to exercise the
	// remediation message escaping (shell-safe quoting for chmod +x).
	dir := t.TempDir()
	specialDir := filepath.Join(dir, "it's an adapter")
	if err := os.MkdirAll(specialDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(specialDir, "my binary")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := sanityCheck(bin)
	if result.Passed {
		t.Fatal("expected sanity check to fail (binary not executable)")
	}
	// Verify remediation properly escapes the single quote in the path.
	// The correct shell-safe escaping for a single quote inside single quotes
	// is: replace ' with '\'' (end quote, escaped quote, restart quote).
	for _, c := range result.Checks {
		if c.Name == "binary_executable" && !c.Passed {
			if !strings.Contains(c.Remediation, "chmod +x") {
				t.Fatalf("expected chmod +x in remediation, got %q", c.Remediation)
			}
			if !strings.Contains(c.Remediation, `'\''`) {
				t.Fatalf("expected shell-escaped single quote ('\\''') in remediation, got %q", c.Remediation)
			}
			return
		}
	}
	t.Fatal("expected binary_executable check in results")
}

func TestSanityCheck_JSONOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "adapter")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	result := sanityCheck(bin)

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal sanity check result: %v", err)
	}

	var parsed sanityCheckResult
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if !parsed.Passed {
		t.Fatal("expected passed=true after round-trip")
	}
	if parsed.BinaryPath != bin {
		t.Fatalf("expected binary_path=%q, got %q", bin, parsed.BinaryPath)
	}
	if len(parsed.Checks) != 2 {
		t.Fatalf("expected 2 checks after round-trip, got %d", len(parsed.Checks))
	}
	// Verify individual check fields survive round-trip.
	for i, orig := range result.Checks {
		got := parsed.Checks[i]
		if got.Name != orig.Name {
			t.Fatalf("check[%d] Name mismatch: got %q, want %q", i, got.Name, orig.Name)
		}
		if got.Passed != orig.Passed {
			t.Fatalf("check[%d] Passed mismatch: got %v, want %v", i, got.Passed, orig.Passed)
		}
		if got.Message != orig.Message {
			t.Fatalf("check[%d] Message mismatch: got %q, want %q", i, got.Message, orig.Message)
		}
	}

	// Verify installLocalResult JSON structure.
	wrapper := installLocalResult{
		SanityCheck: &result,
	}
	wdata, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatalf("failed to marshal installLocalResult: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(wdata, &raw); err != nil {
		t.Fatalf("failed to unmarshal wrapper: %v", err)
	}
	if _, ok := raw["sanity_check"]; !ok {
		t.Fatal("expected sanity_check key in JSON output")
	}
}

func TestPrintSanityCheckResult_Success(t *testing.T) {
	t.Parallel()
	result := sanityCheckResult{
		Passed: true,
		Checks: []sanityCheckEntry{
			{Name: "binary_exists", Passed: true, Message: "binary found"},
			{Name: "binary_executable", Passed: true, Message: "binary is executable"},
		},
	}

	var buf bytes.Buffer
	printSanityCheckResult(&buf, result)
	output := buf.String()

	if !strings.Contains(output, "Sanity check passed") {
		t.Fatalf("expected success message, got %q", output)
	}
	if !strings.Contains(output, "binary executable") {
		t.Fatalf("expected 'binary executable' in output, got %q", output)
	}
}

func TestPrintSanityCheckResult_NoDetailChecks(t *testing.T) {
	t.Parallel()
	// Edge case: all checks are binary_exists (filtered out from success message).
	result := sanityCheckResult{
		Passed: true,
		Checks: []sanityCheckEntry{
			{Name: "binary_exists", Passed: true, Message: "binary found"},
		},
	}

	var buf bytes.Buffer
	printSanityCheckResult(&buf, result)
	output := buf.String()

	if !strings.Contains(output, "Sanity check passed") {
		t.Fatalf("expected success message, got %q", output)
	}
	// Must NOT produce trailing ": " with empty detail.
	if strings.Contains(output, "passed: \n") || strings.HasSuffix(strings.TrimSpace(output), ":") {
		t.Fatalf("expected clean output without trailing colon, got %q", output)
	}
}

func TestPrintSanityCheckResult_Failure(t *testing.T) {
	t.Parallel()
	result := sanityCheckResult{
		Passed: false,
		Checks: []sanityCheckEntry{
			{Name: "binary_exists", Passed: false, Remediation: "binary not found at /tmp/x — check --binary flag or build output"},
		},
	}

	var buf bytes.Buffer
	printSanityCheckResult(&buf, result)
	output := buf.String()

	if !strings.Contains(output, "Sanity check failed") {
		t.Fatalf("expected failure message, got %q", output)
	}
	if !strings.Contains(output, "check --binary flag or build output") {
		t.Fatalf("expected remediation in output, got %q", output)
	}
}

func TestSanityCheck_BinaryPath_StoredInResult(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "my-adapter")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	result := sanityCheck(bin)
	if result.BinaryPath != bin {
		t.Fatalf("expected BinaryPath=%q, got %q", bin, result.BinaryPath)
	}
}

func TestSanityCheck_FailureDoesNotBlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Simulate a failed sanity check (missing binary).
	missing := filepath.Join(dir, "nonexistent-adapter")
	result := sanityCheck(missing)

	// Key contract: sanityCheck returns a result value, never panics, never returns
	// an error type. This means the caller's control flow continues unconditionally
	// after the call — a failed sanity check cannot prevent slot binding in
	// adaptersInstallLocal because there is no error to short-circuit on.
	if result.Passed {
		t.Fatal("expected sanity check to report failure")
	}

	// The result can still be serialized (for JSON mode) and printed (for text mode).
	if _, err := json.Marshal(result); err != nil {
		t.Fatalf("failed to marshal failed sanity result: %v", err)
	}

	// Text mode output works for failed results.
	var buf bytes.Buffer
	printSanityCheckResult(&buf, result)
	if !strings.Contains(buf.String(), "Sanity check failed") {
		t.Fatalf("expected failure output from printSanityCheckResult, got %q", buf.String())
	}

	// Verify the installLocalResult wrapper works with a failed sanity check.
	wrapper := installLocalResult{
		SanityCheck: &result,
	}
	data, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatalf("failed to marshal installLocalResult with failed sanity: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to parse wrapper JSON: %v", err)
	}
	sc, ok := raw["sanity_check"].(map[string]interface{})
	if !ok {
		t.Fatal("expected sanity_check in JSON")
	}
	if sc["passed"] != false {
		t.Fatal("expected passed=false in JSON output")
	}
}

func TestSanityCheck_BareCommand_ResolvedViaPATH(t *testing.T) {
	t.Parallel()
	// In the real install-local flow, normalizeCommand resolves bare commands
	// via exec.LookPath before sanityCheck is called. This test verifies the
	// combined normalizeCommand → sanityCheck pipeline works end-to-end.
	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "cmd"
	} else {
		cmd = "ls"
	}

	normalized := normalizeCommand(cmd, nil)
	result := sanityCheck(normalized)
	if !result.Passed {
		t.Fatalf("expected sanity check to pass for normalized command %q (from %q), got: %+v", normalized, cmd, result.Checks)
	}
	// BinaryPath should be the resolved absolute path, not the bare name.
	if !filepath.IsAbs(result.BinaryPath) {
		t.Fatalf("expected BinaryPath to be absolute after PATH resolution, got %q", result.BinaryPath)
	}
}

func TestSanityCheck_BareCommand_NotInPATH(t *testing.T) {
	t.Parallel()
	// A bare command that does not exist in PATH should fail the binary_exists check.
	result := sanityCheck("nonexistent-command-xyz-12345")
	if result.Passed {
		t.Fatal("expected sanity check to fail for nonexistent bare command")
	}
	if len(result.Checks) < 1 {
		t.Fatal("expected at least 1 check entry")
	}
	c := result.Checks[0]
	if c.Passed || c.Name != "binary_exists" {
		t.Fatalf("expected binary_exists to fail, got %+v", c)
	}
}

func TestInstallLocalResult_NilSanityCheck(t *testing.T) {
	t.Parallel()
	// When transport is remote (gRPC/HTTP) without --copy-binary, sanity check is nil.
	// JSON output must omit the sanity_check key entirely (omitempty).
	wrapper := installLocalResult{
		Adapter: apihttp.AdapterDescriptor{
			ID:   "test-adapter",
			Name: "Test",
			Type: "stt",
		},
		SanityCheck: nil,
	}
	data, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatalf("failed to marshal installLocalResult with nil sanity: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to parse wrapper JSON: %v", err)
	}
	if _, ok := raw["sanity_check"]; ok {
		t.Fatal("expected sanity_check to be omitted when nil (remote transport)")
	}
	if raw["adapter"] == nil {
		t.Fatal("expected adapter key in JSON output")
	}
}

// --- Path normalization tests (exercising the production normalizeCommand helper) ---

func TestNormalizeCommand_BareCommand_ResolvedToAbsolute(t *testing.T) {
	t.Parallel()

	// Pick a bare command that exists on the current platform.
	var bareCmd string
	if runtime.GOOS == "windows" {
		bareCmd = "cmd"
	} else {
		bareCmd = "ls"
	}

	// Verify it actually resolves via LookPath (test prerequisite).
	expectedAbs, err := exec.LookPath(bareCmd)
	if err != nil {
		t.Skipf("bare command %q not found in PATH: %v", bareCmd, err)
	}

	got := normalizeCommand(bareCmd, nil)
	if !filepath.IsAbs(got) {
		t.Fatalf("expected absolute path, got %q", got)
	}
	if got != expectedAbs {
		t.Fatalf("expected %q, got %q", expectedAbs, got)
	}
}

func TestNormalizeCommand_RelativePath_ResolvedToAbsolute(t *testing.T) {
	// DO NOT add t.Parallel() — this test uses t.Chdir which mutates the
	// process-wide working directory for the test duration.
	//
	// Uses t.Chdir (Go 1.24+) which safely manages CWD for the test duration
	// and restores it on cleanup. This avoids raw os.Chdir which is a
	// process-wide mutation that can interfere with concurrent parallel tests.

	dir := t.TempDir()
	binPath := filepath.Join(dir, "subdir", "adapter")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Simulate a relative path with a separator (e.g. "./subdir/adapter").
	relCmd := "." + string(filepath.Separator) + "subdir" + string(filepath.Separator) + "adapter"

	t.Chdir(dir)

	got := normalizeCommand(relCmd, nil)
	if !filepath.IsAbs(got) {
		t.Fatalf("expected absolute path, got %q", got)
	}
	// Resolve symlinks in both paths (e.g. macOS /var → /private/var)
	// because t.Chdir and filepath.Abs may or may not resolve symlinks.
	expected := filepath.Join(dir, "subdir", "adapter")
	gotResolved, err := filepath.EvalSymlinks(filepath.Dir(got))
	if err != nil {
		t.Fatal(err)
	}
	gotResolved = filepath.Join(gotResolved, filepath.Base(got))
	expectedResolved, err := filepath.EvalSymlinks(filepath.Dir(expected))
	if err != nil {
		t.Fatal(err)
	}
	expectedResolved = filepath.Join(expectedResolved, filepath.Base(expected))
	if gotResolved != expectedResolved {
		t.Fatalf("expected %q, got %q", expectedResolved, gotResolved)
	}
}

func TestNormalizeCommand_AlreadyAbsolute_Unchanged(t *testing.T) {
	t.Parallel()

	abs := filepath.Join(string(filepath.Separator), "usr", "bin", "adapter")
	got := normalizeCommand(abs, nil)
	if got != abs {
		t.Fatalf("expected %q unchanged, got %q", abs, got)
	}
}

func TestNormalizeCommand_Empty_Unchanged(t *testing.T) {
	t.Parallel()

	got := normalizeCommand("", nil)
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestNormalizeCommand_BareNotInPATH_WarnsToWriter(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	got := normalizeCommand("surely-nonexistent-command-xyz", &buf)
	if got != "surely-nonexistent-command-xyz" {
		t.Fatalf("expected original value returned, got %q", got)
	}
	if !strings.Contains(buf.String(), "Could not resolve") {
		t.Fatalf("expected warning written to writer, got %q", buf.String())
	}
}

// --- Windows executable extension check tests ---

func TestCheckWindowsExecutableExtension_ValidExtensions(t *testing.T) {
	t.Parallel()
	for _, ext := range []string{".exe", ".bat", ".cmd", ".com", ".EXE", ".Bat", ".CMD"} {
		entry := checkWindowsExecutableExtension("/path/to/binary" + ext)
		if !entry.Passed {
			t.Fatalf("expected pass for extension %q, got: %+v", ext, entry)
		}
		if entry.Name != "binary_executable" {
			t.Fatalf("expected check name binary_executable, got %q", entry.Name)
		}
	}
}

func TestCheckWindowsExecutableExtension_InvalidExtensions(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"/path/to/binary",
		"/path/to/binary.sh",
		"/path/to/binary.py",
		"/path/to/binary.txt",
		"/path/to/binary.dll",
	} {
		entry := checkWindowsExecutableExtension(path)
		if entry.Passed {
			t.Fatalf("expected fail for path %q, got: %+v", path, entry)
		}
		if entry.Name != "binary_executable" {
			t.Fatalf("expected check name binary_executable, got %q", entry.Name)
		}
		if !strings.Contains(entry.Remediation, ".exe, .bat, .cmd, .com") {
			t.Fatalf("expected remediation to list valid extensions, got %q", entry.Remediation)
		}
	}
}

// --- resolveWindowsPATHEXT tests (cross-platform) ---

func TestResolveWindowsPATHEXT_FindsExeExtension(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Create "adapter.exe" but NOT "adapter".
	exePath := filepath.Join(dir, "adapter.exe")
	if err := os.WriteFile(exePath, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	resolved, info, ok := resolveWindowsPATHEXT(filepath.Join(dir, "adapter"))
	if !ok {
		t.Fatal("expected resolveWindowsPATHEXT to find adapter.exe")
	}
	if resolved != exePath {
		t.Fatalf("expected %q, got %q", exePath, resolved)
	}
	if !info.Mode().IsRegular() {
		t.Fatal("expected regular file info")
	}
}

func TestResolveWindowsPATHEXT_SkipsWhenExtensionPresent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	shPath := filepath.Join(dir, "adapter.sh")
	if err := os.WriteFile(shPath, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, _, ok := resolveWindowsPATHEXT(shPath)
	if ok {
		t.Fatal("expected resolveWindowsPATHEXT to skip path with existing extension")
	}
}

func TestResolveWindowsPATHEXT_NoMatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, _, ok := resolveWindowsPATHEXT(filepath.Join(dir, "nonexistent"))
	if ok {
		t.Fatal("expected resolveWindowsPATHEXT to return false for missing file")
	}
}

func TestResolveWindowsPATHEXT_CustomPATHEXT(t *testing.T) {
	// Cannot use t.Parallel() — t.Setenv mutates process-wide environment.
	dir := t.TempDir()
	// Create "adapter.py" — normally not in the hardcoded extension list.
	pyPath := filepath.Join(dir, "adapter.py")
	if err := os.WriteFile(pyPath, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	// With PATHEXT set to include .py, resolution should find adapter.py.
	t.Setenv("PATHEXT", ".EXE;.PY;;.SH")
	resolved, info, ok := resolveWindowsPATHEXT(filepath.Join(dir, "adapter"))
	if !ok {
		t.Fatal("expected resolveWindowsPATHEXT to find adapter.py via PATHEXT env")
	}
	if resolved != pyPath {
		t.Fatalf("expected %q, got %q", pyPath, resolved)
	}
	if !info.Mode().IsRegular() {
		t.Fatal("expected regular file info")
	}

	// Empty entries in PATHEXT (from ";;") should be skipped without error.
	// The ".SH" entry should also be lowercased for matching but the candidate
	// is built from the lowercased extension, so "adapter.sh" would be tried.
}

func TestResolveWindowsPATHEXT_CustomPATHEXT_NoFallbackToHardcoded(t *testing.T) {
	// Cannot use t.Parallel() — t.Setenv mutates process-wide environment.
	dir := t.TempDir()
	// Create "adapter.exe" — normally found via hardcoded list.
	exePath := filepath.Join(dir, "adapter.exe")
	if err := os.WriteFile(exePath, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	// When PATHEXT is set but does NOT include .exe, resolution should fail
	// (custom PATHEXT overrides hardcoded list entirely).
	t.Setenv("PATHEXT", ".PY;.SH")
	_, _, ok := resolveWindowsPATHEXT(filepath.Join(dir, "adapter"))
	if ok {
		t.Fatal("expected resolveWindowsPATHEXT to NOT fall back to hardcoded .exe when PATHEXT is set")
	}
}

// --- containsPathSeparator tests ---

func TestContainsPathSeparator(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		{"bare-command", false},
		{"./relative/path", true},
		{`.\relative\path`, true},
		{"sub/dir", true},
		{`sub\dir`, true},
		{"/absolute/path", true},
		{`C:\absolute\path`, true},
		{"", false},
		{"no-sep", false},
	}
	for _, tt := range tests {
		if got := containsPathSeparator(tt.input); got != tt.want {
			t.Errorf("containsPathSeparator(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// --- Integration tests: adaptersInstallLocal handler path ---

// mockRegisterGRPCMu serializes access to adaptersRegisterGRPCFunc across handler-path
// integration tests. The mutex is locked on mock setup and unlocked on cleanup,
// so accidentally adding t.Parallel() to a handler test will block instead of race.
var mockRegisterGRPCMu sync.Mutex

// mockRegisterGRPC replaces adaptersRegisterGRPCFunc for the duration of the test
// and returns a cleanup function plus a call counter. Acquires mockRegisterGRPCMu to
// prevent concurrent mutation — tests that call this SHOULD NOT use t.Parallel() (the
// mutex serializes them if they do, but sequential execution is still preferred for clarity).
func mockRegisterGRPC(fn func(*OutputFormatter, *apiv1.RegisterAdapterRequest) (*apiv1.RegisterAdapterResponse, error)) (cleanup func(), called *int32) {
	mockRegisterGRPCMu.Lock()
	old := adaptersRegisterGRPCFunc
	var callCount int32
	adaptersRegisterGRPCFunc = func(out *OutputFormatter, req *apiv1.RegisterAdapterRequest) (*apiv1.RegisterAdapterResponse, error) {
		atomic.AddInt32(&callCount, 1)
		return fn(out, req)
	}
	return func() {
		adaptersRegisterGRPCFunc = old
		mockRegisterGRPCMu.Unlock()
	}, &callCount
}

// newAdaptersCommandWithJSON creates an adapters command with the --json persistent
// flag (normally defined on rootCmd) so that tests can exercise JSON output mode.
func newAdaptersCommandWithJSON() *cobra.Command {
	cmd := newAdaptersCommand()
	cmd.PersistentFlags().Bool("json", false, "Output in JSON format")
	return cmd
}

// testBinaryName returns a platform-appropriate binary name. On Windows, binaries
// need an .exe extension to pass the executable extension sanity check.
func testBinaryName() string {
	if runtime.GOOS == "windows" {
		return "adapter.exe"
	}
	return "adapter"
}

// writeMinimalManifest creates a plugin.yaml with the given transport in a temp dir.
// The entrypoint command uses testBinaryName() for cross-platform compatibility.
func writeMinimalManifest(t *testing.T, dir, transport string) string {
	t.Helper()
	binName := testBinaryName()
	manifest := "apiVersion: nap.nupi.ai/v1alpha1\nkind: Plugin\ntype: adapter\nmetadata:\n  name: test-adapter\n  slug: test-sanity\nspec:\n  slot: stt\n  mode: local\n  entrypoint:\n    command: ./" + binName + "\n    transport: " + transport + "\n"
	p := filepath.Join(dir, "plugin.yaml")
	if err := os.WriteFile(p, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAdaptersInstallLocal_ProcessTransport_SanityCheckPassed(t *testing.T) {
	// DO NOT add t.Parallel() — this test uses mockRegisterGRPC which mutates
	// a package-level variable. All handler-path tests must run sequentially.
	//
	// Exercises the full handler path: manifest → register (mocked) → sanityCheck → output.
	// Validates AC2: registration followed by sanity check with passing result.
	dir := t.TempDir()
	manifestPath := writeMinimalManifest(t, dir, "process")

	// Create a valid executable binary at the path the manifest references.
	// Uses testBinaryName() for cross-platform compatibility (Windows needs .exe).
	bin := filepath.Join(dir, testBinaryName())
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cleanup, called := mockRegisterGRPC(func(_ *OutputFormatter, req *apiv1.RegisterAdapterRequest) (*apiv1.RegisterAdapterResponse, error) {
		return &apiv1.RegisterAdapterResponse{
			AdapterId: "test-sanity",
			Name:      "test-adapter",
			Type:      "stt",
			Source:    "local",
		}, nil
	})
	defer cleanup()

	// JSON mode — structured output we can parse.
	cmd := newAdaptersCommandWithJSON()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"install-local", "--manifest-file", manifestPath, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("install-local failed: %v", err)
	}

	if n := atomic.LoadInt32(called); n != 1 {
		t.Fatalf("expected mockRegisterGRPC to be called exactly once, got %d", n)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nraw: %s", err, stdout.String())
	}
	sc, ok := result["sanity_check"]
	if !ok {
		t.Fatal("expected sanity_check key in JSON output for process transport")
	}
	scMap, ok := sc.(map[string]interface{})
	if !ok {
		t.Fatalf("sanity_check should be an object, got %T", sc)
	}
	if passed, _ := scMap["passed"].(bool); !passed {
		t.Fatalf("expected sanity check to pass, got: %v", scMap)
	}

	// Also verify human-readable output includes sanity check line.
	cmd2 := newAdaptersCommandWithJSON()
	var humanOut bytes.Buffer
	cmd2.SetOut(&humanOut)
	cmd2.SetArgs([]string{"install-local", "--manifest-file", manifestPath})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("install-local (human) failed: %v", err)
	}
	if !strings.Contains(humanOut.String(), "Sanity check passed") {
		t.Fatalf("expected human output to contain sanity check result, got: %s", humanOut.String())
	}
}

func TestAdaptersInstallLocal_ProcessTransport_SanityCheckFailed_WarningOnly(t *testing.T) {
	// DO NOT add t.Parallel() — uses mockRegisterGRPC (package-level mutation).
	// Exercises AC2 warning-only contract: sanity check failure does NOT cause
	// the command to return an error.
	dir := t.TempDir()
	manifestPath := writeMinimalManifest(t, dir, "process")

	// Create a binary that will fail sanity check:
	// - Unix: binary is not executable (0644 permission)
	// - Windows: binary has no executable extension (no .exe/.bat/.cmd/.com)
	bin := filepath.Join(dir, testBinaryName())
	if runtime.GOOS == "windows" {
		// On Windows, we need a file WITHOUT a recognised extension to trigger failure.
		// Override binary name (writeMinimalManifest already wrote with .exe).
		// Instead create a file with .txt extension at the expected path but strip ext.
		bin = filepath.Join(dir, "adapter")
		// Re-write manifest with bare "adapter" command (no .exe).
		manifest := "apiVersion: nap.nupi.ai/v1alpha1\nkind: Plugin\ntype: adapter\nmetadata:\n  name: test-adapter\n  slug: test-sanity\nspec:\n  slot: stt\n  mode: local\n  entrypoint:\n    command: ./adapter\n    transport: process\n"
		if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(bin, []byte("not-executable"), 0o644); err != nil {
		t.Fatal(err)
	}

	cleanup, called := mockRegisterGRPC(func(_ *OutputFormatter, _ *apiv1.RegisterAdapterRequest) (*apiv1.RegisterAdapterResponse, error) {
		return &apiv1.RegisterAdapterResponse{
			AdapterId: "test-sanity",
			Name:      "test-adapter",
			Type:      "stt",
			Source:    "local",
		}, nil
	})
	defer cleanup()

	cmd := newAdaptersCommandWithJSON()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"install-local", "--manifest-file", manifestPath, "--json"})

	// Command must succeed even though sanity check fails (warning-only).
	if err := cmd.Execute(); err != nil {
		t.Fatalf("install-local should succeed despite failed sanity check: %v", err)
	}

	if n := atomic.LoadInt32(called); n != 1 {
		t.Fatalf("expected mockRegisterGRPC to be called exactly once, got %d", n)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nraw: %s", err, stdout.String())
	}
	sc, ok := result["sanity_check"]
	if !ok {
		t.Fatal("expected sanity_check key even when check fails")
	}
	scMap := sc.(map[string]interface{})
	if passed, _ := scMap["passed"].(bool); passed {
		t.Fatal("expected sanity check to fail for non-executable binary")
	}

	// Also verify human-readable output includes failure message and remediation (AC3).
	cmd2 := newAdaptersCommandWithJSON()
	var humanOut bytes.Buffer
	cmd2.SetOut(&humanOut)
	cmd2.SetArgs([]string{"install-local", "--manifest-file", manifestPath})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("install-local (human) failed: %v", err)
	}
	humanStr := humanOut.String()
	if !strings.Contains(humanStr, "Sanity check failed") {
		t.Fatalf("expected human output to contain failure message, got: %s", humanStr)
	}
	// Platform-specific remediation: Unix suggests chmod +x, Windows suggests
	// executable extension (.exe/.bat/.cmd/.com).
	if runtime.GOOS == "windows" {
		if !strings.Contains(humanStr, ".exe") {
			t.Fatalf("expected human output to contain Windows extension hint, got: %s", humanStr)
		}
	} else {
		if !strings.Contains(humanStr, "chmod +x") {
			t.Fatalf("expected human output to contain remediation hint, got: %s", humanStr)
		}
	}
}

func TestAdaptersInstallLocal_GRPCTransport_SanityCheckOmitted(t *testing.T) {
	// DO NOT add t.Parallel() — uses mockRegisterGRPC (package-level mutation).
	// Validates LOW feedback: remote transport (gRPC) without --copy-binary
	// must omit sanity_check from JSON output entirely.
	dir := t.TempDir()
	manifestPath := writeMinimalManifest(t, dir, "grpc")

	cleanup, called := mockRegisterGRPC(func(_ *OutputFormatter, _ *apiv1.RegisterAdapterRequest) (*apiv1.RegisterAdapterResponse, error) {
		return &apiv1.RegisterAdapterResponse{
			AdapterId: "test-remote",
			Name:      "test-adapter",
			Type:      "stt",
			Source:    "local",
		}, nil
	})
	defer cleanup()

	// JSON mode — sanity_check key must be absent.
	cmd := newAdaptersCommandWithJSON()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{
		"install-local",
		"--manifest-file", manifestPath,
		"--endpoint-address", "localhost:50051",
		"--json",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("install-local (grpc) failed: %v", err)
	}

	if n := atomic.LoadInt32(called); n != 1 {
		t.Fatalf("expected mockRegisterGRPC to be called exactly once, got %d", n)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nraw: %s", err, stdout.String())
	}
	if _, ok := result["sanity_check"]; ok {
		t.Fatal("sanity_check must be omitted for remote transport without --copy-binary")
	}
	if result["adapter"] == nil {
		t.Fatal("expected adapter key in JSON output")
	}

	// Human-readable mode — must NOT mention sanity check at all.
	cmd2 := newAdaptersCommandWithJSON()
	var humanOut bytes.Buffer
	cmd2.SetOut(&humanOut)
	cmd2.SetArgs([]string{
		"install-local",
		"--manifest-file", manifestPath,
		"--endpoint-address", "localhost:50051",
	})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("install-local (grpc, human) failed: %v", err)
	}
	if strings.Contains(humanOut.String(), "Sanity check") {
		t.Fatalf("human output must not mention sanity check for gRPC transport, got: %s", humanOut.String())
	}
}

func TestAdaptersInstallLocal_RegistrationFails_SanityCheckSkipped(t *testing.T) {
	// DO NOT add t.Parallel() — uses mockRegisterGRPC (package-level mutation).
	// H2: Validates that when registration fails, sanity check does NOT run
	// and the error is propagated correctly.
	dir := t.TempDir()
	manifestPath := writeMinimalManifest(t, dir, "process")

	// Create a valid executable — if sanity check ran, it would pass.
	bin := filepath.Join(dir, testBinaryName())
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	regErr := errors.New("registration failed: daemon unavailable")
	cleanup, called := mockRegisterGRPC(func(_ *OutputFormatter, _ *apiv1.RegisterAdapterRequest) (*apiv1.RegisterAdapterResponse, error) {
		return nil, regErr
	})
	defer cleanup()

	cmd := newAdaptersCommandWithJSON()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"install-local", "--manifest-file", manifestPath, "--json"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected install-local to fail when registration fails")
	}

	if n := atomic.LoadInt32(called); n != 1 {
		t.Fatalf("expected mockRegisterGRPC to be called exactly once, got %d", n)
	}

	// Output must NOT contain sanity check data.
	output := stdout.String()
	if strings.Contains(output, "sanity_check") {
		t.Fatalf("sanity check must not appear when registration fails, got: %s", output)
	}
	if strings.Contains(output, "Sanity check") {
		t.Fatalf("sanity check text must not appear when registration fails, got: %s", output)
	}
}

func TestAdaptersInstallLocal_GRPCTransport_CopyBinary_SanityCheckRuns(t *testing.T) {
	// DO NOT add t.Parallel() — uses mockRegisterGRPC (package-level mutation).
	// M4: Validates that gRPC transport WITH --copy-binary triggers sanity check
	// (the binaryCopied branch at adapters.go:559).
	dir := t.TempDir()

	// Use a unique slug to avoid collisions with real adapter installations.
	uniqueSlug := "test-copy-" + strconv.FormatInt(int64(os.Getpid()), 10)
	binName := testBinaryName()
	manifest := "apiVersion: nap.nupi.ai/v1alpha1\nkind: Plugin\ntype: adapter\nmetadata:\n  name: test-copy-adapter\n  slug: " + uniqueSlug + "\nspec:\n  slot: stt\n  mode: local\n  entrypoint:\n    command: ./" + binName + "\n    transport: grpc\n"
	manifestPath := filepath.Join(dir, "plugin.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a valid executable binary to be "copied".
	bin := filepath.Join(dir, binName)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cleanup, called := mockRegisterGRPC(func(_ *OutputFormatter, req *apiv1.RegisterAdapterRequest) (*apiv1.RegisterAdapterResponse, error) {
		return &apiv1.RegisterAdapterResponse{
			AdapterId: uniqueSlug,
			Name:      "test-copy-adapter",
			Type:      "stt",
			Source:    "local",
		}, nil
	})
	defer cleanup()

	// Clean up the adapter directory after the test to avoid leaking state.
	// Use UserHomeDir directly (not EnsureInstanceDirs, which could silently
	// fail and skip cleanup).
	t.Cleanup(func() {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Logf("cleanup: could not determine home dir: %v", err)
			return
		}
		pluginDir := filepath.Join(home, ".nupi", "instances", "default", "plugins", uniqueSlug)
		if err := os.RemoveAll(pluginDir); err != nil && !os.IsNotExist(err) {
			t.Logf("cleanup: failed to remove %s: %v", pluginDir, err)
		}
	})

	cmd := newAdaptersCommandWithJSON()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{
		"install-local",
		"--manifest-file", manifestPath,
		"--endpoint-address", "localhost:50051",
		"--binary", bin,
		"--copy-binary",
		"--json",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("install-local (grpc+copy-binary) failed: %v", err)
	}

	if n := atomic.LoadInt32(called); n != 1 {
		t.Fatalf("expected mockRegisterGRPC to be called exactly once, got %d", n)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nraw: %s", err, stdout.String())
	}
	sc, ok := result["sanity_check"]
	if !ok {
		t.Fatal("expected sanity_check key when --copy-binary is used with gRPC transport")
	}
	scMap, ok := sc.(map[string]interface{})
	if !ok {
		t.Fatalf("sanity_check should be an object, got %T", sc)
	}
	if passed, _ := scMap["passed"].(bool); !passed {
		t.Fatalf("expected sanity check to pass for copied executable binary, got: %v", scMap)
	}
}

func TestPrintSanityCheckResult_MixedPassFail(t *testing.T) {
	// L1: Validates human-readable output when binary_exists passes but
	// binary_executable fails (mixed result).
	t.Parallel()
	result := sanityCheckResult{
		Passed: false,
		Checks: []sanityCheckEntry{
			{Name: "binary_exists", Passed: true, Message: "binary found at /tmp/adapter"},
			{Name: "binary_executable", Passed: false, Remediation: "binary at /tmp/adapter is not executable — run: chmod +x '/tmp/adapter'"},
		},
	}

	var buf bytes.Buffer
	printSanityCheckResult(&buf, result)
	output := buf.String()

	if !strings.Contains(output, "Sanity check failed") {
		t.Fatalf("expected failure message for mixed result, got %q", output)
	}
	if !strings.Contains(output, "chmod +x") {
		t.Fatalf("expected remediation hint for failed executable check, got %q", output)
	}
	if !strings.Contains(output, "/tmp/adapter") {
		t.Fatalf("expected binary path in remediation, got %q", output)
	}
}

func TestAdaptersInstallLocal_BuildSuccess_SanityCheckRuns(t *testing.T) {
	// DO NOT add t.Parallel() — uses mockRegisterGRPC (package-level mutation).
	// CR16-L1: Validates that --build path includes sanity check in JSON output.
	// Requires Go toolchain — skips if unavailable.
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	dir := t.TempDir()

	// Create a minimal compilable Go project.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cmd", "adapter"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmd", "adapter", "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	manifestPath := writeMinimalManifest(t, dir, "process")

	cleanup, called := mockRegisterGRPC(func(_ *OutputFormatter, req *apiv1.RegisterAdapterRequest) (*apiv1.RegisterAdapterResponse, error) {
		return &apiv1.RegisterAdapterResponse{
			AdapterId: "test-build-sanity",
			Name:      "test-adapter",
			Type:      "stt",
			Source:    "local",
		}, nil
	})
	defer cleanup()

	cmd := newAdaptersCommandWithJSON()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{
		"install-local",
		"--manifest-file", manifestPath,
		"--build",
		"--adapter-dir", dir,
		"--build-timeout", "30s",
		"--json",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("install-local --build failed: %v", err)
	}

	if n := atomic.LoadInt32(called); n != 1 {
		t.Fatalf("expected mockRegisterGRPC to be called exactly once, got %d", n)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nraw: %s", err, stdout.String())
	}
	sc, ok := result["sanity_check"]
	if !ok {
		t.Fatal("expected sanity_check key in JSON output for --build path")
	}
	scMap, ok := sc.(map[string]interface{})
	if !ok {
		t.Fatalf("sanity_check should be an object, got %T", sc)
	}
	if passed, _ := scMap["passed"].(bool); !passed {
		t.Fatalf("expected sanity check to pass for built binary, got: %v", scMap)
	}
}

func TestAdaptersInstallLocal_BinaryVanishesDuringRegistration(t *testing.T) {
	// DO NOT add t.Parallel() — uses mockRegisterGRPC (package-level mutation).
	// CR17-M2: Validates that when a binary exists at registration time but is
	// deleted during the registration call (race condition), sanity check reports
	// failure but the command still succeeds (warning-only contract).
	dir := t.TempDir()
	manifestPath := writeMinimalManifest(t, dir, "process")

	bin := filepath.Join(dir, testBinaryName())
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cleanup, called := mockRegisterGRPC(func(_ *OutputFormatter, req *apiv1.RegisterAdapterRequest) (*apiv1.RegisterAdapterResponse, error) {
		// Simulate binary vanishing during registration (e.g. another process
		// deletes it, or build cleanup races with registration).
		if err := os.Remove(bin); err != nil {
			t.Fatalf("failed to remove binary during mock registration: %v", err)
		}
		return &apiv1.RegisterAdapterResponse{
			AdapterId: "test-vanish",
			Name:      "test-adapter",
			Type:      "stt",
			Source:    "local",
		}, nil
	})
	defer cleanup()

	cmd := newAdaptersCommandWithJSON()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{
		"install-local",
		"--manifest-file", manifestPath,
		"--json",
	})

	// Command must succeed — sanity failure is warning-only.
	if err := cmd.Execute(); err != nil {
		t.Fatalf("install-local should succeed even when binary vanishes, got: %v", err)
	}

	if n := atomic.LoadInt32(called); n != 1 {
		t.Fatalf("expected mockRegisterGRPC to be called exactly once, got %d", n)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nraw: %s", err, stdout.String())
	}
	sc, ok := result["sanity_check"]
	if !ok {
		t.Fatal("expected sanity_check key in JSON output (binary existed at flag-parse time)")
	}
	scMap, ok := sc.(map[string]interface{})
	if !ok {
		t.Fatalf("sanity_check should be an object, got %T", sc)
	}
	if passed, _ := scMap["passed"].(bool); passed {
		t.Fatal("expected sanity check to FAIL because binary was deleted during registration")
	}
}
