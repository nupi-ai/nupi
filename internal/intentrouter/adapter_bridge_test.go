package intentrouter

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
	adapters "github.com/nupi-ai/nupi/internal/plugins/adapters"
	"github.com/nupi-ai/nupi/internal/plugins/manifest"
)

// mockAdaptersController implements AdaptersController for testing.
type mockAdaptersController struct {
	mu              sync.Mutex
	statuses        []adapters.BindingStatus
	err             error
	customLookup    func(ctx context.Context) ([]adapters.BindingStatus, error)
	manifestOptions map[string]manifest.AdapterOption
	manifestErr     error
}

func (m *mockAdaptersController) Overview(ctx context.Context) ([]adapters.BindingStatus, error) {
	m.mu.Lock()
	fn := m.customLookup
	if fn != nil {
		m.mu.Unlock()
		return fn(ctx)
	}
	if m.err != nil {
		m.mu.Unlock()
		return nil, m.err
	}
	// Return a copy to prevent data races on the caller side.
	out := make([]adapters.BindingStatus, len(m.statuses))
	copy(out, m.statuses)
	m.mu.Unlock()
	return out, nil
}

func (m *mockAdaptersController) ManifestOptions(ctx context.Context, adapterID string) (map[string]manifest.AdapterOption, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.manifestErr != nil {
		return nil, m.manifestErr
	}
	return m.manifestOptions, nil
}

// setConfig updates the Config field for the first binding status entry.
func (m *mockAdaptersController) setConfig(config string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.statuses) > 0 {
		m.statuses[0].Config = config
	}
}

// setUpdatedAt updates the UpdatedAt field for the first binding status entry.
func (m *mockAdaptersController) setUpdatedAt(updatedAt string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.statuses) > 0 {
		m.statuses[0].UpdatedAt = updatedAt
	}
}

// setCustomLookup replaces the custom lookup function.
func (m *mockAdaptersController) setCustomLookup(fn func(ctx context.Context) ([]adapters.BindingStatus, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.customLookup = fn
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
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Message:   "adapter ready",
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
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Message:   "adapter ready",
	})

	// Wait for adapter to be configured
	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapters.MockAIAdapterID
	}, "adapter should be set after ready event")

	// Now stop the adapter
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthStopped,
		Message:   "adapter stopped",
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
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Message:   "adapter ready",
	})

	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapters.MockAIAdapterID
	}, "adapter should be set")

	// Now send error event
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthError,
		Message:   "adapter crashed",
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
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Message:   "adapter ready",
	})

	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapters.MockAIAdapterID
	}, "adapter should be set")

	// Send degraded event
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthDegraded,
		Message:   "adapter degraded",
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
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockSTTAdapterID,
		Slot:      string(adapters.SlotSTT),
		Status:    eventbus.AdapterHealthReady,
		Message:   "adapter ready",
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
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthError,
		Message:   "simulated error",
	})

	// Wait for adapter to be cleared
	waitForCondition(t, time.Second, func() bool {
		return intentService.Metrics().AdapterName == ""
	}, "adapter should be cleared after error event")

	// Simulate adapter recovery
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Message:   "adapter recovered",
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

