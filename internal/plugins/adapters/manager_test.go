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

	"github.com/nupi-ai/nupi/internal/adapterrunner"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
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
	defer func(original func() (string, error)) {
		allocateProcessAddressFn = original
	}(allocateProcessAddressFn)
	allocateProcessAddressFn = func() (string, error) {
		return "127.0.0.1:60001", nil
	}

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
	defer func(original func() (string, error)) {
		allocateProcessAddressFn = original
	}(allocateProcessAddressFn)
	allocateProcessAddressFn = func() (string, error) {
		return "127.0.0.1:60001", nil
	}
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Runner:    adapterrunner.NewManager(t.TempDir()),
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}
	records := launcher.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 launch call, got %d", len(records))
	}

	call := records[0]
	if call.Binary != manager.runner.BinaryPath() {
		t.Fatalf("unexpected binary path %q", call.Binary)
	}
	expectArgs := []string{"--slot", string(SlotAI), "--adapter", "adapter.ai"}
	if len(call.Args) != len(expectArgs) {
		t.Fatalf("unexpected args: %v", call.Args)
	}
	for i, arg := range expectArgs {
		if call.Args[i] != arg {
			t.Fatalf("unexpected arg[%d]: %q (expected %q)", i, call.Args[i], arg)
		}
	}

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure (second call) returned error: %v", err)
	}
	if len(launcher.Records()) != 1 {
		t.Fatalf("expected no additional launches when configuration unchanged")
	}

	store.mu.Lock()
	store.bindings = nil
	store.mu.Unlock()

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure after removing bindings returned error: %v", err)
	}

	if launcher.StopCount(string(SlotAI)) != 1 {
		t.Fatalf("expected handle to be stopped once")
	}
}

func TestManagerEnsureProcessTransportAllocFailure(t *testing.T) {
	defer func(original func() (string, error)) {
		allocateProcessAddressFn = original
	}(allocateProcessAddressFn)
	allocateProcessAddressFn = func() (string, error) {
		return "", errors.New("no ports")
	}
	defer func(original func(context.Context, string) error) {
		waitForAdapterReadyFn = original
	}(waitForAdapterReadyFn)
	waitForAdapterReadyFn = func(context.Context, string) error {
		return nil
	}

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
		Runner:    adapterrunner.NewManager(t.TempDir()),
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
	defer func(original func() (string, error)) {
		allocateProcessAddressFn = original
	}(allocateProcessAddressFn)
	allocateProcessAddressFn = func() (string, error) {
		return "127.0.0.1:60100", nil
	}
	defer func(original func(context.Context, string) error) {
		waitForAdapterReadyFn = original
	}(waitForAdapterReadyFn)
	waitForAdapterReadyFn = func(context.Context, string) error {
		return errors.New("dial failed")
	}

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
		Runner:    adapterrunner.NewManager(t.TempDir()),
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
	if len(launcher.Records()) != 1 {
		t.Fatalf("expected launch attempt recorded")
	}
	if launcher.StopCount(string(SlotAI)) == 0 {
		t.Fatalf("expected handle to be stopped after readiness failure")
	}
}

func TestManagerEnsureProcessTransportReallocatesPortOnRestart(t *testing.T) {
	defer func(original func() (string, error)) {
		allocateProcessAddressFn = original
	}(allocateProcessAddressFn)
	ports := []string{"127.0.0.1:60001", "127.0.0.1:60002"}
	var mu sync.Mutex
	allocateProcessAddressFn = func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(ports) == 0 {
			return "", errors.New("no more ports")
		}
		addr := ports[0]
		ports = ports[1:]
		return addr, nil
	}
	defer func(original func(context.Context, string) error) {
		waitForAdapterReadyFn = original
	}(waitForAdapterReadyFn)
	waitForAdapterReadyFn = func(context.Context, string) error {
		return nil
	}

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
		Runner:    adapterrunner.NewManager(t.TempDir()),
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("initial ensure: %v", err)
	}
	mu.Lock()
	inst, ok := manager.instances[SlotAI]
	mu.Unlock()
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

	mu.Lock()
	inst, ok = manager.instances[SlotAI]
	mu.Unlock()
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
	defer func(original func() (string, error)) {
		allocateProcessAddressFn = original
	}(allocateProcessAddressFn)
	ports := []string{"127.0.0.1:60005", "127.0.0.1:60006"}
	var mu sync.Mutex
	allocateProcessAddressFn = func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(ports) == 0 {
			return "", errors.New("no ports left")
		}
		addr := ports[0]
		ports = ports[1:]
		return addr, nil
	}
	defer func(original func(context.Context, string) error) {
		waitForAdapterReadyFn = original
	}(waitForAdapterReadyFn)
	waitForAdapterReadyFn = func(context.Context, string) error {
		return nil
	}

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
		Runner:    adapterrunner.NewManager(t.TempDir()),
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	ctx := context.Background()
	if err := manager.Ensure(ctx); err != nil {
		t.Fatalf("initial ensure: %v", err)
	}

	mu.Lock()
	inst, ok := manager.instances[SlotAI]
	mu.Unlock()
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

	mu.Lock()
	inst, ok = manager.instances[SlotAI]
	mu.Unlock()
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
	defer func(original func() (string, error)) {
		allocateProcessAddressFn = original
	}(allocateProcessAddressFn)
	allocateProcessAddressFn = func() (string, error) {
		return "127.0.0.1:60300", nil
	}
	defer func(original func(context.Context, string) error) {
		waitForAdapterReadyFn = original
	}(waitForAdapterReadyFn)
	var capturedDeadline time.Time
	waitForAdapterReadyFn = func(ctx context.Context, _ string) error {
		capturedDeadline, _ = ctx.Deadline()
		return nil
	}

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
		Runner:    adapterrunner.NewManager(filepath.Join(storeDir, "runner")),
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
		Transport: "grpc",
		Address:   "127.0.0.1:9200",
	})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.v2",
		Transport: "grpc",
		Address:   "127.0.0.1:9201",
	})
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Runner:    adapterrunner.NewManager(t.TempDir()),
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
		Transport: "grpc",
		Address:   "127.0.0.1:9300",
	})
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Runner:    adapterrunner.NewManager(t.TempDir()),
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
		Runner:    adapterrunner.NewManager(t.TempDir()),
		PluginDir: t.TempDir(),
	})
	err := manager.Ensure(context.Background())
	if !errors.Is(err, ErrBindingSourceNotConfigured) {
		t.Fatalf("expected ErrBindingSourceNotConfigured, got %v", err)
	}
}

