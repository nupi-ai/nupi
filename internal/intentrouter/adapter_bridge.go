package intentrouter

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	adapters "github.com/nupi-ai/nupi/internal/plugins/adapters"
)

// AdaptersController provides access to adapter binding state for initial sync.
type AdaptersController interface {
	Overview(ctx context.Context) ([]adapters.BindingStatus, error)
}

// AdapterBridge watches for AI adapter binding changes and updates the intent router.
// It handles two scenarios:
//  1. Builtin mock adapters: Created directly without runner (no process to launch)
//  2. External adapters (NAP): Waits for AdapterHealthReady from adapters.Service
//
// On startup, it syncs with current bindings to handle adapters that were
// configured before the bridge started.
type AdapterBridge struct {
	bus        *eventbus.Bus
	service    *Service
	controller AdaptersController

	mu       sync.Mutex
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	sub      *eventbus.Subscription
	current  string        // currently active adapter ID
	adapter  IntentAdapter // currently active adapter
}

// NewAdapterBridge creates a bridge that connects adapter bindings to the intent router.
// The controller is used for initial sync with current adapter state.
func NewAdapterBridge(bus *eventbus.Bus, service *Service, controller AdaptersController) *AdapterBridge {
	return &AdapterBridge{
		bus:        bus,
		service:    service,
		controller: controller,
	}
}

// Start subscribes to adapter status events and syncs with current bindings.
func (b *AdapterBridge) Start(ctx context.Context) error {
	if b.bus == nil || b.service == nil {
		log.Printf("[IntentRouter/Bridge] Missing bus or service, running in passive mode")
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	b.cancel = cancel

	// Subscribe to adapter status events BEFORE initial sync
	// This ensures we don't miss events that occur during sync
	b.sub = b.bus.Subscribe(
		eventbus.TopicAdaptersStatus,
		eventbus.WithSubscriptionName("intent_router_adapter_bridge"),
	)

	// Initial sync with current adapter bindings
	b.syncWithCurrentBindings(runCtx)

	b.wg.Add(1)
	go b.consumeEvents(runCtx)

	log.Printf("[IntentRouter/Bridge] Started watching for AI adapter status changes")
	return nil
}

// syncWithCurrentBindings checks current adapter bindings and configures
// mock adapters directly. This handles the case where adapters were
// configured before the bridge started.
func (b *AdapterBridge) syncWithCurrentBindings(ctx context.Context) {
	if b.controller == nil {
		log.Printf("[IntentRouter/Bridge] No controller for initial sync, skipping")
		return
	}

	syncCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	statuses, err := b.controller.Overview(syncCtx)
	if err != nil {
		log.Printf("[IntentRouter/Bridge] Initial sync failed: %v", err)
		return
	}

	for _, status := range statuses {
		if status.Slot != adapters.SlotAI {
			continue
		}
		if status.AdapterID == nil || strings.TrimSpace(*status.AdapterID) == "" {
			continue
		}
		if !strings.EqualFold(status.Status, configstore.BindingStatusActive) {
			continue
		}

		adapterID := strings.TrimSpace(*status.AdapterID)

		// For builtin mock adapters, create directly without waiting for Ready event
		if adapters.IsBuiltinMockAdapter(adapterID) {
			b.mu.Lock()
			b.configureAdapter(adapterID)
			b.mu.Unlock()
			log.Printf("[IntentRouter/Bridge] Initial sync: configured mock adapter %s", adapterID)
			return
		}

		// For non-mock adapters, check if already ready from runtime status
		if status.Runtime != nil && status.Runtime.Health == eventbus.AdapterHealthReady {
			b.mu.Lock()
			b.configureAdapter(adapterID)
			b.mu.Unlock()
			log.Printf("[IntentRouter/Bridge] Initial sync: configured adapter %s (already ready)", adapterID)
			return
		}

		log.Printf("[IntentRouter/Bridge] Initial sync: AI slot bound to %s, waiting for ready event", adapterID)
	}
}

// Shutdown stops the bridge and cleans up resources.
func (b *AdapterBridge) Shutdown(ctx context.Context) error {
	if b.cancel != nil {
		b.cancel()
	}
	if b.sub != nil {
		b.sub.Close()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.wg.Wait()
	}()

	select {
	case <-done:
		log.Printf("[IntentRouter/Bridge] Shutdown complete")
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}

func (b *AdapterBridge) consumeEvents(ctx context.Context) {
	defer b.wg.Done()
	if b.sub == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-b.sub.C():
			if !ok {
				return
			}

			evt, ok := env.Payload.(eventbus.AdapterStatusEvent)
			if !ok {
				continue
			}

			b.handleAdapterStatus(ctx, evt)
		}
	}
}

