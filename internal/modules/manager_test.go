package modules

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nupi-ai/nupi/internal/adapterrunner"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
)

type fakeBindingSource struct {
	mu       sync.Mutex
	bindings []configstore.AdapterBinding
	err      error
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

func TestManagerEnsureStartsModules(t *testing.T) {
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
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
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
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
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
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
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
		t.Fatalf("expected module to restart on config change, got %d launches", len(records))
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

func TestManagerStartModuleConfiguresEnvironment(t *testing.T) {
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
  name: Sample Module
  slug: Example Module
spec:
  moduleType: ai
  mode: external
  entrypoint:
    listenEnv: MODULE_LISTEN_ADDR
  assets:
    models:
      cacheDirEnv: MODULE_CACHE_DIR
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
	endpoint := configstore.ModuleEndpoint{
		AdapterID: adapter.ID,
		Transport: "grpc",
		Address:   "127.0.0.1:9500",
		Command:   "serve",
		Args:      []string{"--foo"},
		Env: map[string]string{
			"CUSTOM_FLAG": "1",
		},
	}
	if err := store.UpsertModuleEndpoint(ctx, endpoint); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	pluginDir := filepath.Join(tempDir, "plugins")
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
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

	if slot, ok := envLookup("NUPI_MODULE_SLOT"); !ok || slot != string(SlotAI) {
		t.Fatalf("expected NUPI_MODULE_SLOT=%s, got %q", SlotAI, slot)
	}
	if adapterID, ok := envLookup("NUPI_MODULE_ADAPTER"); !ok || adapterID != adapter.ID {
		t.Fatalf("expected NUPI_MODULE_ADAPTER=%s, got %q", adapter.ID, adapterID)
	}
	if cfg, ok := envLookup("NUPI_MODULE_CONFIG"); !ok || cfg != `{"token":"abc"}` {
		t.Fatalf("expected NUPI_MODULE_CONFIG, got %q", cfg)
	}
	home, ok := envLookup("NUPI_MODULE_HOME")
	if !ok {
		t.Fatalf("missing NUPI_MODULE_HOME in env")
	}
	expectedDir := filepath.Join(pluginDir, "example-module")
	if home != expectedDir {
		t.Fatalf("unexpected module home: %s (expected %s)", home, expectedDir)
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		t.Fatalf("module home directory not created: %v", err)
	}
	if manifestPath, ok := envLookup("NUPI_MODULE_MANIFEST_PATH"); !ok || manifestPath == "" {
		t.Fatalf("missing NUPI_MODULE_MANIFEST_PATH")
	} else {
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		if strings.TrimSpace(string(data)) != strings.TrimSpace(manifestYAML) {
			t.Fatalf("manifest contents mismatch")
		}
	}

	dataDir, ok := envLookup("NUPI_MODULE_DATA_DIR")
	if !ok {
		t.Fatalf("missing NUPI_MODULE_DATA_DIR")
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("data dir not created: %v", err)
	}
	if cacheDir, ok := envLookup("MODULE_CACHE_DIR"); !ok || cacheDir != dataDir {
		t.Fatalf("expected MODULE_CACHE_DIR=%s, got %q", dataDir, cacheDir)
	}
	if transport, ok := envLookup("NUPI_MODULE_TRANSPORT"); !ok || transport != endpoint.Transport {
		t.Fatalf("expected transport %s, got %q", endpoint.Transport, transport)
	}
	if addr, ok := envLookup("NUPI_MODULE_ENDPOINT"); !ok || addr != endpoint.Address {
		t.Fatalf("expected module endpoint %s, got %q", endpoint.Address, addr)
	}
	if listenAddr, ok := envLookup("MODULE_LISTEN_ADDR"); !ok || listenAddr != endpoint.Address {
		t.Fatalf("expected MODULE_LISTEN_ADDR=%s, got %q", endpoint.Address, listenAddr)
	}
	if cmd, ok := envLookup("NUPI_MODULE_COMMAND"); !ok || cmd != endpoint.Command {
		t.Fatalf("expected command %s, got %q", endpoint.Command, cmd)
	}
	if args, ok := envLookup("NUPI_MODULE_ARGS"); !ok || args != `["--foo"]` {
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

	initialEndpoint := configstore.ModuleEndpoint{
		AdapterID: adapter.ID,
		Transport: "grpc",
		Address:   "127.0.0.1:9400",
	}
	if err := store.UpsertModuleEndpoint(ctx, initialEndpoint); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
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

	updatedEndpoint := configstore.ModuleEndpoint{
		AdapterID: adapter.ID,
		Transport: "grpc",
		Address:   "127.0.0.1:9501",
	}
	if err := store.UpsertModuleEndpoint(ctx, updatedEndpoint); err != nil {
		t.Fatalf("update endpoint: %v", err)
	}

	if err := manager.Ensure(ctx); err != nil {
		t.Fatalf("ensure after endpoint change: %v", err)
	}
	if len(launcher.Records()) != 2 {
		t.Fatalf("expected relaunch after endpoint change, got %d", len(launcher.Records()))
	}
	if launcher.StopCount(string(SlotAI)) != 1 {
		t.Fatalf("expected module stop before restart")
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
  moduleType: ai
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

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
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
  moduleType: ai
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
		t.Fatalf("expected module stop before restart")
	}
}

func strPtr(v string) *string {
	return &v
}
