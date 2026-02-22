package awareness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func TestCoreMemoryLoadsAllFiles(t *testing.T) {
	dir := t.TempDir()
	aDir := filepath.Join(dir, "awareness")

	writeFile(t, filepath.Join(aDir, "SOUL.md"), "I am helpful.")
	writeFile(t, filepath.Join(aDir, "IDENTITY.md"), "Name: Nupi")
	writeFile(t, filepath.Join(aDir, "USER.md"), "Name: Mariusz")
	writeFile(t, filepath.Join(aDir, "GLOBAL.md"), "Use Polish for commits.")

	svc := NewService(dir)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer svc.Shutdown(context.Background())

	cm := svc.CoreMemory()

	// Verify all sections present in order
	soulIdx := strings.Index(cm, "## SOUL")
	identityIdx := strings.Index(cm, "## IDENTITY")
	userIdx := strings.Index(cm, "## USER")
	globalIdx := strings.Index(cm, "## GLOBAL")

	if soulIdx < 0 || identityIdx < 0 || userIdx < 0 || globalIdx < 0 {
		t.Fatalf("Missing sections in core memory:\n%s", cm)
	}

	if !(soulIdx < identityIdx && identityIdx < userIdx && userIdx < globalIdx) {
		t.Errorf("Sections not in correct order: SOUL=%d, IDENTITY=%d, USER=%d, GLOBAL=%d",
			soulIdx, identityIdx, userIdx, globalIdx)
	}

	// Verify content
	if !strings.Contains(cm, "I am helpful.") {
		t.Error("SOUL content missing")
	}
	if !strings.Contains(cm, "Name: Nupi") {
		t.Error("IDENTITY content missing")
	}
	if !strings.Contains(cm, "Name: Mariusz") {
		t.Error("USER content missing")
	}
	if !strings.Contains(cm, "Use Polish for commits.") {
		t.Error("GLOBAL content missing")
	}
}

func TestCoreMemoryMissingFilesScaffolded(t *testing.T) {
	dir := t.TempDir()
	aDir := filepath.Join(dir, "awareness")

	// Only create SOUL.md with custom content — others missing.
	// Start() scaffolds missing files with defaults.
	writeFile(t, filepath.Join(aDir, "SOUL.md"), "I am helpful.")

	svc := NewService(dir)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer svc.Shutdown(context.Background())

	cm := svc.CoreMemory()

	// Custom SOUL preserved.
	if !strings.Contains(cm, "I am helpful.") {
		t.Error("custom SOUL content should be preserved")
	}
	// Scaffolded sections should now be present (created from defaults).
	for _, section := range []string{"## IDENTITY", "## USER", "## GLOBAL"} {
		if !strings.Contains(cm, section) {
			t.Errorf("%s section should be present (scaffolded)", section)
		}
	}
	// Verify actual scaffold default content is loaded, not just headers.
	if !strings.Contains(cm, "Fill this in during your first conversation") {
		t.Error("IDENTITY scaffold default content should appear in CoreMemory")
	}
	if !strings.Contains(cm, "Learn about the person") {
		t.Error("USER scaffold default content should appear in CoreMemory")
	}
	if !strings.Contains(cm, "Cross-project rules") {
		t.Error("GLOBAL scaffold default content should appear in CoreMemory")
	}
}

func TestCoreMemoryEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	aDir := filepath.Join(dir, "awareness")

	writeFile(t, filepath.Join(aDir, "SOUL.md"), "Soul content")
	writeFile(t, filepath.Join(aDir, "IDENTITY.md"), "")     // empty
	writeFile(t, filepath.Join(aDir, "USER.md"), "   \n\t ") // whitespace only
	writeFile(t, filepath.Join(aDir, "GLOBAL.md"), "Global rules")

	svc := NewService(dir)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer svc.Shutdown(context.Background())

	cm := svc.CoreMemory()

	if strings.Contains(cm, "## IDENTITY") {
		t.Error("IDENTITY section should be skipped (empty)")
	}
	if strings.Contains(cm, "## USER") {
		t.Error("USER section should be skipped (whitespace only)")
	}
	if !strings.Contains(cm, "## SOUL") {
		t.Error("SOUL should be present")
	}
	if !strings.Contains(cm, "## GLOBAL") {
		t.Error("GLOBAL should be present")
	}
}

func TestCoreMemory15KCap(t *testing.T) {
	dir := t.TempDir()
	aDir := filepath.Join(dir, "awareness")

	// Create content that exceeds 15K characters
	bigContent := strings.Repeat("A", 10000)
	writeFile(t, filepath.Join(aDir, "SOUL.md"), bigContent)
	writeFile(t, filepath.Join(aDir, "GLOBAL.md"), bigContent)

	svc := NewService(dir)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer svc.Shutdown(context.Background())

	cm := svc.CoreMemory()

	runeCount := utf8.RuneCountInString(cm)
	if runeCount != maxCoreMemoryChars {
		t.Errorf("CoreMemory() should be exactly %d runes when truncated, got %d", maxCoreMemoryChars, runeCount)
	}
}

func TestCoreMemory15KCapUTF8(t *testing.T) {
	dir := t.TempDir()
	aDir := filepath.Join(dir, "awareness")

	// Use multi-byte characters (Polish: ą = 2 bytes in UTF-8)
	bigContent := strings.Repeat("ą", 10000)
	writeFile(t, filepath.Join(aDir, "SOUL.md"), bigContent)
	writeFile(t, filepath.Join(aDir, "GLOBAL.md"), bigContent)

	svc := NewService(dir)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer svc.Shutdown(context.Background())

	cm := svc.CoreMemory()

	runeCount := utf8.RuneCountInString(cm)
	if runeCount != maxCoreMemoryChars {
		t.Errorf("CoreMemory() should be exactly %d runes when truncated, got %d", maxCoreMemoryChars, runeCount)
	}

	// Verify the string is valid UTF-8 (not broken mid-character)
	if !utf8.ValidString(cm) {
		t.Error("CoreMemory() produced invalid UTF-8 after truncation")
	}
}

