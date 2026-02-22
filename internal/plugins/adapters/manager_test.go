package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/napdial"
	manifestpkg "github.com/nupi-ai/nupi/internal/plugins/manifest"
)

type fakeBindingSource struct {
	mu        sync.Mutex
	bindings  []configstore.AdapterBinding
	adapters  map[string]configstore.Adapter
	endpoints map[string]configstore.AdapterEndpoint
	err       error
}

func (f *fakeBindingSource) ListAdapterBindings(context.Context) ([]configstore.AdapterBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]configstore.AdapterBinding, len(f.bindings))
	copy(out, f.bindings)
	return out, nil
}

func (f *fakeBindingSource) setAdapter(adapter configstore.Adapter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.adapters == nil {
		f.adapters = make(map[string]configstore.Adapter)
	}
	f.adapters[adapter.ID] = adapter
}

func (f *fakeBindingSource) setEndpoint(endpoint configstore.AdapterEndpoint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.endpoints == nil {
		f.endpoints = make(map[string]configstore.AdapterEndpoint)
	}
	f.endpoints[endpoint.AdapterID] = endpoint
}

func (f *fakeBindingSource) GetAdapter(ctx context.Context, adapterID string) (configstore.Adapter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	adapterID = strings.TrimSpace(adapterID)
	if adapterID == "" || f.adapters == nil {
		return configstore.Adapter{}, configstore.NotFoundError{Entity: "adapter", Key: adapterID}
	}
	adapter, ok := f.adapters[adapterID]
	if !ok {
		return configstore.Adapter{}, configstore.NotFoundError{Entity: "adapter", Key: adapterID}
	}
	copyAdapter := adapter
	return copyAdapter, nil
}

func (f *fakeBindingSource) GetAdapterEndpoint(ctx context.Context, adapterID string) (configstore.AdapterEndpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	adapterID = strings.TrimSpace(adapterID)
	if adapterID == "" || f.endpoints == nil {
		return configstore.AdapterEndpoint{}, configstore.NotFoundError{Entity: "adapter_endpoint", Key: adapterID}
	}
	endpoint, ok := f.endpoints[adapterID]
	if !ok {
		return configstore.AdapterEndpoint{}, configstore.NotFoundError{Entity: "adapter_endpoint", Key: adapterID}
	}
	copyEndpoint := endpoint
	return copyEndpoint, nil
}

func TestManagerEnsureStartsAdapters(t *testing.T) {
	// Mock readiness so remote adapters don't try to actually connect
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	// Test grpc transport - no process is launched, adapter is remote
	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotAI),
				Status:    "active",
				Config:    `{"api_key":"secret"}`,
				AdapterID: strPtr("adapter.ai"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.ai"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.ai",
		Transport: "grpc",
		Address:   "127.0.0.1:9100",
	})

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}

	// For grpc transport, no process should be launched - adapter is remote
	records := launcher.Records()
	if len(records) != 0 {
		t.Fatalf("expected 0 launch calls for grpc transport, got %d", len(records))
	}

	// But the adapter should be registered
	running := manager.Running()
	if len(running) != 1 {
		t.Fatalf("expected 1 running adapter, got %d", len(running))
	}
	if running[0].AdapterID != "adapter.ai" {
		t.Fatalf("unexpected adapter ID: %s", running[0].AdapterID)
	}
	if running[0].Runtime[RuntimeExtraTransport] != "grpc" {
		t.Fatalf("expected grpc transport in runtime, got %s", running[0].Runtime[RuntimeExtraTransport])
	}
	if running[0].Runtime[RuntimeExtraAddress] != "127.0.0.1:9100" {
		t.Fatalf("expected address in runtime, got %s", running[0].Runtime[RuntimeExtraAddress])
	}

	// Second Ensure should not change anything
	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure (second call) returned error: %v", err)
	}
	if len(launcher.Records()) != 0 {
		t.Fatalf("expected no launches on second call")
	}

	// Removing binding should remove the adapter
	store.mu.Lock()
	store.bindings = nil
	store.mu.Unlock()

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure after removing bindings returned error: %v", err)
	}

	running = manager.Running()
	if len(running) != 0 {
		t.Fatalf("expected 0 running adapters after removing binding, got %d", len(running))
	}
}

func TestManagerEnsureProcessTransportAllocFailure(t *testing.T) {
	t.Cleanup(SetAllocateProcessAddress(func() (string, error) {
		return "", errors.New("no ports")
	}))
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error {
		return nil
	}))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotAI),
				Status:    "active",
				AdapterID: strPtr("adapter.ai"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.ai"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.ai",
		Transport: "process",
		Command:   "serve",
	})

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	err := manager.Ensure(context.Background())
	if err == nil {
		t.Fatalf("expected ensure to fail due to port allocation error")
	}
	if !strings.Contains(err.Error(), "allocate process address") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(launcher.Records()) != 0 {
		t.Fatalf("expected no launches when allocation fails")
	}
}

func TestManagerEnsureProcessTransportReadyFailure(t *testing.T) {
	var portCounter int
	t.Cleanup(SetAllocateProcessAddress(func() (string, error) {
		portCounter++
		return fmt.Sprintf("127.0.0.1:%d", 60100+portCounter), nil
	}))
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error {
		return errors.New("dial failed")
	}))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotAI),
				Status:    "active",
				AdapterID: strPtr("adapter.ai"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.ai"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.ai",
		Transport: "process",
		Command:   "serve",
	})

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	err := manager.Ensure(context.Background())
	if err == nil {
		t.Fatalf("expected ensure to fail when adapter not ready")
	}
	if !strings.Contains(err.Error(), "readiness") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(launcher.Records()) != processLaunchMaxAttempts {
		t.Fatalf("expected %d launch attempts, got %d", processLaunchMaxAttempts, len(launcher.Records()))
	}
	if launcher.StopCount(string(SlotAI)) != processLaunchMaxAttempts {
		t.Fatalf("expected handle to be stopped after each readiness failure, got %d", launcher.StopCount(string(SlotAI)))
	}
}

func TestManagerEnsureProcessTransportReallocatesPortOnRestart(t *testing.T) {
	ports := []string{"127.0.0.1:60001", "127.0.0.1:60002"}
	var mu sync.Mutex
	t.Cleanup(SetAllocateProcessAddress(func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(ports) == 0 {
			return "", errors.New("no more ports")
		}
		addr := ports[0]
		ports = ports[1:]
		return addr, nil
	}))
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error {
		return nil
	}))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotAI),
				Status:    "active",
				AdapterID: strPtr("adapter.ai"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.ai"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.ai",
		Transport: "process",
		Command:   "serve",
	})

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("initial ensure: %v", err)
	}
	// Ensure() has returned — no concurrent access to instances.
	inst, ok := manager.instances[SlotAI]
	if !ok {
		t.Fatalf("adapter instance not registered")
	}
	firstAddr := inst.binding.Runtime[RuntimeExtraAddress]
	if firstAddr == "" {
		t.Fatalf("expected runtime address on first start")
	}

	store.mu.Lock()
	store.bindings[0].Config = `{"token":"rotated"}`
	store.mu.Unlock()

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("ensure after config change: %v", err)
	}

	inst, ok = manager.instances[SlotAI]
	if !ok {
		t.Fatalf("adapter instance missing after restart")
	}
	secondAddr := inst.binding.Runtime[RuntimeExtraAddress]
	if secondAddr == "" {
		t.Fatalf("expected runtime address on restart")
	}
	if secondAddr == firstAddr {
		t.Fatalf("expected new port allocation on restart; got %s", secondAddr)
	}
}

func TestManagerEnsureProcessTransportAllocatesFreshPortAfterStop(t *testing.T) {
	ports := []string{"127.0.0.1:60005", "127.0.0.1:60006"}
	var mu sync.Mutex
	t.Cleanup(SetAllocateProcessAddress(func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(ports) == 0 {
			return "", errors.New("no ports left")
		}
		addr := ports[0]
		ports = ports[1:]
		return addr, nil
	}))
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error {
		return nil
	}))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotAI),
				Status:    "active",
				AdapterID: strPtr("adapter.ai"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.ai"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.ai",
		Transport: "process",
		Command:   "serve",
	})

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	ctx := context.Background()
	if err := manager.Ensure(ctx); err != nil {
		t.Fatalf("initial ensure: %v", err)
	}

	// Ensure() has returned — no concurrent access to instances.
	inst, ok := manager.instances[SlotAI]
	if !ok {
		t.Fatalf("adapter instance missing")
	}
	firstAddr := inst.binding.Runtime[RuntimeExtraAddress]
	if firstAddr == "" {
		t.Fatalf("expected runtime address after first ensure")
	}

	if err := manager.StopSlot(ctx, SlotAI); err != nil {
		t.Fatalf("stop slot: %v", err)
	}

	if err := manager.Ensure(ctx); err != nil {
		t.Fatalf("ensure after stop: %v", err)
	}

	inst, ok = manager.instances[SlotAI]
	if !ok {
		t.Fatalf("adapter instance missing after restart")
	}
	secondAddr := inst.binding.Runtime[RuntimeExtraAddress]
	if secondAddr == "" {
		t.Fatalf("expected runtime address after restart")
	}
	if secondAddr == firstAddr {
		t.Fatalf("expected new port after restart; got %s", secondAddr)
	}
}

