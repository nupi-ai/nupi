package integrity

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFile creates a file at dir/relPath (forward-slash) with the given content.
func writeFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	absPath := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hashOf returns the lowercase hex-encoded SHA-256 hash of content.
func hashOf(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func TestVerifyPlugin_AllMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	mainContent := "console.log('hello')"
	yamlContent := "name: test-plugin"

	writeFile(t, dir, "main.js", mainContent)
	writeFile(t, dir, "plugin.yaml", yamlContent)

	expected := []FileChecksum{
		{Path: "plugin.yaml", SHA256: hashOf(yamlContent)},
		{Path: "main.js", SHA256: hashOf(mainContent)},
	}

	result := VerifyPlugin(dir, expected)

	if !result.Verified {
		t.Fatalf("expected Verified=true, got false; mismatches: %v", result.Mismatches)
	}
	if result.NoChecksums {
		t.Fatal("expected NoChecksums=false, got true")
	}
	if len(result.Mismatches) != 0 {
		t.Fatalf("expected no mismatches, got %v", result.Mismatches)
	}
}

func TestVerifyPlugin_UppercaseSHA256(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	content := "console.log('hello')"
	writeFile(t, dir, "main.js", content)

	// Provide uppercase SHA256 — VerifyPlugin must normalize via strings.ToLower.
	expected := []FileChecksum{
		{Path: "main.js", SHA256: strings.ToUpper(hashOf(content))},
	}

	result := VerifyPlugin(dir, expected)

	if !result.Verified {
		t.Fatalf("expected Verified=true for uppercase SHA256, got false; mismatches: %v", result.Mismatches)
	}
}

func TestVerifyPlugin_ModifiedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	writeFile(t, dir, "main.js", "MODIFIED CONTENT")

	expected := []FileChecksum{
		{Path: "main.js", SHA256: hashOf("ORIGINAL CONTENT")},
	}

	result := VerifyPlugin(dir, expected)

	if result.Verified {
		t.Fatal("expected Verified=false, got true")
	}
	if len(result.Mismatches) != 1 {
		t.Fatalf("expected 1 mismatch, got %d: %v", len(result.Mismatches), result.Mismatches)
	}
	if !strings.HasPrefix(result.Mismatches[0], "modified: main.js") {
		t.Fatalf("expected mismatch to start with 'modified: main.js', got %q", result.Mismatches[0])
	}
	if !strings.Contains(result.Mismatches[0], "expected:") || !strings.Contains(result.Mismatches[0], "got:") {
		t.Fatalf("expected mismatch to contain hash excerpts, got %q", result.Mismatches[0])
	}
}

func TestVerifyPlugin_MissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	expected := []FileChecksum{
		{Path: "missing-file.js", SHA256: hashOf("does not matter")},
	}

	result := VerifyPlugin(dir, expected)

	if result.Verified {
		t.Fatal("expected Verified=false, got true")
	}
	if len(result.Mismatches) != 1 {
		t.Fatalf("expected 1 mismatch, got %d: %v", len(result.Mismatches), result.Mismatches)
	}
	if result.Mismatches[0] != "missing: missing-file.js" {
		t.Fatalf("expected 'missing: missing-file.js', got %q", result.Mismatches[0])
	}
}

func TestVerifyPlugin_NoChecksums(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Test with nil slice.
	result := VerifyPlugin(dir, nil)
	if !result.NoChecksums {
		t.Fatal("expected NoChecksums=true for nil slice, got false")
	}
	if result.Verified {
		t.Fatal("expected Verified=false for nil slice, got true")
	}

	// Test with empty slice.
	result = VerifyPlugin(dir, []FileChecksum{})
	if !result.NoChecksums {
		t.Fatal("expected NoChecksums=true for empty slice, got false")
	}
	if result.Verified {
		t.Fatal("expected Verified=false for empty slice, got true")
	}
}

func TestVerifyPlugin_ExtraFilesIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	mainContent := "console.log('hello')"
	writeFile(t, dir, "main.js", mainContent)
	writeFile(t, dir, "models/whisper.bin", "large binary model data")
	writeFile(t, dir, ".cache/temp", "cache data")
	writeFile(t, dir, "node_modules/dep/index.js", "dependency code")

	expected := []FileChecksum{
		{Path: "main.js", SHA256: hashOf(mainContent)},
	}

	result := VerifyPlugin(dir, expected)

	if !result.Verified {
		t.Fatalf("expected Verified=true (extra files should be ignored), got false; mismatches: %v", result.Mismatches)
	}
}

