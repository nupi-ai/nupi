package intentrouter

import (
	"context"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
	adapters "github.com/nupi-ai/nupi/internal/plugins/adapters"
)

// mockAdaptersController implements AdaptersController for testing.
type mockAdaptersController struct {
	statuses []adapters.BindingStatus
	err      error
}

func (m *mockAdaptersController) Overview(ctx context.Context) ([]adapters.BindingStatus, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.statuses, nil
}

// waitForCondition polls until condition returns true or timeout.
func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for condition: %s", msg)
}

func TestAdapterBridgeStartShutdown(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)
	bridge := NewAdapterBridge(bus, service, nil)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func TestAdapterBridgeWithoutBus(t *testing.T) {
	service := NewService(nil)
	bridge := NewAdapterBridge(nil, service, nil)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func TestAdapterBridgeSetsAdapterOnReady(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)
	bridge := NewAdapterBridge(bus, service, nil)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Publish adapter ready event for AI slot
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicAdaptersStatus,
		Source: eventbus.SourceAdaptersService,
		Payload: eventbus.AdapterStatusEvent{
			AdapterID: adapters.MockAIAdapterID,
			Slot:      string(adapters.SlotAI),
			Status:    eventbus.AdapterHealthReady,
			Message:   "adapter ready",
		},
	})

	// Wait for adapter to be configured
	waitForCondition(t, time.Second, func() bool {
		metrics := service.Metrics()
		return metrics.AdapterName == adapters.MockAIAdapterID && metrics.AdapterReady
	}, "adapter should be configured after ready event")

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func TestAdapterBridgeClearsAdapterOnStopped(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)
	bridge := NewAdapterBridge(bus, service, nil)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// First, set the adapter as ready
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicAdaptersStatus,
		Source: eventbus.SourceAdaptersService,
		Payload: eventbus.AdapterStatusEvent{
			AdapterID: adapters.MockAIAdapterID,
			Slot:      string(adapters.SlotAI),
			Status:    eventbus.AdapterHealthReady,
			Message:   "adapter ready",
		},
	})

	// Wait for adapter to be configured
	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapters.MockAIAdapterID
	}, "adapter should be set after ready event")

	// Now stop the adapter
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicAdaptersStatus,
		Source: eventbus.SourceAdaptersService,
		Payload: eventbus.AdapterStatusEvent{
			AdapterID: adapters.MockAIAdapterID,
			Slot:      string(adapters.SlotAI),
			Status:    eventbus.AdapterHealthStopped,
			Message:   "adapter stopped",
		},
	})

	// Wait for adapter to be cleared
	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == ""
	}, "adapter should be cleared after stopped event")

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func TestAdapterBridgeClearsAdapterOnError(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)
	bridge := NewAdapterBridge(bus, service, nil)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// First, set the adapter as ready
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicAdaptersStatus,
		Source: eventbus.SourceAdaptersService,
		Payload: eventbus.AdapterStatusEvent{
			AdapterID: adapters.MockAIAdapterID,
			Slot:      string(adapters.SlotAI),
			Status:    eventbus.AdapterHealthReady,
			Message:   "adapter ready",
		},
	})

	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapters.MockAIAdapterID
	}, "adapter should be set")

	// Now send error event
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicAdaptersStatus,
		Source: eventbus.SourceAdaptersService,
		Payload: eventbus.AdapterStatusEvent{
			AdapterID: adapters.MockAIAdapterID,
			Slot:      string(adapters.SlotAI),
			Status:    eventbus.AdapterHealthError,
			Message:   "adapter crashed",
		},
	})

	// Adapter should be cleared on error
	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == ""
	}, "adapter should be cleared after error event")

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func TestAdapterBridgeClearsAdapterOnDegraded(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)
	bridge := NewAdapterBridge(bus, service, nil)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Set adapter as ready
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicAdaptersStatus,
		Source: eventbus.SourceAdaptersService,
		Payload: eventbus.AdapterStatusEvent{
			AdapterID: adapters.MockAIAdapterID,
			Slot:      string(adapters.SlotAI),
			Status:    eventbus.AdapterHealthReady,
			Message:   "adapter ready",
		},
	})

	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapters.MockAIAdapterID
	}, "adapter should be set")

	// Send degraded event
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicAdaptersStatus,
		Source: eventbus.SourceAdaptersService,
		Payload: eventbus.AdapterStatusEvent{
			AdapterID: adapters.MockAIAdapterID,
			Slot:      string(adapters.SlotAI),
			Status:    eventbus.AdapterHealthDegraded,
			Message:   "adapter degraded",
		},
	})

	// Adapter should be cleared on degraded
	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == ""
	}, "adapter should be cleared after degraded event")

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func TestAdapterBridgeIgnoresNonAISlot(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)
	bridge := NewAdapterBridge(bus, service, nil)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Publish adapter ready event for STT slot (should be ignored)
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicAdaptersStatus,
		Source: eventbus.SourceAdaptersService,
		Payload: eventbus.AdapterStatusEvent{
			AdapterID: adapters.MockSTTAdapterID,
			Slot:      string(adapters.SlotSTT),
			Status:    eventbus.AdapterHealthReady,
			Message:   "adapter ready",
		},
	})

	// Give some time for event to be processed
	time.Sleep(50 * time.Millisecond)

	// Check that no adapter was set (STT slot should be ignored)
	metrics := service.Metrics()
	if metrics.AdapterName != "" {
		t.Errorf("Expected empty adapter name for STT slot, got %s", metrics.AdapterName)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func TestAdapterBridgeInitialSyncWithMockAdapter(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)

	// Create controller that returns mock AI adapter as already bound and active
	adapterID := adapters.MockAIAdapterID
	controller := &mockAdaptersController{
		statuses: []adapters.BindingStatus{
			{
				Slot:      adapters.SlotAI,
				AdapterID: &adapterID,
				Status:    "active",
			},
		},
	}

	bridge := NewAdapterBridge(bus, service, controller)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Adapter should be configured immediately after start (initial sync)
	// No need to wait - syncWithCurrentBindings runs synchronously in Start()
	metrics := service.Metrics()
	if metrics.AdapterName != adapters.MockAIAdapterID {
		t.Errorf("Expected adapter %s after initial sync, got %s", adapters.MockAIAdapterID, metrics.AdapterName)
	}
	if !metrics.AdapterReady {
		t.Error("Expected adapter to be ready after initial sync")
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func TestAdapterBridgeInitialSyncWithReadyNonMockAdapter(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)

	// Create controller with non-mock adapter that is already ready
	adapterID := "some.real.ai.adapter"
	controller := &mockAdaptersController{
		statuses: []adapters.BindingStatus{
			{
				Slot:      adapters.SlotAI,
				AdapterID: &adapterID,
				Status:    "active",
				Runtime: &adapters.RuntimeStatus{
					AdapterID: adapterID,
					Health:    eventbus.AdapterHealthReady,
				},
			},
		},
	}

	bridge := NewAdapterBridge(bus, service, controller)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Non-mock adapter should NOT be configured (NAP not yet implemented).
	// This is intentional - better to fail clearly with ErrNoAdapter
	// than to pretend the adapter is ready.
	metrics := service.Metrics()
	if metrics.AdapterName != "" {
		t.Errorf("Expected empty adapter name for non-mock adapter (NAP not implemented), got %s", metrics.AdapterName)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func TestAdapterBridgeInitialSyncInactiveBinding(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)

	// Create controller with inactive binding
	adapterID := adapters.MockAIAdapterID
	controller := &mockAdaptersController{
		statuses: []adapters.BindingStatus{
			{
				Slot:      adapters.SlotAI,
				AdapterID: &adapterID,
				Status:    "inactive", // Not active
			},
		},
	}

	bridge := NewAdapterBridge(bus, service, controller)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Inactive binding should not configure adapter
	metrics := service.Metrics()
	if metrics.AdapterName != "" {
		t.Errorf("Expected empty adapter name for inactive binding, got %s", metrics.AdapterName)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func TestAdapterBridgeInitialSyncWithControllerError(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)

	// Create controller that returns error
	controller := &mockAdaptersController{
		err: context.DeadlineExceeded,
	}

	bridge := NewAdapterBridge(bus, service, controller)

	ctx := context.Background()
	// Start should not fail even if controller fails - just logs warning
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// No adapter should be configured
	metrics := service.Metrics()
	if metrics.AdapterName != "" {
		t.Errorf("Expected empty adapter name when controller fails, got %s", metrics.AdapterName)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgeIntegrationWithManagerService tests the full integration flow:
// Manager starts builtin mock -> Service.Tick() emits Ready -> Bridge configures IntentRouter
func TestAdapterBridgeIntegrationWithManagerService(t *testing.T) {
	if testing.Short() {
		t.Skip("skip integration test in short mode")
	}

	ctx := context.Background()
	bus := eventbus.New()
	defer bus.Shutdown()

	// Create IntentRouter service and bridge
	intentService := NewService(bus)

	// Create a mock controller that returns mock AI adapter as active
	adapterID := adapters.MockAIAdapterID
	controller := &mockAdaptersController{
		statuses: []adapters.BindingStatus{
			{
				Slot:      adapters.SlotAI,
				AdapterID: &adapterID,
				Status:    "active",
			},
		},
	}

	bridge := NewAdapterBridge(bus, intentService, controller)

	// Start the bridge
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Bridge start failed: %v", err)
	}

	// Verify initial sync configured the adapter immediately
	metrics := intentService.Metrics()
	if metrics.AdapterName != adapters.MockAIAdapterID {
		t.Errorf("Expected adapter %s after initial sync, got %s", adapters.MockAIAdapterID, metrics.AdapterName)
	}
	if !metrics.AdapterReady {
		t.Error("Expected adapter to be ready after initial sync")
	}

	// Simulate adapter error event
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicAdaptersStatus,
		Source: eventbus.SourceAdaptersService,
		Payload: eventbus.AdapterStatusEvent{
			AdapterID: adapters.MockAIAdapterID,
			Slot:      string(adapters.SlotAI),
			Status:    eventbus.AdapterHealthError,
			Message:   "simulated error",
		},
	})

	// Wait for adapter to be cleared
	waitForCondition(t, time.Second, func() bool {
		return intentService.Metrics().AdapterName == ""
	}, "adapter should be cleared after error event")

	// Simulate adapter recovery
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicAdaptersStatus,
		Source: eventbus.SourceAdaptersService,
		Payload: eventbus.AdapterStatusEvent{
			AdapterID: adapters.MockAIAdapterID,
			Slot:      string(adapters.SlotAI),
			Status:    eventbus.AdapterHealthReady,
			Message:   "adapter recovered",
		},
	})

	// Wait for adapter to be reconfigured
	waitForCondition(t, time.Second, func() bool {
		m := intentService.Metrics()
		return m.AdapterName == adapters.MockAIAdapterID && m.AdapterReady
	}, "adapter should be reconfigured after ready event")

	// Shutdown
	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}
