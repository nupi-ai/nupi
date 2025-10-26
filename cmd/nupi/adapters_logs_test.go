package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestAdaptersLogsCommand(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network not permitted: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/adapters/logs" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"type":"log","timestamp":"2025-01-01T00:00:00Z","adapter_id":"example","slot":"stt","level":"info","message":"hello"}`)
	}))
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	prevBase := os.Getenv("NUPI_BASE_URL")
	if err := os.Setenv("NUPI_BASE_URL", srv.URL); err != nil {
		t.Fatalf("set env: %v", err)
	}
	t.Cleanup(func() {
		os.Setenv("NUPI_BASE_URL", prevBase)
	})

	cmd := &cobra.Command{}
	cmd.Flags().Bool("grpc", false, "")
	cmd.Flags().String("slot", "", "")
	cmd.Flags().String("adapter", "", "")

	output := captureOutput(func() {
		if err := adaptersLogs(cmd, nil); err != nil {
			t.Fatalf("adaptersLogs returned error: %v", err)
		}
	})

	if !strings.Contains(output, "hello") {
		t.Fatalf("expected log output to contain message, got %q", output)
	}
}

func captureOutput(fn func()) string {
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
	}()

	fn()
	w.Close()

	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}