func TestManagerProcessTransportReadyTimeoutOverride(t *testing.T) {
	t.Cleanup(SetAllocateProcessAddress(func() (string, error) {
		return "127.0.0.1:60300", nil
	}))
	var capturedDeadline time.Time
	t.Cleanup(SetReadinessChecker(func(ctx context.Context, _, _ string, _ *napdial.TLSConfig) error {
		capturedDeadline, _ = ctx.Deadline()
		return nil
	}))

	storeDir := t.TempDir()
	store, err := configstore.Open(configstore.Options{DBPath: filepath.Join(storeDir, "config.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	manifest := `
apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Ready Adapter
spec:
  slot: ai
  mode: local
  entrypoint:
    command: sleep
    args: ["1"]
    transport: process
    readyTimeout: 2s
`
	if err := store.UpsertAdapter(context.Background(), configstore.Adapter{
		ID:       "adapter.ready",
		Source:   "test",
		Type:     "ai",
		Name:     "Ready Adapter",
		Manifest: manifest,
	}); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.SetActiveAdapter(context.Background(), string(SlotAI), "adapter.ready", nil); err != nil {
		t.Fatalf("bind adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(context.Background(), configstore.AdapterEndpoint{
		AdapterID: "adapter.ready",
		Transport: "process",
		Command:   "sleep",
		Args:      []string{"1"},
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: filepath.Join(storeDir, "plugins"),
	})

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if capturedDeadline.IsZero() {
		t.Fatalf("waitForAdapterReady was not called")
	}
	if d := time.Until(capturedDeadline); d < time.Second || d > 3*time.Second {
		t.Fatalf("expected ready timeout around 2s, got %v", d)
	}
}

func TestManagerEnsureUpdatesOnAdapterChange(t *testing.T) {
	// Mock readiness so process adapters don't try to actually connect
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotTTS),
				Status:    "active",
				AdapterID: strPtr("adapter.tts.v1"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.tts.v1"})
	store.setAdapter(configstore.Adapter{ID: "adapter.tts.v2"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.v1",
		Transport: "process",
		Command:   "./mock-adapter",
	})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.v2",
		Transport: "process",
		Command:   "./mock-adapter",
	})
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}

	store.mu.Lock()
	store.bindings[0].AdapterID = strPtr("adapter.tts.v2")
	store.mu.Unlock()

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure after adapter change returned error: %v", err)
	}

	records := launcher.Records()
	if len(records) != 2 {
		t.Fatalf("expected 2 launches (initial + restart), got %d", len(records))
	}
	if launcher.StopCount(string(SlotTTS)) != 1 {
		t.Fatalf("expected previous handle to be stopped on adapter change")
	}
}

func TestManagerEnsureConfigChange(t *testing.T) {
	// Mock readiness so process adapters don't try to actually connect
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotAI),
				Status:    "active",
				AdapterID: strPtr("adapter.ai"),
				Config:    `{"token":"first"}`,
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.ai"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.ai",
		Transport: "process",
		Command:   "./mock-adapter",
	})
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}

	store.mu.Lock()
	store.bindings[0].Config = `{"token":"second"}`
	store.mu.Unlock()

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure after config change returned error: %v", err)
	}

	records := launcher.Records()
	if len(records) != 2 {
		t.Fatalf("expected adapter to restart on config change, got %d launches", len(records))
	}
	if launcher.StopCount(string(SlotAI)) != 1 {
		t.Fatalf("expected previous handle to be stopped on config change")
	}
}

func TestManagerEnsureMissingStore(t *testing.T) {
	manager := NewManager(ManagerOptions{
		PluginDir: t.TempDir(),
	})
	err := manager.Ensure(context.Background())
	if !errors.Is(err, ErrBindingSourceNotConfigured) {
		t.Fatalf("expected ErrBindingSourceNotConfigured, got %v", err)
	}
}

func TestManagerStartAdapterConfiguresEnvironment(t *testing.T) {
	// Mock readiness so process adapters don't try to actually connect
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	ctx := context.Background()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "config.db")

	store, err := configstore.Open(configstore.Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
	})

	manifestYAML := `
apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Sample Adapter
  slug: Example Adapter
spec:
  slot: ai
  mode: local
  entrypoint:
    transport: process
    command: serve
    listenEnv: ADAPTER_LISTEN_ADDR
  options:
    token:
      type: string
      description: API token
  assets:
    models:
      cacheDirEnv: ADAPTER_CACHE_DIR
  telemetry:
    stdout: true
    stderr: false
`

	adapter := configstore.Adapter{
		ID:       "adapter.ai",
		Source:   "test",
		Type:     "ai",
		Name:     "Primary AI",
		Manifest: manifestYAML,
	}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(SlotAI), adapter.ID, map[string]any{"token": "abc"}); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}
	endpoint := configstore.AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "process",
		Command:   "serve",
		Args:      []string{"--foo"},
		Env: map[string]string{
			"CUSTOM_FLAG": "1",
		},
	}
	if err := store.UpsertAdapterEndpoint(ctx, endpoint); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	pluginDir := filepath.Join(tempDir, "plugins")
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: pluginDir,
	})

	if err := manager.Ensure(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	records := launcher.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 launch record, got %d", len(records))
	}
	record := records[0]

	envLookup := func(key string) (string, bool) {
		for _, entry := range record.Env {
			if !strings.HasPrefix(entry, key+"=") {
				continue
			}
			return strings.TrimPrefix(entry, key+"="), true
		}
		return "", false
	}

	if slot, ok := envLookup("NUPI_ADAPTER_SLOT"); !ok || slot != string(SlotAI) {
		t.Fatalf("expected NUPI_ADAPTER_SLOT=%s, got %q", SlotAI, slot)
	}
	if adapterID, ok := envLookup("NUPI_ADAPTER_ID"); !ok || adapterID != adapter.ID {
		t.Fatalf("expected NUPI_ADAPTER_ID=%s, got %q", adapter.ID, adapterID)
	}
	if cfg, ok := envLookup("NUPI_ADAPTER_CONFIG"); !ok || cfg != `{"token":"abc"}` {
		t.Fatalf("expected NUPI_ADAPTER_CONFIG, got %q", cfg)
	}
	home, ok := envLookup("NUPI_ADAPTER_HOME")
	if !ok {
		t.Fatalf("missing NUPI_ADAPTER_HOME in env")
	}
	expectedDir := filepath.Join(pluginDir, "example-adapter")
	if home != expectedDir {
		t.Fatalf("unexpected adapter home: %s (expected %s)", home, expectedDir)
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		t.Fatalf("adapter home directory not created: %v", err)
	}
	if manifestPath, ok := envLookup("NUPI_ADAPTER_MANIFEST_PATH"); !ok || manifestPath == "" {
		t.Fatalf("missing NUPI_ADAPTER_MANIFEST_PATH")
	} else {
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		if strings.TrimSpace(string(data)) != strings.TrimSpace(manifestYAML) {
			t.Fatalf("manifest contents mismatch")
		}
	}

	dataDir, ok := envLookup("NUPI_ADAPTER_DATA_DIR")
	if !ok {
		t.Fatalf("missing NUPI_ADAPTER_DATA_DIR")
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("data dir not created: %v", err)
	}
	if cacheDir, ok := envLookup("ADAPTER_CACHE_DIR"); !ok || cacheDir != dataDir {
		t.Fatalf("expected ADAPTER_CACHE_DIR=%s, got %q", dataDir, cacheDir)
	}
	if transport, ok := envLookup("NUPI_ADAPTER_TRANSPORT"); !ok || transport != endpoint.Transport {
		t.Fatalf("expected transport %s, got %q", endpoint.Transport, transport)
	}
	// For process transport, address is dynamically allocated
	allocatedAddr, ok := envLookup("NUPI_ADAPTER_ENDPOINT")
	if !ok || allocatedAddr == "" {
		t.Fatalf("expected dynamically allocated adapter endpoint, got %q", allocatedAddr)
	}
	if !strings.HasPrefix(allocatedAddr, "127.0.0.1:") {
		t.Fatalf("expected localhost address, got %q", allocatedAddr)
	}
	if listenAddr, ok := envLookup("ADAPTER_LISTEN_ADDR"); !ok || listenAddr != allocatedAddr {
		t.Fatalf("expected ADAPTER_LISTEN_ADDR=%s, got %q", allocatedAddr, listenAddr)
	}
	if cmd, ok := envLookup("NUPI_ADAPTER_COMMAND"); !ok || cmd != endpoint.Command {
		t.Fatalf("expected command %s, got %q", endpoint.Command, cmd)
	}
	if args, ok := envLookup("NUPI_ADAPTER_ARGS"); !ok || args != `["--foo"]` {
		t.Fatalf("expected args [\"--foo\"], got %q", args)
	}
	if customEnv, ok := envLookup("CUSTOM_FLAG"); !ok || customEnv != "1" {
		t.Fatalf("expected CUSTOM_FLAG=1, got %q", customEnv)
	}
}

func TestManagerEnsureRestartsOnEndpointChange(t *testing.T) {
	// Mock readiness so process adapters don't try to actually connect
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	ctx := context.Background()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "config.db")

	store, err := configstore.Open(configstore.Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
	})

	adapter := configstore.Adapter{
		ID:     "adapter.ai",
		Source: "builtin",
		Type:   "ai",
		Name:   "Primary AI",
	}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(SlotAI), adapter.ID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "process",
		Command:   "./mock-adapter",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}
	if endpoint, err := store.GetAdapterEndpoint(ctx, adapter.ID); err != nil {
		t.Fatalf("verify endpoint: %v", err)
	} else if !strings.EqualFold(endpoint.Transport, "process") {
		t.Fatalf("expected process transport, got %s", endpoint.Transport)
	}

	initialEndpoint := configstore.AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "process",
		Command:   "./mock-adapter-v1",
	}
	if err := store.UpsertAdapterEndpoint(ctx, initialEndpoint); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: filepath.Join(tempDir, "plugins"),
	})

	if err := manager.Ensure(ctx); err != nil {
		t.Fatalf("initial ensure: %v", err)
	}
	if len(launcher.Records()) != 1 {
		t.Fatalf("expected initial launch, got %d", len(launcher.Records()))
	}

	updatedEndpoint := configstore.AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "process",
		Command:   "./mock-adapter-v2",
	}
	if err := store.UpsertAdapterEndpoint(ctx, updatedEndpoint); err != nil {
		t.Fatalf("update endpoint: %v", err)
	}

	if err := manager.Ensure(ctx); err != nil {
		t.Fatalf("ensure after endpoint change: %v", err)
	}
	if len(launcher.Records()) != 2 {
		t.Fatalf("expected relaunch after endpoint change, got %d", len(launcher.Records()))
	}
	if launcher.StopCount(string(SlotAI)) != 1 {
		t.Fatalf("expected adapter stop before restart")
	}
}

func TestManagerRemoteGRPCPassesTransportAndTLS(t *testing.T) {
	// Verify that the remote gRPC adapter call site passes the correct
	// transport type ("grpc") and TLS config to the readiness checker.
	var capturedTransport string
	var capturedTLS *napdial.TLSConfig
	t.Cleanup(SetReadinessChecker(func(_ context.Context, _ string, transport string, tlsCfg *napdial.TLSConfig) error {
		capturedTransport = transport
		capturedTLS = tlsCfg
		return nil
	}))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotAI),
				Status:    "active",
				AdapterID: strPtr("adapter.ai.remote"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.ai.remote"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID:   "adapter.ai.remote",
		Transport:   "grpc",
		Address:     "10.0.0.1:50051",
		TLSCertPath: "/certs/client.pem",
		TLSKeyPath:  "/certs/client-key.pem",
		TLSInsecure: true,
	})
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  NewMockLauncher(),
		PluginDir: t.TempDir(),
	})

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if capturedTransport != "grpc" {
		t.Fatalf("expected transport %q, got %q", "grpc", capturedTransport)
	}
	if capturedTLS == nil {
		t.Fatal("expected non-nil TLS config for remote gRPC with TLS fields")
	}
	if capturedTLS.CertPath != "/certs/client.pem" {
		t.Fatalf("expected CertPath %q, got %q", "/certs/client.pem", capturedTLS.CertPath)
	}
	if capturedTLS.KeyPath != "/certs/client-key.pem" {
		t.Fatalf("expected KeyPath %q, got %q", "/certs/client-key.pem", capturedTLS.KeyPath)
	}
	if !capturedTLS.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify=true")
	}
}

func TestManagerRemoteHTTPPassesTransportAndTLS(t *testing.T) {
	// Verify that the remote HTTP adapter call site passes the correct
	// transport type ("http") and TLS config to the readiness checker.
	var capturedTransport string
	var capturedTLS *napdial.TLSConfig
	t.Cleanup(SetReadinessChecker(func(_ context.Context, _ string, transport string, tlsCfg *napdial.TLSConfig) error {
		capturedTransport = transport
		capturedTLS = tlsCfg
		return nil
	}))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotTTS),
				Status:    "active",
				AdapterID: strPtr("adapter.tts.remote"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.tts.remote"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID:    "adapter.tts.remote",
		Transport:    "http",
		Address:      "10.0.0.2:8080",
		TLSCACertPath: "/certs/ca.pem",
	})
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  NewMockLauncher(),
		PluginDir: t.TempDir(),
	})

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if capturedTransport != "http" {
		t.Fatalf("expected transport %q, got %q", "http", capturedTransport)
	}
	if capturedTLS == nil {
		t.Fatal("expected non-nil TLS config for remote HTTP with CA cert")
	}
	if capturedTLS.CACertPath != "/certs/ca.pem" {
		t.Fatalf("expected CACertPath %q, got %q", "/certs/ca.pem", capturedTLS.CACertPath)
	}
}

func TestManagerRemoteNoTLSPassesNilConfig(t *testing.T) {
	// Verify that a remote adapter without any TLS fields passes nil TLS config.
	var capturedTransport string
	var capturedTLS *napdial.TLSConfig
	capturedTLS = &napdial.TLSConfig{} // sentinel to detect nil
	t.Cleanup(SetReadinessChecker(func(_ context.Context, _ string, transport string, tlsCfg *napdial.TLSConfig) error {
		capturedTransport = transport
		capturedTLS = tlsCfg
		return nil
	}))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotAI),
				Status:    "active",
				AdapterID: strPtr("adapter.ai.plain"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.ai.plain"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.ai.plain",
		Transport: "grpc",
		Address:   "10.0.0.3:50051",
		// No TLS fields
	})
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  NewMockLauncher(),
		PluginDir: t.TempDir(),
	})

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if capturedTransport != "grpc" {
		t.Fatalf("expected transport %q, got %q", "grpc", capturedTransport)
	}
	if capturedTLS != nil {
		t.Fatalf("expected nil TLS config for remote adapter without TLS fields, got %+v", capturedTLS)
	}
}

func TestManagerProcessPassesTransportAndNilTLS(t *testing.T) {
	// Verify that the process adapter call site passes transport="process"
	// and nil TLS config to the readiness checker (symmetric with remote tests).
	t.Cleanup(SetAllocateProcessAddress(func() (string, error) {
		return "127.0.0.1:60999", nil
	}))

	var capturedTransport string
	var capturedTLS *napdial.TLSConfig
	capturedTLS = &napdial.TLSConfig{} // sentinel to detect nil
	t.Cleanup(SetReadinessChecker(func(_ context.Context, _ string, transport string, tlsCfg *napdial.TLSConfig) error {
		capturedTransport = transport
		capturedTLS = tlsCfg
		return nil
	}))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotAI),
				Status:    "active",
				AdapterID: strPtr("adapter.ai.process"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.ai.process"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.ai.process",
		Transport: "process",
		Command:   "serve",
	})

	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  NewMockLauncher(),
		PluginDir: t.TempDir(),
	})

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if capturedTransport != "process" {
		t.Fatalf("expected transport %q, got %q", "process", capturedTransport)
	}
	if capturedTLS != nil {
		t.Fatalf("expected nil TLS config for process transport, got %+v", capturedTLS)
	}
}

func TestManagerEnsureRestartFailureRemovesStaleInstance(t *testing.T) {
	// When a restart's start phase fails, the stopped instance must be
	// removed from the map so Running() does not return a stale entry.
	var ensureCount int
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error {
		ensureCount++
		if ensureCount >= 2 {
			return errors.New("health check: NOT_SERVING")
		}
		return nil
	}))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotAI),
				Status:    "active",
				AdapterID: strPtr("adapter.ai"),
				Config:    `{"v":1}`,
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.ai"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.ai",
		Transport: "grpc",
		Address:   "127.0.0.1:9400",
	})
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	ctx := context.Background()

	// First Ensure — adapter starts successfully.
	if err := manager.Ensure(ctx); err != nil {
		t.Fatalf("initial ensure: %v", err)
	}
	if len(manager.Running()) != 1 {
		t.Fatalf("expected 1 running adapter, got %d", len(manager.Running()))
	}

	// Change config to trigger restart path.
	store.mu.Lock()
	store.bindings[0].Config = `{"v":2}`
	store.mu.Unlock()

	// Second Ensure — stop succeeds but start fails (readiness returns error).
	err := manager.Ensure(ctx)
	if err == nil {
		t.Fatal("expected error from failed restart, got nil")
	}

	// The stale instance must NOT appear in Running().
	if len(manager.Running()) != 0 {
		t.Fatalf("expected 0 running adapters after failed restart, got %d", len(manager.Running()))
	}
}

func TestManagerEnsureRemovalKeepsInstanceOnStopFailure(t *testing.T) {
	// When removing an adapter whose stop fails, the instance should be
	// retained in the map so the next reconcile retries the stop.
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotAI),
				Status:    "active",
				AdapterID: strPtr("adapter.ai.remote"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.ai.remote"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.ai.remote",
		Transport: "grpc",
		Address:   "127.0.0.1:9100",
	})
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	ctx := context.Background()

	// First Ensure — adapter starts (remote gRPC, no process).
	if err := manager.Ensure(ctx); err != nil {
		t.Fatalf("initial ensure: %v", err)
	}
	if len(manager.Running()) != 1 {
		t.Fatalf("expected 1 running adapter, got %d", len(manager.Running()))
	}

	// Remove the binding so the removal path triggers.
	store.mu.Lock()
	store.bindings = nil
	store.mu.Unlock()

	// Inject a handle that fails on stop to simulate stop failure.
	manager.mu.Lock()
	instance := manager.instances[SlotAI]
	instance.handle = &mockHandle{parent: launcher, slot: string(SlotAI), pid: 1, stopErr: errors.New("stop failed")}
	manager.mu.Unlock()

	// Second Ensure — stop fails, instance should be retained for retry.
	err := manager.Ensure(ctx)
	if err == nil {
		t.Fatal("expected error from failed stop, got nil")
	}
	if len(manager.Running()) != 1 {
		t.Fatal("expected instance to be retained after stop failure for retry")
	}
}

func TestManagerEnsurePrepareFailurePublishesPerSlotEvent(t *testing.T) {
	// When all bindings fail in prepareBinding (e.g. malformed manifest),
	// logSlotError should publish per-slot events before the early return
	// (review #10 regression test).
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotAI),
				Status:    "active",
				AdapterID: strPtr("adapter.ai.badmanifest"),
			},
		},
	}
	// Set adapter with a malformed manifest — prepareBinding will fail
	// on manifest.Parse, before reaching startAdapter.
	store.setAdapter(configstore.Adapter{
		ID:       "adapter.ai.badmanifest",
		Manifest: "{{not valid yaml or json}}",
	})
	bus := eventbus.New()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  NewMockLauncher(),
		Bus:       bus,
		PluginDir: t.TempDir(),
	})

	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status)
	defer sub.Close()

	err := manager.Ensure(context.Background())
	if err == nil {
		t.Fatal("expected error from prepare failure, got nil")
	}
	if !strings.Contains(err.Error(), "parse manifest") {
		t.Fatalf("expected manifest parse error, got: %v", err)
	}

	select {
	case evt := <-sub.C():
		status := evt.Payload
		if status.Status != eventbus.AdapterHealthError {
			t.Fatalf("expected error status, got %s", status.Status)
		}
		if status.AdapterID != "adapter.ai.badmanifest" {
			t.Fatalf("expected AdapterID adapter.ai.badmanifest, got %q", status.AdapterID)
		}
		if status.Slot != string(SlotAI) {
			t.Fatalf("expected Slot %s, got %q", SlotAI, status.Slot)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for prepare-failure event")
	}
}

func TestManagerEnsureRestartsOnManifestChange(t *testing.T) {
	// Mock readiness so process adapters don't try to actually connect
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	ctx := context.Background()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "config.db")

	store, err := configstore.Open(configstore.Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
	})

	manifestV1 := `
apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Primary AI
spec:
  slot: ai
  mode: local
  entrypoint:
    transport: process
    command: ./mock-adapter
`
	adapter := configstore.Adapter{
		ID:       "adapter.ai",
		Source:   "builtin",
		Type:     "ai",
		Name:     "Primary AI",
		Manifest: manifestV1,
	}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(SlotAI), adapter.ID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "process",
		Command:   "./mock-adapter",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}
	if endpoint, err := store.GetAdapterEndpoint(ctx, adapter.ID); err != nil {
		t.Fatalf("verify endpoint: %v", err)
	} else if !strings.EqualFold(endpoint.Transport, "process") {
		t.Fatalf("expected process transport, got %s", endpoint.Transport)
	}

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: filepath.Join(tempDir, "plugins"),
	})

	if err := manager.Ensure(ctx); err != nil {
		t.Fatalf("initial ensure: %v", err)
	}
	if len(launcher.Records()) != 1 {
		t.Fatalf("expected initial launch, got %d", len(launcher.Records()))
	}

	manifestV2 := `
apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Primary AI
  version: v2
spec:
  slot: ai
  mode: local
  entrypoint:
    transport: process
    command: ./mock-adapter
  telemetry:
    stdout: true
`
	adapter.Manifest = manifestV2
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("update adapter manifest: %v", err)
	}

	if err := manager.Ensure(ctx); err != nil {
		t.Fatalf("ensure after manifest change: %v", err)
	}
	if len(launcher.Records()) != 2 {
		t.Fatalf("expected relaunch after manifest change, got %d", len(launcher.Records()))
	}
	if launcher.StopCount(string(SlotAI)) != 1 {
		t.Fatalf("expected adapter stop before restart")
	}
}

func TestEnsureFailsOnInvalidConfig(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()

	source := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotSTT),
				AdapterID: strPtr("ai.nupi/stt-local-whisper"),
				Config:    `{"threads":"abc"}`,
				Status:    configstore.BindingStatusActive,
			},
		},
	}

	manifest := `
apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Whisper STT
  slug: stt-local-whisper
  namespace: ai.nupi
spec:
  slot: stt
  mode: local
  entrypoint:
    command: ./adapter
  options:
    threads:
      type: integer
`
	source.setAdapter(configstore.Adapter{
		ID:       "ai.nupi/stt-local-whisper",
		Manifest: manifest,
	})

	manager := NewManager(ManagerOptions{
		Store:     source,
		Adapters:  source,
		Launcher:  NewMockLauncher(),
		PluginDir: filepath.Join(tempDir, "plugins"),
	})

	if err := manager.Ensure(ctx); err == nil {
		t.Fatalf("expected Ensure to fail when config cannot be coerced")
	}
}

func TestEnsurePopulatesAdapterConfigEnv(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "config.db")

	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error {
		return nil
	}))

	var portCounter int
	t.Cleanup(SetAllocateProcessAddress(func() (string, error) {
		portCounter++
		return fmt.Sprintf("127.0.0.1:%d", 55050+portCounter), nil
	}))

	store, err := configstore.Open(configstore.Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
	})

	manifest := `
apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Whisper STT
  slug: stt-local-whisper
  namespace: ai.nupi
spec:
  slot: stt
  mode: local
  entrypoint:
    command: ./adapter
    transport: process
  options:
    use_gpu:
      type: boolean
      default: true
    threads:
      type: integer
      default: 4
    voice:
      type: enum
      values: [en-US, pl-PL]
      default: en-US
    accuracy:
      type: number
      default: 0.5
`

	adapterID := "ai.nupi/stt-local-whisper"
	adapter := configstore.Adapter{
		ID:       adapterID,
		Source:   "test",
		Type:     "stt",
		Name:     "Whisper STT",
		Manifest: manifest,
	}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	userConfig := map[string]any{
		"use_gpu": "false",
		"threads": "6",
		"voice":   "pl-PL",
	}
	if err := store.SetActiveAdapter(ctx, string(SlotSTT), adapter.ID, userConfig); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "process",
		Command:   "./adapter-bin",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: filepath.Join(tempDir, "plugins"),
	})

	if err := manager.Ensure(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	records := launcher.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 launch record, got %d", len(records))
	}

	var cfgJSON string
	for _, env := range records[0].Env {
		if strings.HasPrefix(env, "NUPI_ADAPTER_CONFIG=") {
			cfgJSON = strings.TrimPrefix(env, "NUPI_ADAPTER_CONFIG=")
			break
		}
	}
	if cfgJSON == "" {
		t.Fatalf("missing NUPI_ADAPTER_CONFIG in launch environment")
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(cfgJSON), &payload); err != nil {
		t.Fatalf("unmarshal adapter config: %v", err)
	}

	if val, ok := payload["use_gpu"].(bool); !ok || val {
		t.Fatalf("expected use_gpu=false, got %#v", payload["use_gpu"])
	}
	if val, ok := payload["threads"].(float64); !ok || val != 6 {
		t.Fatalf("expected threads=6, got %#v", payload["threads"])
	}
	if val, ok := payload["voice"].(string); !ok || val != "pl-PL" {
		t.Fatalf("expected voice=pl-PL, got %#v", payload["voice"])
	}
	if val, ok := payload["accuracy"].(float64); !ok || val != 0.5 {
		t.Fatalf("expected accuracy=0.5, got %#v", payload["accuracy"])
	}

	if len(payload) != 4 {
		t.Fatalf("expected 4 keys in payload, got %d: %v", len(payload), payload)
	}
}

func TestResolveAdapterConfig(t *testing.T) {
	t.Run("no options rejects non-empty config", func(t *testing.T) {
		// Empty config with no options is valid
		result, err := resolveAdapterConfig(nil, nil)
		if err != nil {
			t.Fatalf("expected nil result without error, got %v", err)
		}
		if result != nil {
			t.Fatalf("expected nil map for empty input, got %#v", result)
		}

		// Non-empty config with no options should be rejected (manifest as single source of truth)
		current := map[string]any{
			"api_key": "secret",
		}
		_, err = resolveAdapterConfig(nil, current)
		if err == nil {
			t.Fatal("expected error for non-empty config when manifest declares no options")
		}
		if !strings.Contains(err.Error(), "declares no options") {
			t.Errorf("expected 'declares no options' error, got: %v", err)
		}
	})

	t.Run("applies defaults and coerces values", func(t *testing.T) {
		opts := map[string]manifestpkg.AdapterOption{
			"use_gpu":  {Type: "boolean", Default: true},
			"threads":  {Type: "integer", Default: 4},
			"voice":    {Type: "enum", Values: []any{"en-US", "pl-PL"}, Default: "en-US"},
			"model":    {Type: "string", Default: "base"},
			"accuracy": {Type: "number", Default: 0.5},
		}
		current := map[string]any{
			"use_gpu":  "false",
			"threads":  "6",
			"voice":    "pl-PL",
			"accuracy": "0.75",
		}

		result, err := resolveAdapterConfig(opts, current)
		if err != nil {
			t.Fatalf("resolveAdapterConfig returned error: %v", err)
		}

		if val, ok := result["use_gpu"].(bool); !ok || val {
			t.Fatalf("expected coerced boolean false, got %#v", result["use_gpu"])
		}
		if val, ok := result["threads"].(int); !ok || val != 6 {
			t.Fatalf("expected coerced integer 6, got %#v", result["threads"])
		}
		if val, ok := result["accuracy"].(float64); !ok || val != 0.75 {
			t.Fatalf("expected coerced number 0.75, got %#v", result["accuracy"])
		}
		if val, ok := result["voice"].(string); !ok || val != "pl-PL" {
			t.Fatalf("expected enum selection pl-PL, got %#v", result["voice"])
		}
		if val, ok := result["model"].(string); !ok || val != "base" {
			t.Fatalf("expected default string base, got %#v", result["model"])
		}
	})

	t.Run("rejects invalid values", func(t *testing.T) {
		opts := map[string]manifestpkg.AdapterOption{
			"use_gpu": {Type: "boolean", Default: true},
			"threads": {Type: "integer"},
		}
		badBool := map[string]any{"use_gpu": "maybe"}
		if _, err := resolveAdapterConfig(opts, badBool); err == nil {
			t.Fatalf("expected error for invalid boolean")
		}

		badInt := map[string]any{"threads": "abc"}
		if _, err := resolveAdapterConfig(opts, badInt); err == nil {
			t.Fatalf("expected error for invalid integer")
		}
	})

	t.Run("nil user value removes default", func(t *testing.T) {
		opts := map[string]manifestpkg.AdapterOption{
			"use_gpu": {Type: "boolean", Default: true},
		}
		current := map[string]any{"use_gpu": nil}
		result, err := resolveAdapterConfig(opts, current)
		if err != nil {
			t.Fatalf("resolveAdapterConfig returned error: %v", err)
		}
		if _, exists := result["use_gpu"]; exists {
			t.Fatalf("expected nil value to drop option from result")
		}
	})

	t.Run("rejects unknown keys", func(t *testing.T) {
		opts := map[string]manifestpkg.AdapterOption{
			"use_gpu": {Type: "boolean", Default: true},
		}
		current := map[string]any{"custom": "value"}
		_, err := resolveAdapterConfig(opts, current)
		if err == nil {
			t.Fatalf("expected error for unknown option, got success")
		}
		if !strings.Contains(err.Error(), "unknown option") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})
}

func TestMergeManifestEndpointDefaultsMissingValues(t *testing.T) {
	manifest := &manifestpkg.Manifest{
		Adapter: &manifestpkg.AdapterSpec{
			Entrypoint: manifestpkg.AdapterEntrypoint{
				Command:   "./bin/mock",
				Args:      []string{"--foo", "bar"},
				Transport: "process",
			},
		},
	}
	endpoint, err := mergeManifestEndpoint(manifest, "adapter.test", configstore.AdapterEndpoint{})
	if err != nil {
		t.Fatalf("merge should succeed: %v", err)
	}
	if endpoint.Transport != "process" {
		t.Fatalf("expected transport process, got %q", endpoint.Transport)
	}
	if endpoint.Command != "./bin/mock" {
		t.Fatalf("expected command from manifest, got %q", endpoint.Command)
	}
	if len(endpoint.Args) != 2 || endpoint.Args[0] != "--foo" || endpoint.Args[1] != "bar" {
		t.Fatalf("expected args from manifest, got %v", endpoint.Args)
	}
}

func TestMergeManifestEndpointRejectsTransportMismatch(t *testing.T) {
	manifest := &manifestpkg.Manifest{
		Adapter: &manifestpkg.AdapterSpec{
			Entrypoint: manifestpkg.AdapterEntrypoint{
				Transport: "grpc",
				Command:   "./bin/mock",
			},
		},
	}
	_, err := mergeManifestEndpoint(manifest, "adapter.test", configstore.AdapterEndpoint{
		Transport: "process",
		Command:   "./bin/override",
	})
	if err == nil {
		t.Fatalf("expected transport mismatch error")
	}
}

func TestMergeManifestEndpointRequiresAddressForNetworkTransports(t *testing.T) {
	manifest := &manifestpkg.Manifest{
		Adapter: &manifestpkg.AdapterSpec{
			Entrypoint: manifestpkg.AdapterEntrypoint{
				Transport: "grpc",
			},
		},
	}
	if _, err := mergeManifestEndpoint(manifest, "adapter.test", configstore.AdapterEndpoint{}); err == nil {
		t.Fatalf("expected address requirement error for grpc transport")
	}

	manifestHTTP := &manifestpkg.Manifest{
		Adapter: &manifestpkg.AdapterSpec{
			Entrypoint: manifestpkg.AdapterEntrypoint{},
		},
	}
	endpoint := configstore.AdapterEndpoint{
		Transport: "http",
		Address:   "127.0.0.1:7000",
	}
	if merged, err := mergeManifestEndpoint(manifestHTTP, "adapter.test", endpoint); err != nil {
		t.Fatalf("expected merge to succeed: %v", err)
	} else if merged.Transport != "http" || merged.Address != endpoint.Address {
		t.Fatalf("unexpected merged endpoint: %+v", merged)
	}
}

func TestManagerStopStopsEveryInstance(t *testing.T) {
	manager := NewManager(ManagerOptions{
		Launcher: NewMockLauncher(),
	})
	manager.instances = map[Slot]*adapterInstance{
		SlotAI: {
			binding: Binding{Slot: SlotAI},
			handle:  &mockHandle{slot: string(SlotAI), parent: manager.launcher.(*MockLauncher)},
		},
		SlotSTT: {
			binding: Binding{Slot: SlotSTT},
			handle:  &mockHandle{slot: string(SlotSTT), parent: manager.launcher.(*MockLauncher)},
		},
	}

	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if len(manager.instances) != 0 {
		t.Fatalf("expected instances to be cleared, got %d", len(manager.instances))
	}
	launcher := manager.launcher.(*MockLauncher)
	if launcher.StopCount(string(SlotAI)) != 1 || launcher.StopCount(string(SlotSTT)) != 1 {
		t.Fatalf("expected Stop called for both slots, got ai=%d stt=%d", launcher.StopCount(string(SlotAI)), launcher.StopCount(string(SlotSTT)))
	}
}

func TestStartAdapterRejectsInvalidEnvVars(t *testing.T) {
	ctx := context.Background()

	t.Cleanup(SetAllocateProcessAddress(func() (string, error) {
		return "127.0.0.1:55555", nil
	}))

	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error {
		return nil
	}))

	manifestRaw := `
apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Mock AI
spec:
  slot: ai
  mode: local
  entrypoint:
    command: ./bin/mock
    transport: process
`
	manifest, err := manifestpkg.Parse([]byte(manifestRaw))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	manager := NewManager(ManagerOptions{
		Launcher:  NewMockLauncher(),
		PluginDir: t.TempDir(),
	})

	badPlans := []struct {
		name     string
		endpoint configstore.AdapterEndpoint
	}{
		{
			name: "newline in key",
			endpoint: configstore.AdapterEndpoint{
				Transport: "process",
				Command:   "./bin/mock",
				Env: map[string]string{
					"BAD\nKEY": "value",
				},
			},
		},
		{
			name: "newline in value",
			endpoint: configstore.AdapterEndpoint{
				Transport: "process",
				Command:   "./bin/mock",
				Env: map[string]string{
					"GOOD_KEY": "bad\nvalue",
				},
			},
		},
		{
			name: "null byte in value",
			endpoint: configstore.AdapterEndpoint{
				Transport: "process",
				Command:   "./bin/mock",
				Env: map[string]string{
					"GOOD_KEY": "bad\x00value",
				},
			},
		},
		{
			name: "equals in key",
			endpoint: configstore.AdapterEndpoint{
				Transport: "process",
				Command:   "./bin/mock",
				Env: map[string]string{
					"BAD=KEY": "value",
				},
			},
		},
	}

	for _, tc := range badPlans {
		t.Run(tc.name, func(t *testing.T) {
			plan := bindingPlan{
				binding: Binding{
					Slot:      SlotAI,
					AdapterID: "adapter.ai",
				},
				adapter: configstore.Adapter{
					ID:       "adapter.ai",
					Manifest: manifestRaw,
				},
				manifest: manifest,
				endpoint: tc.endpoint,
			}
			if _, err := manager.startAdapter(ctx, plan); err == nil {
				t.Fatalf("expected startAdapter to fail for %s", tc.name)
			}
		})
	}
}

func TestStartAdapterRetriesOnReadinessFailure(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()

	var portCounter int
	t.Cleanup(SetAllocateProcessAddress(func() (string, error) {
		portCounter++
		return fmt.Sprintf("127.0.0.1:%d", 55000+portCounter), nil
	}))

	var readyCalls int
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error {
		readyCalls++
		if readyCalls == 1 {
			return fmt.Errorf("not ready yet")
		}
		return nil
	}))

	manifestRaw := `
apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Mock AI
spec:
  slot: ai
  mode: local
  entrypoint:
    command: ./bin/mock
    transport: process
`
	manifest, err := manifestpkg.Parse([]byte(manifestRaw))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	manager := NewManager(ManagerOptions{
		Launcher:  NewMockLauncher(),
		PluginDir: filepath.Join(tempDir, "plugins"),
	})

	plan := bindingPlan{
		binding: Binding{
			Slot:      SlotAI,
			AdapterID: "adapter.ai",
		},
		adapter: configstore.Adapter{
			ID:       "adapter.ai",
			Manifest: manifestRaw,
		},
		manifest: manifest,
		endpoint: configstore.AdapterEndpoint{
			Transport: "process",
			Command:   "./bin/mock",
		},
	}

	inst, err := manager.startAdapter(ctx, plan)
	if err != nil {
		t.Fatalf("startAdapter returned error: %v", err)
	}
	if readyCalls != 2 {
		t.Fatalf("expected 2 readiness checks (with retry), got %d", readyCalls)
	}
	if portCounter != 2 {
		t.Fatalf("expected 2 port allocations, got %d", portCounter)
	}
	if inst.binding.Runtime[RuntimeExtraAddress] != "127.0.0.1:55002" {
		t.Fatalf("expected runtime address to use last allocation, got %q", inst.binding.Runtime[RuntimeExtraAddress])
	}
	launcher := manager.launcher.(*MockLauncher)
	if len(launcher.Records()) != 2 {
		t.Fatalf("expected 2 launch attempts, got %d", len(launcher.Records()))
	}
	if launcher.StopCount(string(SlotAI)) != 1 {
		t.Fatalf("expected one stop after failed readiness, got %d", launcher.StopCount(string(SlotAI)))
	}
}

func TestStartAdapterCleansUpOnLaunchFailure(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()

	t.Cleanup(SetAllocateProcessAddress(func() (string, error) {
		return "127.0.0.1:59000", nil
	}))

	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error {
		return nil
	}))

	manifestRaw := `
apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: AdapterClean
spec:
  slot: ai
  mode: local
  entrypoint:
    command: ./bin/mock
    transport: process
`
	manifest, err := manifestpkg.Parse([]byte(manifestRaw))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	launcher := NewMockLauncher()
	launcher.SetError(fmt.Errorf("failed to launch"))

	pluginDir := filepath.Join(tempDir, "plugins")
	manager := NewManager(ManagerOptions{
		Launcher:  launcher,
		PluginDir: pluginDir,
	})

	plan := bindingPlan{
		binding: Binding{
			Slot:      SlotAI,
			AdapterID: "adapter.clean",
		},
		adapter: configstore.Adapter{
			ID:       "adapter.clean",
			Manifest: manifestRaw,
		},
		manifest: manifest,
		endpoint: configstore.AdapterEndpoint{
			Transport: "process",
			Command:   "./bin/mock",
		},
	}

	if _, err := manager.startAdapter(ctx, plan); err == nil {
		t.Fatalf("expected startAdapter to fail")
	}
	adapterHome := filepath.Join(pluginDir, "adapterclean")
	if _, err := os.Stat(adapterHome); !os.IsNotExist(err) {
		t.Fatalf("expected adapter home cleaned up, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(adapterHome, "data")); !os.IsNotExist(err) {
		t.Fatalf("expected data dir cleaned up, got err=%v", err)
	}
}

func TestResolveAdapterConfigRejectsUnknownOptions(t *testing.T) {
	options := map[string]manifestpkg.AdapterOption{
		"known_option": {Type: "string", Default: "default"},
	}

	config := map[string]any{
		"known_option":   "value",
		"unknown_option": "should_fail",
	}

	_, err := resolveAdapterConfig(options, config)
	if err == nil {
		t.Fatalf("expected error for unknown option")
	}
	if !strings.Contains(err.Error(), "unknown option") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func strPtr(v string) *string {
	return &v
}

func TestManagerEnsureBuiltinMockAdapterNoRunner(t *testing.T) {
	// Test that builtin mock adapters can be started without runner manager
	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotAI),
				Status:    "active",
				AdapterID: strPtr(MockAIAdapterID),
			},
		},
	}
	store.setAdapter(configstore.Adapter{
		ID:     MockAIAdapterID,
		Source: "builtin",
		Type:   "ai",
		Name:   "Nupi Mock AI",
	})

	// No runner configured - should still work for builtin mocks
	manager := NewManager(ManagerOptions{
		Store:    store,
		Adapters: store,
	})

	ctx := context.Background()
	if err := manager.Ensure(ctx); err != nil {
		t.Fatalf("Ensure failed for builtin mock: %v", err)
	}

	running := manager.Running()
	if len(running) != 1 {
		t.Fatalf("expected 1 running adapter, got %d", len(running))
	}

	binding := running[0]
	if binding.AdapterID != MockAIAdapterID {
		t.Errorf("expected adapter %s, got %s", MockAIAdapterID, binding.AdapterID)
	}
	if binding.Runtime[RuntimeExtraTransport] != "builtin" {
		t.Errorf("expected transport=builtin, got %s", binding.Runtime[RuntimeExtraTransport])
	}
}

func TestManagerEnsureAllBuiltinMockAdapters(t *testing.T) {
	mockAdapters := []struct {
		id   string
		slot Slot
	}{
		{MockSTTAdapterID, SlotSTT},
		{MockTTSAdapterID, SlotTTS},
		{MockVADAdapterID, SlotVAD},
		{MockAIAdapterID, SlotAI},
	}

	for _, tc := range mockAdapters {
		t.Run(tc.id, func(t *testing.T) {
			store := &fakeBindingSource{
				bindings: []configstore.AdapterBinding{
					{
						Slot:      string(tc.slot),
						Status:    "active",
						AdapterID: strPtr(tc.id),
					},
				},
			}
			store.setAdapter(configstore.Adapter{
				ID:     tc.id,
				Source: "builtin",
			})

			manager := NewManager(ManagerOptions{
				Store:    store,
				Adapters: store,
			})

			ctx := context.Background()
			if err := manager.Ensure(ctx); err != nil {
				t.Fatalf("Ensure failed: %v", err)
			}

			running := manager.Running()
			if len(running) != 1 {
				t.Fatalf("expected 1 running adapter, got %d", len(running))
			}
			if running[0].AdapterID != tc.id {
				t.Errorf("expected adapter %s, got %s", tc.id, running[0].AdapterID)
			}
		})
	}
}

func TestIsBuiltinMockAdapter(t *testing.T) {
	builtins := []string{
		MockSTTAdapterID,
		MockTTSAdapterID,
		MockVADAdapterID,
		MockAIAdapterID,
	}

	for _, id := range builtins {
		if !IsBuiltinMockAdapter(id) {
			t.Errorf("expected %s to be builtin mock adapter", id)
		}
	}

	nonBuiltins := []string{
		"adapter.ai.openai",
		"adapter.stt.whisper",
		"custom.adapter",
		"",
	}

	for _, id := range nonBuiltins {
		if IsBuiltinMockAdapter(id) {
			t.Errorf("expected %s to NOT be builtin mock adapter", id)
		}
	}
}

func TestStartAdapterWorkingDirResolution(t *testing.T) {
	var portCounter int
	t.Cleanup(SetAllocateProcessAddress(func() (string, error) {
		portCounter++
		return fmt.Sprintf("127.0.0.1:%d", 56000+portCounter), nil
	}))

	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	baseManifestYAML := `
apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: WD Test
spec:
  slot: ai
  mode: local
  entrypoint:
    command: ./bin/mock
    transport: process
`

	t.Run("explicit relative workingDir resolved against manifest.Dir", func(t *testing.T) {
		manifest, err := manifestpkg.Parse([]byte(baseManifestYAML))
		if err != nil {
			t.Fatalf("parse manifest: %v", err)
		}
		manifest.Dir = "/tmp/plugins/my-plugin"
		manifest.Adapter.Entrypoint.WorkingDir = "runtime"

		launcher := NewMockLauncher()
		manager := NewManager(ManagerOptions{
			Launcher:  launcher,
			PluginDir: t.TempDir(),
		})

		plan := bindingPlan{
			binding:  Binding{Slot: SlotAI, AdapterID: "adapter.wd-test"},
			adapter:  configstore.Adapter{ID: "adapter.wd-test", Manifest: baseManifestYAML},
			manifest: manifest,
			endpoint: configstore.AdapterEndpoint{Transport: "process", Command: "./bin/mock"},
		}

		if _, err := manager.startAdapter(context.Background(), plan); err != nil {
			t.Fatalf("startAdapter: %v", err)
		}

		records := launcher.Records()
		if len(records) != 1 {
			t.Fatalf("expected 1 launch, got %d", len(records))
		}
		want := filepath.Join("/tmp/plugins/my-plugin", "runtime")
		if records[0].WorkingDir != want {
			t.Errorf("workingDir = %q, want %q", records[0].WorkingDir, want)
		}
	})

	t.Run("explicit absolute workingDir used as-is", func(t *testing.T) {
		manifest, err := manifestpkg.Parse([]byte(baseManifestYAML))
		if err != nil {
			t.Fatalf("parse manifest: %v", err)
		}
		manifest.Dir = "/tmp/plugins/my-plugin"
		manifest.Adapter.Entrypoint.WorkingDir = "/opt/custom-dir"

		launcher := NewMockLauncher()
		manager := NewManager(ManagerOptions{
			Launcher:  launcher,
			PluginDir: t.TempDir(),
		})

		plan := bindingPlan{
			binding:  Binding{Slot: SlotAI, AdapterID: "adapter.wd-abs"},
			adapter:  configstore.Adapter{ID: "adapter.wd-abs", Manifest: baseManifestYAML},
			manifest: manifest,
			endpoint: configstore.AdapterEndpoint{Transport: "process", Command: "./bin/mock"},
		}

		if _, err := manager.startAdapter(context.Background(), plan); err != nil {
			t.Fatalf("startAdapter: %v", err)
		}

		records := launcher.Records()
		if len(records) != 1 {
			t.Fatalf("expected 1 launch, got %d", len(records))
		}
		if records[0].WorkingDir != "/opt/custom-dir" {
			t.Errorf("workingDir = %q, want %q", records[0].WorkingDir, "/opt/custom-dir")
		}
	})

	t.Run("no workingDir defaults to plugin root directory", func(t *testing.T) {
		manifest, err := manifestpkg.Parse([]byte(baseManifestYAML))
		if err != nil {
			t.Fatalf("parse manifest: %v", err)
		}
		manifest.Dir = "/tmp/plugins/my-plugin"
		// WorkingDir empty — should default to manifest.Dir (plugin root)

		launcher := NewMockLauncher()
		manager := NewManager(ManagerOptions{
			Launcher:  launcher,
			PluginDir: t.TempDir(),
		})

		plan := bindingPlan{
			binding:  Binding{Slot: SlotAI, AdapterID: "adapter.wd-empty"},
			adapter:  configstore.Adapter{ID: "adapter.wd-empty", Manifest: baseManifestYAML},
			manifest: manifest,
			endpoint: configstore.AdapterEndpoint{Transport: "process", Command: "./bin/mock"},
		}

		if _, err := manager.startAdapter(context.Background(), plan); err != nil {
			t.Fatalf("startAdapter: %v", err)
		}

		records := launcher.Records()
		if len(records) != 1 {
			t.Fatalf("expected 1 launch, got %d", len(records))
		}
		if records[0].WorkingDir != "/tmp/plugins/my-plugin" {
			t.Errorf("workingDir = %q, want %q", records[0].WorkingDir, "/tmp/plugins/my-plugin")
		}
	})

	t.Run("no manifest inherits daemon cwd", func(t *testing.T) {
		launcher := NewMockLauncher()
		manager := NewManager(ManagerOptions{
			Launcher:  launcher,
			PluginDir: t.TempDir(),
		})

		plan := bindingPlan{
			binding:  Binding{Slot: SlotAI, AdapterID: "adapter.wd-nodir"},
			adapter:  configstore.Adapter{ID: "adapter.wd-nodir"},
			manifest: nil,
			endpoint: configstore.AdapterEndpoint{Transport: "process", Command: "./bin/mock"},
		}

		if _, err := manager.startAdapter(context.Background(), plan); err != nil {
			t.Fatalf("startAdapter: %v", err)
		}

		records := launcher.Records()
		if len(records) != 1 {
			t.Fatalf("expected 1 launch, got %d", len(records))
		}
		if records[0].WorkingDir != "" {
			t.Errorf("workingDir = %q, want empty (inherit daemon cwd)", records[0].WorkingDir)
		}
	})

	t.Run("relative workingDir with empty manifest.Dir stays relative", func(t *testing.T) {
		manifest, err := manifestpkg.Parse([]byte(baseManifestYAML))
		if err != nil {
			t.Fatalf("parse manifest: %v", err)
		}
		manifest.Dir = "" // no plugin root directory
		manifest.Adapter.Entrypoint.WorkingDir = "runtime"

		launcher := NewMockLauncher()
		manager := NewManager(ManagerOptions{
			Launcher:  launcher,
			PluginDir: t.TempDir(),
		})

		plan := bindingPlan{
			binding:  Binding{Slot: SlotAI, AdapterID: "adapter.wd-relnodir"},
			adapter:  configstore.Adapter{ID: "adapter.wd-relnodir", Manifest: baseManifestYAML},
			manifest: manifest,
			endpoint: configstore.AdapterEndpoint{Transport: "process", Command: "./bin/mock"},
		}

		if _, err := manager.startAdapter(context.Background(), plan); err != nil {
			t.Fatalf("startAdapter: %v", err)
		}

		records := launcher.Records()
		if len(records) != 1 {
			t.Fatalf("expected 1 launch, got %d", len(records))
		}
		// Relative path stays relative when manifest.Dir is empty
		if records[0].WorkingDir != "runtime" {
			t.Errorf("workingDir = %q, want %q", records[0].WorkingDir, "runtime")
		}
	})

	t.Run("dot workingDir resolves to manifest.Dir", func(t *testing.T) {
		manifest, err := manifestpkg.Parse([]byte(baseManifestYAML))
		if err != nil {
			t.Fatalf("parse manifest: %v", err)
		}
		manifest.Dir = "/tmp/plugins/vad-silero"
		manifest.Adapter.Entrypoint.WorkingDir = "."

		launcher := NewMockLauncher()
		manager := NewManager(ManagerOptions{
			Launcher:  launcher,
			PluginDir: t.TempDir(),
		})

		plan := bindingPlan{
			binding:  Binding{Slot: SlotAI, AdapterID: "adapter.wd-dot"},
			adapter:  configstore.Adapter{ID: "adapter.wd-dot", Manifest: baseManifestYAML},
			manifest: manifest,
			endpoint: configstore.AdapterEndpoint{Transport: "process", Command: "./bin/mock"},
		}

		if _, err := manager.startAdapter(context.Background(), plan); err != nil {
			t.Fatalf("startAdapter: %v", err)
		}

		records := launcher.Records()
		if len(records) != 1 {
			t.Fatalf("expected 1 launch, got %d", len(records))
		}
		want := "/tmp/plugins/vad-silero"
		if records[0].WorkingDir != want {
			t.Errorf("workingDir = %q, want %q", records[0].WorkingDir, want)
		}
	})
}

// Validate adapter hot-swap and lifecycle consistency

func TestManagerHotSwapVADWithoutAffectingSTT(t *testing.T) {
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotSTT),
				Status:    "active",
				AdapterID: strPtr("adapter.stt.whisper"),
			},
			{
				Slot:      string(SlotVAD),
				Status:    "active",
				AdapterID: strPtr("adapter.vad.silero"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.stt.whisper"})
	store.setAdapter(configstore.Adapter{ID: "adapter.vad.silero"})
	store.setAdapter(configstore.Adapter{ID: "adapter.vad.webrtc"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.stt.whisper",
		Transport: "process",
		Command:   "./stt-whisper",
	})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.vad.silero",
		Transport: "process",
		Command:   "./vad-silero",
	})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.vad.webrtc",
		Transport: "process",
		Command:   "./vad-webrtc",
	})

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	// Initial reconciliation — both adapters start
	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("initial Ensure: %v", err)
	}

	running := manager.Running()
	if len(running) != 2 {
		t.Fatalf("expected 2 running adapters, got %d", len(running))
	}

	initialLaunches := len(launcher.Records())

	// Swap VAD binding to a different adapter
	store.mu.Lock()
	store.bindings[1].AdapterID = strPtr("adapter.vad.webrtc")
	store.mu.Unlock()

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("hot-swap Ensure: %v", err)
	}

	// STT should NOT have been restarted (fingerprint unchanged)
	// VAD should have been stopped and relaunched (different adapter ID)
	if launcher.StopCount(string(SlotSTT)) != 0 {
		t.Fatalf("STT adapter should not be stopped during VAD swap, got stop count %d", launcher.StopCount(string(SlotSTT)))
	}
	if launcher.StopCount(string(SlotVAD)) != 1 {
		t.Fatalf("VAD adapter should be stopped once during swap, got stop count %d", launcher.StopCount(string(SlotVAD)))
	}

	// Verify new VAD launch happened
	newLaunches := len(launcher.Records()) - initialLaunches
	if newLaunches != 1 {
		t.Fatalf("expected 1 new launch after swap, got %d", newLaunches)
	}

	// Both slots still running
	running = manager.Running()
	if len(running) != 2 {
		t.Fatalf("expected 2 running adapters after swap, got %d", len(running))
	}

	// Verify correct adapter IDs
	runMap := make(map[Slot]string)
	for _, r := range running {
		runMap[r.Slot] = r.AdapterID
	}
	if runMap[SlotSTT] != "adapter.stt.whisper" {
		t.Fatalf("expected STT adapter unchanged, got %q", runMap[SlotSTT])
	}
	if runMap[SlotVAD] != "adapter.vad.webrtc" {
		t.Fatalf("expected VAD adapter swapped to webrtc, got %q", runMap[SlotVAD])
	}
}

func TestManagerUnbindSlotLeavesOthersRunning(t *testing.T) {
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotSTT),
				Status:    "active",
				AdapterID: strPtr("adapter.stt"),
			},
			{
				Slot:      string(SlotVAD),
				Status:    "active",
				AdapterID: strPtr("adapter.vad"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.stt"})
	store.setAdapter(configstore.Adapter{ID: "adapter.vad"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.stt",
		Transport: "process",
		Command:   "./stt",
	})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.vad",
		Transport: "process",
		Command:   "./vad",
	})

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("initial Ensure: %v", err)
	}
	if running := manager.Running(); len(running) != 2 {
		t.Fatalf("expected 2 running, got %d", len(running))
	}

	// Unbind VAD (remove binding)
	store.mu.Lock()
	store.bindings = store.bindings[:1] // keep only STT
	store.mu.Unlock()

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("unbind Ensure: %v", err)
	}

	running := manager.Running()
	if len(running) != 1 {
		t.Fatalf("expected 1 running after unbind, got %d", len(running))
	}
	if running[0].Slot != SlotSTT {
		t.Fatalf("expected remaining adapter to be STT, got %s", running[0].Slot)
	}
	if launcher.StopCount(string(SlotSTT)) != 0 {
		t.Fatalf("STT should not have been stopped, got count %d", launcher.StopCount(string(SlotSTT)))
	}
	if launcher.StopCount(string(SlotVAD)) != 1 {
		t.Fatalf("unbound VAD adapter should have been stopped, got count %d", launcher.StopCount(string(SlotVAD)))
	}
}

func TestLaunchEnvConsistencyAllSlots(t *testing.T) {
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	slots := []Slot{SlotSTT, SlotTTS, SlotVAD, SlotAI, SlotTunnel}
	requiredEnvKeys := []string{
		"NUPI_ADAPTER_SLOT",
		"NUPI_ADAPTER_ID",
		"NUPI_ADAPTER_CONFIG",
		"NUPI_ADAPTER_HOME",
		"NUPI_ADAPTER_DATA_DIR",
		"NUPI_ADAPTER_TRANSPORT",
		"NUPI_ADAPTER_ENDPOINT",
	}

	for _, slot := range slots {
		t.Run(string(slot), func(t *testing.T) {
			adapterID := "adapter." + string(slot) + ".test"
			store := &fakeBindingSource{
				bindings: []configstore.AdapterBinding{
					{
						Slot:      string(slot),
						Status:    "active",
						AdapterID: strPtr(adapterID),
						Config:    `{"key":"val"}`,
					},
				},
			}
			store.setAdapter(configstore.Adapter{ID: adapterID})
			store.setEndpoint(configstore.AdapterEndpoint{
				AdapterID: adapterID,
				Transport: "process",
				Command:   "./adapter",
			})

			launcher := NewMockLauncher()
			manager := NewManager(ManagerOptions{
				Store:     store,
				Adapters:  store,
				Launcher:  launcher,
				PluginDir: t.TempDir(),
			})

			if err := manager.Ensure(context.Background()); err != nil {
				t.Fatalf("Ensure for slot %s: %v", slot, err)
			}

			records := launcher.Records()
			if len(records) != 1 {
				t.Fatalf("expected 1 launch for %s, got %d", slot, len(records))
			}

			envMap := make(map[string]string)
			for _, e := range records[0].Env {
				parts := strings.SplitN(e, "=", 2)
				if len(parts) == 2 {
					envMap[parts[0]] = parts[1]
				}
			}

			for _, key := range requiredEnvKeys {
				if _, ok := envMap[key]; !ok {
					t.Errorf("missing env key %s for slot %s", key, slot)
				}
			}

			if envMap["NUPI_ADAPTER_SLOT"] != string(slot) {
				t.Errorf("NUPI_ADAPTER_SLOT = %q, want %q", envMap["NUPI_ADAPTER_SLOT"], string(slot))
			}
			if envMap["NUPI_ADAPTER_ID"] != adapterID {
				t.Errorf("NUPI_ADAPTER_ID = %q, want %q", envMap["NUPI_ADAPTER_ID"], adapterID)
			}
			if envMap["NUPI_ADAPTER_CONFIG"] != `{"key":"val"}` {
				t.Errorf("NUPI_ADAPTER_CONFIG = %q, want %q", envMap["NUPI_ADAPTER_CONFIG"], `{"key":"val"}`)
			}
			if envMap["NUPI_ADAPTER_TRANSPORT"] != "process" {
				t.Errorf("NUPI_ADAPTER_TRANSPORT = %q, want %q", envMap["NUPI_ADAPTER_TRANSPORT"], "process")
			}
			if envMap["NUPI_ADAPTER_ENDPOINT"] == "" {
				t.Errorf("NUPI_ADAPTER_ENDPOINT should not be empty for slot %s", slot)
			}
			if envMap["NUPI_ADAPTER_HOME"] == "" {
				t.Errorf("NUPI_ADAPTER_HOME should not be empty for slot %s", slot)
			}
			if envMap["NUPI_ADAPTER_DATA_DIR"] == "" {
				t.Errorf("NUPI_ADAPTER_DATA_DIR should not be empty for slot %s", slot)
			}
		})
	}
}

func TestFingerprintDeterministicAndSlotSensitive(t *testing.T) {
	sttManifestYAML := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Test STT
spec:
  slot: stt
  entrypoint:
    command: ./adapter
    transport: process`

	vadManifestYAML := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Test VAD
spec:
  slot: vad
  entrypoint:
    command: ./adapter
    transport: process`

	sttMF, err := manifestpkg.Parse([]byte(sttManifestYAML))
	if err != nil {
		t.Fatalf("parse stt manifest: %v", err)
	}
	vadMF, err := manifestpkg.Parse([]byte(vadManifestYAML))
	if err != nil {
		t.Fatalf("parse vad manifest: %v", err)
	}

	endpoint := configstore.AdapterEndpoint{
		Transport: "process",
		Command:   "./adapter",
	}

	bindingA := Binding{Slot: SlotSTT, AdapterID: "adapter.test", RawConfig: `{"key":"val"}`}
	bindingB := Binding{Slot: SlotVAD, AdapterID: "adapter.test", RawConfig: `{"key":"val"}`}

	fpA := computePlanFingerprint(bindingA, sttMF, sttManifestYAML, endpoint)
	fpB := computePlanFingerprint(bindingB, vadMF, vadManifestYAML, endpoint)

	if fpA == "" || fpB == "" {
		t.Fatalf("fingerprints should not be empty: fpA=%q, fpB=%q", fpA, fpB)
	}

	// Different manifest slot values should produce different fingerprints
	if fpA == fpB {
		t.Fatalf("fingerprints should differ for different manifest slots, both got %q", fpA)
	}

	// Same inputs should produce deterministic output
	fpA2 := computePlanFingerprint(bindingA, sttMF, sttManifestYAML, endpoint)
	if fpA != fpA2 {
		t.Fatalf("fingerprint not deterministic: %q vs %q", fpA, fpA2)
	}

	// Changing config should change fingerprint
	bindingC := Binding{Slot: SlotSTT, AdapterID: "adapter.test", RawConfig: `{"key":"changed"}`}
	fpC := computePlanFingerprint(bindingC, sttMF, sttManifestYAML, endpoint)
	if fpA == fpC {
		t.Fatalf("fingerprint should change when config changes, both are %q", fpA)
	}
}

// TestFingerprintSensitivity verifies that computePlanFingerprint detects
// changes in every field that should trigger an adapter restart. If a new
// runtime-relevant field is added to Binding, AdapterEndpoint, or AdapterSpec
// but not wired into computePlanFingerprint, the corresponding sub-test will
// fail — catching the "forgot to update the hash" class of bug at CI time.
func TestFingerprintSensitivity(t *testing.T) {
	baseManifestYAML := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Test Adapter
  slug: test-adapter
spec:
  slot: stt
  entrypoint:
    runtime: binary
    command: ./adapter
    args: ["--mode", "fast"]
    transport: process
    listenEnv: LISTEN_ADDR
    workingDir: /opt/adapter
    readyTimeout: 10s
    shutdownTimeout: 5s
  telemetry:
    stdout: true
    stderr: false
  assets:
    models:
      cacheDirEnv: MODEL_CACHE
  options:
    model:
      type: string
      description: Model name
      default: base`

	baseMF, err := manifestpkg.Parse([]byte(baseManifestYAML))
	if err != nil {
		t.Fatalf("parse base manifest: %v", err)
	}

	trueVal := true
	falseVal := false

	baseBinding := Binding{
		Slot:      SlotSTT,
		AdapterID: "adapter.test",
		RawConfig: `{"model":"base"}`,
	}

	baseEndpoint := configstore.AdapterEndpoint{
		AdapterID:     "adapter.test",
		Transport:     "process",
		Command:       "./adapter",
		Args:          []string{"--mode", "fast"},
		Env:           map[string]string{"API_KEY": "secret", "REGION": "us"},
		TLSCertPath:   "/certs/client.pem",
		TLSKeyPath:    "/certs/client-key.pem",
		TLSCACertPath: "/certs/ca.pem",
		TLSInsecure:   false,
	}

	baseFP := computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, baseEndpoint)
	if baseFP == "" {
		t.Fatal("base fingerprint should not be empty")
	}

	// Helper: clone the base manifest struct so each sub-test can modify
	// a single field without copy-pasting 30-line YAML blocks.
	cloneManifest := func() *manifestpkg.Manifest {
		cp := *baseMF
		if baseMF.Adapter != nil {
			spec := *baseMF.Adapter
			if baseMF.Adapter.Telemetry.Stdout != nil {
				v := *baseMF.Adapter.Telemetry.Stdout
				spec.Telemetry.Stdout = &v
			}
			if baseMF.Adapter.Telemetry.Stderr != nil {
				v := *baseMF.Adapter.Telemetry.Stderr
				spec.Telemetry.Stderr = &v
			}
			if baseMF.Adapter.Entrypoint.Args != nil {
				spec.Entrypoint.Args = append([]string(nil), baseMF.Adapter.Entrypoint.Args...)
			}
			if baseMF.Adapter.Options != nil {
				spec.Options = make(map[string]manifestpkg.AdapterOption, len(baseMF.Adapter.Options))
				for k, v := range baseMF.Adapter.Options {
					optCopy := v
					if v.Values != nil {
						optCopy.Values = append([]any(nil), v.Values...)
					}
					spec.Options[k] = optCopy
				}
			}
			cp.Adapter = &spec
		}
		return &cp
	}

	assertChanged := func(t *testing.T, fp string) {
		t.Helper()
		if fp == baseFP {
			t.Fatal("fingerprint should have changed but didn't")
		}
	}

	assertUnchanged := func(t *testing.T, fp string) {
		t.Helper()
		if fp != baseFP {
			t.Fatal("fingerprint should NOT have changed but did")
		}
	}

	// Sanity: unmodified clone must produce identical fingerprint.
	// Guards against a broken cloneManifest causing false-positive assertChanged tests.
	t.Run("clone_sanity", func(t *testing.T) {
		cloneFP := computePlanFingerprint(baseBinding, cloneManifest(), baseManifestYAML, baseEndpoint)
		if cloneFP != baseFP {
			t.Fatalf("unmodified clone fingerprint %q differs from base %q — cloneManifest is broken", cloneFP, baseFP)
		}
	})

	// --- Binding fields ---

	t.Run("binding/AdapterID", func(t *testing.T) {
		b := baseBinding
		b.AdapterID = "adapter.other"
		assertChanged(t, computePlanFingerprint(b, baseMF, baseManifestYAML, baseEndpoint))
	})

	t.Run("binding/RawConfig", func(t *testing.T) {
		b := baseBinding
		b.RawConfig = `{"model":"large"}`
		assertChanged(t, computePlanFingerprint(b, baseMF, baseManifestYAML, baseEndpoint))
	})

	t.Run("binding/Config_when_RawConfig_empty", func(t *testing.T) {
		b := baseBinding
		b.RawConfig = ""
		b.Config = map[string]any{"model": "base"}
		fp1 := computePlanFingerprint(b, baseMF, baseManifestYAML, baseEndpoint)

		b2 := baseBinding
		b2.RawConfig = ""
		b2.Config = map[string]any{"model": "large"}
		fp2 := computePlanFingerprint(b2, baseMF, baseManifestYAML, baseEndpoint)

		if fp1 == fp2 {
			t.Fatal("fingerprint should differ when Config map values differ")
		}
	})

	t.Run("binding/Slot_ignored", func(t *testing.T) {
		b := baseBinding
		b.Slot = SlotVAD
		assertUnchanged(t, computePlanFingerprint(b, baseMF, baseManifestYAML, baseEndpoint))
	})

	t.Run("binding/Fingerprint_ignored", func(t *testing.T) {
		b := baseBinding
		b.Fingerprint = "should-be-ignored"
		assertUnchanged(t, computePlanFingerprint(b, baseMF, baseManifestYAML, baseEndpoint))
	})

	t.Run("binding/Runtime_ignored", func(t *testing.T) {
		b := baseBinding
		b.Runtime = map[string]string{"transport": "process", "address": "127.0.0.1:5555"}
		assertUnchanged(t, computePlanFingerprint(b, baseMF, baseManifestYAML, baseEndpoint))
	})

	// --- Endpoint fields ---

	t.Run("endpoint/Transport", func(t *testing.T) {
		e := baseEndpoint
		e.Transport = "grpc"
		assertChanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	t.Run("endpoint/Address_nonprocess", func(t *testing.T) {
		e := baseEndpoint
		e.Transport = "grpc"
		e.Address = "localhost:9090"
		fp1 := computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e)

		e2 := e
		e2.Address = "localhost:9091"
		fp2 := computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e2)
		if fp1 == fp2 {
			t.Fatal("address change should affect fingerprint for non-process transport")
		}
	})

	t.Run("endpoint/Address_process_ignored", func(t *testing.T) {
		e := baseEndpoint
		e.Transport = "process"
		e.Address = "127.0.0.1:12345"
		fp1 := computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e)

		e2 := e
		e2.Address = "127.0.0.1:54321"
		fp2 := computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e2)
		if fp1 != fp2 {
			t.Fatal("address change should NOT affect fingerprint for process transport")
		}
	})

	t.Run("endpoint/Command", func(t *testing.T) {
		e := baseEndpoint
		e.Command = "./other-binary"
		assertChanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	t.Run("endpoint/Args_value_changed", func(t *testing.T) {
		e := baseEndpoint
		e.Args = []string{"--mode", "slow"}
		assertChanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	t.Run("endpoint/Args_added", func(t *testing.T) {
		e := baseEndpoint
		e.Args = append(append([]string(nil), baseEndpoint.Args...), "--extra")
		assertChanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	t.Run("endpoint/Args_empty", func(t *testing.T) {
		e := baseEndpoint
		e.Args = nil
		assertChanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	t.Run("endpoint/Env_value_changed", func(t *testing.T) {
		e := baseEndpoint
		e.Env = map[string]string{"API_KEY": "changed", "REGION": "us"}
		assertChanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	t.Run("endpoint/Env_key_added", func(t *testing.T) {
		e := baseEndpoint
		e.Env = map[string]string{"API_KEY": "secret", "REGION": "us", "NEW": "val"}
		assertChanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	t.Run("endpoint/Env_key_removed", func(t *testing.T) {
		e := baseEndpoint
		e.Env = map[string]string{"API_KEY": "secret"}
		assertChanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	t.Run("endpoint/TLSCertPath", func(t *testing.T) {
		e := baseEndpoint
		e.TLSCertPath = "/other/cert.pem"
		assertChanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	t.Run("endpoint/TLSKeyPath", func(t *testing.T) {
		e := baseEndpoint
		e.TLSKeyPath = "/other/key.pem"
		assertChanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	t.Run("endpoint/TLSCACertPath", func(t *testing.T) {
		e := baseEndpoint
		e.TLSCACertPath = "/other/ca.pem"
		assertChanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	t.Run("endpoint/TLSInsecure", func(t *testing.T) {
		e := baseEndpoint
		e.TLSInsecure = true
		assertChanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	t.Run("endpoint/AdapterID_ignored", func(t *testing.T) {
		e := baseEndpoint
		e.AdapterID = "something-else"
		assertUnchanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	t.Run("endpoint/CreatedAt_ignored", func(t *testing.T) {
		e := baseEndpoint
		e.CreatedAt = "2025-01-01T00:00:00Z"
		assertUnchanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	t.Run("endpoint/UpdatedAt_ignored", func(t *testing.T) {
		e := baseEndpoint
		e.UpdatedAt = "2025-01-01T00:00:00Z"
		assertUnchanged(t, computePlanFingerprint(baseBinding, baseMF, baseManifestYAML, e))
	})

	// --- Manifest entrypoint fields (modify cloned struct, not YAML) ---

	t.Run("manifest/entrypoint/Runtime", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Entrypoint.Runtime = "js"
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/entrypoint/Command", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Entrypoint.Command = "./other-adapter"
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/entrypoint/Args", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Entrypoint.Args = []string{"--mode", "slow"}
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/entrypoint/Transport", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Entrypoint.Transport = "grpc"
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/entrypoint/ListenEnv", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Entrypoint.ListenEnv = "OTHER_LISTEN"
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/entrypoint/WorkingDir", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Entrypoint.WorkingDir = "/other/dir"
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/entrypoint/ReadyTimeout", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Entrypoint.ReadyTimeout = "30s"
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/entrypoint/ShutdownTimeout", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Entrypoint.ShutdownTimeout = "60s"
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	// --- Manifest telemetry ---

	t.Run("manifest/telemetry/Stdout_changed", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Telemetry.Stdout = &falseVal
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/telemetry/Stderr_changed", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Telemetry.Stderr = &trueVal
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/telemetry/Stdout_nil_vs_explicit", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Telemetry.Stdout = nil
		fpNil := computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint)

		mf2 := cloneManifest()
		mf2.Adapter.Telemetry.Stdout = &trueVal
		fpExplicit := computePlanFingerprint(baseBinding, mf2, baseManifestYAML, baseEndpoint)

		if fpNil == fpExplicit {
			t.Fatal("nil Stdout pointer should produce different fingerprint than explicit true")
		}
	})

	// --- Manifest assets ---

	t.Run("manifest/assets/CacheDirEnv", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Assets.Models.CacheDirEnv = "OTHER_CACHE"
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	// --- Manifest slot ---

	t.Run("manifest/Slot", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Slot = "vad"
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	// --- Manifest options ---

	t.Run("manifest/options/default_changed", func(t *testing.T) {
		mf := cloneManifest()
		opt := mf.Adapter.Options["model"]
		opt.Default = "large"
		mf.Adapter.Options["model"] = opt
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/options/new_option_added", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Options["lang"] = manifestpkg.AdapterOption{
			Type:        "string",
			Description: "Language",
			Default:     "en",
		}
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/options/type_changed", func(t *testing.T) {
		mf := cloneManifest()
		opt := mf.Adapter.Options["model"]
		opt.Type = "enum"
		opt.Values = []any{"base", "large"}
		mf.Adapter.Options["model"] = opt
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/options/description_changed", func(t *testing.T) {
		mf := cloneManifest()
		opt := mf.Adapter.Options["model"]
		opt.Description = "Changed description"
		mf.Adapter.Options["model"] = opt
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/options/values_changed", func(t *testing.T) {
		mf := cloneManifest()
		opt := mf.Adapter.Options["model"]
		opt.Values = []any{"base", "large", "tiny"}
		mf.Adapter.Options["model"] = opt
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/options/all_removed", func(t *testing.T) {
		mf := cloneManifest()
		mf.Adapter.Options = nil
		assertChanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	t.Run("manifest/options/Required_ignored", func(t *testing.T) {
		mf := cloneManifest()
		opt := mf.Adapter.Options["model"]
		opt.Required = true
		mf.Adapter.Options["model"] = opt
		assertUnchanged(t, computePlanFingerprint(baseBinding, mf, baseManifestYAML, baseEndpoint))
	})

	// --- Nil manifest falls back to raw YAML ---

	t.Run("nil_manifest_uses_raw", func(t *testing.T) {
		fp1 := computePlanFingerprint(baseBinding, nil, "manifest-v1", baseEndpoint)
		fp2 := computePlanFingerprint(baseBinding, nil, "manifest-v2", baseEndpoint)
		if fp1 == fp2 {
			t.Fatal("fingerprint should change when raw manifest string changes")
		}
	})

	t.Run("nil_adapter_spec_uses_raw", func(t *testing.T) {
		mf := &manifestpkg.Manifest{Adapter: nil}
		fp1 := computePlanFingerprint(baseBinding, mf, "raw-v1", baseEndpoint)
		fp2 := computePlanFingerprint(baseBinding, mf, "raw-v2", baseEndpoint)
		if fp1 == fp2 {
			t.Fatal("nil Adapter should fall back to raw YAML comparison")
		}
	})
}

func TestDiscoveredManifestFeedsManagerPlan(t *testing.T) {
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	// Create a plugin directory with a real manifest
	pluginRoot := t.TempDir()
	vadDir := filepath.Join(pluginRoot, "ai.nupi", "vad-local-silero")
	if err := os.MkdirAll(vadDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	vadYAML := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Test Silero VAD
  namespace: ai.nupi
  slug: vad-local-silero
  version: 1.0.0
spec:
  slot: vad
  entrypoint:
    command: ./vad-local-silero
    transport: process
    readyTimeout: 10s`

	if err := os.WriteFile(filepath.Join(vadDir, "plugin.yaml"), []byte(vadYAML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Discover the plugin — this is the entry point being tested
	manifests, err := manifestpkg.Discover(pluginRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("expected 1 discovered manifest, got %d", len(manifests))
	}
	discovered := manifests[0]

	// Feed discovered manifest data into the store — simulating the real
	// flow where discovery results populate the config store
	adapterID := "adapter.vad.silero"
	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      discovered.Adapter.Slot,
				Status:    "active",
				AdapterID: strPtr(adapterID),
			},
		},
	}
	store.setAdapter(configstore.Adapter{
		ID:       adapterID,
		Manifest: vadYAML,
	})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: adapterID,
		Transport: discovered.Adapter.Entrypoint.Transport,
		Command:   discovered.Adapter.Entrypoint.Command,
	})

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: pluginRoot,
	})

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	records := launcher.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 launch, got %d", len(records))
	}

	// Verify the launch plan reflects discovered manifest properties
	envMap := make(map[string]string)
	for _, e := range records[0].Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	if envMap["NUPI_ADAPTER_SLOT"] != discovered.Adapter.Slot {
		t.Fatalf("launch slot %q != discovered slot %q", envMap["NUPI_ADAPTER_SLOT"], discovered.Adapter.Slot)
	}
	if envMap["NUPI_ADAPTER_TRANSPORT"] != discovered.Adapter.Entrypoint.Transport {
		t.Fatalf("launch transport %q != discovered transport %q", envMap["NUPI_ADAPTER_TRANSPORT"], discovered.Adapter.Entrypoint.Transport)
	}
	if envMap["NUPI_ADAPTER_ID"] != adapterID {
		t.Fatalf("launch adapter ID %q != expected %q", envMap["NUPI_ADAPTER_ID"], adapterID)
	}
	if !strings.HasSuffix(records[0].Binary, "vad-local-silero") {
		t.Fatalf("launch binary %q should end with discovered command 'vad-local-silero'", records[0].Binary)
	}
}

func TestEnsureIdempotentWithManifestDefaults(t *testing.T) {
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

	// Manifest with options that have defaults
	manifestYAML := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: VAD with defaults
spec:
  slot: vad
  entrypoint:
    command: ./vad
    transport: process
  options:
    threshold:
      type: number
      default: 0.5`

	adapterID := "adapter.vad.defaults"
	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotVAD),
				Status:    "active",
				AdapterID: strPtr(adapterID),
				// No config — defaults should be applied from manifest
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: adapterID, Manifest: manifestYAML})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: adapterID,
		Transport: "process",
		Command:   "./vad",
	})

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	// First Ensure — adapter starts
	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if len(launcher.Records()) != 1 {
		t.Fatalf("expected 1 launch, got %d", len(launcher.Records()))
	}

	// Verify the manifest defaults were actually applied to launch config
	records := launcher.Records()
	envMap := make(map[string]string)
	for _, e := range records[0].Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}
	adapterConfig := envMap["NUPI_ADAPTER_CONFIG"]
	if !strings.Contains(adapterConfig, "threshold") {
		t.Fatalf("NUPI_ADAPTER_CONFIG should contain default 'threshold', got %q", adapterConfig)
	}

	// Second Ensure — nothing changed, adapter should NOT restart
	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if launcher.StopCount(string(SlotVAD)) != 0 {
		t.Fatalf("adapter should not restart on idempotent Ensure with manifest defaults, got stop count %d", launcher.StopCount(string(SlotVAD)))
	}
	if len(launcher.Records()) != 1 {
		t.Fatalf("expected still 1 launch (no restart), got %d", len(launcher.Records()))
	}
}

func TestValidateCommandPath(t *testing.T) {
	// Create a temporary directory structure for testing
	trustedDir := t.TempDir()
	untrustedDir := t.TempDir()

	// Create a binary inside the trusted directory
	trustedBin := filepath.Join(trustedDir, "adapter-bin")
	if err := os.WriteFile(trustedBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a binary inside the untrusted directory
	untrustedBin := filepath.Join(untrustedDir, "evil-bin")
	if err := os.WriteFile(untrustedBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a subdirectory inside trusted dir
	subDir := filepath.Join(trustedDir, "sub")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subBin := filepath.Join(subDir, "nested-bin")
	if err := os.WriteFile(subBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(ManagerOptions{
		PluginDir:          trustedDir,
		AllowedCommandDirs: []string{trustedDir},
	})

	t.Run("binary under pluginRoot", func(t *testing.T) {
		if err := manager.validateCommandPath(trustedBin); err != nil {
			t.Errorf("expected trusted path to be valid, got: %v", err)
		}
	})

	t.Run("binary in subdirectory of pluginRoot", func(t *testing.T) {
		if err := manager.validateCommandPath(subBin); err != nil {
			t.Errorf("expected nested trusted path to be valid, got: %v", err)
		}
	})

	t.Run("binary under allowedCommandDirs", func(t *testing.T) {
		extraDir := t.TempDir()
		extraBin := filepath.Join(extraDir, "extra-bin")
		if err := os.WriteFile(extraBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		m := NewManager(ManagerOptions{
			PluginDir:          trustedDir,
			AllowedCommandDirs: []string{extraDir},
		})
		if err := m.validateCommandPath(extraBin); err != nil {
			t.Errorf("expected extra dir binary to be valid, got: %v", err)
		}
	})

	t.Run("binary outside trusted dirs", func(t *testing.T) {
		err := manager.validateCommandPath(untrustedBin)
		if err == nil {
			t.Fatal("expected error for untrusted path")
		}
		if !errors.Is(err, ErrCommandPathNotTrusted) {
			t.Errorf("expected ErrCommandPathNotTrusted, got: %v", err)
		}
	})

	t.Run("path traversal", func(t *testing.T) {
		traversalPath := filepath.Join(trustedDir, "..", "..", "etc", "passwd")
		err := manager.validateCommandPath(traversalPath)
		if err == nil {
			t.Fatal("expected error for path traversal")
		}
		if !errors.Is(err, ErrCommandPathNotTrusted) {
			t.Errorf("expected ErrCommandPathNotTrusted, got: %v", err)
		}
	})

	t.Run("symlink pointing outside trusted dir", func(t *testing.T) {
		symlinkPath := filepath.Join(trustedDir, "evil-link")
		if err := os.Symlink(untrustedBin, symlinkPath); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		err := manager.validateCommandPath(symlinkPath)
		if err == nil {
			t.Fatal("expected error for symlink pointing outside trusted dir")
		}
		if !errors.Is(err, ErrCommandPathNotTrusted) {
			t.Errorf("expected ErrCommandPathNotTrusted, got: %v", err)
		}
	})

	t.Run("symlink pointing inside trusted dir", func(t *testing.T) {
		symlinkPath := filepath.Join(trustedDir, "good-link")
		if err := os.Symlink(trustedBin, symlinkPath); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		if err := manager.validateCommandPath(symlinkPath); err != nil {
			t.Errorf("expected valid symlink within trusted dir, got: %v", err)
		}
	})
}

func TestResolveAdapterCommandPathValidation(t *testing.T) {
	trustedDir := t.TempDir()
	untrustedDir := t.TempDir()

	// Create binaries
	trustedBin := filepath.Join(trustedDir, "my-adapter")
	if err := os.WriteFile(trustedBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	untrustedBin := filepath.Join(untrustedDir, "evil-adapter")
	if err := os.WriteFile(untrustedBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(ManagerOptions{
		PluginDir:          trustedDir,
		AllowedCommandDirs: []string{trustedDir},
	})

	t.Run("trusted binary adapter accepted", func(t *testing.T) {
		plan := bindingPlan{
			binding: Binding{AdapterID: "test-adapter"},
			endpoint: configstore.AdapterEndpoint{
				Command: trustedBin,
			},
		}
		binary, _, err := manager.resolveAdapterCommand(plan, trustedDir)
		if err != nil {
			t.Fatalf("expected trusted binary to be accepted, got: %v", err)
		}
		if binary != trustedBin {
			t.Errorf("expected binary %s, got %s", trustedBin, binary)
		}
	})

	t.Run("untrusted binary adapter rejected", func(t *testing.T) {
		plan := bindingPlan{
			binding: Binding{AdapterID: "evil-adapter"},
			endpoint: configstore.AdapterEndpoint{
				Command: untrustedBin,
			},
		}
		_, _, err := manager.resolveAdapterCommand(plan, untrustedDir)
		if err == nil {
			t.Fatal("expected error for untrusted binary")
		}
		if !errors.Is(err, ErrCommandPathNotTrusted) {
			t.Errorf("expected ErrCommandPathNotTrusted, got: %v", err)
		}
	})

	t.Run("relative command without adapterHome stays relative and is rejected", func(t *testing.T) {
		plan := bindingPlan{
			binding: Binding{AdapterID: "relative-adapter"},
			endpoint: configstore.AdapterEndpoint{
				Command: "./some-binary",
			},
		}
		_, _, err := manager.resolveAdapterCommand(plan, "")
		if err == nil {
			t.Fatal("expected error for relative command without adapterHome")
		}
		if !errors.Is(err, ErrCommandPathNotTrusted) {
			t.Errorf("expected ErrCommandPathNotTrusted, got: %v", err)
		}
	})
}
