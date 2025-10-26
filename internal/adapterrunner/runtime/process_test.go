package runtime

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	// Optional stdout/stderr writes for testing.
	if msg := os.Getenv("ADAPTER_RUNNER_TEST_STDOUT"); msg != "" {
		_, _ = os.Stdout.WriteString(msg)
	}
	if msg := os.Getenv("ADAPTER_RUNNER_TEST_STDERR"); msg != "" {
		_, _ = os.Stderr.WriteString(msg)
	}
	if os.Getenv("ADAPTER_RUNNER_TEST_SLEEP") != "" {
		time.Sleep(2 * time.Second)
	}
	os.Exit(0)
}

func TestStartAndWait(t *testing.T) {
	cfg := Config{
		Command:        os.Args[0],
		Args:           []string{"-test.run=TestHelperProcess"},
		AdapterHome:    t.TempDir(),
		AdapterDataDir: filepath.Join(t.TempDir(), "data"),
		RawEnvironment: append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"ADAPTER_RUNNER_TEST_STDOUT=hello stdout\n",
			"ADAPTER_RUNNER_TEST_STDERR=hello stderr\n",
		),
	}
	opts := &StartOptions{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	}
	ctx := contextFromTest(t)

	proc, err := Start(ctx, cfg, opts)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := proc.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}

	stdout := opts.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(stdout, "hello stdout") {
		t.Fatalf("stdout not captured: %q", stdout)
	}
	stderr := opts.Stderr.(*bytes.Buffer).String()
	if !strings.Contains(stderr, "hello stderr") {
		t.Fatalf("stderr not captured: %q", stderr)
	}
}

func TestTerminateGraceful(t *testing.T) {
	cfg := Config{
		Command:        os.Args[0],
		Args:           []string{"-test.run=TestHelperProcess"},
		AdapterHome:    t.TempDir(),
		AdapterDataDir: filepath.Join(t.TempDir(), "data"),
		RawEnvironment: append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"ADAPTER_RUNNER_TEST_SLEEP=1",
		),
	}
	opts := &StartOptions{
		Stdout: io.Discard,
		Stderr: io.Discard,
	}
	ctx := contextFromTest(t)

	proc, err := Start(ctx, cfg, opts)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()

	time.Sleep(200 * time.Millisecond)
	if err := proc.Terminate(os.Interrupt, 500*time.Millisecond); err == nil {
		t.Fatalf("expected interrupt error")
	}
	if err := <-done; err == nil {
		t.Fatalf("expected non-nil error after terminate")
	}
}

func TestExitCodeExtraction(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	cmd.Env = append(cmd.Env, "ADAPTER_RUNNER_TEST_STDOUT=")
	// Force non-zero exit by killing process quickly.
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	_ = cmd.Process.Kill()
	err := cmd.Wait()
	code := ExitCode(err)
	if code == 0 {
		t.Fatalf("expected non-zero exit code, got %d", code)
	}
}

// contextFromTest returns a cancellable context bound to the test lifetime.
func contextFromTest(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