func TestVerifyPlugin_MultipleFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	writeFile(t, dir, "main.js", "TAMPERED")

	expected := []FileChecksum{
		{Path: "main.js", SHA256: hashOf("ORIGINAL")},
		{Path: "gone.js", SHA256: hashOf("whatever")},
	}

	result := VerifyPlugin(dir, expected)

	if result.Verified {
		t.Fatal("expected Verified=false, got true")
	}
	if len(result.Mismatches) != 2 {
		t.Fatalf("expected 2 mismatches, got %d: %v", len(result.Mismatches), result.Mismatches)
	}

	// Mismatches are sorted by path, so "gone.js" comes before "main.js".
	if !strings.HasPrefix(result.Mismatches[0], "missing: gone.js") {
		t.Fatalf("expected first mismatch to be 'missing: gone.js', got %q", result.Mismatches[0])
	}
	if !strings.HasPrefix(result.Mismatches[1], "modified: main.js") {
		t.Fatalf("expected second mismatch to be 'modified: main.js', got %q", result.Mismatches[1])
	}
}

func TestVerifyPlugin_UnreadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod not effective on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root can read any file, skip permission test")
	}
	t.Parallel()
	dir := t.TempDir()

	writeFile(t, dir, "secret.js", "secret content")
	if err := os.Chmod(filepath.Join(dir, "secret.js"), 0o000); err != nil {
		t.Fatal(err)
	}

	expected := []FileChecksum{
		{Path: "secret.js", SHA256: hashOf("secret content")},
	}

	result := VerifyPlugin(dir, expected)

	if result.Verified {
		t.Fatal("expected Verified=false for unreadable file, got true")
	}
	if len(result.Mismatches) != 1 {
		t.Fatalf("expected 1 mismatch, got %d: %v", len(result.Mismatches), result.Mismatches)
	}
	if !strings.HasPrefix(result.Mismatches[0], "error: secret.js:") {
		t.Fatalf("expected error entry for secret.js, got %q", result.Mismatches[0])
	}
}

func TestVerifyPlugin_ShortSHA256(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	writeFile(t, dir, "main.js", "content")

	// Provide a SHA256 value shorter than 8 chars — must not panic.
	expected := []FileChecksum{
		{Path: "main.js", SHA256: "abc"},
	}

	result := VerifyPlugin(dir, expected)

	if result.Verified {
		t.Fatal("expected Verified=false for short SHA256, got true")
	}
	if len(result.Mismatches) != 1 {
		t.Fatalf("expected 1 mismatch, got %d: %v", len(result.Mismatches), result.Mismatches)
	}
	if !strings.HasPrefix(result.Mismatches[0], "modified: main.js") {
		t.Fatalf("expected modified entry, got %q", result.Mismatches[0])
	}
	// Verify the short hash appears in its entirety (no truncation panic).
	if !strings.Contains(result.Mismatches[0], "expected: abc...") {
		t.Fatalf("expected short hash 'abc' in message, got %q", result.Mismatches[0])
	}
}

func TestVerifyPlugin_NestedPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	utilContent := "module.exports = {}"
	deepContent := "export default 42"

	writeFile(t, dir, "lib/util.js", utilContent)
	writeFile(t, dir, "src/deep/nested/file.js", deepContent)

	expected := []FileChecksum{
		{Path: "lib/util.js", SHA256: hashOf(utilContent)},
		{Path: "src/deep/nested/file.js", SHA256: hashOf(deepContent)},
	}

	result := VerifyPlugin(dir, expected)

	if !result.Verified {
		t.Fatalf("expected Verified=true for nested paths, got false; mismatches: %v", result.Mismatches)
	}
}

func TestVerifyPlugin_PathTraversal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create a file inside the plugin dir so we have at least one valid entry.
	writeFile(t, dir, "main.js", "ok")

	// All of these paths attempt to escape the plugin directory.
	escapePaths := []string{
		"../../../etc/passwd",
		"../../secret.txt",
		"../sibling/file.js",
		"lib/../../outside.js",
	}

	for _, esc := range escapePaths {
		t.Run(esc, func(t *testing.T) {
			t.Parallel()

			expected := []FileChecksum{
				{Path: esc, SHA256: hashOf("irrelevant")},
			}

			result := VerifyPlugin(dir, expected)

			if result.Verified {
				t.Fatalf("expected Verified=false for escape path %q, got true", esc)
			}
			if len(result.Mismatches) != 1 {
				t.Fatalf("expected 1 mismatch for %q, got %d: %v", esc, len(result.Mismatches), result.Mismatches)
			}
			if !strings.Contains(result.Mismatches[0], "path escapes plugin directory") {
				t.Fatalf("expected 'path escapes plugin directory' for %q, got %q", esc, result.Mismatches[0])
			}
		})
	}
}

