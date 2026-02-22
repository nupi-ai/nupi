package adapters

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/napdial"
)

func TestServicePublishesStatusOnStart(t *testing.T) {
	// Mock readiness so remote adapters don't try to actually connect
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

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
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	bus := eventbus.New()
	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status)
	defer sub.Close()

	svc := NewService(manager, nil, bus, WithEnsureInterval(0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	select {
	case evt := <-sub.C():
		status := evt.Payload
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
	// Mock readiness so remote adapters don't try to actually connect
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

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
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	bus := eventbus.New()
	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status)
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
			payload := evt.Payload
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
	// Use process transport to test launch failures
	// (grpc transport doesn't launch a process, so launcher errors won't occur)
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
		Transport: "process",
		Command:   "./mock-adapter",
	})
	launcher := NewMockLauncher()
	launcher.SetError(errors.New("launch failure"))
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	bus := eventbus.New()
	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status)
	defer sub.Close()

	svc := NewService(manager, nil, bus, WithEnsureInterval(0))

	ctx := context.Background()
	_ = svc.reconcile(ctx)

	select {
	case evt := <-sub.C():
		status := evt.Payload
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

func TestServicePublishesErrorOnRemoteHealthCheckFailure(t *testing.T) {
	// Verify that when a remote (gRPC) adapter's health check fails,
	// the failure reason propagates to adapters.status event bus.
	// This is the end-to-end path: health check error → startAdapter → Ensure → reconcile → publishError.
	t.Cleanup(SetReadinessChecker(func(_ context.Context, _ string, transport string, _ *napdial.TLSConfig) error {
		if transport == "grpc" {
			return errors.New("grpc health check: status NOT_SERVING (expected SERVING)")
		}
		return nil
	}))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotAI),
				Status:    "active",
				AdapterID: strPtr("adapter.ai.unhealthy"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.ai.unhealthy"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.ai.unhealthy",
		Transport: "grpc",
		Address:   "127.0.0.1:9999",
	})
	bus := eventbus.New()
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		Bus:       bus,
		PluginDir: t.TempDir(),
	})

	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status)
	defer sub.Close()

	svc := NewService(manager, nil, bus, WithEnsureInterval(0))

	ctx := context.Background()
	_ = svc.reconcile(ctx)

	// logSlotError publishes a per-slot event (with AdapterID and Slot)
	// before publishError publishes a catch-all event. Verify the first event.
	select {
	case evt := <-sub.C():
		status := evt.Payload
		if status.Status != eventbus.AdapterHealthError {
			t.Fatalf("expected error status, got %s", status.Status)
		}
		if status.Message == "" {
			t.Fatal("expected error message to be populated")
		}
		if !strings.Contains(status.Message, "NOT_SERVING") {
			t.Fatalf("expected error message to contain health check failure reason, got: %s", status.Message)
		}
		if status.AdapterID != "adapter.ai.unhealthy" {
			t.Fatalf("expected AdapterID adapter.ai.unhealthy, got %q", status.AdapterID)
		}
		if status.Slot != string(SlotAI) {
			t.Fatalf("expected Slot %s, got %q", SlotAI, status.Slot)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for health check error event")
	}
}

func TestServicePublishesErrorOnHTTPHealthCheckFailure(t *testing.T) {
	// Verify that when a remote (HTTP) adapter's health check fails,
	// the failure reason propagates to adapters.status event bus.
	t.Cleanup(SetReadinessChecker(func(_ context.Context, _ string, transport string, _ *napdial.TLSConfig) error {
		if transport == "http" {
			return errors.New("http health check: adapter does not expose /health endpoint — remote HTTP adapters must implement GET /health (got 404)")
		}
		return nil
	}))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotTTS),
				Status:    "active",
				AdapterID: strPtr("adapter.tts.nohealth"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.tts.nohealth"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.nohealth",
		Transport: "http",
		Address:   "127.0.0.1:8080",
	})
	bus := eventbus.New()
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		Bus:       bus,
		PluginDir: t.TempDir(),
	})

	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status)
	defer sub.Close()

	svc := NewService(manager, nil, bus, WithEnsureInterval(0))

	ctx := context.Background()
	_ = svc.reconcile(ctx)

	select {
	case evt := <-sub.C():
		status := evt.Payload
		if status.Status != eventbus.AdapterHealthError {
			t.Fatalf("expected error status, got %s", status.Status)
		}
		if !strings.Contains(status.Message, "/health") {
			t.Fatalf("expected error message to mention /health, got: %s", status.Message)
		}
		if status.AdapterID != "adapter.tts.nohealth" {
			t.Fatalf("expected AdapterID adapter.tts.nohealth, got %q", status.AdapterID)
		}
		if status.Slot != string(SlotTTS) {
			t.Fatalf("expected Slot %s, got %q", SlotTTS, status.Slot)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for health check error event")
	}
}

