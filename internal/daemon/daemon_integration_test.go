package daemon_test

import (
	"bytes"
	"context"
	"io"
	"log"
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

// lockedBuffer wraps a bytes.Buffer with a mutex for thread-safe writes
// (via io.Writer) and reads (via String). Used to capture log output from
// concurrent daemon goroutines without data races.
type lockedBuffer struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (lb lockedBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.buf.Write(p)
}

func (lb lockedBuffer) String() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.buf.String()
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

func TestDaemonIntegrityCheckerRefusesTamperedPlugin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration tests not supported on Windows")
	}

	// Resolve the JS runtime path BEFORE changing HOME, since the bundled
	// bun lives under ~/.nupi/bin/ which won't exist in the temp HOME.
	bunPath, err := jsrunner.GetRuntimePath()
	if err != nil {
		bunPath2, err2 := exec.LookPath("bun")
		if err2 != nil {
			t.Skip("JS runtime not available: not bundled and bun not in PATH")
		}
		bunPath = bunPath2
	}

	_, cleanupHome := mustSetTempHome(t)
	// LIFO cleanup order: daemon shutdown → store.Close → log restore → cleanupHome
	t.Cleanup(cleanupHome)

	// Set NUPI_JS_RUNTIME explicitly so the daemon can find bun after HOME changed.
	t.Setenv("NUPI_JS_RUNTIME", bunPath)

	// Capture log output with a thread-safe buffer to avoid data races
	// between daemon goroutines writing via log.Printf and the test reading.
	lb := lockedBuffer{mu: new(sync.Mutex), buf: new(bytes.Buffer)}
	prevLogOutput := log.Writer()
	log.SetOutput(io.MultiWriter(prevLogOutput, lb))
	t.Cleanup(func() { log.SetOutput(prevLogOutput) })

	ctx := context.Background()

	store, err := configstore.Open(configstore.Options{
		InstanceName: config.DefaultInstance,
		ProfileName:  config.DefaultProfile,
	})
	if err != nil {
		if shouldSkipRuntimeError(err) {
			t.Skipf("skipping: %v", err)
		}
		t.Fatalf("open store: %v", err)
	}
	// store.Close is also called by d.Start() on return; double-close is safe
	// (sql.DB.Close is idempotent) and covers the case where Start() never runs.
	t.Cleanup(func() { store.Close() })

	// Create a pipeline-cleaner plugin on disk.
	paths := config.GetInstancePaths(config.DefaultInstance)
	pluginDir := filepath.Join(paths.Home, "plugins", "test-ns", "tampered-plugin")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifestYAML := "apiVersion: nap.nupi.ai/v1alpha1\nkind: Plugin\ntype: pipeline-cleaner\nmetadata:\n  name: tampered-plugin\n  slug: tampered-plugin\n  namespace: test-ns\n  version: 0.1.0\nspec:\n  main: main.js\n"
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "main.js"), []byte(`module.exports={name:"t",transform:function(x){return x}};`), 0o644); err != nil {
		t.Fatalf("write main.js: %v", err)
	}

	// Register the plugin in the store with WRONG checksums so the
	// integrity checker (wired by daemon.New) will refuse it.
	mp, err := store.AddMarketplace(ctx, "test-ns", "https://test.example.com")
	if err != nil {
		t.Fatalf("add marketplace: %v", err)
	}
	pluginID, err := store.InsertInstalledPlugin(ctx, mp.ID, "tampered-plugin", "")
	if err != nil {
		t.Fatalf("insert plugin: %v", err)
	}
	if err := store.SetPluginEnabled(ctx, "test-ns", "tampered-plugin", true); err != nil {
		t.Fatalf("enable plugin: %v", err)
	}
	if err := store.SetPluginChecksums(ctx, pluginID, map[string]string{
		"main.js": "0000000000000000000000000000000000000000000000000000000000000000",
	}); err != nil {
		t.Fatalf("set checksums: %v", err)
	}

	// Create and start daemon — this wires SetIntegrityChecker.
	d, err := daemon.New(daemon.Options{Store: store})
	if err != nil {
		if shouldSkipRuntimeError(err) {
			t.Skipf("skipping: %v", err)
		}
		t.Fatalf("create daemon: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	startErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		startErr <- d.Start()
	}()

	// Ensure cleanup even if waitForSocket fatals.
	t.Cleanup(func() {
		d.Shutdown()
		if err := <-startErr; err != nil {
			if !shouldSkipRuntimeError(err) {
				t.Errorf("daemon start error: %v", err)
			}
		}
		wg.Wait()
	})

	waitForSocket(t, startErr, paths.Socket)
	// Plugin loading completes synchronously before the transport gateway
	// creates the socket, so the REFUSED log is already in the buffer.

	// The integrity checker closure (wired in daemon.New) should have
	// refused the tampered plugin during plugin service startup.
	logOutput := lb.String()
	if !strings.Contains(logOutput, "REFUSED") {
		t.Errorf("expected log to contain 'REFUSED' for tampered plugin, got:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "integrity check failed") {
		t.Errorf("expected log to contain 'integrity check failed', got:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "tampered-plugin") {
		t.Errorf("expected log to mention 'tampered-plugin', got:\n%s", logOutput)
	}
}

func TestDaemonIntegrityCheckerAllowsManualInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration tests not supported on Windows")
	}

	bunPath, err := jsrunner.GetRuntimePath()
	if err != nil {
		bunPath2, err2 := exec.LookPath("bun")
		if err2 != nil {
			t.Skip("JS runtime not available: not bundled and bun not in PATH")
		}
		bunPath = bunPath2
	}

	_, cleanupHome := mustSetTempHome(t)
	// LIFO cleanup order: daemon shutdown → store.Close → log restore → cleanupHome
	t.Cleanup(cleanupHome)

	t.Setenv("NUPI_JS_RUNTIME", bunPath)

	lb := lockedBuffer{mu: new(sync.Mutex), buf: new(bytes.Buffer)}
	prevLogOutput := log.Writer()
	log.SetOutput(io.MultiWriter(prevLogOutput, lb))
	t.Cleanup(func() { log.SetOutput(prevLogOutput) })

	// Create a plugin on disk WITHOUT registering it in the store.
	// This simulates a manual install — no marketplace, no checksums.
	paths := config.GetInstancePaths(config.DefaultInstance)
	pluginDir := filepath.Join(paths.Home, "plugins", "manual-ns", "manual-plugin")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifestYAML := "apiVersion: nap.nupi.ai/v1alpha1\nkind: Plugin\ntype: pipeline-cleaner\nmetadata:\n  name: manual-plugin\n  slug: manual-plugin\n  namespace: manual-ns\n  version: 0.1.0\nspec:\n  main: main.js\n"
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "main.js"), []byte(`module.exports={name:"manual",transform:function(x){return x}};`), 0o644); err != nil {
		t.Fatalf("write main.js: %v", err)
	}

	store, err := configstore.Open(configstore.Options{
		InstanceName: config.DefaultInstance,
		ProfileName:  config.DefaultProfile,
	})
	if err != nil {
		if shouldSkipRuntimeError(err) {
			t.Skipf("skipping: %v", err)
		}
		t.Fatalf("open store: %v", err)
	}
	// store.Close is also called by d.Start() on return; double-close is safe
	// (sql.DB.Close is idempotent) and covers the case where Start() never runs.
	t.Cleanup(func() { store.Close() })

	d, err := daemon.New(daemon.Options{Store: store})
	if err != nil {
		if shouldSkipRuntimeError(err) {
			t.Skipf("skipping: %v", err)
		}
		t.Fatalf("create daemon: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	startErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		startErr <- d.Start()
	}()

	t.Cleanup(func() {
		d.Shutdown()
		if err := <-startErr; err != nil {
			if !shouldSkipRuntimeError(err) {
				t.Errorf("daemon start error: %v", err)
			}
		}
		wg.Wait()
	})

	waitForSocket(t, startErr, paths.Socket)

	logOutput := lb.String()

	// Manual install should NOT be refused.
	if strings.Contains(logOutput, "REFUSED") && strings.Contains(logOutput, "manual-plugin") {
		t.Errorf("manual-install plugin should NOT be refused, got:\n%s", logOutput)
	}

	// Should log the "no integrity checksums" message.
	if !strings.Contains(logOutput, "no integrity checksums") {
		t.Errorf("expected 'no integrity checksums' log for manual install, got:\n%s", logOutput)
	}

	// Verify the plugin was included in the index (not silently skipped).
	if !strings.Contains(logOutput, "1 valid") {
		t.Errorf("expected manual-install plugin to appear in index as valid, got:\n%s", logOutput)
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
