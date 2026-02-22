package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	nupiversion "github.com/nupi-ai/nupi/internal/version"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

// captureStdout runs fn with stdout redirected to a pipe and returns the output.
// Reading happens in a goroutine to avoid deadlock if output exceeds the pipe buffer.
// WARNING: Modifies the global os.Stdout — incompatible with t.Parallel().
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	// Read concurrently to avoid pipe buffer deadlock.
	ch := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		buf.ReadFrom(r)
		ch <- buf.String()
	}()

	fn()
	w.Close()

	return <-ch
}

func TestVersionCommandOutputFormat(t *testing.T) {
	// Force connection failure so the test doesn't accidentally connect to a running daemon.
	t.Setenv("NUPI_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("NUPI_API_TOKEN", "")

	output := captureStdout(t, func() {
		cmd := newVersionCommand()
		cmd.SetArgs([]string{})
		_ = cmd.Execute()
	})

	clientLine := "Client: " + nupiversion.FormatVersion(nupiversion.String())
	if !strings.Contains(output, clientLine) {
		t.Errorf("output missing client version line %q, got:\n%s", clientLine, output)
	}
	// Daemon shows "unavailable" when daemon is not reachable.
	if !strings.Contains(output, "Daemon: unavailable (") {
		t.Errorf("output missing daemon status line with error detail, got:\n%s", output)
	}
}

func TestVersionCommandJSONOutput(t *testing.T) {
	// Force connection failure so the test doesn't accidentally connect to a running daemon.
	t.Setenv("NUPI_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("NUPI_API_TOKEN", "")

	output := captureStdout(t, func() {
		// Mirror production: version command inherits --json from parent's PersistentFlags.
		root := &cobra.Command{Use: "test"}
		root.PersistentFlags().Bool("json", false, "Output in JSON format")
		root.AddCommand(newVersionCommand())
		root.SetArgs([]string{"version", "--json"})
		_ = root.Execute()
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &result); err != nil {
		t.Fatalf("JSON output is not valid JSON: %v\nOutput:\n%s", err, output)
	}

	clientVal, ok := result["client"]
	if !ok {
		t.Error("JSON output missing 'client' key")
	} else if clientVal != nupiversion.String() {
		t.Errorf("client = %v, want %q", clientVal, nupiversion.String())
	}

	// Daemon should be null (not connected) when daemon is unreachable.
	daemonVal, ok := result["daemon"]
	if !ok {
		t.Error("JSON output missing 'daemon' key")
	} else if daemonVal != nil {
		t.Errorf("daemon = %v, want nil (daemon unreachable)", daemonVal)
	}
	// daemon_error should be present when daemon is unreachable.
	if _, ok := result["daemon_error"]; !ok {
		t.Log("daemon_error key absent — daemon might be running or grpcclient.New() returned cleanly")
	}
}

func TestVersionCommandEmptyDaemonVersion(t *testing.T) {
	// Start a gRPC server that returns empty version (simulates old daemon).
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network not permitted: %v", err)
	}
	srv := grpc.NewServer()
	apiv1.RegisterDaemonServiceServer(srv, &fakeDaemonServer{version: ""})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	t.Setenv("NUPI_BASE_URL", "http://"+lis.Addr().String())
	t.Setenv("NUPI_API_TOKEN", "")

	output := captureStdout(t, func() {
		cmd := newVersionCommand()
		cmd.SetArgs([]string{})
		_ = cmd.Execute()
	})

	expectedClient := "Client: " + nupiversion.FormatVersion(nupiversion.String())
	if !strings.Contains(output, expectedClient) {
		t.Errorf("missing client version %q, got:\n%s", expectedClient, output)
	}
	if !strings.Contains(output, "version unknown") {
		t.Errorf("expected 'version unknown' for empty daemon version, got:\n%s", output)
	}
	if strings.Contains(output, "WARNING") {
		t.Errorf("expected no mismatch warning for empty daemon version, got:\n%s", output)
	}
}

// fakeDaemonServer returns a fixed version in DaemonStatusResponse.
type fakeDaemonServer struct {
	apiv1.UnimplementedDaemonServiceServer
	version string
}

func (s *fakeDaemonServer) Status(_ context.Context, _ *apiv1.DaemonStatusRequest) (*apiv1.DaemonStatusResponse, error) {
	return &apiv1.DaemonStatusResponse{Version: s.version}, nil
}

func TestVersionCommandMismatchOutput(t *testing.T) {
	// Start a gRPC server that returns daemon version "9.9.9".
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network not permitted: %v", err)
	}
	srv := grpc.NewServer()
	apiv1.RegisterDaemonServiceServer(srv, &fakeDaemonServer{version: "9.9.9"})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	cleanup := nupiversion.ForTesting("1.0.0")
	t.Cleanup(cleanup)

	t.Setenv("NUPI_BASE_URL", "http://"+lis.Addr().String())
	t.Setenv("NUPI_API_TOKEN", "")

	output := captureStdout(t, func() {
		cmd := newVersionCommand()
		cmd.SetArgs([]string{})
		_ = cmd.Execute()
	})

	if !strings.Contains(output, "Client: v1.0.0") {
		t.Errorf("missing client version, got:\n%s", output)
	}
	if !strings.Contains(output, "Daemon: v9.9.9") {
		t.Errorf("missing daemon version, got:\n%s", output)
	}
	if !strings.Contains(output, "WARNING") {
		t.Errorf("missing mismatch warning, got:\n%s", output)
	}
}