func TestServiceErrorCacheClearedOnRecovery(t *testing.T) {
	// Use process transport to test launch failures
	// (grpc transport doesn't launch a process, so launcher errors won't occur)

	// Mock readiness to succeed immediately when process is launched
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

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
		Command:   "./mock-adapter",
	})
	launcher := NewMockLauncher()
	launcher.SetError(errors.New("launch failure"))
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	bus := eventbus.New()
	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status)
	defer sub.Close()

	svc := NewService(manager, nil, bus, WithEnsureInterval(0))

	ctx := context.Background()
	_ = svc.reconcile(ctx)

	select {
	case evt := <-sub.C():
		status := evt.Payload
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
		status := evt.Payload
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
		status := evt.Payload
		if status.Status != eventbus.AdapterHealthError {
			t.Fatalf("expected repeated error status, got %s", status.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("expected repeated error event")
	}
}

func TestServiceErrorDeduplication(t *testing.T) {
	// Verify that consecutive identical reconciliation errors publish only
	// one event — the second duplicate is suppressed by publishError.
	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotTTS),
				Status:    "active",
				AdapterID: strPtr("adapter.tts.flaky"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.tts.flaky"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.flaky",
		Transport: "process",
		Command:   "./mock-adapter",
	})
	launcher := NewMockLauncher()
	launcher.SetError(errors.New("persistent failure"))
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		PluginDir: t.TempDir(),
	})

	bus := eventbus.New()
	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status)
	defer sub.Close()

	svc := NewService(manager, nil, bus, WithEnsureInterval(0))

	ctx := context.Background()

	// First reconcile — should publish error event.
	_ = svc.reconcile(ctx)
	select {
	case evt := <-sub.C():
		if evt.Payload.Status != eventbus.AdapterHealthError {
			t.Fatalf("expected error status on first reconcile, got %s", evt.Payload.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for first error event")
	}

	// Second reconcile with same error — should be suppressed (deduplicated).
	_ = svc.reconcile(ctx)
	select {
	case evt := <-sub.C():
		t.Fatalf("expected no event on duplicate error, but got status=%s msg=%q", evt.Payload.Status, evt.Payload.Message)
	case <-time.After(200 * time.Millisecond):
		// Expected: no event published for duplicate error.
	}

	// Third reconcile with a different error — should publish again.
	launcher.SetError(errors.New("different failure"))
	_ = svc.reconcile(ctx)
	select {
	case evt := <-sub.C():
		if evt.Payload.Status != eventbus.AdapterHealthError {
			t.Fatalf("expected error status on changed error, got %s", evt.Payload.Status)
		}
		if !strings.Contains(evt.Payload.Message, "different failure") {
			t.Fatalf("expected new error message, got %q", evt.Payload.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for changed error event")
	}
}

func TestServiceOverviewStartStop(t *testing.T) {
	// Mock readiness so remote adapters don't try to actually connect
	t.Cleanup(SetReadinessChecker(func(context.Context, string, string, *napdial.TLSConfig) error { return nil }))

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

func TestServicePublishesErrorOnProcessReadinessFailure(t *testing.T) {
	// Verify that when a process adapter's readiness check (TCP dial) fails,
	// the failure reason propagates to adapters.status with correct AdapterID and Slot.
	// Symmetric with TestServicePublishesErrorOnRemoteHealthCheckFailure (gRPC)
	// and TestServicePublishesErrorOnHTTPHealthCheckFailure (HTTP).
	t.Cleanup(SetAllocateProcessAddress(func() (string, error) {
		return "127.0.0.1:60777", nil
	}))
	t.Cleanup(SetReadinessChecker(func(_ context.Context, _ string, transport string, _ *napdial.TLSConfig) error {
		if transport == "process" {
			return errors.New("dial 127.0.0.1:60777: connection refused")
		}
		return nil
	}))

	store := &fakeBindingSource{
		bindings: []configstore.AdapterBinding{
			{
				Slot:      string(SlotSTT),
				Status:    "active",
				AdapterID: strPtr("adapter.stt.local"),
			},
		},
	}
	store.setAdapter(configstore.Adapter{ID: "adapter.stt.local"})
	store.setEndpoint(configstore.AdapterEndpoint{
		AdapterID: "adapter.stt.local",
		Transport: "process",
		Command:   "./mock-adapter",
	})
	bus := eventbus.New()
	launcher := NewMockLauncher()
	manager := NewManager(ManagerOptions{
		Store:     store,
		Adapters:  store,
		Launcher:  launcher,
		Bus:       bus,
		PluginDir: t.TempDir(),
	})

	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Status)
	defer sub.Close()

	svc := NewService(manager, nil, bus, WithEnsureInterval(0))

	ctx := context.Background()
	_ = svc.reconcile(ctx)

	select {
	case evt := <-sub.C():
		status := evt.Payload
		if status.Status != eventbus.AdapterHealthError {
			t.Fatalf("expected error status, got %s", status.Status)
		}
		if status.Message == "" {
			t.Fatal("expected error message to be populated")
		}
		if !strings.Contains(status.Message, "readiness") {
			t.Fatalf("expected error message to contain readiness failure reason, got: %s", status.Message)
		}
		if status.AdapterID != "adapter.stt.local" {
			t.Fatalf("expected AdapterID adapter.stt.local, got %q", status.AdapterID)
		}
		if status.Slot != string(SlotSTT) {
			t.Fatalf("expected Slot %s, got %q", SlotSTT, status.Slot)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for process readiness failure event")
	}
}
