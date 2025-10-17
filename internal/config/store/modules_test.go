package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestModuleEndpointCRUD(t *testing.T) {
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

	endpoint := ModuleEndpoint{
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

	if err := store.UpsertModuleEndpoint(ctx, endpoint); err != nil {
		t.Fatalf("upsert module endpoint: %v", err)
	}

	got, err := store.GetModuleEndpoint(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("get module endpoint: %v", err)
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
	endpoint.Args = []string{"--config", "/etc/nupi/module.json"}
	endpoint.Env = map[string]string{"API_KEY": "other"}

	if err := store.UpsertModuleEndpoint(ctx, endpoint); err != nil {
		t.Fatalf("update module endpoint: %v", err)
	}

	list, err := store.ListModuleEndpoints(ctx)
	if err != nil {
		t.Fatalf("list module endpoints: %v", err)
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

	if err := store.RemoveModuleEndpoint(ctx, endpoint.AdapterID); err != nil {
		t.Fatalf("remove module endpoint: %v", err)
	}

	if _, err := store.GetModuleEndpoint(ctx, endpoint.AdapterID); !IsNotFound(err) {
		t.Fatalf("expected not found after removal, got %v", err)
	}
}

func TestWatchDetectsModuleEndpointChanges(t *testing.T) {
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

	if err := store.UpsertModuleEndpoint(ctx, ModuleEndpoint{
		AdapterID: adapter.ID,
		Transport: "grpc",
		Address:   "127.0.0.1:9000",
	}); err != nil {
		t.Fatalf("upsert module endpoint: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("watch channel closed unexpectedly")
			}
			if ev.ModuleEndpointsChanged {
				return
			}
		case <-deadline:
			t.Fatalf("timeout waiting for module endpoint change event")
		}
	}
}

func TestModuleEndpointRejectsInvalidTransport(t *testing.T) {
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

	err = store.UpsertModuleEndpoint(ctx, ModuleEndpoint{
		AdapterID: adapter.ID,
		Transport: "tcp",
	})
	if err == nil {
		t.Fatal("expected transport validation error, got nil")
	}
}
