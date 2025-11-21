package adapters

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/adapterrunner"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

func TestProcessAdapterLaunchesFromManifestViaRunner(t *testing.T) {
	if testing.Short() {
		t.Skip("skip adapter-runner integration in short mode")
	}

	if ln, err := net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Skipf("tcp listen disallowed in this environment: %v", err)
	} else {
		_ = ln.Close()
	}

	ctx := context.Background()
	tempDir := t.TempDir()
	pluginDir := filepath.Join(tempDir, "plugins")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}

	runnerBinary := filepath.Join(tempDir, binaryName("adapter-runner-test"))
	buildAdapterRunnerBinary(t, runnerBinary)
	runnerRoot := filepath.Join(tempDir, "runner-root")
	runnerMgr := adapterrunner.NewManager(runnerRoot)
	if err := runnerMgr.InstallFromFile(runnerBinary); err != nil {
		t.Fatalf("install adapter-runner: %v", err)
	}

	adapterSlug := "plugin-stt-local-whisper"
	adapterBinaryName := binaryName("mock-stt")
	adapterHome := filepath.Join(pluginDir, adapterSlug)
	adapterBinDir := filepath.Join(adapterHome, "bin")
	if err := os.MkdirAll(adapterBinDir, 0o755); err != nil {
		t.Fatalf("mkdir adapter bin dir: %v", err)
	}
	adapterBinaryPath := filepath.Join(adapterBinDir, adapterBinaryName)
	buildMockAdapterBinary(t, adapterBinaryPath)
	if err := os.Chmod(adapterBinaryPath, 0o755); err != nil {
		t.Fatalf("chmod adapter binary: %v", err)
	}

	manifest := fmt.Sprintf(`
apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Mock Whisper STT
  slug: %s
spec:
  slot: stt
  entrypoint:
    command: %s
`, adapterSlug, filepath.ToSlash(filepath.Join(".", "bin", adapterBinaryName)))

	dbPath := filepath.Join(tempDir, "config.db")
	store, err := configstore.Open(configstore.Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	adapterID := "mock.local/stt"
	adapter := configstore.Adapter{
		ID:       adapterID,
		Source:   "local",
		Type:     "stt",
		Name:     "Mock Whisper STT",
		Manifest: manifest,
	}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	bindingCfg := map[string]any{"model": "base"}
	if err := store.SetActiveAdapter(ctx, string(SlotSTT), adapter.ID, bindingCfg); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Runner:    runnerMgr,
		PluginDir: pluginDir,
		Slots:     []Slot{SlotSTT},
	})

	bus := eventbus.New()
	svc := NewService(manager, store, bus, WithEnsureInterval(0))
	statusSub := bus.Subscribe(eventbus.TopicAdaptersStatus, eventbus.WithSubscriptionBuffer(16))
	defer statusSub.Close()
	logSub := bus.Subscribe(eventbus.TopicAdaptersLog, eventbus.WithSubscriptionBuffer(16))
	defer logSub.Close()

	if err := svc.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ready := waitForStatusEvent(t, statusSub, adapterID, eventbus.AdapterHealthReady)
	if ready.Extra == nil || strings.TrimSpace(ready.Extra[RuntimeExtraAddress]) == "" {
		t.Fatalf("expected runtime address in ready event: %+v", ready)
	}

	if evt, ok := waitForLogEvent(t, logSub, adapterID); ok {
		t.Logf("captured adapter log: %s", evt.Message)
	}

	if _, err := svc.StopSlot(ctx, SlotSTT); err != nil {
		t.Fatalf("stop slot: %v", err)
	}
	waitForStatusEvent(t, statusSub, adapterID, eventbus.AdapterHealthStopped)
}

func waitForStatusEvent(t *testing.T, sub *eventbus.Subscription, adapterID string, status eventbus.AdapterHealth) eventbus.AdapterStatusEvent {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case env := <-sub.C():
			if env.Payload == nil {
				continue
			}
			evt, ok := env.Payload.(eventbus.AdapterStatusEvent)
			if !ok {
				continue
			}
			if strings.TrimSpace(evt.AdapterID) == adapterID && evt.Status == status {
				return evt
			}
		case <-timeout:
			t.Fatalf("timeout waiting for adapter %s status %s", adapterID, status)
		}
	}
}

func waitForLogEvent(t *testing.T, sub *eventbus.Subscription, adapterID string) (eventbus.AdapterLogEvent, bool) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case env := <-sub.C():
			evt, ok := env.Payload.(eventbus.AdapterLogEvent)
			if !ok {
				continue
			}
			if strings.TrimSpace(evt.AdapterID) == adapterID {
				return evt, true
			}
		case <-timeout:
			return eventbus.AdapterLogEvent{}, false
		}
	}
}

func buildAdapterRunnerBinary(t *testing.T, output string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", output, "./cmd/adapter-runner")
	cmd.Dir = repoRoot(t)
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build adapter-runner: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

func buildMockAdapterBinary(t *testing.T, output string) {
	t.Helper()
	srcDir := t.TempDir()
	mainSrc := `package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	addr := os.Getenv("NUPI_ADAPTER_ENDPOINT")
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen error:", err)
		os.Exit(1)
	}
	fmt.Println("mock adapter ready on", addr)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	sig := <-sigCh
	fmt.Println("mock adapter shutting down", sig.String())
	_ = ln.Close()
}
`
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatalf("write adapter main: %v", err)
	}
	goMod := "module mockadapter\n\ngo 1.24\n"
	if err := os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write adapter go.mod: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", output, ".")
	cmd.Dir = srcDir
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mock adapter: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", "..", ".."))
}

func binaryName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}
