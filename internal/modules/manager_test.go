package modules

import (
	"context"
	"errors"
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
				AdapterID: strPtr("adapter.ai.primary"),
			},
		},
	}
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:    store,
		Runner:   adapterrunner.NewManager(t.TempDir()),
		Launcher: launcher,
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
	expectArgs := []string{"--slot", string(SlotAI), "--adapter", "adapter.ai.primary"}
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
		Store:    store,
		Runner:   adapterrunner.NewManager(t.TempDir()),
		Launcher: launcher,
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
				AdapterID: strPtr("adapter.ai.primary"),
				Config:    `{"token":"first"}`,
			},
		},
	}
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:    store,
		Runner:   adapterrunner.NewManager(t.TempDir()),
		Launcher: launcher,
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
		Runner: adapterrunner.NewManager(t.TempDir()),
	})
	err := manager.Ensure(context.Background())
	if !errors.Is(err, ErrBindingSourceNotConfigured) {
		t.Fatalf("expected ErrBindingSourceNotConfigured, got %v", err)
	}
}

func strPtr(v string) *string {
	return &v
}
