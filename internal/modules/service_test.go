package modules

import (
	"context"
	"errors"
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

	bus := eventbus.New()
	sub := bus.Subscribe(eventbus.TopicModulesStatus)
	defer sub.Close()

	svc := NewService(manager, nil, bus, WithEnsureInterval(0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	select {
	case evt := <-sub.C():
		status, ok := evt.Payload.(eventbus.ModuleStatusEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", evt.Payload)
		}
		if status.Status != eventbus.ModuleHealthReady {
			t.Fatalf("expected ready status, got %s", status.Status)
		}
		if status.ModuleID != "adapter.ai.primary" {
			t.Fatalf("unexpected module id %q", status.ModuleID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for module status event")
	}
}

func TestServicePublishesRestartOnConfigChange(t *testing.T) {
	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotSTT),
				Status:    "active",
				AdapterID: strPtr("adapter.stt.primary"),
				Config:    `{"threshold":0.5}`,
			},
		},
	}
	launcher := &fakeLauncher{}
	manager := NewManager(ManagerOptions{
		Store:    store,
		Runner:   adapterrunner.NewManager(t.TempDir()),
		Launcher: launcher,
	})

	bus := eventbus.New()
	sub := bus.Subscribe(eventbus.TopicModulesStatus)
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

	var events []eventbus.ModuleStatusEvent
collect:
	for {
		select {
		case evt := <-sub.C():
			payload, ok := evt.Payload.(eventbus.ModuleStatusEvent)
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
	if events[0].Status != eventbus.ModuleHealthStopped {
		t.Fatalf("expected first event to be stopped, got %s", events[0].Status)
	}
	if events[1].Status != eventbus.ModuleHealthReady {
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
	launcher := &fakeLauncher{err: errors.New("launch failure")}
	manager := NewManager(ManagerOptions{
		Store:    store,
		Runner:   adapterrunner.NewManager(t.TempDir()),
		Launcher: launcher,
	})

	bus := eventbus.New()
	sub := bus.Subscribe(eventbus.TopicModulesStatus)
	defer sub.Close()

	svc := NewService(manager, nil, bus, WithEnsureInterval(0))

	ctx := context.Background()
	_ = svc.reconcile(ctx)

	select {
	case evt := <-sub.C():
		status, ok := evt.Payload.(eventbus.ModuleStatusEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", evt.Payload)
		}
		if status.Status != eventbus.ModuleHealthError {
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
				AdapterID: strPtr("adapter.ai.primary"),
			},
		},
	}
	launcher := &fakeLauncher{err: errors.New("launch failure")}
	manager := NewManager(ManagerOptions{
		Store:    store,
		Runner:   adapterrunner.NewManager(t.TempDir()),
		Launcher: launcher,
	})

	bus := eventbus.New()
	sub := bus.Subscribe(eventbus.TopicModulesStatus)
	defer sub.Close()

	svc := NewService(manager, nil, bus, WithEnsureInterval(0))

	ctx := context.Background()
	_ = svc.reconcile(ctx)

	select {
	case evt := <-sub.C():
		status, ok := evt.Payload.(eventbus.ModuleStatusEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", evt.Payload)
		}
		if status.Status != eventbus.ModuleHealthError {
			t.Fatalf("expected error status, got %s", status.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for error event")
	}

	launcher.mu.Lock()
	launcher.err = nil
	launcher.mu.Unlock()

	if err := svc.reconcile(ctx); err != nil {
		t.Fatalf("reconcile after fix: %v", err)
	}
	select {
	case evt := <-sub.C():
		status := evt.Payload.(eventbus.ModuleStatusEvent)
		if status.Status != eventbus.ModuleHealthReady {
			t.Fatalf("expected ready status, got %s", status.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for ready event")
	}

	launcher.mu.Lock()
	launcher.err = errors.New("launch failure")
	launcher.mu.Unlock()

	store.mu.Lock()
	store.bindings[0].Config = `{"version":2}`
	store.mu.Unlock()

	_ = svc.reconcile(ctx)

	select {
	case evt := <-sub.C():
		status := evt.Payload.(eventbus.ModuleStatusEvent)
		if status.Status != eventbus.ModuleHealthError {
			t.Fatalf("expected repeated error status, got %s", status.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("expected repeated error event")
	}
}