func TestVerifyPlugin_DotDotPrefixedFilename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Filenames starting with ".." are valid — they must NOT be rejected
	// by the path-traversal guard.
	content := "dotdot file content"
	writeFile(t, dir, "..manifest", content)

	expected := []FileChecksum{
		{Path: "..manifest", SHA256: hashOf(content)},
	}

	result := VerifyPlugin(dir, expected)

	if !result.Verified {
		t.Fatalf("expected Verified=true for ..manifest, got false; mismatches: %v", result.Mismatches)
	}
}

func TestVerifyPlugin_SymlinkRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated privileges on Windows")
	}
	t.Parallel()
	dir := t.TempDir()

	// Create a real file outside the plugin dir and a symlink inside pointing to it.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(dir, "link.js")); err != nil {
		t.Fatal(err)
	}

	expected := []FileChecksum{
		{Path: "link.js", SHA256: hashOf("secret")},
	}

	result := VerifyPlugin(dir, expected)

	if result.Verified {
		t.Fatal("expected Verified=false for symlink, got true")
	}
	if len(result.Mismatches) != 1 {
		t.Fatalf("expected 1 mismatch, got %d: %v", len(result.Mismatches), result.Mismatches)
	}
	// EvalSymlinks resolves the symlink to the external target; the containment
	// check rejects it before the IsRegular check is reached.
	if !strings.Contains(result.Mismatches[0], "resolves outside plugin directory") {
		t.Fatalf("expected 'resolves outside plugin directory' error, got %q", result.Mismatches[0])
	}
}

func TestVerifyPlugin_IntermediateSymlinkRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated privileges on Windows")
	}
	t.Parallel()
	dir := t.TempDir()

	// Create a real file in an external directory.
	outsideDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outsideDir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	utilContent := "module.exports = {}"
	if err := os.WriteFile(filepath.Join(outsideDir, "lib", "util.js"), []byte(utilContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create an intermediate directory symlink: pluginDir/lib -> outsideDir/lib.
	// The lexical path "lib/util.js" looks local, but the physical path escapes.
	if err := os.Symlink(filepath.Join(outsideDir, "lib"), filepath.Join(dir, "lib")); err != nil {
		t.Fatal(err)
	}

	expected := []FileChecksum{
		{Path: "lib/util.js", SHA256: hashOf(utilContent)},
	}

	result := VerifyPlugin(dir, expected)

	if result.Verified {
		t.Fatal("expected Verified=false for intermediate symlink escape, got true")
	}
	if len(result.Mismatches) != 1 {
		t.Fatalf("expected 1 mismatch, got %d: %v", len(result.Mismatches), result.Mismatches)
	}
	if !strings.Contains(result.Mismatches[0], "resolves outside plugin directory") {
		t.Fatalf("expected 'resolves outside plugin directory' error, got %q", result.Mismatches[0])
	}
}

func TestVerifyPlugin_DirectoryAsExpectedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create a subdirectory that is listed as an expected file.
	writeFile(t, dir, "lib/util.js", "content")

	expected := []FileChecksum{
		{Path: "lib", SHA256: hashOf("irrelevant")},
	}

	result := VerifyPlugin(dir, expected)

	if result.Verified {
		t.Fatal("expected Verified=false for directory entry, got true")
	}
	if len(result.Mismatches) != 1 {
		t.Fatalf("expected 1 mismatch, got %d: %v", len(result.Mismatches), result.Mismatches)
	}
	if !strings.Contains(result.Mismatches[0], "not a regular file") {
		t.Fatalf("expected 'not a regular file' error, got %q", result.Mismatches[0])
	}
}

func TestVerifyPlugin_PathTraversalMixedWithValid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	mainContent := "console.log('hello')"
	writeFile(t, dir, "main.js", mainContent)

	// Mix a valid file with an escape attempt.
	expected := []FileChecksum{
		{Path: "main.js", SHA256: hashOf(mainContent)},
		{Path: "../escape.js", SHA256: hashOf("x")},
	}

	result := VerifyPlugin(dir, expected)

	if result.Verified {
		t.Fatal("expected Verified=false when escape path is present, got true")
	}
	if len(result.Mismatches) != 1 {
		t.Fatalf("expected 1 mismatch (only the escape), got %d: %v", len(result.Mismatches), result.Mismatches)
	}
	if !strings.Contains(result.Mismatches[0], "path escapes plugin directory") {
		t.Fatalf("expected escape error, got %q", result.Mismatches[0])
	}
}
