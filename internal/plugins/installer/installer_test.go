package installer

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/marketplace"
)

// ---------------------------------------------------------------------------
// verifySHA256
// ---------------------------------------------------------------------------

func TestVerifySHA256_CorrectHash(t *testing.T) {
	content := []byte("hello, nupi plugin installer")
	tmp := writeTempFile(t, content)

	h := sha256.Sum256(content)
	expected := hex.EncodeToString(h[:])

	if err := verifySHA256(tmp, expected); err != nil {
		t.Fatalf("expected nil error for correct hash, got: %v", err)
	}
}

func TestVerifySHA256_CorrectHash_UpperCase(t *testing.T) {
	content := []byte("uppercase test")
	tmp := writeTempFile(t, content)

	h := sha256.Sum256(content)
	expected := strings.ToUpper(hex.EncodeToString(h[:]))

	if err := verifySHA256(tmp, expected); err != nil {
		t.Fatalf("expected nil error for upper-case hash, got: %v", err)
	}
}

func TestVerifySHA256_IncorrectHash(t *testing.T) {
	content := []byte("some data")
	tmp := writeTempFile(t, content)

	err := verifySHA256(tmp, "0000000000000000000000000000000000000000000000000000000000000000")
	if err == nil {
		t.Fatal("expected error for incorrect hash, got nil")
	}
	if !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestVerifySHA256_EmptyHash_ReturnsError(t *testing.T) {
	content := []byte("anything")
	tmp := writeTempFile(t, content)

	if err := verifySHA256(tmp, ""); err == nil {
		t.Fatal("expected error for empty hash, got nil")
	}
	if err := verifySHA256(tmp, "   "); err == nil {
		t.Fatal("expected error for whitespace-only hash, got nil")
	}
}

func TestVerifySHA256_FileNotFound(t *testing.T) {
	err := verifySHA256("/tmp/nonexistent-file-installer-test-9999", "abc123")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// ---------------------------------------------------------------------------
// extractArchive — zip
// ---------------------------------------------------------------------------

func TestExtractArchive_Zip(t *testing.T) {
	// Create a zip archive in memory with two files.
	archivePath := createTestZip(t, map[string]string{
		"hello.txt":        "Hello, world!",
		"subdir/nested.go": "package main\n",
	})

	destDir := t.TempDir()
	if err := extractArchive(context.Background(), archivePath, destDir); err != nil {
		t.Fatalf("extractArchive zip: %v", err)
	}

	assertFileContent(t, filepath.Join(destDir, "hello.txt"), "Hello, world!")
	assertFileContent(t, filepath.Join(destDir, "subdir", "nested.go"), "package main\n")
}

// ---------------------------------------------------------------------------
// extractArchive — tar.gz
// ---------------------------------------------------------------------------

func TestExtractArchive_TarGz(t *testing.T) {
	archivePath := createTestTarGz(t, map[string]string{
		"README":           "readme contents",
		"lib/plugin.so":    "fake binary",
		"lib/plugin.yaml":  "slug: test",
	})

	destDir := t.TempDir()
	if err := extractArchive(context.Background(), archivePath, destDir); err != nil {
		t.Fatalf("extractArchive tar.gz: %v", err)
	}

	assertFileContent(t, filepath.Join(destDir, "README"), "readme contents")
	assertFileContent(t, filepath.Join(destDir, "lib", "plugin.so"), "fake binary")
	assertFileContent(t, filepath.Join(destDir, "lib", "plugin.yaml"), "slug: test")
}

// ---------------------------------------------------------------------------
// extractArchive — auto-detect (no extension)
// ---------------------------------------------------------------------------

func TestExtractArchive_AutoDetect_Zip(t *testing.T) {
	// Write a zip but give it a non-zip extension so magic-byte detection kicks in.
	archivePath := createTestZip(t, map[string]string{
		"data.txt": "detected via magic bytes",
	})
	noExt := filepath.Join(t.TempDir(), "archive.download")
	copyFile(t, archivePath, noExt)

	destDir := t.TempDir()
	if err := extractArchive(context.Background(), noExt, destDir); err != nil {
		t.Fatalf("extractArchive auto-detect zip: %v", err)
	}
	assertFileContent(t, filepath.Join(destDir, "data.txt"), "detected via magic bytes")
}

func TestExtractArchive_AutoDetect_TarGz(t *testing.T) {
	archivePath := createTestTarGz(t, map[string]string{
		"info.txt": "gzip magic",
	})
	noExt := filepath.Join(t.TempDir(), "archive.download")
	copyFile(t, archivePath, noExt)

	destDir := t.TempDir()
	if err := extractArchive(context.Background(), noExt, destDir); err != nil {
		t.Fatalf("extractArchive auto-detect tar.gz: %v", err)
	}
	assertFileContent(t, filepath.Join(destDir, "info.txt"), "gzip magic")
}

// ---------------------------------------------------------------------------
// detectArchiveFormat
// ---------------------------------------------------------------------------

func TestDetectArchiveFormat_Zip(t *testing.T) {
	tmp := writeTempFile(t, []byte{0x50, 0x4B, 0x03, 0x04})
	if got := detectArchiveFormat(tmp); got != "zip" {
		t.Fatalf("expected zip, got %q", got)
	}
}

func TestDetectArchiveFormat_Gzip(t *testing.T) {
	tmp := writeTempFile(t, []byte{0x1F, 0x8B, 0x08, 0x00})
	if got := detectArchiveFormat(tmp); got != "gzip" {
		t.Fatalf("expected gzip, got %q", got)
	}
}

func TestDetectArchiveFormat_Unknown(t *testing.T) {
	tmp := writeTempFile(t, []byte{0x00, 0x00, 0x00, 0x00})
	if got := detectArchiveFormat(tmp); got != "" {
		t.Fatalf("expected empty string for unknown format, got %q", got)
	}
}

func TestDetectArchiveFormat_MissingFile(t *testing.T) {
	if got := detectArchiveFormat("/tmp/no-such-file-installer-test-8888"); got != "" {
		t.Fatalf("expected empty string for missing file, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// isValidSlug / isValidNamespace
// ---------------------------------------------------------------------------

func TestIsValidSlug(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"hello", true},
		{"my-plugin", true},
		{"my.plugin", true},
		{"my_plugin", true},
		{"Plugin123", true},
		{"a", true},
		{"9start", true},
		{"", false},
		{"-start", false},
		{".start", false},
		{"_start", false},
		{"has space", false},
		{"has/slash", false},
		{"café", false},
		{strings.Repeat("a", 128), true},
		{strings.Repeat("a", 129), false},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			if got := isValidSlug(tc.input); got != tc.want {
				t.Errorf("isValidSlug(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestIsValidNamespace(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"official", true},
		{"others", true},
		{"my-org", true},
		{"", false},
		{"bad!name", false},
		{"@scope", false},
		{strings.Repeat("x", 128), true},
		{strings.Repeat("x", 129), false},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			if got := isValidNamespace(tc.input); got != tc.want {
				t.Errorf("isValidNamespace(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// validateHTTPURL tests moved to internal/validate/validate_test.go

// ---------------------------------------------------------------------------
// extractZip — zip slip prevention
// ---------------------------------------------------------------------------

func TestExtractZip_ZipSlipPrevention(t *testing.T) {
	// Craft a zip with a path traversal entry.
	archivePath := createZipWithRawEntries(t, map[string]string{
		"../../../etc/evil.txt": "malicious content",
	})

	destDir := t.TempDir()
	err := extractZip(context.Background(), archivePath, destDir)
	if err == nil {
		t.Fatal("expected error for zip slip path, got nil")
	}
	if !strings.Contains(err.Error(), "invalid path") {
		t.Fatalf("expected 'invalid path' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// extractZip — symlink rejection
// ---------------------------------------------------------------------------

func TestExtractZip_SymlinkRejection(t *testing.T) {
	// Create a zip that contains a symlink entry.
	// Go's zip writer does not have a direct "add symlink" API, so we set the
	// mode bits to include ModeSymlink manually.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	hdr := &zip.FileHeader{
		Name: "evil-link",
	}
	hdr.SetMode(os.ModeSymlink | 0o777)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatalf("create zip header: %v", err)
	}
	// The body of a symlink entry in zip is the target path.
	w.Write([]byte("/etc/passwd"))

	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}

	archivePath := filepath.Join(t.TempDir(), "symlink.zip")
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write zip: %v", err)
	}

	destDir := t.TempDir()
	err = extractZip(context.Background(), archivePath, destDir)
	if err == nil {
		t.Fatal("expected error for symlink in zip, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// extractZip — file count limit
// ---------------------------------------------------------------------------

func TestExtractZip_FileCountLimit(t *testing.T) {
	// Create a zip with maxFileCount+1 entries.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	for i := 0; i <= maxFileCount; i++ {
		name := filepath.Join("files", strings.Repeat("0", 5-len(itoa(i)))+itoa(i)+".txt")
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create entry %d: %v", i, err)
		}
		w.Write([]byte("x"))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	archivePath := filepath.Join(t.TempDir(), "toomany.zip")
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write zip: %v", err)
	}

	destDir := t.TempDir()
	err := extractZip(context.Background(), archivePath, destDir)
	if err == nil {
		t.Fatal("expected error for too many files, got nil")
	}
	if !strings.Contains(err.Error(), "too many files") {
		t.Fatalf("expected 'too many files' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// extractTarGz — symlink / hardlink rejection
// ---------------------------------------------------------------------------

func TestExtractTarGz_SymlinkRejection(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	tw.WriteHeader(&tar.Header{
		Name:     "evil-link",
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
	})
	tw.Close()
	gw.Close()

	archivePath := filepath.Join(t.TempDir(), "symlink.tar.gz")
	os.WriteFile(archivePath, buf.Bytes(), 0o644)

	destDir := t.TempDir()
	err := extractTarGz(context.Background(), archivePath, destDir)
	if err == nil {
		t.Fatal("expected error for symlink in tar.gz, got nil")
	}
	if !strings.Contains(err.Error(), "link entry") {
		t.Fatalf("expected link error, got: %v", err)
	}
}

func TestExtractTarGz_PathTraversalPrevention(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	content := []byte("evil")
	tw.WriteHeader(&tar.Header{
		Name:     "../../../etc/evil.txt",
		Size:     int64(len(content)),
		Typeflag: tar.TypeReg,
		Mode:     0o644,
	})
	tw.Write(content)
	tw.Close()
	gw.Close()

	archivePath := filepath.Join(t.TempDir(), "traversal.tar.gz")
	os.WriteFile(archivePath, buf.Bytes(), 0o644)

	destDir := t.TempDir()
	err := extractTarGz(context.Background(), archivePath, destDir)
	if err == nil {
		t.Fatal("expected error for path traversal in tar.gz, got nil")
	}
	if !strings.Contains(err.Error(), "invalid path") {
		t.Fatalf("expected 'invalid path' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeTempFile(t *testing.T, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	name := f.Name()
	f.Close()
	return name
}

func createTestZip(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}

	path := filepath.Join(t.TempDir(), "test.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write zip file: %v", err)
	}
	return path
}

// createZipWithRawEntries creates a zip where file names are used verbatim
// (including path traversal sequences).
func createZipWithRawEntries(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	for name, content := range files {
		hdr := &zip.FileHeader{
			Name:   name,
			Method: zip.Deflate,
		}
		hdr.SetMode(0o644)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("zip create header %s: %v", name, err)
		}
		w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}

	path := filepath.Join(t.TempDir(), "traversal.zip")
	os.WriteFile(path, buf.Bytes(), 0o644)
	return path
}

func createTestTarGz(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for name, content := range files {
		hdr := &tar.Header{
			Name:     name,
			Size:     int64(len(content)),
			Mode:     0o644,
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	path := filepath.Join(t.TempDir(), "test.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write tar.gz file: %v", err)
	}
	return path
}

func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != expected {
		t.Errorf("file %s: got %q, want %q", path, string(data), expected)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

// itoa is a simple int-to-string for test use (avoids importing strconv).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	if neg {
		s = "-" + s
	}
	return s
}

// ===========================================================================
// Integration tests for InstallFromPath, Uninstall, InstallFromMarketplace,
// and InstallFromURL.
// ===========================================================================

// ---------------------------------------------------------------------------
// Integration test helpers
// ---------------------------------------------------------------------------

// openTestStore creates a fresh SQLite-backed config store in a temp directory.
// t.Setenv("HOME") prevents tests using this helper from calling t.Parallel().
func openTestStore(t *testing.T) (*configstore.Store, context.Context) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	dbPath := filepath.Join(tmpDir, "config.db")
	store, err := configstore.Open(configstore.Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, context.Background()
}

// newTestInstaller creates a fresh installer backed by a real config store.
// pluginDir is the directory where plugins will be installed.
func newTestInstaller(t *testing.T) (*Installer, *configstore.Store, context.Context, string) {
	t.Helper()
	store, ctx := openTestStore(t)
	pluginDir := filepath.Join(t.TempDir(), "plugins")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("create plugin dir: %v", err)
	}
	inst := NewInstaller(store, pluginDir)
	return inst, store, ctx, pluginDir
}

// toolHandlerManifest returns a valid plugin.yaml for a tool-handler plugin.
func toolHandlerManifest(slug, version string) string {
	return "apiVersion: nupi.ai/v1\nkind: Plugin\ntype: tool-handler\nmetadata:\n  name: Test Plugin\n  slug: " + slug + "\n  version: " + version + "\nspec:\n  main: main.js\n"
}

// adapterManifest returns a valid plugin.yaml for an adapter plugin.
func adapterManifest(slug, version, slot string) string {
	return "apiVersion: nupi.ai/v1\nkind: Plugin\ntype: adapter\nmetadata:\n  name: Test Adapter\n  slug: " + slug + "\n  version: " + version + "\nspec:\n  slot: " + slot + "\n  entrypoint:\n    runtime: binary\n    command: ./run.sh\n    transport: process\n"
}

// pipelineCleanerManifest returns a valid plugin.yaml for a pipeline-cleaner plugin.
func pipelineCleanerManifest(slug, version string) string {
	return "apiVersion: nupi.ai/v1\nkind: Plugin\ntype: pipeline-cleaner\nmetadata:\n  name: Test Cleaner\n  slug: " + slug + "\n  version: " + version + "\nspec:\n  main: main.js\n"
}

// createPluginDir creates a directory containing a plugin.yaml and optional extra files.
func createPluginDir(t *testing.T, manifest string, extraFiles map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write plugin.yaml: %v", err)
	}
	for name, content := range extraFiles {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// createPluginZip creates a zip archive containing a plugin.yaml and optional extra files.
func createPluginZip(t *testing.T, manifest string, extraFiles map[string]string) string {
	t.Helper()
	files := map[string]string{
		"plugin.yaml": manifest,
	}
	for name, content := range extraFiles {
		files[name] = content
	}
	return createTestZip(t, files)
}

// createPluginTarGz creates a tar.gz archive containing a plugin.yaml and optional extra files.
func createPluginTarGz(t *testing.T, manifest string, extraFiles map[string]string) string {
	t.Helper()
	files := map[string]string{
		"plugin.yaml": manifest,
	}
	for name, content := range extraFiles {
		files[name] = content
	}
	return createTestTarGz(t, files)
}

// createPluginZipInSubdir creates a zip archive where plugin.yaml lives inside
// a single subdirectory (e.g., my-plugin/plugin.yaml).
func createPluginZipInSubdir(t *testing.T, subdir, manifest string, extraFiles map[string]string) string {
	t.Helper()
	files := map[string]string{
		filepath.Join(subdir, "plugin.yaml"): manifest,
	}
	for name, content := range extraFiles {
		files[filepath.Join(subdir, name)] = content
	}
	return createTestZip(t, files)
}

// ---------------------------------------------------------------------------
// InstallFromPath — directory source
// ---------------------------------------------------------------------------

func TestInstallFromPath_Directory_ToolHandler(t *testing.T) {
	inst, store, ctx, pluginDir := newTestInstaller(t)

	manifest := toolHandlerManifest("my-tool", "1.0.0")
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js": "console.log('hello');",
	})

	result, err := inst.InstallFromPath(ctx, dir)
	if err != nil {
		t.Fatalf("InstallFromPath: %v", err)
	}

	if result.Namespace != "others" {
		t.Errorf("Namespace = %q, want %q", result.Namespace, "others")
	}
	if result.Slug != "my-tool" {
		t.Errorf("Slug = %q, want %q", result.Slug, "my-tool")
	}
	if result.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", result.Version, "1.0.0")
	}
	if result.Category != "tool-handler" {
		t.Errorf("Category = %q, want %q", result.Category, "tool-handler")
	}

	expectedDir := filepath.Join(pluginDir, "others", "my-tool")
	if result.Dir != expectedDir {
		t.Errorf("Dir = %q, want %q", result.Dir, expectedDir)
	}

	// Verify files end up in the right place
	assertFileContent(t, filepath.Join(expectedDir, "plugin.yaml"), manifest)
	assertFileContent(t, filepath.Join(expectedDir, "main.js"), "console.log('hello');")

	// Verify the plugin is recorded in the database
	p, err := store.GetInstalledPlugin(ctx, "others", "my-tool")
	if err != nil {
		t.Fatalf("GetInstalledPlugin: %v", err)
	}
	if p.Slug != "my-tool" {
		t.Errorf("DB slug = %q, want %q", p.Slug, "my-tool")
	}
	if p.Namespace != "others" {
		t.Errorf("DB namespace = %q, want %q", p.Namespace, "others")
	}
	if p.Enabled {
		t.Error("expected plugin to be disabled by default")
	}
}

func TestInstallFromPath_Directory_Adapter(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	manifest := adapterManifest("my-adapter", "2.0.0", "stt")
	dir := createPluginDir(t, manifest, map[string]string{
		"run.sh": "#!/bin/bash\necho hello",
	})

	result, err := inst.InstallFromPath(ctx, dir)
	if err != nil {
		t.Fatalf("InstallFromPath adapter: %v", err)
	}

	if result.Category != "stt" {
		t.Errorf("Category = %q, want %q", result.Category, "stt")
	}
	if result.Slug != "my-adapter" {
		t.Errorf("Slug = %q, want %q", result.Slug, "my-adapter")
	}
	if result.Version != "2.0.0" {
		t.Errorf("Version = %q, want %q", result.Version, "2.0.0")
	}
}

func TestInstallFromPath_Directory_PipelineCleaner(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	manifest := pipelineCleanerManifest("my-cleaner", "0.1.0")
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js": "module.exports = {};",
	})

	result, err := inst.InstallFromPath(ctx, dir)
	if err != nil {
		t.Fatalf("InstallFromPath pipeline-cleaner: %v", err)
	}

	if result.Category != "cleaner" {
		t.Errorf("Category = %q, want %q", result.Category, "cleaner")
	}
	if result.Slug != "my-cleaner" {
		t.Errorf("Slug = %q, want %q", result.Slug, "my-cleaner")
	}
}

// ---------------------------------------------------------------------------
// InstallFromPath — zip archive source
// ---------------------------------------------------------------------------

func TestInstallFromPath_ZipArchive(t *testing.T) {
	inst, store, ctx, pluginDir := newTestInstaller(t)

	manifest := toolHandlerManifest("zip-plugin", "1.2.3")
	archivePath := createPluginZip(t, manifest, map[string]string{
		"main.js": "// zip plugin",
		"lib/util.js": "// util",
	})

	result, err := inst.InstallFromPath(ctx, archivePath)
	if err != nil {
		t.Fatalf("InstallFromPath zip: %v", err)
	}

	if result.Slug != "zip-plugin" {
		t.Errorf("Slug = %q, want %q", result.Slug, "zip-plugin")
	}
	if result.Version != "1.2.3" {
		t.Errorf("Version = %q, want %q", result.Version, "1.2.3")
	}

	expectedDir := filepath.Join(pluginDir, "others", "zip-plugin")
	assertFileContent(t, filepath.Join(expectedDir, "main.js"), "// zip plugin")
	assertFileContent(t, filepath.Join(expectedDir, "lib", "util.js"), "// util")

	// Verify in database
	_, err = store.GetInstalledPlugin(ctx, "others", "zip-plugin")
	if err != nil {
		t.Fatalf("GetInstalledPlugin: %v", err)
	}
}

// ---------------------------------------------------------------------------
// InstallFromPath — tar.gz archive source
// ---------------------------------------------------------------------------

func TestInstallFromPath_TarGzArchive(t *testing.T) {
	inst, store, ctx, pluginDir := newTestInstaller(t)

	manifest := toolHandlerManifest("tgz-plugin", "3.0.0")
	archivePath := createPluginTarGz(t, manifest, map[string]string{
		"main.js": "// tar.gz plugin",
	})

	result, err := inst.InstallFromPath(ctx, archivePath)
	if err != nil {
		t.Fatalf("InstallFromPath tar.gz: %v", err)
	}

	if result.Slug != "tgz-plugin" {
		t.Errorf("Slug = %q, want %q", result.Slug, "tgz-plugin")
	}
	if result.Version != "3.0.0" {
		t.Errorf("Version = %q, want %q", result.Version, "3.0.0")
	}

	expectedDir := filepath.Join(pluginDir, "others", "tgz-plugin")
	assertFileContent(t, filepath.Join(expectedDir, "main.js"), "// tar.gz plugin")

	_, err = store.GetInstalledPlugin(ctx, "others", "tgz-plugin")
	if err != nil {
		t.Fatalf("GetInstalledPlugin: %v", err)
	}
}

// ---------------------------------------------------------------------------
// InstallFromPath — archive with plugin.yaml in a subdirectory
// ---------------------------------------------------------------------------

func TestInstallFromPath_ArchiveWithSubdirectory(t *testing.T) {
	inst, _, ctx, pluginDir := newTestInstaller(t)

	manifest := toolHandlerManifest("subdir-plugin", "0.5.0")
	archivePath := createPluginZipInSubdir(t, "subdir-plugin", manifest, map[string]string{
		"main.js": "// in subdir",
	})

	result, err := inst.InstallFromPath(ctx, archivePath)
	if err != nil {
		t.Fatalf("InstallFromPath subdir: %v", err)
	}

	if result.Slug != "subdir-plugin" {
		t.Errorf("Slug = %q, want %q", result.Slug, "subdir-plugin")
	}

	expectedDir := filepath.Join(pluginDir, "others", "subdir-plugin")
	assertFileContent(t, filepath.Join(expectedDir, "plugin.yaml"), manifest)
	assertFileContent(t, filepath.Join(expectedDir, "main.js"), "// in subdir")
}

// ---------------------------------------------------------------------------
// InstallFromPath — already installed plugin fails
// ---------------------------------------------------------------------------

func TestInstallFromPath_AlreadyInstalled_Fails(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	manifest := toolHandlerManifest("dup-plugin", "1.0.0")
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js": "// first install",
	})

	// First install
	_, err := inst.InstallFromPath(ctx, dir)
	if err != nil {
		t.Fatalf("first InstallFromPath: %v", err)
	}

	// Second install of same slug should fail
	dir2 := createPluginDir(t, manifest, map[string]string{
		"main.js": "// second install",
	})
	_, err = inst.InstallFromPath(ctx, dir2)
	if err == nil {
		t.Fatal("expected error for already installed plugin")
	}
	if !strings.Contains(err.Error(), "already installed") {
		t.Errorf("error = %q, want it to mention 'already installed'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromPath — invalid slug in manifest fails
// ---------------------------------------------------------------------------

func TestInstallFromPath_InvalidSlug_Fails(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	// Slug starting with hyphen is invalid
	manifest := toolHandlerManifest("-bad-slug", "1.0.0")
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js": "// bad slug",
	})

	_, err := inst.InstallFromPath(ctx, dir)
	if err == nil {
		t.Fatal("expected error for invalid slug")
	}
	if !strings.Contains(err.Error(), "invalid slug") {
		t.Errorf("error = %q, want it to mention 'invalid slug'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromPath — missing slug in manifest fails
// ---------------------------------------------------------------------------

func TestInstallFromPath_MissingSlug_Fails(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	manifest := "apiVersion: nupi.ai/v1\nkind: Plugin\ntype: tool-handler\nmetadata:\n  name: No Slug\n  version: 1.0.0\nspec:\n  main: main.js\n"
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js": "// no slug",
	})

	_, err := inst.InstallFromPath(ctx, dir)
	if err == nil {
		t.Fatal("expected error for missing slug")
	}
	if !strings.Contains(err.Error(), "missing required field") {
		t.Errorf("error = %q, want it to mention 'missing required field'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromPath — missing plugin.yaml fails
// ---------------------------------------------------------------------------

func TestInstallFromPath_MissingManifest_Fails(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	// Directory with no plugin.yaml
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.js"), []byte("// no manifest"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := inst.InstallFromPath(ctx, dir)
	if err == nil {
		t.Fatal("expected error for missing plugin.yaml")
	}
	if !strings.Contains(err.Error(), "plugin.yaml") {
		t.Errorf("error = %q, want it to mention 'plugin.yaml'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromPath — missing plugin.yaml in archive fails
// ---------------------------------------------------------------------------

func TestInstallFromPath_ArchiveMissingManifest_Fails(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	archivePath := createTestZip(t, map[string]string{
		"main.js": "// no manifest",
		"readme.txt": "nothing here",
	})

	_, err := inst.InstallFromPath(ctx, archivePath)
	if err == nil {
		t.Fatal("expected error for archive without plugin.yaml")
	}
	if !strings.Contains(err.Error(), "plugin.yaml") {
		t.Errorf("error = %q, want it to mention 'plugin.yaml'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromPath — nonexistent path fails
// ---------------------------------------------------------------------------

func TestInstallFromPath_NonexistentPath_Fails(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	_, err := inst.InstallFromPath(ctx, "/tmp/nonexistent-path-installer-test-99999")
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
	if !strings.Contains(err.Error(), "stat path") {
		t.Errorf("error = %q, want it to mention 'stat path'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromPath — invalid plugin type (missing category) fails
// ---------------------------------------------------------------------------

func TestInstallFromPath_MissingCategory_Fails(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	// Adapter without slot fails manifest validation during LoadFromDir.
	// Since the manifest is invalid, installFromDir falls through to the
	// "no valid plugin.yaml found" error.
	manifest := "apiVersion: nupi.ai/v1\nkind: Plugin\ntype: adapter\nmetadata:\n  name: No Slot\n  slug: no-slot\n  version: 1.0.0\nspec:\n  entrypoint:\n    runtime: binary\n    command: ./run.sh\n    transport: process\n"
	dir := createPluginDir(t, manifest, nil)

	_, err := inst.InstallFromPath(ctx, dir)
	if err == nil {
		t.Fatal("expected error for adapter with missing slot")
	}
	// The manifest fails validation, so installFromDir reports no valid
	// plugin.yaml. Either mention of "slot" or "plugin.yaml" is acceptable.
	if !strings.Contains(err.Error(), "slot") && !strings.Contains(err.Error(), "plugin.yaml") {
		t.Errorf("error = %q, want it to mention 'slot' or 'plugin.yaml'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromPath — multiple installs of different plugins succeed
// ---------------------------------------------------------------------------

func TestInstallFromPath_MultiplePlugins(t *testing.T) {
	inst, store, ctx, _ := newTestInstaller(t)

	// Install first plugin
	dir1 := createPluginDir(t, toolHandlerManifest("plugin-a", "1.0.0"), map[string]string{
		"main.js": "// plugin a",
	})
	result1, err := inst.InstallFromPath(ctx, dir1)
	if err != nil {
		t.Fatalf("install plugin-a: %v", err)
	}
	if result1.Slug != "plugin-a" {
		t.Errorf("first slug = %q, want %q", result1.Slug, "plugin-a")
	}

	// Install second plugin
	dir2 := createPluginDir(t, toolHandlerManifest("plugin-b", "2.0.0"), map[string]string{
		"main.js": "// plugin b",
	})
	result2, err := inst.InstallFromPath(ctx, dir2)
	if err != nil {
		t.Fatalf("install plugin-b: %v", err)
	}
	if result2.Slug != "plugin-b" {
		t.Errorf("second slug = %q, want %q", result2.Slug, "plugin-b")
	}

	// Both should be in database
	plugins, err := store.ListInstalledPlugins(ctx)
	if err != nil {
		t.Fatalf("list installed plugins: %v", err)
	}
	if len(plugins) != 2 {
		t.Fatalf("expected 2 installed plugins, got %d", len(plugins))
	}
}

// ---------------------------------------------------------------------------
// Uninstall — previously installed plugin
// ---------------------------------------------------------------------------

func TestUninstall_Success(t *testing.T) {
	inst, store, ctx, pluginDir := newTestInstaller(t)

	// Install a plugin first
	manifest := toolHandlerManifest("uninstall-me", "1.0.0")
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js": "// will be uninstalled",
	})

	result, err := inst.InstallFromPath(ctx, dir)
	if err != nil {
		t.Fatalf("InstallFromPath: %v", err)
	}

	// Verify it exists on disk
	pluginPath := filepath.Join(pluginDir, "others", "uninstall-me")
	if _, err := os.Stat(pluginPath); err != nil {
		t.Fatalf("plugin directory should exist: %v", err)
	}

	// Verify it exists in database
	_, err = store.GetInstalledPlugin(ctx, "others", "uninstall-me")
	if err != nil {
		t.Fatalf("plugin should exist in database: %v", err)
	}

	// Uninstall
	if err := inst.Uninstall(ctx, result.Namespace, result.Slug); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	// Verify directory is removed
	if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
		t.Errorf("plugin directory should be removed, got err: %v", err)
	}

	// Verify removed from database
	_, err = store.GetInstalledPlugin(ctx, "others", "uninstall-me")
	if err == nil {
		t.Fatal("expected not-found error after uninstall")
	}
	if !configstore.IsNotFound(err) {
		t.Errorf("expected NotFoundError, got %T: %v", err, err)
	}
}

// ---------------------------------------------------------------------------
// Uninstall — nonexistent plugin fails
// ---------------------------------------------------------------------------

func TestUninstall_NonexistentPlugin_Fails(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	err := inst.Uninstall(ctx, "others", "nonexistent-plugin")
	if err == nil {
		t.Fatal("expected error for uninstalling nonexistent plugin")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to mention 'not found'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Uninstall — enabled plugin is disabled before removal
// ---------------------------------------------------------------------------

func TestUninstall_EnabledPlugin(t *testing.T) {
	inst, store, ctx, _ := newTestInstaller(t)

	// Install a plugin
	dir := createPluginDir(t, toolHandlerManifest("enabled-plugin", "1.0.0"), map[string]string{
		"main.js": "// enabled",
	})
	_, err := inst.InstallFromPath(ctx, dir)
	if err != nil {
		t.Fatalf("InstallFromPath: %v", err)
	}

	// Enable it
	if err := store.SetPluginEnabled(ctx, "others", "enabled-plugin", true); err != nil {
		t.Fatalf("enable plugin: %v", err)
	}

	// Uninstall should succeed even though plugin is enabled
	if err := inst.Uninstall(ctx, "others", "enabled-plugin"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	// Verify removed from database
	_, err = store.GetInstalledPlugin(ctx, "others", "enabled-plugin")
	if err == nil {
		t.Fatal("expected not-found error after uninstall")
	}
}

// ---------------------------------------------------------------------------
// Uninstall — empty namespace directory is cleaned up
// ---------------------------------------------------------------------------

func TestUninstall_CleansEmptyNamespaceDir(t *testing.T) {
	inst, _, ctx, pluginDir := newTestInstaller(t)

	// Install a single plugin (creates the "others" namespace dir)
	dir := createPluginDir(t, toolHandlerManifest("only-plugin", "1.0.0"), map[string]string{
		"main.js": "// only",
	})
	_, err := inst.InstallFromPath(ctx, dir)
	if err != nil {
		t.Fatalf("InstallFromPath: %v", err)
	}

	nsDir := filepath.Join(pluginDir, "others")
	if _, err := os.Stat(nsDir); err != nil {
		t.Fatalf("namespace dir should exist: %v", err)
	}

	// Uninstall the only plugin
	if err := inst.Uninstall(ctx, "others", "only-plugin"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	// Namespace directory should be cleaned up
	if _, err := os.Stat(nsDir); !os.IsNotExist(err) {
		t.Errorf("empty namespace directory should be removed, got err: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Uninstall — non-empty namespace directory is preserved
// ---------------------------------------------------------------------------

func TestUninstall_PreservesNonEmptyNamespaceDir(t *testing.T) {
	inst, _, ctx, pluginDir := newTestInstaller(t)

	// Install two plugins
	dir1 := createPluginDir(t, toolHandlerManifest("keep-me", "1.0.0"), map[string]string{
		"main.js": "// keep",
	})
	_, err := inst.InstallFromPath(ctx, dir1)
	if err != nil {
		t.Fatalf("install keep-me: %v", err)
	}

	dir2 := createPluginDir(t, toolHandlerManifest("remove-me", "1.0.0"), map[string]string{
		"main.js": "// remove",
	})
	_, err = inst.InstallFromPath(ctx, dir2)
	if err != nil {
		t.Fatalf("install remove-me: %v", err)
	}

	// Uninstall only one
	if err := inst.Uninstall(ctx, "others", "remove-me"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	// Namespace directory should still exist (keep-me is still installed)
	nsDir := filepath.Join(pluginDir, "others")
	if _, err := os.Stat(nsDir); err != nil {
		t.Errorf("namespace dir should still exist: %v", err)
	}

	// The remaining plugin should still be on disk
	keepDir := filepath.Join(pluginDir, "others", "keep-me")
	if _, err := os.Stat(keepDir); err != nil {
		t.Errorf("keep-me should still exist: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Uninstall — install then uninstall then reinstall succeeds
// ---------------------------------------------------------------------------

func TestUninstall_ThenReinstall(t *testing.T) {
	inst, store, ctx, _ := newTestInstaller(t)

	manifest := toolHandlerManifest("reinstall-me", "1.0.0")
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js": "// v1",
	})

	// Install
	_, err := inst.InstallFromPath(ctx, dir)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}

	// Uninstall
	if err := inst.Uninstall(ctx, "others", "reinstall-me"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	// Reinstall with updated content
	dir2 := createPluginDir(t, toolHandlerManifest("reinstall-me", "2.0.0"), map[string]string{
		"main.js": "// v2",
	})
	result, err := inst.InstallFromPath(ctx, dir2)
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if result.Version != "2.0.0" {
		t.Errorf("Version = %q, want %q", result.Version, "2.0.0")
	}

	// Verify in database
	_, err = store.GetInstalledPlugin(ctx, "others", "reinstall-me")
	if err != nil {
		t.Fatalf("GetInstalledPlugin after reinstall: %v", err)
	}
}

// ---------------------------------------------------------------------------
// InstallFromMarketplace — invalid namespace rejected
// ---------------------------------------------------------------------------

func TestInstallFromMarketplace_InvalidNamespace(t *testing.T) {
	inst, store, ctx, _ := newTestInstaller(t)
	mClient := marketplace.NewClient(store)

	_, err := inst.InstallFromMarketplace(ctx, mClient, "-invalid", "some-slug")
	if err == nil {
		t.Fatal("expected error for invalid namespace")
	}
	if !strings.Contains(err.Error(), "invalid namespace") {
		t.Errorf("error = %q, want it to mention 'invalid namespace'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromMarketplace — invalid slug rejected
// ---------------------------------------------------------------------------

func TestInstallFromMarketplace_InvalidSlug(t *testing.T) {
	inst, store, ctx, _ := newTestInstaller(t)
	mClient := marketplace.NewClient(store)

	_, err := inst.InstallFromMarketplace(ctx, mClient, "others", "-bad-slug")
	if err == nil {
		t.Fatal("expected error for invalid slug")
	}
	if !strings.Contains(err.Error(), "invalid slug") {
		t.Errorf("error = %q, want it to mention 'invalid slug'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromMarketplace — empty namespace rejected
// ---------------------------------------------------------------------------

func TestInstallFromMarketplace_EmptyNamespace(t *testing.T) {
	inst, store, ctx, _ := newTestInstaller(t)
	mClient := marketplace.NewClient(store)

	_, err := inst.InstallFromMarketplace(ctx, mClient, "", "some-slug")
	if err == nil {
		t.Fatal("expected error for empty namespace")
	}
	if !strings.Contains(err.Error(), "invalid namespace") {
		t.Errorf("error = %q, want it to mention 'invalid namespace'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromMarketplace — empty slug rejected
// ---------------------------------------------------------------------------

func TestInstallFromMarketplace_EmptySlug(t *testing.T) {
	inst, store, ctx, _ := newTestInstaller(t)
	mClient := marketplace.NewClient(store)

	_, err := inst.InstallFromMarketplace(ctx, mClient, "others", "")
	if err == nil {
		t.Fatal("expected error for empty slug")
	}
	if !strings.Contains(err.Error(), "invalid slug") {
		t.Errorf("error = %q, want it to mention 'invalid slug'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromMarketplace — nonexistent plugin returns error
// ---------------------------------------------------------------------------

func TestInstallFromMarketplace_PluginNotFound(t *testing.T) {
	inst, store, ctx, _ := newTestInstaller(t)
	mClient := marketplace.NewClient(store)

	// The "others" marketplace has no index/URL, so FindPlugin will fail
	_, err := inst.InstallFromMarketplace(ctx, mClient, "others", "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent plugin")
	}
	if !strings.Contains(err.Error(), "find plugin") {
		t.Errorf("error = %q, want it to mention 'find plugin'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromURL — invalid URL scheme rejected
// ---------------------------------------------------------------------------

func TestInstallFromURL_InvalidScheme(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	_, err := inst.InstallFromURL(ctx, "ftp://example.com/plugin.tar.gz")
	if err == nil {
		t.Fatal("expected error for ftp:// URL")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error = %q, want it to mention scheme 'not allowed'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromURL — file:// scheme rejected
// ---------------------------------------------------------------------------

func TestInstallFromURL_FileScheme(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	_, err := inst.InstallFromURL(ctx, "file:///etc/passwd")
	if err == nil {
		t.Fatal("expected error for file:// URL")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error = %q, want it to mention scheme 'not allowed'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromURL — private IP rejected (SSRF protection)
// ---------------------------------------------------------------------------

func TestInstallFromURL_PrivateIP(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	_, err := inst.InstallFromURL(ctx, "http://127.0.0.1:8080/plugin.tar.gz")
	if err == nil {
		t.Fatal("expected error for private IP URL")
	}
	if !strings.Contains(err.Error(), "private") {
		t.Errorf("error = %q, want it to mention 'private'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromURL — localhost rejected (SSRF protection)
// ---------------------------------------------------------------------------

func TestInstallFromURL_Localhost(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	_, err := inst.InstallFromURL(ctx, "http://localhost:9999/plugin.tar.gz")
	if err == nil {
		t.Fatal("expected error for localhost URL")
	}
	if !strings.Contains(err.Error(), "private") {
		t.Errorf("error = %q, want it to mention 'private'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromURL — 10.x.x.x private network rejected
// ---------------------------------------------------------------------------

func TestInstallFromURL_RFC1918(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	_, err := inst.InstallFromURL(ctx, "http://10.0.0.1/plugin.tar.gz")
	if err == nil {
		t.Fatal("expected error for RFC1918 private IP")
	}
	if !strings.Contains(err.Error(), "private") {
		t.Errorf("error = %q, want it to mention 'private'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromURL — link-local metadata endpoint rejected
// ---------------------------------------------------------------------------

func TestInstallFromURL_MetadataEndpoint(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	_, err := inst.InstallFromURL(ctx, "http://169.254.169.254/latest/meta-data")
	if err == nil {
		t.Fatal("expected error for cloud metadata endpoint")
	}
	if !strings.Contains(err.Error(), "private") {
		t.Errorf("error = %q, want it to mention 'private'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromURL — URL missing scheme rejected
// ---------------------------------------------------------------------------

func TestInstallFromURL_MissingScheme(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	_, err := inst.InstallFromURL(ctx, "example.com/plugin.tar.gz")
	if err == nil {
		t.Fatal("expected error for URL without scheme")
	}
	if !strings.Contains(err.Error(), "missing scheme") {
		t.Errorf("error = %q, want it to mention 'missing scheme'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InstallFromPath — verify NewInstaller creates correct Installer
// ---------------------------------------------------------------------------

func TestNewInstaller_Fields(t *testing.T) {
	store, _ := openTestStore(t)
	pluginDir := t.TempDir()

	inst := NewInstaller(store, pluginDir)
	if inst == nil {
		t.Fatal("NewInstaller returned nil")
	}
	if inst.store != store {
		t.Error("store field not set correctly")
	}
	if inst.pluginDir != pluginDir {
		t.Errorf("pluginDir = %q, want %q", inst.pluginDir, pluginDir)
	}
	if inst.http == nil {
		t.Error("http client should not be nil")
	}
}

// ---------------------------------------------------------------------------
// InstallFromPath — manifest with unsupported type fails
// ---------------------------------------------------------------------------

func TestInstallFromPath_UnsupportedType_Fails(t *testing.T) {
	inst, _, ctx, _ := newTestInstaller(t)

	manifest := "apiVersion: nupi.ai/v1\nkind: Plugin\ntype: unknown-type\nmetadata:\n  name: Bad Type\n  slug: bad-type\n  version: 1.0.0\nspec:\n  main: main.js\n"
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js": "// bad type",
	})

	_, err := inst.InstallFromPath(ctx, dir)
	if err == nil {
		t.Fatal("expected error for unsupported plugin type")
	}
}

// ---------------------------------------------------------------------------
// InstallFromPath — verify plugin.yaml is copied correctly
// ---------------------------------------------------------------------------

func TestInstallFromPath_ManifestPreserved(t *testing.T) {
	inst, _, ctx, pluginDir := newTestInstaller(t)

	manifest := toolHandlerManifest("preserved-plugin", "1.0.0")
	dir := createPluginDir(t, manifest, nil)

	_, err := inst.InstallFromPath(ctx, dir)
	if err != nil {
		t.Fatalf("InstallFromPath: %v", err)
	}

	installedManifest := filepath.Join(pluginDir, "others", "preserved-plugin", "plugin.yaml")
	assertFileContent(t, installedManifest, manifest)
}

// ---------------------------------------------------------------------------
// InstallFromPath — verify file permissions are reasonable
// ---------------------------------------------------------------------------

func TestInstallFromPath_FilePermissions(t *testing.T) {
	inst, _, ctx, pluginDir := newTestInstaller(t)

	manifest := toolHandlerManifest("perms-plugin", "1.0.0")
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js": "// check perms",
	})

	_, err := inst.InstallFromPath(ctx, dir)
	if err != nil {
		t.Fatalf("InstallFromPath: %v", err)
	}

	mainPath := filepath.Join(pluginDir, "others", "perms-plugin", "main.js")
	info, err := os.Stat(mainPath)
	if err != nil {
		t.Fatalf("stat main.js: %v", err)
	}

	// File should be readable by owner at minimum
	perm := info.Mode().Perm()
	if perm&0o400 == 0 {
		t.Errorf("main.js should be owner-readable, got permissions %o", perm)
	}
}

// ---------------------------------------------------------------------------
// computeChecksums — unit tests
// ---------------------------------------------------------------------------

func TestComputeChecksums_RegularFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := map[string]string{
		"main.js":     "console.log('hello')",
		"lib/util.js": "module.exports = {}",
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	checksums, err := computeChecksums(context.Background(), dir)
	if err != nil {
		t.Fatalf("computeChecksums: %v", err)
	}

	if len(checksums) != 2 {
		t.Fatalf("expected 2 checksums, got %d", len(checksums))
	}

	// Verify each hash matches
	for name, content := range files {
		h := sha256.Sum256([]byte(content))
		want := hex.EncodeToString(h[:])
		got, ok := checksums[name]
		if !ok {
			t.Errorf("missing checksum for %s", name)
			continue
		}
		if got != want {
			t.Errorf("checksum mismatch for %s: got %s, want %s", name, got, want)
		}
	}
}

func TestComputeChecksums_EmptyDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	checksums, err := computeChecksums(context.Background(), dir)
	if err != nil {
		t.Fatalf("computeChecksums: %v", err)
	}
	if len(checksums) != 0 {
		t.Fatalf("expected 0 checksums for empty dir, got %d", len(checksums))
	}
}

func TestComputeChecksums_SkipsDirectoriesAndSymlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a symlink (should be skipped); track whether it was created for assertion
	symlinkCreated := true
	if err := os.Symlink(filepath.Join(dir, "file.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Logf("symlink creation skipped (platform does not support symlinks): %v", err)
		symlinkCreated = false
	}

	checksums, err := computeChecksums(context.Background(), dir)
	if err != nil {
		t.Fatalf("computeChecksums: %v", err)
	}
	if len(checksums) != 1 {
		t.Fatalf("expected 1 checksum (regular file only), got %d: %v", len(checksums), checksums)
	}
	if _, ok := checksums["file.txt"]; !ok {
		t.Error("expected checksum for file.txt")
	}
	if symlinkCreated {
		if _, ok := checksums["link.txt"]; ok {
			t.Error("symlink link.txt should not have been checksummed")
		}
	}
}

func TestComputeChecksums_ForwardSlashPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "deep.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	checksums, err := computeChecksums(context.Background(), dir)
	if err != nil {
		t.Fatalf("computeChecksums: %v", err)
	}
	if _, ok := checksums["a/b/c/deep.js"]; !ok {
		t.Errorf("expected forward-slash path a/b/c/deep.js, got keys: %v", checksums)
	}
}

func TestComputeChecksums_UnreadableFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file permission test not reliable on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("file permission test not reliable when running as root")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "readable.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("hidden"), 0o000); err != nil {
		t.Fatal(err)
	}

	_, err := computeChecksums(context.Background(), dir)
	if err == nil {
		t.Fatal("expected error for unreadable file, got nil")
	}
	if !strings.Contains(err.Error(), "open") {
		t.Errorf("expected open error, got: %v", err)
	}
}

func TestComputeChecksums_ZeroByteFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	checksums, err := computeChecksums(context.Background(), dir)
	if err != nil {
		t.Fatalf("computeChecksums: %v", err)
	}
	if len(checksums) != 1 {
		t.Fatalf("expected 1 checksum, got %d", len(checksums))
	}
	got, ok := checksums["empty.txt"]
	if !ok {
		t.Fatal("missing checksum for empty.txt")
	}
	// SHA-256 of empty content
	h := sha256.Sum256([]byte{})
	want := hex.EncodeToString(h[:])
	if got != want {
		t.Errorf("checksum for empty file: got %s, want %s", got, want)
	}
}

func TestComputeChecksums_CancelledContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := computeChecksums(ctx, dir)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Installer checksum integration tests
// ---------------------------------------------------------------------------

func TestInstallFromPath_StoresChecksums(t *testing.T) {
	inst, store, ctx, _ := newTestInstaller(t)

	manifest := toolHandlerManifest("checksum-plugin", "1.0.0")
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js":     "console.log('hello')",
		"lib/util.js": "module.exports = {}",
	})

	result, err := inst.InstallFromPath(ctx, dir)
	if err != nil {
		t.Fatalf("InstallFromPath: %v", err)
	}

	checksums, err := store.GetPluginChecksumsByPlugin(ctx, result.Namespace, result.Slug)
	if err != nil {
		t.Fatalf("GetPluginChecksumsByPlugin: %v", err)
	}

	// Should have exactly 3 checksums: plugin.yaml, main.js, lib/util.js
	if len(checksums) != 3 {
		paths := make([]string, 0, len(checksums))
		for _, c := range checksums {
			paths = append(paths, c.FilePath)
		}
		t.Fatalf("expected 3 checksums, got %d: %v", len(checksums), paths)
	}

	pathSet := make(map[string]bool)
	for _, c := range checksums {
		pathSet[c.FilePath] = true
	}
	for _, expected := range []string{"plugin.yaml", "main.js", "lib/util.js"} {
		if !pathSet[expected] {
			t.Errorf("missing checksum for %s; stored paths: %v", expected, pathSet)
		}
	}
}

func TestUninstall_CleansUpChecksums(t *testing.T) {
	inst, store, ctx, _ := newTestInstaller(t)

	manifest := toolHandlerManifest("uninstall-chk", "1.0.0")
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js": "console.log('test')",
	})

	result, err := inst.InstallFromPath(ctx, dir)
	if err != nil {
		t.Fatalf("InstallFromPath: %v", err)
	}

	// Verify checksums exist
	checksums, err := store.GetPluginChecksumsByPlugin(ctx, result.Namespace, result.Slug)
	if err != nil {
		t.Fatalf("GetPluginChecksumsByPlugin: %v", err)
	}
	if len(checksums) == 0 {
		t.Fatal("expected checksums after install")
	}

	// Uninstall
	if err := inst.Uninstall(ctx, result.Namespace, result.Slug); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	// Verify checksums are gone (CASCADE delete)
	checksums, err = store.GetPluginChecksumsByPlugin(ctx, result.Namespace, result.Slug)
	if err != nil {
		t.Fatalf("GetPluginChecksumsByPlugin after uninstall: %v", err)
	}
	if len(checksums) != 0 {
		t.Fatalf("expected 0 checksums after uninstall, got %d", len(checksums))
	}
}

func TestInstallFromPath_ChecksumsMatchDisk(t *testing.T) {
	inst, store, ctx, pluginDir := newTestInstaller(t)

	manifest := toolHandlerManifest("disk-match", "1.0.0")
	extraFiles := map[string]string{
		"main.js":     "console.log('disk-match')",
		"lib/util.js": "module.exports = { v: 1 }",
	}
	dir := createPluginDir(t, manifest, extraFiles)

	result, err := inst.InstallFromPath(ctx, dir)
	if err != nil {
		t.Fatalf("InstallFromPath: %v", err)
	}

	checksums, err := store.GetPluginChecksumsByPlugin(ctx, result.Namespace, result.Slug)
	if err != nil {
		t.Fatalf("GetPluginChecksumsByPlugin: %v", err)
	}

	installedDir := filepath.Join(pluginDir, result.Namespace, result.Slug)

	checksumSet := make(map[string]string, len(checksums))
	for _, c := range checksums {
		checksumSet[c.FilePath] = c.SHA256
		diskPath := filepath.Join(installedDir, filepath.FromSlash(c.FilePath))
		data, err := os.ReadFile(diskPath)
		if err != nil {
			t.Fatalf("read installed file %s: %v", c.FilePath, err)
		}
		h := sha256.Sum256(data)
		diskHash := hex.EncodeToString(h[:])
		if diskHash != c.SHA256 {
			t.Errorf("checksum mismatch for %s: DB=%s, disk=%s", c.FilePath, c.SHA256, diskHash)
		}
	}

	// Reverse check: verify all disk files are represented in DB checksums
	diskChecksums, err := computeChecksums(context.Background(), installedDir)
	if err != nil {
		t.Fatalf("computeChecksums on installed dir: %v", err)
	}
	if len(diskChecksums) != len(checksums) {
		t.Errorf("disk has %d files but DB has %d checksums", len(diskChecksums), len(checksums))
	}
	for path := range diskChecksums {
		if _, ok := checksumSet[path]; !ok {
			t.Errorf("file %s exists on disk but has no checksum in DB", path)
		}
	}
}

// ---------------------------------------------------------------------------
// Installer checksum rollback tests
// ---------------------------------------------------------------------------

func TestInstallFromPath_ComputeChecksumsError_CleansUp(t *testing.T) {
	inst, store, ctx, pluginDir := newTestInstaller(t)

	// Override computeChecksums to simulate failure after copyDir + InsertInstalledPlugin succeed
	inst.computeChecksums = func(_ context.Context, _ string) (map[string]string, error) {
		return nil, fmt.Errorf("simulated checksum failure")
	}

	manifest := toolHandlerManifest("cleanup-chk-err", "1.0.0")
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js": "console.log('test')",
	})

	_, err := inst.InstallFromPath(ctx, dir)
	if err == nil {
		t.Fatal("expected error from computeChecksums failure")
	}
	if !strings.Contains(err.Error(), "simulated checksum failure") {
		t.Errorf("expected simulated error in message, got: %v", err)
	}

	// Verify cleanup: plugin directory removed
	destDir := filepath.Join(pluginDir, "others", "cleanup-chk-err")
	if _, statErr := os.Stat(destDir); !os.IsNotExist(statErr) {
		t.Errorf("plugin directory should be cleaned up after failed install, stat err: %v", statErr)
	}

	// Verify cleanup: DB record removed
	_, dbErr := store.GetInstalledPlugin(ctx, "others", "cleanup-chk-err")
	if dbErr == nil {
		t.Error("DB record should be cleaned up after failed install")
	}
	if !configstore.IsNotFound(dbErr) {
		t.Errorf("expected NotFoundError, got: %v", dbErr)
	}
}

func TestInstallFromPath_SetChecksumsError_CleansUp(t *testing.T) {
	inst, store, ctx, pluginDir := newTestInstaller(t)

	// Override computeChecksums to return invalid data that SetPluginChecksums rejects
	// (hash is not 64-char hex, so store validation fails)
	inst.computeChecksums = func(_ context.Context, _ string) (map[string]string, error) {
		return map[string]string{"file.txt": "not-valid-hex"}, nil
	}

	manifest := toolHandlerManifest("cleanup-set-err", "1.0.0")
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js": "console.log('test')",
	})

	_, err := inst.InstallFromPath(ctx, dir)
	if err == nil {
		t.Fatal("expected error from SetPluginChecksums failure")
	}
	if !strings.Contains(err.Error(), "store checksums") {
		t.Errorf("expected 'store checksums' in error, got: %v", err)
	}

	// Verify cleanup: plugin directory removed
	destDir := filepath.Join(pluginDir, "others", "cleanup-set-err")
	if _, statErr := os.Stat(destDir); !os.IsNotExist(statErr) {
		t.Errorf("plugin directory should be cleaned up after failed install, stat err: %v", statErr)
	}

	// Verify cleanup: DB record removed
	_, dbErr := store.GetInstalledPlugin(ctx, "others", "cleanup-set-err")
	if dbErr == nil {
		t.Error("DB record should be cleaned up after failed install")
	}
	if !configstore.IsNotFound(dbErr) {
		t.Errorf("expected NotFoundError, got: %v", dbErr)
	}
}

// ---------------------------------------------------------------------------
// DB-error guard on pre-install check
// ---------------------------------------------------------------------------

func TestInstallFromPath_DBErrorOnPrecheck_ReturnsError(t *testing.T) {
	inst, store, ctx, _ := newTestInstaller(t)

	manifest := toolHandlerManifest("db-err-test", "1.0.0")
	dir := createPluginDir(t, manifest, nil)

	// Close DB so GetInstalledPlugin returns a real DB error (not NotFound)
	store.Close()

	_, err := inst.InstallFromPath(ctx, dir)
	if err == nil {
		t.Fatal("expected error when DB is closed")
	}
	if !strings.Contains(err.Error(), "check plugin") {
		t.Errorf("expected 'check plugin' error from DB guard, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Cleanup-also-failed error composition
// ---------------------------------------------------------------------------

func TestInstallFromPath_CleanupAlsoFailed_SurfacesError(t *testing.T) {
	inst, store, ctx, _ := newTestInstaller(t)

	// Override computeChecksums: close DB (so DeleteInstalledPlugin fails
	// during cleanup) then return an error to trigger the rollback path.
	inst.computeChecksums = func(_ context.Context, _ string) (map[string]string, error) {
		store.Close()
		return nil, fmt.Errorf("simulated failure")
	}

	manifest := toolHandlerManifest("cleanup-fail", "1.0.0")
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js": "console.log('test')",
	})

	_, err := inst.InstallFromPath(ctx, dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "simulated failure") {
		t.Errorf("expected original error in message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "cleanup also failed") {
		t.Errorf("expected 'cleanup also failed' in message, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Cleanup-also-failed: filesystem removal failure
// ---------------------------------------------------------------------------

func TestInstallFromPath_CleanupFSFailed_SurfacesError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based test not reliable on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("chmod-based test not reliable when running as root")
	}

	inst, _, ctx, _ := newTestInstaller(t)

	// Override computeChecksums: make destDir non-writable so os.RemoveAll
	// fails during cleanup, then return an error to trigger the rollback path.
	inst.computeChecksums = func(_ context.Context, dir string) (map[string]string, error) {
		os.Chmod(dir, 0o555)
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		return nil, fmt.Errorf("simulated failure")
	}

	manifest := toolHandlerManifest("cleanup-fs-fail", "1.0.0")
	dir := createPluginDir(t, manifest, map[string]string{
		"main.js": "console.log('test')",
	})

	_, err := inst.InstallFromPath(ctx, dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "simulated failure") {
		t.Errorf("expected original error in message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "cleanup also failed") {
		t.Errorf("expected 'cleanup also failed' in message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "remove dir") {
		t.Errorf("expected 'remove dir' in cleanup error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ctxReader — mid-stream cancellation
// ---------------------------------------------------------------------------

func TestCtxReader_CancelledDuringRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	r := &ctxReader{ctx: ctx, r: strings.NewReader("hello world")}

	// First read succeeds (context still active)
	buf := make([]byte, 5)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("first read should succeed, got: %v", err)
	}
	if n != 5 || string(buf) != "hello" {
		t.Fatalf("first read: got %d bytes %q, want 5 bytes \"hello\"", n, buf[:n])
	}

	// Cancel context
	cancel()

	// Next read returns context error without reading underlying reader
	_, err = r.Read(buf)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled after cancel, got: %v", err)
	}
}
