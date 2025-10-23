package adapterrunner

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestManagerLayoutAndVersionRecording(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, ".nupi")

	m := NewManager(base)
	if err := m.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout failed: %v", err)
	}

	layout := m.Layout()
	expectBin := filepath.Join(base, "bin")
	if layout.BinDir != expectBin {
		t.Fatalf("BinDir = %q; want %q", layout.BinDir, expectBin)
	}

	expectedBinary := "adapter-runner"
	if runtime.GOOS == "windows" {
		expectedBinary += ".exe"
	}
	if filepath.Base(m.BinaryPath()) != expectedBinary {
		t.Fatalf("BinaryPath should end with %q, got %q", expectedBinary, m.BinaryPath())
	}
}

func TestInstallFromFile(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, ".nupi")

	src := buildStubRunner(t, tmp, "1.2.3")

	m := NewManager(base)
	if err := m.InstallFromFile(src); err != nil {
		t.Fatalf("InstallFromFile failed: %v", err)
	}

	info, err := os.Stat(m.BinaryPath())
	if err != nil {
		t.Fatalf("failed to stat active binary: %v", err)
	}
	if info.Size() == 0 {
		t.Fatalf("active binary size should be greater than zero")
	}

	version, err := m.InstalledVersion()
	if err != nil {
		t.Fatalf("InstalledVersion failed: %v", err)
	}
	if version != "1.2.3" {
		t.Fatalf("InstalledVersion = %q; want 1.2.3", version)
	}
}

func TestInstallFromFileEmptySource(t *testing.T) {
	m := NewManager(t.TempDir())
	if err := m.InstallFromFile(""); err == nil {
		t.Fatal("expected error for empty source path")
	}
}