func TestCoreMemoryStripsMatchingH1(t *testing.T) {
	dir := t.TempDir()
	aDir := filepath.Join(dir, "awareness")

	// Matching H1 heading → stripped (prevents ## SOUL wrapping # Soul).
	writeFile(t, filepath.Join(aDir, "SOUL.md"), "# Soul\n\nSoul body content.")
	writeFile(t, filepath.Join(aDir, "IDENTITY.md"), "# Identity\n\nIdentity body.")
	// Non-matching H1 → preserved.
	writeFile(t, filepath.Join(aDir, "USER.md"), "# My Profile\n\nUser info.")
	// No heading at all → unchanged.
	writeFile(t, filepath.Join(aDir, "GLOBAL.md"), "Global rules here.")

	svc := NewService(dir)
	if err := svc.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	svc.loadCoreMemory("")
	cm := svc.CoreMemory()

	// Matching H1 headings should be stripped — only ## wrapper remains.
	if strings.Contains(cm, "\n# Soul\n") {
		t.Error("matching H1 '# Soul' should be stripped from SOUL section")
	}
	if strings.Contains(cm, "\n# Identity\n") {
		t.Error("matching H1 '# Identity' should be stripped from IDENTITY section")
	}

	// Body content must survive stripping.
	if !strings.Contains(cm, "Soul body content.") {
		t.Error("SOUL body content should remain after H1 stripping")
	}
	if !strings.Contains(cm, "Identity body.") {
		t.Error("IDENTITY body should remain after H1 stripping")
	}

	// Non-matching H1 must be preserved.
	if !strings.Contains(cm, "# My Profile") {
		t.Error("non-matching H1 '# My Profile' should be preserved in USER section")
	}

	// Content without heading should work normally.
	if !strings.Contains(cm, "Global rules here.") {
		t.Error("content without H1 heading should be present")
	}
}

func TestCoreMemorySlugPathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	aDir := filepath.Join(dir, "awareness")

	writeFile(t, filepath.Join(aDir, "SOUL.md"), "Soul content")

	// Create a PROJECT.md file at a path that path traversal would reach
	escapedDir := filepath.Join(aDir, "memory", "projects", "..", "..", "evil")
	writeFile(t, filepath.Join(escapedDir, "PROJECT.md"), "Evil content")

	svc := NewService(dir)
	if err := svc.ensureDirectories(); err != nil {
		t.Fatalf("ensureDirectories() error = %v", err)
	}

	// Slug with path traversal should be rejected
	for _, maliciousSlug := range []string{"../evil", "..\\evil", "foo/../evil", ".."} {
		svc.loadCoreMemory(maliciousSlug)
		cm := svc.CoreMemory()
		if strings.Contains(cm, "Evil content") {
			t.Errorf("Slug %q should have been rejected (path traversal), but PROJECT content appeared", maliciousSlug)
		}
		if strings.Contains(cm, "## PROJECT") {
			t.Errorf("Slug %q should have been rejected, but PROJECT section appeared", maliciousSlug)
		}
	}
}

func TestCoreMemoryWithProjectSlug(t *testing.T) {
	dir := t.TempDir()
	aDir := filepath.Join(dir, "awareness")

	writeFile(t, filepath.Join(aDir, "SOUL.md"), "Soul content")

	// Create project core memory
	projectDir := filepath.Join(aDir, "memory", "projects", "nupi")
	writeFile(t, filepath.Join(projectDir, "PROJECT.md"), "Project: nupi voice assistant")

	svc := NewService(dir)
	if err := svc.ensureDirectories(); err != nil {
		t.Fatalf("ensureDirectories() error = %v", err)
	}

	// Directly call loadCoreMemory with a slug
	svc.loadCoreMemory("nupi")

	cm := svc.CoreMemory()

	if !strings.Contains(cm, "## PROJECT") {
		t.Error("PROJECT section should be present")
	}
	if !strings.Contains(cm, "Project: nupi voice assistant") {
		t.Error("PROJECT content missing")
	}
}

func TestCoreMemoryWithoutProjectSlug(t *testing.T) {
	dir := t.TempDir()
	aDir := filepath.Join(dir, "awareness")

	writeFile(t, filepath.Join(aDir, "SOUL.md"), "Soul content")

	// Create project file but don't pass slug
	projectDir := filepath.Join(aDir, "memory", "projects", "nupi")
	writeFile(t, filepath.Join(projectDir, "PROJECT.md"), "Should not appear")

	svc := NewService(dir)
	if err := svc.ensureDirectories(); err != nil {
		t.Fatalf("ensureDirectories() error = %v", err)
	}

	svc.loadCoreMemory("") // no active project

	cm := svc.CoreMemory()

	if strings.Contains(cm, "## PROJECT") {
		t.Error("PROJECT section should NOT be present when no slug provided")
	}
}

func TestCoreMemoryConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	aDir := filepath.Join(dir, "awareness")

	writeFile(t, filepath.Join(aDir, "SOUL.md"), "Soul content")

	svc := NewService(dir)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer svc.Shutdown(context.Background())

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = svc.CoreMemory()
		}()
	}

	// Concurrent reload
	wg.Add(1)
	go func() {
		defer wg.Done()
		svc.loadCoreMemory("")
	}()

	wg.Wait()
}