func TestManagerStartAdapterConfiguresEnvironment(t *testing.T) {
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
  mode: external
  entrypoint:
    listenEnv: ADAPTER_LISTEN_ADDR
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
		Transport: "grpc",
		Address:   "127.0.0.1:9500",
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
		Runner:    adapterrunner.NewManager(filepath.Join(tempDir, "runner")),
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
	if addr, ok := envLookup("NUPI_ADAPTER_ENDPOINT"); !ok || addr != endpoint.Address {
		t.Fatalf("expected adapter endpoint %s, got %q", endpoint.Address, addr)
	}
	if listenAddr, ok := envLookup("ADAPTER_LISTEN_ADDR"); !ok || listenAddr != endpoint.Address {
		t.Fatalf("expected ADAPTER_LISTEN_ADDR=%s, got %q", endpoint.Address, listenAddr)
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
		Transport: "grpc",
		Address:   "127.0.0.1:9601",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}
	if endpoint, err := store.GetAdapterEndpoint(ctx, adapter.ID); err != nil {
		t.Fatalf("verify endpoint: %v", err)
	} else if !strings.EqualFold(endpoint.Transport, "grpc") {
		t.Fatalf("expected grpc transport, got %s", endpoint.Transport)
	}

	initialEndpoint := configstore.AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "grpc",
		Address:   "127.0.0.1:9400",
	}
	if err := store.UpsertAdapterEndpoint(ctx, initialEndpoint); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Runner:    adapterrunner.NewManager(filepath.Join(tempDir, "runner")),
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
		Transport: "grpc",
		Address:   "127.0.0.1:9501",
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

func TestManagerEnsureRestartsOnManifestChange(t *testing.T) {
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
		Transport: "grpc",
		Address:   "127.0.0.1:9601",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}
	if endpoint, err := store.GetAdapterEndpoint(ctx, adapter.ID); err != nil {
		t.Fatalf("verify endpoint: %v", err)
	} else if !strings.EqualFold(endpoint.Transport, "grpc") {
		t.Fatalf("expected grpc transport, got %s", endpoint.Transport)
	}

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Runner:    adapterrunner.NewManager(filepath.Join(tempDir, "runner")),
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
  catalog: ai.nupi
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
		Runner:    adapterrunner.NewManager(filepath.Join(tempDir, "runner")),
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

	originalWait := waitForAdapterReadyFn
	t.Cleanup(func() {
		waitForAdapterReadyFn = originalWait
	})
	waitForAdapterReadyFn = func(context.Context, string) error {
		return nil
	}

	originalAlloc := allocateProcessAddressFn
	t.Cleanup(func() {
		allocateProcessAddressFn = originalAlloc
	})
	var portCounter int
	allocateProcessAddressFn = func() (string, error) {
		portCounter++
		return fmt.Sprintf("127.0.0.1:%d", 55050+portCounter), nil
	}

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
  catalog: ai.nupi
spec:
  slot: stt
  mode: local
  entrypoint:
    command: ./adapter
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
		"custom":  "user-value",
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
		Runner:    adapterrunner.NewManager(filepath.Join(tempDir, "runner")),
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
	if val, ok := payload["custom"].(string); !ok || val != "user-value" {
		t.Fatalf("expected custom=user-value, got %#v", payload["custom"])
	}

	if len(payload) != 5 {
		t.Fatalf("expected 5 keys in payload, got %d", len(payload))
	}
}

func TestResolveAdapterConfig(t *testing.T) {
	t.Run("no options returns trimmed copy of config", func(t *testing.T) {
		result, err := resolveAdapterConfig(nil, nil)
		if err != nil {
			t.Fatalf("expected nil result without error, got %v", err)
		}
		if result != nil {
			t.Fatalf("expected nil map for empty input, got %#v", result)
		}

		current := map[string]any{
			"  api_key  ": "secret",
		}
		result, err = resolveAdapterConfig(nil, current)
		if err != nil {
			t.Fatalf("resolveAdapterConfig returned error: %v", err)
		}
		if result["api_key"] != "secret" {
			t.Fatalf("expected api_key preserved, got %#v", result["api_key"])
		}
		if _, exists := result["  api_key  "]; exists {
			t.Fatalf("expected trimmed key in result")
		}
		if &result == &current {
			t.Fatalf("expected result map to be a copy")
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

	t.Run("preserves unknown keys", func(t *testing.T) {
		opts := map[string]manifestpkg.AdapterOption{
			"use_gpu": {Type: "boolean", Default: true},
		}
		current := map[string]any{"custom": "value"}
		result, err := resolveAdapterConfig(opts, current)
		if err != nil {
			t.Fatalf("resolveAdapterConfig returned error: %v", err)
		}
		if result["custom"] != "value" {
			t.Fatalf("expected unknown key preserved, got %#v", result["custom"])
		}
	})
}

func strPtr(v string) *string {
	return &v
}
