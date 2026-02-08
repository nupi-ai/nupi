package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestAdapterBindingPersistsAcrossStoreReopen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "config.db")
	ctx := context.Background()

	// Open store, register adapter, bind to slot.
	store1, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	adapter := Adapter{ID: "adapter.ai.persist", Source: "builtin", Type: "ai", Name: "Persist AI", Version: "1.0.0"}
	if err := store1.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store1.SetActiveAdapter(ctx, "ai", adapter.ID, map[string]any{"model": "claude"}); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close store before reopen: %v", err)
	}

	// Re-open store and verify persistence.
	store2, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { store2.Close() })

	bindings, err := store2.ListAdapterBindings(ctx)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}

	var found bool
	for _, b := range bindings {
		if b.Slot == "ai" {
			found = true
			if b.AdapterID == nil || *b.AdapterID != adapter.ID {
				t.Fatalf("expected adapter %q, got %v", adapter.ID, b.AdapterID)
			}
			if b.Status != BindingStatusActive {
				t.Fatalf("expected status active, got %s", b.Status)
			}
			if b.Config == "" || b.Config == "{}" {
				t.Fatalf("expected config to be persisted, got %q", b.Config)
			}
			var cfgMap map[string]interface{}
			if err := json.Unmarshal([]byte(b.Config), &cfgMap); err != nil {
				t.Fatalf("parse persisted config %q: %v", b.Config, err)
			}
			if cfgMap["model"] != "claude" {
				t.Fatalf("expected config model=claude, got %v", cfgMap)
			}
			break
		}
	}
	if !found {
		t.Fatal("ai binding not found after reopen")
	}
}

func TestAdapterEndpointPersistsAcrossStoreReopen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "config.db")
	ctx := context.Background()

	store1, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	adapter := Adapter{ID: "adapter.stt.ep", Source: "ext", Type: "stt", Name: "EP STT"}
	if err := store1.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	endpoint := AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "grpc",
		Address:   "localhost:50051",
		Command:   "/usr/local/bin/whisper",
		Args:      []string{"--model", "base"},
		Env:       map[string]string{"WHISPER_MODEL": "base"},
	}
	if err := store1.UpsertAdapterEndpoint(ctx, endpoint); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close store before reopen: %v", err)
	}

	// Re-open and verify.
	store2, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { store2.Close() })

	loaded, err := store2.GetAdapterEndpoint(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("get endpoint: %v", err)
	}
	if loaded.Transport != "grpc" {
		t.Fatalf("expected transport grpc, got %s", loaded.Transport)
	}
	if loaded.Address != "localhost:50051" {
		t.Fatalf("expected address localhost:50051, got %s", loaded.Address)
	}
	if loaded.Command != "/usr/local/bin/whisper" {
		t.Fatalf("expected command /usr/local/bin/whisper, got %s", loaded.Command)
	}
	if len(loaded.Args) != 2 || loaded.Args[0] != "--model" || loaded.Args[1] != "base" {
		t.Fatalf("expected args [--model base], got %v", loaded.Args)
	}
	if loaded.Env["WHISPER_MODEL"] != "base" {
		t.Fatalf("expected env WHISPER_MODEL=base, got %v", loaded.Env)
	}
}

func TestSecuritySettingsPersistAcrossStoreReopen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "config.db")
	ctx := context.Background()

	store1, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store1.SaveSecuritySettings(ctx, map[string]string{
		"elevenlabs_key": "sk-persist-test",
	}); err != nil {
		t.Fatalf("save security: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close store before reopen: %v", err)
	}

	// Re-open and verify.
	store2, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { store2.Close() })

	loaded, err := store2.LoadSecuritySettings(ctx, "elevenlabs_key")
	if err != nil {
		t.Fatalf("load security: %v", err)
	}
	if loaded["elevenlabs_key"] != "sk-persist-test" {
		t.Fatalf("expected sk-persist-test, got %q", loaded["elevenlabs_key"])
	}
}

func TestTransportConfigPersistsAcrossStoreReopen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "config.db")
	ctx := context.Background()

	store1, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := TransportConfig{
		Port:           8080,
		Binding:        "loopback",
		GRPCPort:       50051,
		GRPCBinding:    "loopback",
		AllowedOrigins: []string{"http://localhost:3000"},
	}
	if err := store1.SaveTransportConfig(ctx, cfg); err != nil {
		t.Fatalf("save transport config: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close store before reopen: %v", err)
	}

	// Re-open and verify.
	store2, err := Open(Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { store2.Close() })

	loaded, err := store2.GetTransportConfig(ctx)
	if err != nil {
		t.Fatalf("get transport config: %v", err)
	}
	if loaded.Port != 8080 {
		t.Fatalf("expected port 8080, got %d", loaded.Port)
	}
	if loaded.Binding != "loopback" {
		t.Fatalf("expected binding loopback, got %s", loaded.Binding)
	}
	if loaded.GRPCPort != 50051 {
		t.Fatalf("expected grpc_port 50051, got %d", loaded.GRPCPort)
	}
	if loaded.GRPCBinding != "loopback" {
		t.Fatalf("expected grpc_binding loopback, got %s", loaded.GRPCBinding)
	}
	if len(loaded.AllowedOrigins) != 1 || loaded.AllowedOrigins[0] != "http://localhost:3000" {
		t.Fatalf("expected allowed_origins [http://localhost:3000], got %v", loaded.AllowedOrigins)
	}
}
