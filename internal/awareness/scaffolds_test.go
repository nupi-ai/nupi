package awareness

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestScaffoldCoreFilesCreatesAllFiles(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	for _, sf := range coreScaffolds {
		path := filepath.Join(svc.awarenessDir, sf.filename)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", sf.filename, err)
		}
		if string(got) != sf.content {
			t.Fatalf("%s: content mismatch (got %d bytes, want %d bytes)", sf.filename, len(got), len(sf.content))
		}
	}
}

func TestScaffoldCoreFilesIdempotent(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}

	// Pre-create SOUL.md and IDENTITY.md with custom content.
	customSoul := "# My Custom Soul\n"
	customIdentity := "# My Custom Identity\n"
	if err := os.WriteFile(filepath.Join(svc.awarenessDir, "SOUL.md"), []byte(customSoul), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.awarenessDir, "IDENTITY.md"), []byte(customIdentity), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	// Custom files must be preserved.
	got, err := os.ReadFile(filepath.Join(svc.awarenessDir, "SOUL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != customSoul {
		t.Fatal("SOUL.md was overwritten")
	}

	got, err = os.ReadFile(filepath.Join(svc.awarenessDir, "IDENTITY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != customIdentity {
		t.Fatal("IDENTITY.md was overwritten")
	}

	// Remaining files must have correct default content.
	remaining := []struct {
		name string
		want string
	}{
		{"USER.md", defaultUserContent},
		{"GLOBAL.md", defaultGlobalContent},
		{"BOOTSTRAP.md", defaultBootstrapContent},
	}
	for _, r := range remaining {
		data, err := os.ReadFile(filepath.Join(svc.awarenessDir, r.name))
		if err != nil {
			t.Fatalf("read %s: %v", r.name, err)
		}
		if string(data) != r.want {
			t.Fatalf("%s: content mismatch (got %d bytes, want %d bytes)", r.name, len(data), len(r.want))
		}
	}
}

func TestScaffoldCoreFilesPartialRecovery(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}

	// Pre-create only SOUL.md and GLOBAL.md.
	if err := os.WriteFile(filepath.Join(svc.awarenessDir, "SOUL.md"), []byte("existing soul"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.awarenessDir, "GLOBAL.md"), []byte("existing global"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := svc.scaffoldCoreFiles(); err != nil {
		t.Fatal(err)
	}

	// Existing files untouched.
	got, err := os.ReadFile(filepath.Join(svc.awarenessDir, "SOUL.md"))
	if err != nil {
		t.Fatalf("read SOUL.md: %v", err)
	}
	if string(got) != "existing soul" {
		t.Fatal("SOUL.md was overwritten")
	}
	got, err = os.ReadFile(filepath.Join(svc.awarenessDir, "GLOBAL.md"))
	if err != nil {
		t.Fatalf("read GLOBAL.md: %v", err)
	}
	if string(got) != "existing global" {
		t.Fatal("GLOBAL.md was overwritten")
	}

	// Missing files created with correct default content.
	for _, sf := range coreScaffolds {
		if sf.filename == "SOUL.md" || sf.filename == "GLOBAL.md" {
			continue
		}
		got, err = os.ReadFile(filepath.Join(svc.awarenessDir, sf.filename))
		if err != nil {
			t.Fatalf("read %s: %v", sf.filename, err)
		}
		if string(got) != sf.content {
			t.Fatalf("%s: content mismatch", sf.filename)
		}
	}
}

func TestScaffoldCoreFilesContainExpectedMarkers(t *testing.T) {
	markers := map[string]string{
		"SOUL.md":      "Personality",
		"IDENTITY.md":  "Name:",
		"USER.md":      "User",
		"GLOBAL.md":    "Global",
		"BOOTSTRAP.md": "Welcome",
	}

	for _, sf := range coreScaffolds {
		marker, ok := markers[sf.filename]
		if !ok {
			t.Fatalf("no marker defined for %s", sf.filename)
		}
		if !strings.Contains(sf.content, marker) {
			t.Fatalf("%s: expected to contain %q", sf.filename, marker)
		}
	}
}

func TestServiceStartCreatesScaffolds(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	// No event bus — scaffolds don't need it.

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = svc.Shutdown(context.Background())
	}()

	// Verify all 5 scaffold files exist with correct default content.
	for _, sf := range coreScaffolds {
		path := filepath.Join(svc.awarenessDir, sf.filename)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s not created by Start(): %v", sf.filename, err)
		}
		if string(got) != sf.content {
			t.Fatalf("%s: content mismatch (got %d bytes, want %d bytes)", sf.filename, len(got), len(sf.content))
		}
	}

	// CoreMemory should include all 4 core memory sections after Start.
	cm := svc.CoreMemory()
	for _, section := range []string{"## SOUL", "## IDENTITY", "## USER", "## GLOBAL"} {
		if !strings.Contains(cm, section) {
			t.Fatalf("CoreMemory() missing %s section after Start", section)
		}
	}

	// BOOTSTRAP.md must NOT leak into CoreMemory — it's for onboarding detection only.
	if strings.Contains(cm, "onboarding_complete") {
		t.Fatal("BOOTSTRAP.md content should not appear in CoreMemory()")
	}
}

