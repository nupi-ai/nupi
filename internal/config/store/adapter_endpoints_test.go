package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestAdapterEndpointCRUD(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer os.Remove(dbPath)
	defer store.Close()

	ctx := context.Background()

	adapter := Adapter{ID: "adapter.test", Source: "builtin", Type: "ai", Name: "Test Adapter"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	endpoint := AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "grpc",
		Address:   "unix:///tmp/nupi.sock",
		Command:   "/usr/local/bin/adapter",
		Args:      []string{"--flag", "value"},
		Env: map[string]string{
			"API_KEY": "secret",
			"DEBUG":   "true",
		},
	}

	if err := store.UpsertAdapterEndpoint(ctx, endpoint); err != nil {
		t.Fatalf("upsert adapter endpoint: %v", err)
	}

	got, err := store.GetAdapterEndpoint(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("get adapter endpoint: %v", err)
	}

	if got.AdapterID != endpoint.AdapterID {
		t.Fatalf("expected adapter id %q, got %q", endpoint.AdapterID, got.AdapterID)
	}
	if got.Transport != endpoint.Transport {
		t.Fatalf("expected transport %q, got %q", endpoint.Transport, got.Transport)
	}
	if got.Address != endpoint.Address {
		t.Fatalf("expected address %q, got %q", endpoint.Address, got.Address)
	}
	if got.Command != endpoint.Command {
		t.Fatalf("expected command %q, got %q", endpoint.Command, got.Command)
	}
	if !reflect.DeepEqual(got.Args, endpoint.Args) {
		t.Fatalf("expected args %v, got %v", endpoint.Args, got.Args)
	}
	if !reflect.DeepEqual(got.Env, endpoint.Env) {
		t.Fatalf("expected env %v, got %v", endpoint.Env, got.Env)
	}

	endpoint.Command = "/opt/adapter-runner"
	endpoint.Args = []string{"--config", "/etc/nupi/adapter.json"}
	endpoint.Env = map[string]string{"API_KEY": "other"}

	if err := store.UpsertAdapterEndpoint(ctx, endpoint); err != nil {
		t.Fatalf("update adapter endpoint: %v", err)
	}

	list, err := store.ListAdapterEndpoints(ctx)
	if err != nil {
		t.Fatalf("list adapter endpoints: %v", err)
	}

	if len(list) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(list))
	}

	if list[0].Command != endpoint.Command {
		t.Fatalf("expected updated command %q, got %q", endpoint.Command, list[0].Command)
	}
	if !reflect.DeepEqual(list[0].Args, endpoint.Args) {
		t.Fatalf("expected updated args %v, got %v", endpoint.Args, list[0].Args)
	}
	if !reflect.DeepEqual(list[0].Env, endpoint.Env) {
		t.Fatalf("expected updated env %v, got %v", endpoint.Env, list[0].Env)
	}

	if err := store.RemoveAdapterEndpoint(ctx, endpoint.AdapterID); err != nil {
		t.Fatalf("remove adapter endpoint: %v", err)
	}

	if _, err := store.GetAdapterEndpoint(ctx, endpoint.AdapterID); !IsNotFound(err) {
		t.Fatalf("expected not found after removal, got %v", err)
	}
}

func TestWatchDetectsAdapterEndpointChanges(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer os.Remove(dbPath)
	defer store.Close()

	ctx := context.Background()

	adapter := Adapter{ID: "adapter.watch", Source: "builtin", Type: "tts", Name: "Watcher"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	watchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := store.Watch(watchCtx, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("start watch: %v", err)
	}

	if err := store.UpsertAdapterEndpoint(ctx, AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "grpc",
		Address:   "127.0.0.1:9000",
	}); err != nil {
		t.Fatalf("upsert adapter endpoint: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("watch channel closed unexpectedly")
			}
			if ev.AdapterEndpointsChanged {
				return
			}
		case <-deadline:
			t.Fatalf("timeout waiting for adapter endpoint change event")
		}
	}
}

