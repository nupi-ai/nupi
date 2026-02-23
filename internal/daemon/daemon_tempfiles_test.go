package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupInstallerTempFiles(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	staleA := filepath.Join(tmpDir, "nupi-plugin-a.download")
	staleB := filepath.Join(tmpDir, "nupi-plugin-b.download")
	keepA := filepath.Join(tmpDir, "nupi-plugin-a.tmp")
	keepB := filepath.Join(tmpDir, "unrelated.download")

	for _, path := range []string{staleA, staleB, keepA, keepB} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write file %s: %v", path, err)
		}
	}

	cleanupInstallerTempFiles()

	for _, path := range []string{staleA, staleB} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected stale temp file to be removed: %s (err=%v)", path, err)
		}
	}
	for _, path := range []string{keepA, keepB} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected non-matching file to remain: %s (err=%v)", path, err)
		}
	}
}

func TestCleanupInstallerTempFilesSkipsDirectories(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	dirPath := filepath.Join(tmpDir, "nupi-plugin-dir.download")
	if err := os.Mkdir(dirPath, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dirPath, err)
	}

	cleanupInstallerTempFiles()

	if info, err := os.Stat(dirPath); err != nil {
		t.Fatalf("expected matching directory to remain: %v", err)
	} else if !info.IsDir() {
		t.Fatalf("expected %s to remain a directory", dirPath)
	}
}