func TestEveryCoreMemoryFileHasMatchingScaffold(t *testing.T) {
	scaffoldNames := make(map[string]bool, len(coreScaffolds))
	for _, sf := range coreScaffolds {
		scaffoldNames[sf.filename] = true
	}

	for _, f := range coreMemoryFiles {
		if !scaffoldNames[f.filename] {
			t.Errorf("coreMemoryFiles entry %q has no corresponding scaffold in coreScaffolds", f.filename)
		}
	}
}

func TestNoScaffoldLeaksIntoCoreMemoryUnexpectedly(t *testing.T) {
	// Reverse guard: every scaffold that is NOT in coreMemoryFiles must be
	// explicitly known here. Catches accidental addition of BOOTSTRAP.md
	// (or future onboarding-only files) to coreMemoryFiles.
	memoryNames := make(map[string]bool, len(coreMemoryFiles))
	for _, f := range coreMemoryFiles {
		memoryNames[f.filename] = true
	}

	expectedExcluded := map[string]bool{
		"BOOTSTRAP.md": true, // onboarding sentinel, not system prompt content
	}

	for _, sf := range coreScaffolds {
		if memoryNames[sf.filename] {
			continue
		}
		if !expectedExcluded[sf.filename] {
			t.Errorf("scaffold %q is not in coreMemoryFiles and not in expectedExcluded — was it accidentally added to coreScaffolds without updating this test?", sf.filename)
		}
	}
}

func TestScaffoldCoreFilesStatError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-specific permission test")
	}

	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}

	// Remove execute permission so os.Stat fails with EACCES (not ErrNotExist).
	if err := os.Chmod(svc.awarenessDir, 0o200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(svc.awarenessDir, 0o755) })

	err := svc.scaffoldCoreFiles()
	if err == nil {
		t.Fatal("scaffoldCoreFiles() should fail when directory lacks execute permission")
	}
	if !strings.Contains(err.Error(), "stat") {
		t.Fatalf("error should mention 'stat', got: %v", err)
	}
}

func TestScaffoldCoreFilesWriteError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-specific permission test")
	}

	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}

	// Make awareness dir read-only to prevent file creation.
	if err := os.Chmod(svc.awarenessDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(svc.awarenessDir, 0o755) })

	err := svc.scaffoldCoreFiles()
	if err == nil {
		t.Fatal("scaffoldCoreFiles() should fail when directory is read-only")
	}
	if !strings.Contains(err.Error(), "write ") {
		t.Fatalf("error should mention 'write' prefix, got: %v", err)
	}
}
