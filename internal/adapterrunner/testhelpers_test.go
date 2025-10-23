package adapterrunner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func buildStubRunner(t *testing.T, dir, version string) string {
	t.Helper()

	source := fmt.Sprintf(`package main

import (
	"flag"
	"fmt"
)

func main() {
	showVersion := flag.Bool("version", false, "print version")
	flag.String("slot", "", "slot")
	flag.String("adapter", "", "adapter")
	flag.Parse()
	if *showVersion {
		fmt.Println("%s")
		return
	}
}
`, version)

	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(source), 0o644); err != nil {
		t.Fatalf("failed to write stub source: %v", err)
	}

	binaryPath := filepath.Join(dir, "adapter-runner-stub")
	if runtime.GOOS == "windows" {
		binaryPath += ".exe"
	}

	cmd := exec.Command("go", "build", "-o", binaryPath, srcPath)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build stub runner: %v\n%s", err, string(output))
	}
	return binaryPath
}
