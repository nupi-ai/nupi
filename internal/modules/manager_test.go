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

type launchCall struct {
	Binary string
	Args   []string
	Env    []string
}

type fakeLauncher struct {
	mu      sync.Mutex
	calls   []launchCall
	handles []*fakeHandle
	err     error
}

func (f *fakeLauncher) Launch(ctx context.Context, binary string, args []string, env []string) (ProcessHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	call := launchCall{
		Binary: binary,
		Args:   append([]string(nil), args...),
		Env:    append([]string(nil), env...),
	}
	f.calls = append(f.calls, call)
	handle := &fakeHandle{}
	f.handles = append(f.handles, handle)
	return handle, nil
}

type fakeHandle struct {
	mu        sync.Mutex
	stopCount int
	pid       int
	err       error
}

func (f *fakeHandle) Stop(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCount++
	return f.err
}

func (f *fakeHandle) PID() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pid == 0 {
		f.pid = 1234
	}
	return f.pid
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
	launcher := &fakeLauncher{}
	manager := NewManager(ManagerOptions{
		Store:    store,
		Runner:   adapterrunner.NewManager(t.TempDir()),
		Launcher: launcher,
	})

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}
	if len(launcher.calls) != 1 {
		t.Fatalf("expected 1 launch call, got %d", len(launcher.calls))
	}

	call := launcher.calls[0]
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
	if len(launcher.calls) != 1 {
		t.Fatalf("expected no additional launches when configuration unchanged")
	}

	store.mu.Lock()
	store.bindings = nil
	store.mu.Unlock()

	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure after removing bindings returned error: %v", err)
	}

	launcher.mu.Lock()
	if len(launcher.handles) != 1 {
		t.Fatalf("expected one handle, got %d", len(launcher.handles))
	}
	if launcher.handles[0].stopCount == 0 {
		t.Fatalf("expected handle to be stopped")
	}
	launcher.mu.Unlock()
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
	launcher := &fakeLauncher{}
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

	launcher.mu.Lock()
	if len(launcher.calls) != 2 {
		t.Fatalf("expected 2 launches (initial + restart), got %d", len(launcher.calls))
	}
	if launcher.handles[0].stopCount == 0 {
		t.Fatalf("expected previous handle to be stopped on adapter change")
	}
	launcher.mu.Unlock()
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
	launcher := &fakeLauncher{}
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

	launcher.mu.Lock()
	if len(launcher.calls) != 2 {
		t.Fatalf("expected module to restart on config change, got %d launches", len(launcher.calls))
	}
	if launcher.handles[0].stopCount == 0 {
		t.Fatalf("expected previous handle to be stopped on config change")
	}
	launcher.mu.Unlock()
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
