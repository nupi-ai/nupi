package adapters

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExecLauncherSetsWorkingDir(t *testing.T) {
	if testing.Short() {
		t.Skip("skip launcher test in short mode")
	}

	tmpDir := t.TempDir()
	binaryPath := filepath.Join(tmpDir, testBinaryName("print-cwd"))
	buildPrintCwdBinary(t, binaryPath)

	pluginDir := filepath.Join(tmpDir, "plugin-root")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}

	var stdout bytes.Buffer
	launcher := execLauncher{}
	handle, err := launcher.Launch(context.Background(), binaryPath, nil, nil, &stdout, nil, pluginDir)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	waitForExit(t, handle)

	got := strings.TrimSpace(stdout.String())
	if got != pluginDir {
		t.Errorf("working directory = %q, want %q", got, pluginDir)
	}
}

func TestExecLauncherEmptyWorkingDirInheritsParent(t *testing.T) {
	if testing.Short() {
		t.Skip("skip launcher test in short mode")
	}

	tmpDir := t.TempDir()
	binaryPath := filepath.Join(tmpDir, testBinaryName("print-cwd"))
	buildPrintCwdBinary(t, binaryPath)

	parentCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	var stdout bytes.Buffer
	launcher := execLauncher{}
	handle, err := launcher.Launch(context.Background(), binaryPath, nil, nil, &stdout, nil, "")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	waitForExit(t, handle)

	got := strings.TrimSpace(stdout.String())
	if got != parentCwd {
		t.Errorf("working directory = %q, want parent cwd %q", got, parentCwd)
	}
}

func TestExecLauncherPluginCanReadRelativeFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skip launcher test in short mode")
	}

	tmpDir := t.TempDir()
	binaryPath := filepath.Join(tmpDir, testBinaryName("read-marker"))
	buildReadMarkerBinary(t, binaryPath)

	pluginDir := filepath.Join(tmpDir, "my-plugin")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	markerContent := "nupi-marker-ok"
	if err := os.WriteFile(filepath.Join(pluginDir, "marker.txt"), []byte(markerContent), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	var stdout bytes.Buffer
	launcher := execLauncher{}
	handle, err := launcher.Launch(context.Background(), binaryPath, nil, nil, &stdout, nil, pluginDir)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	waitForExit(t, handle)

	got := strings.TrimSpace(stdout.String())
	if got != markerContent {
		t.Errorf("marker content = %q, want %q", got, markerContent)
	}
}

// waitForExit waits for the process behind handle to exit naturally.
func waitForExit(t *testing.T, handle ProcessHandle) {
	t.Helper()
	h, ok := handle.(*execHandle)
	if !ok {
		t.Fatal("expected *execHandle")
	}
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for process to exit")
	}
}

// buildPrintCwdBinary compiles a Go binary that prints os.Getwd() to stdout.
func buildPrintCwdBinary(t *testing.T, output string) {
	t.Helper()
	src := `package main

import (
	"fmt"
	"os"
)

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(cwd)
}
`
	buildLauncherTestBinary(t, output, src)
}

// buildReadMarkerBinary compiles a Go binary that reads ./marker.txt and prints its content.
func buildReadMarkerBinary(t *testing.T, output string) {
	t.Helper()
	src := `package main

import (
	"fmt"
	"os"
)

func main() {
	data, err := os.ReadFile("marker.txt")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(string(data))
}
`
	buildLauncherTestBinary(t, output, src)
}

func buildLauncherTestBinary(t *testing.T, output, mainSrc string) {
	t.Helper()
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	goMod := "module testbin\n\ngo 1.24\n"
	if err := os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", output, ".")
	cmd.Dir = srcDir
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build test binary: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

func testBinaryName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}
