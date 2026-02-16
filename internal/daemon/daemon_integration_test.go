package daemon_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/daemon"
	"github.com/nupi-ai/nupi/internal/grpcclient"
	"github.com/nupi-ai/nupi/internal/jsrunner"
)

// ensureJSRuntime sets up NUPI_JS_RUNTIME if the runtime is not available
// via standard resolution but bun is in PATH. This allows tests to run
// in dev environments where bun is installed globally.
func ensureJSRuntime(t *testing.T) {
	t.Helper()
	if jsrunner.IsAvailable() {
		return
	}
	bunPath, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("JS runtime not available: not bundled and bun not in PATH")
	}
	t.Setenv("NUPI_JS_RUNTIME", bunPath)
}

type lockedBuffer struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (lb lockedBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.buf.Write(p)
}

func TestDaemonClientIntegration_CreateAttachStream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY integration tests are not supported on Windows")
	}
	ensureJSRuntime(t)

	tmpHome, cleanupHome := mustSetTempHome(t)
	defer cleanupHome()

	d, startErr, wg := startDaemonForTest(t)
	defer func() {
		d.Shutdown()
		if err := <-startErr; err != nil {
			if shouldSkipRuntimeError(err) {
				return
			}
			t.Errorf("daemon start returned error: %v", err)
		}
		wg.Wait()
	}()

	paths := config.GetInstancePaths(config.DefaultInstance)
	waitForSocket(t, startErr, paths.Socket)

	gc := connectGRPCClient(t)
	defer gc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var streamBuf bytes.Buffer

	customBin := filepath.Join(tmpHome, "custom-bin")
	if err := os.MkdirAll(customBin, 0o755); err != nil {
		t.Fatalf("failed to create custom bin dir: %v", err)
	}

	scriptPath := filepath.Join(customBin, "customcmd")
	scriptContent := "#!/bin/sh\nprintf \"ready:%s\" \"$CUSTOM_VAR\"\n"
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0o755); err != nil {
		t.Fatalf("failed to write custom script: %v", err)
	}

	pathValue := customBin
	if existing := os.Getenv("PATH"); existing != "" {
		pathValue = pathValue + string(os.PathListSeparator) + existing
	}

	envList := make([]string, 0, len(os.Environ())+2)
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "PATH=") {
			continue
		}
		envList = append(envList, env)
	}
	envList = append(envList, "PATH="+pathValue, "CUSTOM_VAR=client-env-value")

	createResp, err := gc.CreateSession(ctx, &apiv1.CreateSessionRequest{
		Command:  "/bin/sh",
		Args:     []string{"-c", "customcmd"},
		Detached: false,
		Rows:     24,
		Cols:     80,
		Env:      envList,
	})
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	sessionID := createResp.GetSession().GetId()

	stream, err := gc.AttachSession(ctx)
	if err != nil {
		t.Fatalf("AttachSession failed: %v", err)
	}

	if err := stream.Send(&apiv1.AttachSessionRequest{
		Payload: &apiv1.AttachSessionRequest_Init{
			Init: &apiv1.AttachInit{
				SessionId:      sessionID,
				IncludeHistory: true,
			},
		},
	}); err != nil {
		t.Fatalf("AttachSession init send failed: %v", err)
	}

	streamDone := make(chan error, 1)
	var streamMu sync.Mutex
	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				if err == io.EOF || ctx.Err() != nil {
					streamDone <- ctx.Err()
					return
				}
				streamDone <- err
				return
			}
			if output := resp.GetOutput(); len(output) > 0 {
				lb := lockedBuffer{buf: &streamBuf, mu: &streamMu}
				lb.Write(output)
			}
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		streamMu.Lock()
		snapshot := streamBuf.String()
		streamMu.Unlock()
		if strings.Contains(snapshot, "ready:client-env-value") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	streamMu.Lock()
	finalSnapshot := streamBuf.String()
	streamMu.Unlock()
	if !strings.Contains(finalSnapshot, "ready:client-env-value") {
		t.Fatalf("expected stream output to contain 'ready:client-env-value', got %q", finalSnapshot)
	}

	cancel()
	if err := <-streamDone; err != nil && err != context.Canceled {
		t.Fatalf("stream ended with error: %v", err)
	}

	listCtx, listCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer listCancel()

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		listResp, err := gc.ListSessions(listCtx)
		if err != nil {
			t.Fatalf("ListSessions failed: %v", err)
		}
		for _, s := range listResp.GetSessions() {
			if s.GetId() == sessionID && s.GetStatus() == "stopped" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("session %s did not reach stopped state", sessionID)
}