// TestAdapterBridgePreservesAdapterOnCreationFailure verifies that when a new
// adapter fails to be created, the previous working adapter is preserved.
func TestAdapterBridgePreservesAdapterOnCreationFailure(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)
	bridge := NewAdapterBridge(bus, service, nil)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// First, set up a working mock adapter
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Message:   "adapter ready",
	})

	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapters.MockAIAdapterID
	}, "mock adapter should be configured")

	// Now try to switch to a NAP adapter without address (will fail)
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: "some.nap.adapter",
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Message:   "adapter ready",
		Extra:     nil, // No address - will fail
	})

	// Give time for event to be processed
	time.Sleep(50 * time.Millisecond)

	// Previous mock adapter should still be active
	metrics := service.Metrics()
	if metrics.AdapterName != adapters.MockAIAdapterID {
		t.Errorf("Expected mock adapter to be preserved, got %s", metrics.AdapterName)
	}
	if !metrics.AdapterReady {
		t.Error("Expected adapter to still be ready")
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgeReconnectsOnAddressChange verifies that when an adapter
// restarts on a different address, the bridge reconnects.
func TestAdapterBridgeReconnectsOnAddressChange(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)
	bridge := NewAdapterBridge(bus, service, nil)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Configure mock adapter (address doesn't matter for mocks)
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Message:   "adapter ready",
		Extra: map[string]string{
			adapters.RuntimeExtraAddress: "/tmp/socket1.sock",
		},
	})

	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapters.MockAIAdapterID
	}, "adapter should be configured")

	// Track that we've seen the first configuration
	bridge.mu.Lock()
	firstAddress := bridge.currentAddress
	bridge.mu.Unlock()

	if firstAddress != "/tmp/socket1.sock" {
		t.Errorf("Expected first address /tmp/socket1.sock, got %s", firstAddress)
	}

	// Same adapter, different address - should reconfigure
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Message:   "adapter restarted on new port",
		Extra: map[string]string{
			adapters.RuntimeExtraAddress: "/tmp/socket2.sock",
		},
	})

	// Wait for address to change
	waitForCondition(t, time.Second, func() bool {
		bridge.mu.Lock()
		defer bridge.mu.Unlock()
		return bridge.currentAddress == "/tmp/socket2.sock"
	}, "address should be updated after restart")

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgeInitialSyncWithConfig verifies that config from binding
// is parsed and used during initial sync.
func TestAdapterBridgeInitialSyncWithConfig(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)

	// Create controller with config
	adapterID := adapters.MockAIAdapterID
	controller := &mockAdaptersController{
		statuses: []adapters.BindingStatus{
			{
				Slot:      adapters.SlotAI,
				AdapterID: &adapterID,
				Status:    "active",
				Config:    `{"model": "gpt-4", "temperature": 0.7}`,
			},
		},
	}

	bridge := NewAdapterBridge(bus, service, controller)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Adapter should be configured (mock adapter ignores config, but parsing shouldn't fail)
	metrics := service.Metrics()
	if metrics.AdapterName != adapters.MockAIAdapterID {
		t.Errorf("Expected adapter %s, got %s", adapters.MockAIAdapterID, metrics.AdapterName)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgeInitialSyncWithInvalidConfig verifies that invalid config
// JSON causes initial sync to abort without configuring the adapter.
func TestAdapterBridgeInitialSyncWithInvalidConfig(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)

	// Create controller with invalid JSON config
	adapterID := adapters.MockAIAdapterID
	controller := &mockAdaptersController{
		statuses: []adapters.BindingStatus{
			{
				Slot:      adapters.SlotAI,
				AdapterID: &adapterID,
				Status:    "active",
				Config:    `{invalid json`,
			},
		},
	}

	bridge := NewAdapterBridge(bus, service, controller)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Adapter should NOT be configured due to invalid config
	metrics := service.Metrics()
	if metrics.AdapterName != "" {
		t.Errorf("Expected empty adapter name for invalid config, got %s", metrics.AdapterName)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgeReconfiguresOnEveryReady verifies that READY events
// always trigger reconfiguration, ensuring config changes in DB are picked up.
// No time-based debounce blocks hot-reload - only circuit-breaker for errors.
func TestAdapterBridgeReconfiguresOnEveryReady(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)

	// Controller that can change config between READY events
	adapterID := adapters.MockAIAdapterID
	controller := &mockAdaptersController{
		statuses: []adapters.BindingStatus{
			{
				Slot:      adapters.SlotAI,
				AdapterID: &adapterID,
				Status:    "active",
				Config:    `{"model": "v1"}`,
			},
		},
		// Declare manifest options so config validation passes
		manifestOptions: map[string]manifest.AdapterOption{
			"model": {Type: "string", Description: "Model version"},
		},
	}

	bridge := NewAdapterBridge(bus, service, controller)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// First READY event
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Extra: map[string]string{
			adapters.RuntimeExtraAddress: "/tmp/socket.sock",
		},
	})

	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapters.MockAIAdapterID
	}, "adapter should be configured")

	// Get the first adapter reference
	bridge.mu.Lock()
	firstAdapter := bridge.adapter
	bridge.mu.Unlock()

	// Simulate config change in DB (controller returns new config)
	controller.setConfig(`{"model": "v2"}`)

	// Second READY event immediately (no debounce - always reconfigures)
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Extra: map[string]string{
			adapters.RuntimeExtraAddress: "/tmp/socket.sock",
		},
	})

	// Wait for reconfiguration (should happen immediately, no debounce)
	waitForCondition(t, time.Second, func() bool {
		bridge.mu.Lock()
		defer bridge.mu.Unlock()
		return bridge.adapter != firstAdapter
	}, "adapter should be recreated to pick up config changes")

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgePublishesErrorOnInvalidConfig verifies that config parse
// errors are published to the event bus for UI/CLI visibility.
func TestAdapterBridgePublishesErrorOnInvalidConfig(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	// Subscribe to bridge diagnostics topic to capture errors
	sub := bus.Subscribe(
		eventbus.TopicIntentRouterDiagnostics,
		eventbus.WithSubscriptionName("test_error_capture"),
	)
	defer sub.Close()

	service := NewService(bus)

	// Controller with invalid JSON config
	adapterID := adapters.MockAIAdapterID
	controller := &mockAdaptersController{
		statuses: []adapters.BindingStatus{
			{
				Slot:      adapters.SlotAI,
				AdapterID: &adapterID,
				Status:    "active",
				Config:    `{invalid json`,
			},
		},
	}

	bridge := NewAdapterBridge(bus, service, controller)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for error event to be published
	var errorReceived bool
	var diagnosticType eventbus.BridgeDiagnosticType
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case env := <-sub.C():
			evt, ok := env.Payload.(eventbus.BridgeDiagnosticEvent)
			if ok {
				errorReceived = true
				if evt.AdapterID != adapters.MockAIAdapterID {
					t.Errorf("Expected adapter ID %s, got %s", adapters.MockAIAdapterID, evt.AdapterID)
				}
				diagnosticType = evt.Type
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
		if errorReceived {
			break
		}
	}

	if !errorReceived {
		t.Error("Expected diagnostic event to be published for invalid config")
	}
	if diagnosticType != eventbus.BridgeDiagnosticConfigInvalid {
		t.Errorf("Expected type %s, got %s", eventbus.BridgeDiagnosticConfigInvalid, diagnosticType)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgePublishesErrorOnNoAddress verifies that connection errors
// (valid config but no address) are published with correct error type.
func TestAdapterBridgePublishesErrorOnNoAddress(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	// Subscribe to bridge diagnostics topic
	sub := bus.Subscribe(
		eventbus.TopicIntentRouterDiagnostics,
		eventbus.WithSubscriptionName("test_error_capture"),
	)
	defer sub.Close()

	service := NewService(bus)
	bridge := NewAdapterBridge(bus, service, nil)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Publish READY for NAP adapter without address (will fail to create)
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: "some.nap.adapter",
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Extra:     nil, // No address
	})

	// Wait for error event
	var errorReceived bool
	var diagnosticType eventbus.BridgeDiagnosticType
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case env := <-sub.C():
			evt, ok := env.Payload.(eventbus.BridgeDiagnosticEvent)
			if ok && evt.AdapterID == "some.nap.adapter" {
				errorReceived = true
				diagnosticType = evt.Type
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
		if errorReceived {
			break
		}
	}

	if !errorReceived {
		t.Error("Expected diagnostic event for adapter without address")
	}
	if diagnosticType != eventbus.BridgeDiagnosticConnectionFailed {
		t.Errorf("Expected type %s, got %s", eventbus.BridgeDiagnosticConnectionFailed, diagnosticType)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgePublishesErrorOnControllerError verifies that lookup errors
// are published when controller fails during READY handling.
func TestAdapterBridgePublishesErrorOnControllerError(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)

	// Controller that returns error on Overview()
	controller := &mockAdaptersController{
		err: context.DeadlineExceeded,
	}

	bridge := NewAdapterBridge(bus, service, controller)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Subscribe AFTER bridge.Start() to avoid receiving duplicate events
	sub := bus.Subscribe(
		eventbus.TopicIntentRouterDiagnostics,
		eventbus.WithSubscriptionName("test_error_capture"),
	)
	defer sub.Close()

	// Publish READY - config lookup will fail because controller returns error
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: "some.adapter",
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
	})

	// Collect diagnostic events for our adapter
	var diagnosticEvents []eventbus.BridgeDiagnosticEvent
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case env := <-sub.C():
			evt, ok := env.Payload.(eventbus.BridgeDiagnosticEvent)
			if ok && evt.AdapterID == "some.adapter" {
				diagnosticEvents = append(diagnosticEvents, evt)
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
		// Stop after collecting one event
		if len(diagnosticEvents) > 0 {
			break
		}
	}

	if len(diagnosticEvents) == 0 {
		t.Fatal("Expected diagnostic event for controller lookup failure")
	}

	// Check that diagnostic event has the lookup_failed type
	evt := diagnosticEvents[0]
	if evt.Type != eventbus.BridgeDiagnosticLookupFailed {
		t.Errorf("Expected type %s, got %s (message: %s)", eventbus.BridgeDiagnosticLookupFailed, evt.Type, evt.Message)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgeCircuitBreakerBackoff verifies that after repeated errors,
// the circuit-breaker applies exponential backoff to prevent resource exhaustion.
func TestAdapterBridgeCircuitBreakerBackoff(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)

	// Controller that always fails
	controller := &mockAdaptersController{
		err: context.DeadlineExceeded,
	}

	bridge := NewAdapterBridge(bus, service, controller)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Send multiple READY events to trigger circuit breaker
	for i := 0; i < maxConsecutiveErrors+1; i++ {
		eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
			AdapterID: "some.adapter",
			Slot:      string(adapters.SlotAI),
			Status:    eventbus.AdapterHealthReady,
			Extra: map[string]string{
				adapters.RuntimeExtraAddress: "/tmp/test.sock",
			},
		})
		time.Sleep(20 * time.Millisecond)
	}

	// Verify circuit breaker is engaged
	bridge.mu.Lock()
	errorCount := bridge.consecutiveErrors
	bridge.mu.Unlock()

	if errorCount < maxConsecutiveErrors {
		t.Errorf("Expected at least %d consecutive errors, got %d", maxConsecutiveErrors, errorCount)
	}

	// Verify backoff calculation
	backoff := bridge.calculateBackoff()
	if backoff < baseBackoffDelay {
		t.Errorf("Expected backoff >= %v, got %v", baseBackoffDelay, backoff)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgeCircuitBreakerReset verifies that successful configuration
// resets the circuit breaker, allowing immediate processing of future events.
func TestAdapterBridgeCircuitBreakerReset(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)
	bridge := NewAdapterBridge(bus, service, nil)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Manually set error state
	bridge.mu.Lock()
	bridge.consecutiveErrors = maxConsecutiveErrors + 2
	bridge.lastErrorTime = time.Now()
	bridge.mu.Unlock()

	// Now send a successful READY (mock adapter works without controller)
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
	})

	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapters.MockAIAdapterID
	}, "adapter should be configured")

	// Circuit breaker should be reset
	bridge.mu.Lock()
	errorCount := bridge.consecutiveErrors
	bridge.mu.Unlock()

	if errorCount != 0 {
		t.Errorf("Expected error count to be reset to 0, got %d", errorCount)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgeDiagnosticRecoverable verifies that diagnostic events include
// the recoverable field to help UI/CLI distinguish error types.
func TestAdapterBridgeDiagnosticRecoverable(t *testing.T) {
	tests := []struct {
		name           string
		diagnosticType eventbus.BridgeDiagnosticType
		recoverable    bool
	}{
		{"config error not recoverable", eventbus.BridgeDiagnosticConfigInvalid, false},
		{"connection error recoverable", eventbus.BridgeDiagnosticConnectionFailed, true},
		{"lookup error recoverable", eventbus.BridgeDiagnosticLookupFailed, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := eventbus.New()
			defer bus.Shutdown()

			sub := bus.Subscribe(
				eventbus.TopicIntentRouterDiagnostics,
				eventbus.WithSubscriptionName("test"),
			)
			defer sub.Close()

			service := NewService(bus)
			bridge := NewAdapterBridge(bus, service, nil)

			ctx := context.Background()

			// Publish diagnostic directly
			bridge.publishBridgeDiagnostic(ctx, "test-adapter", "test error", tt.diagnosticType)

			// Capture the event
			select {
			case env := <-sub.C():
				evt, ok := env.Payload.(eventbus.BridgeDiagnosticEvent)
				if !ok {
					t.Fatal("Expected BridgeDiagnosticEvent")
				}
				if evt.Recoverable != tt.recoverable {
					t.Errorf("Expected recoverable=%v, got %v", tt.recoverable, evt.Recoverable)
				}
				if evt.Type != tt.diagnosticType {
					t.Errorf("Expected type=%s, got %s", tt.diagnosticType, evt.Type)
				}
			case <-time.After(time.Second):
				t.Fatal("Timeout waiting for diagnostic event")
			}
		})
	}
}