func TestAdapterEndpointRejectsInvalidTransport(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer os.Remove(dbPath)
	defer store.Close()

	ctx := context.Background()

	adapter := Adapter{ID: "adapter.invalid", Source: "builtin", Type: "ai", Name: "Invalid Transport"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	err = store.UpsertAdapterEndpoint(ctx, AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "tcp",
	})
	if err == nil {
		t.Fatal("expected transport validation error, got nil")
	}
}

func TestUpsertAdapterEndpointTLS(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	adapter := Adapter{ID: "adapter.tls", Source: "builtin", Type: "ai", Name: "TLS Adapter"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	endpoint := AdapterEndpoint{
		AdapterID:     adapter.ID,
		Transport:     "grpc",
		Address:       "remote.host:443",
		TLSCertPath:   "/etc/nupi/client.crt",
		TLSKeyPath:    "/etc/nupi/client.key",
		TLSCACertPath: "/etc/nupi/ca.pem",
		TLSInsecure:   true,
	}

	if err := store.UpsertAdapterEndpoint(ctx, endpoint); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Verify round-trip via Get
	got, err := store.GetAdapterEndpoint(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TLSCertPath != endpoint.TLSCertPath {
		t.Fatalf("expected TLSCertPath %q, got %q", endpoint.TLSCertPath, got.TLSCertPath)
	}
	if got.TLSKeyPath != endpoint.TLSKeyPath {
		t.Fatalf("expected TLSKeyPath %q, got %q", endpoint.TLSKeyPath, got.TLSKeyPath)
	}
	if got.TLSCACertPath != endpoint.TLSCACertPath {
		t.Fatalf("expected TLSCACertPath %q, got %q", endpoint.TLSCACertPath, got.TLSCACertPath)
	}
	if !got.TLSInsecure {
		t.Fatal("expected TLSInsecure=true")
	}

	// Verify round-trip via List
	list, err := store.ListAdapterEndpoints(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(list))
	}
	if list[0].TLSCertPath != endpoint.TLSCertPath {
		t.Fatalf("list: expected TLSCertPath %q, got %q", endpoint.TLSCertPath, list[0].TLSCertPath)
	}
	if list[0].TLSKeyPath != endpoint.TLSKeyPath {
		t.Fatalf("list: expected TLSKeyPath %q, got %q", endpoint.TLSKeyPath, list[0].TLSKeyPath)
	}
	if list[0].TLSCACertPath != endpoint.TLSCACertPath {
		t.Fatalf("list: expected TLSCACertPath %q, got %q", endpoint.TLSCACertPath, list[0].TLSCACertPath)
	}
	if !list[0].TLSInsecure {
		t.Fatal("list: expected TLSInsecure=true")
	}

	// Update to clear TLS fields
	endpoint.TLSCertPath = ""
	endpoint.TLSKeyPath = ""
	endpoint.TLSCACertPath = ""
	endpoint.TLSInsecure = false

	if err := store.UpsertAdapterEndpoint(ctx, endpoint); err != nil {
		t.Fatalf("update: %v", err)
	}

	got2, err := store.GetAdapterEndpoint(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got2.TLSCertPath != "" {
		t.Fatalf("expected empty TLSCertPath, got %q", got2.TLSCertPath)
	}
	if got2.TLSInsecure {
		t.Fatal("expected TLSInsecure=false after update")
	}
}

func TestAdapterEndpointTLSDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	adapter := Adapter{ID: "adapter.defaults", Source: "builtin", Type: "stt", Name: "Defaults"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	// Insert without TLS fields — should default to empty/false.
	if err := store.UpsertAdapterEndpoint(ctx, AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "process",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.GetAdapterEndpoint(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TLSCertPath != "" {
		t.Fatalf("expected empty TLSCertPath, got %q", got.TLSCertPath)
	}
	if got.TLSKeyPath != "" {
		t.Fatalf("expected empty TLSKeyPath, got %q", got.TLSKeyPath)
	}
	if got.TLSCACertPath != "" {
		t.Fatalf("expected empty TLSCACertPath, got %q", got.TLSCACertPath)
	}
	if got.TLSInsecure {
		t.Fatal("expected TLSInsecure=false")
	}
}