func TestDaemonClientIntegration_DetachAndKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY integration tests are not supported on Windows")
	}
	ensureJSRuntime(t)

	_, cleanupHome := mustSetTempHome(t)
	defer cleanupHome()

	d, startErr, wg := startDaemonForTest(t)
	defer func() {
		d.Shutdown()
		if err := <-startErr; err != nil {
			if shouldSkipRuntimeError(err) {
				return
			}
			t.Errorf("daemon start returned error: %v", err)
		}
		wg.Wait()
	}()

	paths := config.GetInstancePaths(config.DefaultInstance)
	waitForSocket(t, startErr, paths.Socket)

	gc := connectGRPCClient(t)
	defer gc.Close()

	ctx := context.Background()

	createResp, err := gc.CreateSession(ctx, &apiv1.CreateSessionRequest{
		Command:  "/bin/sh",
		Args:     []string{"-c", "sleep 1"},
		Detached: true,
	})
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	sessionID := createResp.GetSession().GetId()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		listResp, err := gc.ListSessions(ctx)
		if err != nil {
			t.Fatalf("ListSessions failed: %v", err)
		}
		for _, s := range listResp.GetSessions() {
			if s.GetId() == sessionID && s.GetStatus() == "detached" {
				goto kill
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session did not report detached status")

kill:
	if _, err := gc.KillSession(ctx, sessionID); err != nil {
		t.Fatalf("KillSession failed: %v", err)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		listResp, err := gc.ListSessions(ctx)
		if err != nil {
			t.Fatalf("ListSessions failed: %v", err)
		}
		foundStopped := false
		otherSessions := false
		for _, s := range listResp.GetSessions() {
			if s.GetId() == sessionID {
				if s.GetStatus() == "stopped" {
					foundStopped = true
				}
				continue
			}
			otherSessions = true
		}
		if foundStopped && !otherSessions {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %s did not stop after kill", sessionID)
}

func connectGRPCClient(t *testing.T) *grpcclient.Client {
	t.Helper()

	var gc *grpcclient.Client
	var err error
	for i := 0; i < 50; i++ {
		gc, err = grpcclient.New()
		if err == nil {
			return gc
		}
		time.Sleep(20 * time.Millisecond)
	}
	if shouldSkipRuntimeError(err) {
		t.Skipf("skipping: gRPC client connection failed: %v", err)
	}
	t.Fatalf("failed to connect gRPC client to daemon: %v", err)
	return nil
}

func mustSetTempHome(t *testing.T) (string, func()) {
	t.Helper()

	tmpHome, err := os.MkdirTemp("/tmp", "nupi-integ-")
	if err != nil {
		t.Fatalf("failed to create temp home: %v", err)
	}

	oldHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", tmpHome); err != nil {
		t.Fatalf("failed to set HOME: %v", err)
	}

	return tmpHome, func() {
		_ = os.Setenv("HOME", oldHome)
		_ = os.RemoveAll(tmpHome)
	}
}

func startDaemonForTest(t *testing.T) (*daemon.Daemon, chan error, *sync.WaitGroup) {
	t.Helper()

	store, err := configstore.Open(configstore.Options{
		InstanceName: config.DefaultInstance,
		ProfileName:  config.DefaultProfile,
	})
	if err != nil {
		if shouldSkipRuntimeError(err) {
			t.Skipf("skipping daemon integration test: %v", err)
		}
		t.Fatalf("failed to open config store: %v", err)
	}

	d, err := daemon.New(daemon.Options{Store: store})
	if err != nil {
		store.Close()
		if shouldSkipRuntimeError(err) {
			t.Skipf("skipping daemon integration test: %v", err)
		}
		t.Fatalf("failed to create daemon: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)

	startErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		startErr <- d.Start()
	}()

	return d, startErr, &wg
}

func waitForSocket(t *testing.T, startErr chan error, socketPath string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("daemon socket was not created in time: %s", socketPath)
		}
		if _, err := os.Stat(socketPath); err == nil {
			return
		}
		select {
		case err := <-startErr:
			startErr <- err
			if err != nil {
				if shouldSkipRuntimeError(err) {
					t.Skipf("skipping daemon integration test: %v", err)
				}
				t.Fatalf("daemon failed to start: %v", err)
			}
			t.Fatalf("daemon stopped unexpectedly during startup")
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func shouldSkipRuntimeError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "operation not permitted") {
		return true
	}
	if strings.Contains(msg, "unable to open database file") {
		return true
	}
	if strings.Contains(msg, "permission denied") {
		return true
	}
	// Socket bind errors (sandboxed environments)
	if strings.Contains(msg, "bind:") {
		return true
	}
	if strings.Contains(msg, "address already in use") {
		return true
	}
	// JS runtime not available (bundled or NUPI_JS_RUNTIME not set)
	if strings.Contains(msg, "runtime not found") {
		return true
	}
	if strings.Contains(msg, "jsrunner") {
		return true
	}
	return false
}