// TestAdapterBridgeCircuitBreakerResetOnStopped verifies that STOPPED events
// reset the circuit breaker, allowing immediate retry on next READY.
func TestAdapterBridgeCircuitBreakerResetOnStopped(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)
	bridge := NewAdapterBridge(bus, service, nil)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// First, configure an adapter
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
	})

	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapters.MockAIAdapterID
	}, "adapter should be configured")

	// Simulate accumulated errors
	bridge.mu.Lock()
	bridge.consecutiveErrors = maxConsecutiveErrors + 2
	bridge.lastErrorTime = time.Now()
	bridge.mu.Unlock()

	// Send STOPPED event
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthStopped,
	})

	// Wait for STOPPED to be processed
	waitForCondition(t, time.Second, func() bool {
		bridge.mu.Lock()
		defer bridge.mu.Unlock()
		return bridge.current == ""
	}, "adapter should be cleared")

	// Circuit breaker should be reset
	bridge.mu.Lock()
	errorCount := bridge.consecutiveErrors
	bridge.mu.Unlock()

	if errorCount != 0 {
		t.Errorf("Expected error count to be reset to 0 after STOPPED, got %d", errorCount)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgeCircuitBreakerResetOnError verifies that ERROR events
// from adapters.Service reset the circuit breaker for fast recovery.
func TestAdapterBridgeCircuitBreakerResetOnError(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)
	bridge := NewAdapterBridge(bus, service, nil)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// First, configure an adapter
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
	})

	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapters.MockAIAdapterID
	}, "adapter should be configured")

	// Simulate accumulated errors
	bridge.mu.Lock()
	bridge.consecutiveErrors = maxConsecutiveErrors + 2
	bridge.lastErrorTime = time.Now()
	bridge.mu.Unlock()

	// Send ERROR event (from adapters.Service, e.g., adapter crashed)
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthError,
		Message:   "adapter crashed",
	})

	// Wait for ERROR to be processed
	waitForCondition(t, time.Second, func() bool {
		bridge.mu.Lock()
		defer bridge.mu.Unlock()
		return bridge.current == ""
	}, "adapter should be cleared")

	// Circuit breaker should be reset
	bridge.mu.Lock()
	errorCount := bridge.consecutiveErrors
	bridge.mu.Unlock()

	if errorCount != 0 {
		t.Errorf("Expected error count to be reset to 0 after ERROR, got %d", errorCount)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgeCircuitBreakerResetOnAdapterChange verifies that switching
// to a different adapter resets the circuit breaker for immediate processing.
func TestAdapterBridgeCircuitBreakerResetOnAdapterChange(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)

	// Controller that fails for "failing.adapter" but succeeds for mock adapter
	adapterID := adapters.MockAIAdapterID
	controller := &mockAdaptersController{
		statuses: []adapters.BindingStatus{
			{
				Slot:      adapters.SlotAI,
				AdapterID: &adapterID,
				Status:    "active",
			},
		},
		// Custom lookup function that fails for specific adapter
		customLookup: func(ctx context.Context) ([]adapters.BindingStatus, error) {
			// Return error to trigger lookup failures (simulates flaky controller)
			return nil, context.DeadlineExceeded
		},
	}

	bridge := NewAdapterBridge(bus, service, controller)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Build up errors for "failing.adapter"
	for i := 0; i < maxConsecutiveErrors+1; i++ {
		eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
			AdapterID: "failing.adapter",
			Slot:      string(adapters.SlotAI),
			Status:    eventbus.AdapterHealthReady,
			Extra: map[string]string{
				adapters.RuntimeExtraAddress: "/tmp/fail.sock",
			},
		})
		time.Sleep(20 * time.Millisecond)
	}

	// Verify circuit breaker is engaged
	bridge.mu.Lock()
	errorCount := bridge.consecutiveErrors
	bridge.mu.Unlock()

	if errorCount < maxConsecutiveErrors {
		t.Fatalf("Expected circuit breaker to be engaged, got %d errors", errorCount)
	}

	// Now switch controller to succeed
	controller.setCustomLookup(nil)

	// Now switch to a different adapter (mock adapter)
	// This should reset circuit breaker and process immediately
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
	})

	// Should configure immediately (no backoff for different adapter)
	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapters.MockAIAdapterID
	}, "different adapter should be configured immediately")

	// Circuit breaker should be reset
	bridge.mu.Lock()
	finalErrorCount := bridge.consecutiveErrors
	bridge.mu.Unlock()

	if finalErrorCount != 0 {
		t.Errorf("Expected error count to be reset to 0 after adapter change, got %d", finalErrorCount)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgeCircuitBreakerResetOnAddressChange verifies that an address
// change resets the circuit breaker for fast recovery after config fix.
func TestAdapterBridgeCircuitBreakerResetOnAddressChange(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)
	bridge := NewAdapterBridge(bus, service, nil)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Configure adapter at address1
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Extra: map[string]string{
			adapters.RuntimeExtraAddress: "/tmp/addr1.sock",
		},
	})

	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapters.MockAIAdapterID
	}, "adapter should be configured")

	// Simulate accumulated errors (e.g., from flapping)
	bridge.mu.Lock()
	bridge.consecutiveErrors = maxConsecutiveErrors + 2
	bridge.lastErrorTime = time.Now()
	bridge.mu.Unlock()

	// Same adapter but different address (e.g., restarted on new port after config fix)
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapters.MockAIAdapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Extra: map[string]string{
			adapters.RuntimeExtraAddress: "/tmp/addr2.sock", // Different address!
		},
	})

	// Wait for reconfiguration
	waitForCondition(t, time.Second, func() bool {
		bridge.mu.Lock()
		defer bridge.mu.Unlock()
		return bridge.currentAddress == "/tmp/addr2.sock"
	}, "adapter should be reconfigured at new address")

	// Circuit breaker should be reset
	bridge.mu.Lock()
	errorCount := bridge.consecutiveErrors
	bridge.mu.Unlock()

	if errorCount != 0 {
		t.Errorf("Expected error count to be reset to 0 after address change, got %d", errorCount)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestParseConfig verifies config JSON parsing.
func TestParseConfig(t *testing.T) {
	bridge := &AdapterBridge{}

	tests := []struct {
		name      string
		input     string
		wantNil   bool
		wantError bool
	}{
		{"empty string", "", true, false},
		{"valid json", `{"key": "value"}`, false, false},
		{"valid nested", `{"model": "gpt-4", "options": {"temp": 0.7}}`, false, false},
		{"invalid json", `{invalid`, true, true},
		{"truncated json", `{"key":`, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := bridge.parseConfig(tt.input)
			if tt.wantError && err == nil {
				t.Error("Expected error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			if tt.wantNil && config != nil {
				t.Errorf("Expected nil config, got %v", config)
			}
			if !tt.wantNil && !tt.wantError && config == nil {
				t.Error("Expected non-nil config, got nil")
			}
		})
	}
}

// TestAdapterBridgeConfigValidationAgainstManifest verifies that config is
// validated against manifest options during READY handling.
func TestAdapterBridgeConfigValidationAgainstManifest(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)

	adapterID := "test.ai.adapter"

	// Controller with manifest that declares specific options
	controller := &mockAdaptersController{
		statuses: []adapters.BindingStatus{
			{
				Slot:      adapters.SlotAI,
				AdapterID: &adapterID,
				Status:    "active",
				Config:    `{"model": "gpt-4", "unknown_option": true}`, // unknown_option not in manifest
				UpdatedAt: "2025-01-01T00:00:00Z",
			},
		},
		manifestOptions: map[string]manifest.AdapterOption{
			"model": {Type: "string", Description: "Model name"},
			// "unknown_option" is NOT declared in manifest
		},
	}

	bridge := NewAdapterBridge(bus, service, controller)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Subscribe to catch diagnostic events
	sub := bus.Subscribe(
		eventbus.TopicIntentRouterDiagnostics,
		eventbus.WithSubscriptionName("test_validation"),
	)
	defer sub.Close()

	// Publish READY - config validation should fail
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
	})

	// Wait for diagnostic event
	var errorReceived bool
	var diagnosticType eventbus.BridgeDiagnosticType
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case env := <-sub.C():
			evt, ok := env.Payload.(eventbus.BridgeDiagnosticEvent)
			if ok && evt.AdapterID == adapterID {
				errorReceived = true
				diagnosticType = evt.Type
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
		if errorReceived {
			break
		}
	}

	if !errorReceived {
		t.Error("Expected diagnostic event for config with unknown option")
	}
	// Should be config_invalid since validation failed
	if diagnosticType != eventbus.BridgeDiagnosticConfigInvalid {
		t.Errorf("Expected diagnostic type %s for validation failure, got %s", eventbus.BridgeDiagnosticConfigInvalid, diagnosticType)
	}

	// Adapter should NOT be configured
	if service.Metrics().AdapterName != "" {
		t.Errorf("Expected no adapter configured after validation failure, got %s", service.Metrics().AdapterName)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgeCircuitBreakerResetOnConfigChange verifies that changing
// config (UpdatedAt) without changing adapter/address resets the circuit breaker.
func TestAdapterBridgeCircuitBreakerResetOnConfigChange(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)
	adapterID := adapters.MockAIAdapterID

	updatedAt := "2025-01-01T00:00:00Z"
	controller := &mockAdaptersController{
		statuses: []adapters.BindingStatus{
			{
				Slot:      adapters.SlotAI,
				AdapterID: &adapterID,
				Status:    "active",
				Config:    `{}`,
				UpdatedAt: updatedAt,
			},
		},
	}

	bridge := NewAdapterBridge(bus, service, controller)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// First, configure the adapter
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Extra: map[string]string{
			adapters.RuntimeExtraAddress: "/tmp/test.sock",
		},
	})

	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapterID
	}, "adapter should be configured")

	// Simulate accumulated errors from previous failures
	bridge.mu.Lock()
	bridge.consecutiveErrors = maxConsecutiveErrors + 2
	bridge.lastErrorTime = time.Now()
	bridge.lastFailedAdapter = adapterID
	bridge.lastFailedAddress = "/tmp/test.sock"
	bridge.lastConfigVersion = updatedAt // Same config version that failed
	bridge.mu.Unlock()

	// Update config in controller (same adapter/address, new UpdatedAt)
	newUpdatedAt := "2025-01-02T00:00:00Z" // Config was updated
	controller.setUpdatedAt(newUpdatedAt)

	// Publish READY with same adapter and address
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Extra: map[string]string{
			adapters.RuntimeExtraAddress: "/tmp/test.sock", // Same address
		},
	})

	// Wait for reconfiguration
	waitForCondition(t, time.Second, func() bool {
		bridge.mu.Lock()
		defer bridge.mu.Unlock()
		// Should be reconfigured because circuit breaker was reset due to config change
		return bridge.consecutiveErrors == 0
	}, "circuit breaker should reset after config update")

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestAdapterBridgeConfigHashFallback verifies that when UpdatedAt is empty,
// the bridge uses config JSON hash for change detection.
func TestAdapterBridgeConfigHashFallback(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	service := NewService(bus)
	adapterID := adapters.MockAIAdapterID

	// Controller with empty UpdatedAt - forces hash-based versioning
	controller := &mockAdaptersController{
		statuses: []adapters.BindingStatus{
			{
				Slot:      adapters.SlotAI,
				AdapterID: &adapterID,
				Status:    "active",
				Config:    `{"key":"value1"}`,
				UpdatedAt: "", // Empty - will use hash
			},
		},
		manifestOptions: map[string]manifest.AdapterOption{
			"key": {Type: "string"},
		},
	}

	bridge := NewAdapterBridge(bus, service, controller)

	ctx := context.Background()
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// First, configure the adapter
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Extra: map[string]string{
			adapters.RuntimeExtraAddress: "/tmp/test.sock",
		},
	})

	waitForCondition(t, time.Second, func() bool {
		return service.Metrics().AdapterName == adapterID
	}, "adapter should be configured")

	// Simulate errors with hash-based version
	bridge.mu.Lock()
	bridge.consecutiveErrors = maxConsecutiveErrors + 2
	bridge.lastErrorTime = time.Now()
	bridge.lastFailedAdapter = adapterID
	bridge.lastFailedAddress = "/tmp/test.sock"
	// lastConfigVersion will have hash prefix from previous attempt
	bridge.lastConfigVersion = "hash:abcd1234" // Some hash
	bridge.mu.Unlock()

	// Update config in controller (same adapter/address, different config JSON)
	controller.setConfig(`{"key":"value2"}`) // Different value = different hash

	// Publish READY - should detect config change via hash
	eventbus.Publish(ctx, bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		AdapterID: adapterID,
		Slot:      string(adapters.SlotAI),
		Status:    eventbus.AdapterHealthReady,
		Extra: map[string]string{
			adapters.RuntimeExtraAddress: "/tmp/test.sock", // Same address
		},
	})

	// Circuit breaker should reset because config hash changed
	waitForCondition(t, time.Second, func() bool {
		bridge.mu.Lock()
		defer bridge.mu.Unlock()
		return bridge.consecutiveErrors == 0
	}, "circuit breaker should reset on config hash change")

	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bridge.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestComputeConfigVersion tests the config version computation function.
