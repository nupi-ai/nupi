package adapters

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/adapterrunner"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

func TestServicePublishesStatusOnStart(t *testing.T) {
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
		Transport: "grpc",
		Address:   "127.0.0.1:9400",
	})
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Runner:    adapterrunner.NewManager(t.TempDir()),
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	bus := eventbus.New()
	sub := bus.Subscribe(eventbus.TopicAdaptersStatus)
	defer sub.Close()

	svc := NewService(manager, nil, bus, WithEnsureInterval(0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	select {
	case evt := <-sub.C():
		status, ok := evt.Payload.(eventbus.AdapterStatusEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", evt.Payload)
		}
		if status.Status != eventbus.AdapterHealthReady {
			t.Fatalf("expected ready status, got %s", status.Status)
		}
		if status.AdapterID != "adapter.ai" {
			t.Fatalf("unexpected adapter id %q", status.AdapterID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for adapter status event")
	}
}

func TestServicePublishesRestartOnConfigChange(t *testing.T) {
	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotSTT),
				Status:    "active",
				AdapterID: strPtr("adapter.stt"),
				Config:    `{"threshold":0.5}`,
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.stt"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.stt",
		Transport: "grpc",
		Address:   "127.0.0.1:9500",
	})
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Runner:    adapterrunner.NewManager(t.TempDir()),
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	bus := eventbus.New()
	sub := bus.Subscribe(eventbus.TopicAdaptersStatus)
	defer sub.Close()

	svc := NewService(manager, nil, bus, WithEnsureInterval(0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// drain initial event
	select {
	case <-sub.C():
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for initial event")
	}

	store.mu.Lock()
	store.bindings[0].Config = `{"threshold":0.8}`
	store.mu.Unlock()

	if err := svc.reconcile(ctx); err != nil {
		t.Fatalf("reconcile after config change: %v", err)
	}

	var events []eventbus.AdapterStatusEvent
collect:
	for {
		select {
		case evt := <-sub.C():
			payload, ok := evt.Payload.(eventbus.AdapterStatusEvent)
			if !ok {
				t.Fatalf("unexpected payload type %T", evt.Payload)
			}
			events = append(events, payload)
			if len(events) >= 2 {
				break collect
			}
		case <-time.After(time.Second):
			break collect
		}
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events (stop + start), got %d", len(events))
	}
	if events[0].Status != eventbus.AdapterHealthStopped {
		t.Fatalf("expected first event to be stopped, got %s", events[0].Status)
	}
	if events[1].Status != eventbus.AdapterHealthReady {
		t.Fatalf("expected second event to be ready, got %s", events[1].Status)
	}
}

func TestServicePublishesErrorOnEnsureFailure(t *testing.T) {
	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotTTS),
				Status:    "active",
				AdapterID: strPtr("adapter.tts.fail"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.tts.fail"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.fail",
		Transport: "grpc",
		Address:   "127.0.0.1:9600",
	})
	launcher := NewMockLauncher()
	launcher.SetError(errors.New("launch failure"))
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Runner:    adapterrunner.NewManager(t.TempDir()),
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	bus := eventbus.New()
	sub := bus.Subscribe(eventbus.TopicAdaptersStatus)
	defer sub.Close()

	svc := NewService(manager, nil, bus, WithEnsureInterval(0))

	ctx := context.Background()
	_ = svc.reconcile(ctx)

	select {
	case evt := <-sub.C():
		status, ok := evt.Payload.(eventbus.AdapterStatusEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", evt.Payload)
		}
		if status.Status != eventbus.AdapterHealthError {
			t.Fatalf("expected error status, got %s", status.Status)
		}
		if status.Message == "" {
			t.Fatalf("expected error message to be populated")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for error event")
	}
}

func TestServiceErrorCacheClearedOnRecovery(t *testing.T) {
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
		Transport: "grpc",
		Address:   "127.0.0.1:9700",
	})
	launcher := NewMockLauncher()
	launcher.SetError(errors.New("launch failure"))
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Runner:    adapterrunner.NewManager(t.TempDir()),
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	bus := eventbus.New()
	sub := bus.Subscribe(eventbus.TopicAdaptersStatus)
	defer sub.Close()

	svc := NewService(manager, nil, bus, WithEnsureInterval(0))

	ctx := context.Background()
	_ = svc.reconcile(ctx)

	select {
	case evt := <-sub.C():
		status, ok := evt.Payload.(eventbus.AdapterStatusEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", evt.Payload)
		}
		if status.Status != eventbus.AdapterHealthError {
			t.Fatalf("expected error status, got %s", status.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for error event")
	}

	launcher.SetError(nil)

	if err := svc.reconcile(ctx); err != nil {
		t.Fatalf("reconcile after fix: %v", err)
	}
	select {
	case evt := <-sub.C():
		status := evt.Payload.(eventbus.AdapterStatusEvent)
		if status.Status != eventbus.AdapterHealthReady {
			t.Fatalf("expected ready status, got %s", status.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for ready event")
	}

	launcher.SetError(errors.New("launch failure"))

	store.mu.Lock()
	store.bindings[0].Config = `{"version":2}`
	store.mu.Unlock()

	_ = svc.reconcile(ctx)

	select {
	case evt := <-sub.C():
		status := evt.Payload.(eventbus.AdapterStatusEvent)
		if status.Status != eventbus.AdapterHealthError {
			t.Fatalf("expected repeated error status, got %s", status.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("expected repeated error event")
	}
}

func TestServiceOverviewStartStop(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	store, err := configstore.Open(configstore.Options{DBPath: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer os.Remove(dbPath)
	defer store.Close()

	ctx := context.Background()

	adapter := configstore.Adapter{ID: "adapter.ai", Source: "builtin", Type: "ai", Name: "Primary AI"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(SlotAI), adapter.ID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "grpc",
		Address:   "127.0.0.1:9800",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Runner:    adapterrunner.NewManager(t.TempDir()),
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	bus := eventbus.New()
	svc := NewService(manager, store, bus, WithEnsureInterval(0))

	if err := svc.reconcile(ctx); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	overview, err := svc.Overview(ctx)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if len(overview) == 0 {
		t.Fatalf("expected overview entries")
	}

	var ai BindingStatus
	var found bool
	for _, entry := range overview {
		if entry.Slot == SlotAI {
			ai = entry
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ai slot not found in overview")
	}
	if ai.Status != configstore.BindingStatusActive {
		t.Fatalf("expected status active, got %s", ai.Status)
	}
	if ai.Runtime == nil || ai.Runtime.Health != eventbus.AdapterHealthReady {
		t.Fatalf("expected runtime health ready, got %+v", ai.Runtime)
	}

	stopped, err := svc.StopSlot(ctx, SlotAI)
	if err != nil {
		t.Fatalf("stop slot: %v", err)
	}
	if stopped.Status != configstore.BindingStatusInactive {
		t.Fatalf("expected status inactive after stop, got %s", stopped.Status)
	}
	if stopped.Runtime == nil || stopped.Runtime.Health != eventbus.AdapterHealthStopped {
		t.Fatalf("expected runtime stopped after stop, got %+v", stopped.Runtime)
	}

	started, err := svc.StartSlot(ctx, SlotAI)
	if err != nil {
		t.Fatalf("start slot: %v", err)
	}
	if started.Status != configstore.BindingStatusActive {
		t.Fatalf("expected status active after start, got %s", started.Status)
	}
	if started.Runtime == nil || started.Runtime.Health != eventbus.AdapterHealthReady {
		t.Fatalf("expected runtime ready after start, got %+v", started.Runtime)
	}
}
