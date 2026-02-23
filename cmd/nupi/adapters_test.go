package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

func TestLoadAdapterManifestFile(t *testing.T) {
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
	cases := map[string]string{
		"Local STT":         "local-stt",
		"UPPER_case--Slug":  "upper_case--slug",
		"   spaced slug   ": "spaced-slug",
		"../dangerous/path": "dangerous-path",
		"@@@":               "adapter",
		"slug-with-extremely-long-name-that-should-be-trimmed-because-it-exceeds-sixty-four-characters": "slug-with-extremely-long-name-that-should-be-trimmed-because-it-",
	}

	for input, expected := range cases {
		if actual := sanitizeAdapterSlug(input); actual != expected {
			t.Fatalf("sanitizeAdapterSlug(%q) = %q, expected %q", input, actual, expected)
		}
	}
}

func TestCopyFile(t *testing.T) {
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

	// Binding failure: cleanup is NOT called → directory preserved.
	// This subtest documents the intentional design decision (Task 3.5).
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

		// Binding failure: production code intentionally does NOT call
		// cleanupAdapterDir — the adapter is validly registered.

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
