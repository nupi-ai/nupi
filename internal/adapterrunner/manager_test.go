package adapterrunner

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	if filepath.Dir(layout.BaseDir) != filepath.Join(base, "bin") {
		t.Fatalf("unexpected base dir: %s", layout.BaseDir)
	}

	expectedBinary := "adapter-runner"
	if runtime.GOOS == "windows" {
		expectedBinary += ".exe"
	}
	if filepath.Base(m.BinaryPath()) != expectedBinary {
		t.Fatalf("BinaryPath should end with %q, got %q", expectedBinary, m.BinaryPath())
	}

	version, err := m.InstalledVersion()
	if !errors.Is(err, ErrNotInstalled) || version != "" {
		t.Fatalf("expected ErrNotInstalled, got version=%q err=%v", version, err)
	}

	if err := m.RecordInstalledVersion("v1.2.3"); err != nil {
		t.Fatalf("RecordInstalledVersion failed: %v", err)
	}

	v, err := m.InstalledVersion()
	if err != nil {
		t.Fatalf("InstalledVersion failed: %v", err)
	}
	if v != "v1.2.3" {
		t.Fatalf("InstalledVersion = %q, want %q", v, "v1.2.3")
	}

	if _, err := os.Stat(m.BinaryForVersion("v1.2.3")); err == nil {
		t.Fatalf("BinaryForVersion should not pre-create files")
	}
}

func TestActivateVersion(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, ".nupi")

	m := NewManager(base)
	if err := m.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout failed: %v", err)
	}

	versionDir := m.VersionRoot("v2.0.0")
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		t.Fatalf("failed to create version directory: %v", err)
	}

	srcBinary := m.BinaryForVersion("v2.0.0")
	if err := os.WriteFile(srcBinary, []byte("#!/bin/sh\necho runner\n"), 0o755); err != nil {
		t.Fatalf("failed to seed runner binary: %v", err)
	}

	if err := m.ActivateVersion("v2.0.0"); err != nil {
		t.Fatalf("ActivateVersion failed: %v", err)
	}

	active := m.BinaryPath()
	data, err := os.ReadFile(active)
	if err != nil {
		t.Fatalf("failed to read active binary: %v", err)
	}
	if !strings.Contains(string(data), "echo runner") {
		t.Fatalf("active binary contents mismatch: %q", string(data))
	}

	version, err := m.InstalledVersion()
	if err != nil {
		t.Fatalf("InstalledVersion failed: %v", err)
	}
	if version != "v2.0.0" {
		t.Fatalf("InstalledVersion = %q; want v2.0.0", version)
	}
}

func TestSanitizeVersion(t *testing.T) {
	cases := map[string]string{
		"":             "unknown",
		"  v1.0.0  ":   "v1.0.0",
		"dev/build-1":  "dev_build-1",
		"foo\\bar":     "foo_bar",
		"../relative":  ".._relative",
		"release?cand": "release?cand",
	}

	for input, expected := range cases {
		if got := sanitizeVersion(input); got != expected {
			t.Fatalf("sanitizeVersion(%q) = %q; want %q", input, got, expected)
		}
	}
}