func (b *AdapterBridge) handleAdapterStatus(ctx context.Context, evt eventbus.AdapterStatusEvent) {
	// Only handle AI slot events
	if evt.Slot != string(adapters.SlotAI) {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	switch evt.Status {
	case eventbus.AdapterHealthReady:
		b.handleAdapterReady(evt)

	case eventbus.AdapterHealthStopped:
		b.handleAdapterStopped(evt)

	case eventbus.AdapterHealthError, eventbus.AdapterHealthDegraded:
		b.handleAdapterError(ctx, evt)
	}
}

func (b *AdapterBridge) handleAdapterReady(evt eventbus.AdapterStatusEvent) {
	if b.current == evt.AdapterID && b.adapter != nil {
		// Same adapter, already configured
		return
	}

	log.Printf("[IntentRouter/Bridge] AI adapter ready: %s", evt.AdapterID)
	b.configureAdapter(evt.AdapterID)
}

func (b *AdapterBridge) handleAdapterStopped(evt eventbus.AdapterStatusEvent) {
	if b.current != evt.AdapterID {
		// Not our current adapter
		return
	}

	log.Printf("[IntentRouter/Bridge] AI adapter stopped: %s", evt.AdapterID)
	b.clearAdapter()
}

func (b *AdapterBridge) handleAdapterError(ctx context.Context, evt eventbus.AdapterStatusEvent) {
	log.Printf("[IntentRouter/Bridge] AI adapter %s %s: %s", evt.AdapterID, evt.Status, evt.Message)

	// Only clear if this is our current adapter
	if b.current != evt.AdapterID {
		return
	}

	// Clear the adapter so requests fail fast with ErrNoAdapter
	// instead of trying to use a broken adapter.
	//
	// Note: We don't publish to ConversationReply here because:
	// 1. Adapter errors are system-level, not session-level (no SessionID/PromptID)
	// 2. The error is already published to TopicAdaptersStatus by adapters.Service
	// 3. UI should subscribe to TopicAdaptersStatus for adapter lifecycle events
	b.clearAdapter()
}

// configureAdapter creates and sets the appropriate IntentAdapter.
// Must be called with b.mu held.
func (b *AdapterBridge) configureAdapter(adapterID string) {
	adapter := b.createAdapter(adapterID)
	if adapter == nil {
		log.Printf("[IntentRouter/Bridge] Failed to create adapter for %s", adapterID)
		return
	}

	b.current = adapterID
	b.adapter = adapter
	b.service.SetAdapter(adapter)

	log.Printf("[IntentRouter/Bridge] Intent router configured with adapter: %s", adapterID)
}

// clearAdapter removes the current adapter.
// Must be called with b.mu held.
func (b *AdapterBridge) clearAdapter() {
	if b.current == "" {
		return
	}

	b.current = ""
	b.adapter = nil
	b.service.SetAdapter(nil)

	log.Printf("[IntentRouter/Bridge] Intent router adapter cleared")
}

// createAdapter constructs the appropriate IntentAdapter for the given adapter ID.
// Note: This is only called for AI slot adapters (bridge filters by SlotAI).
func (b *AdapterBridge) createAdapter(adapterID string) IntentAdapter {
	// Handle all builtin mock adapters (currently only AI mock, but future-proof).
	// The bridge only handles AI slot, so non-AI mocks won't reach this code.
	if adapters.IsBuiltinMockAdapter(adapterID) {
		return NewMockAdapter(
			WithMockName(adapterID),
			WithMockParseCommands(true),
			WithMockEchoMode(false),
		)
	}

	// For non-mock adapters, NAP AI protocol client is required.
	// Until NAP is implemented, return nil so the router stays in "no adapter" mode.
	// This is intentional - it's better to fail clearly (ErrNoAdapter on each request)
	// than to pretend the adapter is ready and fail silently.
	//
	// When a prompt arrives and adapter is nil, the service will:
	// 1. Log "No adapter configured for prompt X"
	// 2. Publish ConversationReplyEvent with SessionID/PromptID and error
	// 3. User gets clear feedback about the issue
	//
	// TODO: Implement NAP AI gRPC client when proto is defined.
	log.Printf("[IntentRouter/Bridge] NAP AI adapter not yet implemented: %s (requires NAP AI protocol)", adapterID)
	return nil
}