func TestComputeConfigVersion(t *testing.T) {
	tests := []struct {
		name       string
		updatedAt  string
		configJSON string
		wantPrefix string // Expected prefix or exact value
	}{
		{
			name:       "uses UpdatedAt when available",
			updatedAt:  "2025-01-01T12:00:00Z",
			configJSON: `{"key":"value"}`,
			wantPrefix: "2025-01-01T12:00:00Z",
		},
		{
			name:       "uses hash when UpdatedAt empty",
			updatedAt:  "",
			configJSON: `{"key":"value"}`,
			wantPrefix: "hash:",
		},
		{
			name:       "uses hash when UpdatedAt whitespace",
			updatedAt:  "   ",
			configJSON: `{"key":"value"}`,
			wantPrefix: "hash:",
		},
		{
			name:       "returns hash for empty config",
			updatedAt:  "",
			configJSON: "",
			wantPrefix: "hash:",
		},
		{
			name:       "returns hash for whitespace config",
			updatedAt:  "",
			configJSON: "   ",
			wantPrefix: "hash:",
		},
		{
			name:       "different configs produce different hashes",
			updatedAt:  "",
			configJSON: `{"other":"data"}`,
			wantPrefix: "hash:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeConfigVersion(tt.updatedAt, tt.configJSON)

			if tt.wantPrefix == "hash:" {
				if !strings.HasPrefix(got, "hash:") {
					t.Errorf("computeConfigVersion() = %q, want prefix %q", got, tt.wantPrefix)
				}
				// Verify it's a valid hex string after prefix
				hexPart := strings.TrimPrefix(got, "hash:")
				if len(hexPart) != 16 { // 8 bytes = 16 hex chars
					t.Errorf("hash should be 16 hex chars, got %d: %s", len(hexPart), hexPart)
				}
			} else if got != tt.wantPrefix {
				t.Errorf("computeConfigVersion() = %q, want %q", got, tt.wantPrefix)
			}
		})
	}

	// Test that different configs produce different hashes
	hash1 := computeConfigVersion("", `{"a":"1"}`)
	hash2 := computeConfigVersion("", `{"a":"2"}`)
	if hash1 == hash2 {
		t.Error("different configs should produce different hashes")
	}
}
