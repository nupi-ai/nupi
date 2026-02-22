package intentrouter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	adapters "github.com/nupi-ai/nupi/internal/plugins/adapters"
	"github.com/nupi-ai/nupi/internal/plugins/manifest"
)

// ErrInvalidConfig is returned when adapter configuration JSON is malformed.
var ErrInvalidConfig = errors.New("invalid adapter configuration")

// Backoff constants for circuit-breaker pattern on repeated failures.
const (
	// maxConsecutiveErrors is the threshold after which backoff kicks in.
	maxConsecutiveErrors = 3

	// baseBackoffDelay is the initial backoff delay after maxConsecutiveErrors.
	baseBackoffDelay = 500 * time.Millisecond

	// maxBackoffDelay caps the exponential backoff.
	maxBackoffDelay = 30 * time.Second
)

// AdaptersController provides access to adapter binding state for initial sync.
type AdaptersController interface {
	Overview(ctx context.Context) ([]adapters.BindingStatus, error)
	ManifestOptions(ctx context.Context, adapterID string) (map[string]manifest.AdapterOption, error)
}

// AdapterBridge watches for AI adapter binding changes and updates the intent router.
// It handles two scenarios:
//  1. Builtin mock adapters: Created directly without runner (no process to launch)
//  2. External adapters (NAP): Connects via gRPC when AdapterHealthReady is received
//
// On startup, it syncs with current bindings to handle adapters that were
// configured before the bridge started.
//
// Circuit-breaker pattern: After maxConsecutiveErrors failures, the bridge applies
// exponential backoff before retrying. This prevents resource exhaustion from
// unstable adapters while still allowing recovery.
type AdapterBridge struct {
	bus        *eventbus.Bus
	service    *Service
	controller AdaptersController

	mu             sync.Mutex
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	sub            *eventbus.TypedSubscription[eventbus.AdapterStatusEvent]
	current        string        // currently active adapter ID
	currentAddress string        // currently active adapter endpoint address
	adapter        IntentAdapter // currently active adapter
	napAdapter     *NAPAdapter   // NAP adapter reference for cleanup

	// Circuit-breaker state for backoff on repeated failures
	consecutiveErrors int       // count of consecutive READY handling failures
	lastErrorTime     time.Time // when the last error occurred (for backoff calculation)
	lastFailedAdapter string    // adapter ID that triggered the errors (for reset detection)
	lastFailedAddress string    // address that triggered the errors (for reset detection)
	lastConfigVersion string    // config version from last failed attempt (UpdatedAt or hash fallback)

	// Embedding bridge for awareness system vector search
	embeddingBridge *EmbeddingBridge

	// Manifest options cache to reduce I/O on repeated READY events
	manifestCache    map[string]*manifestCacheEntry
	manifestCacheTTL time.Duration
}

// manifestCacheEntry stores cached manifest options with expiry time.
type manifestCacheEntry struct {
	options   map[string]manifest.AdapterOption
	err       error // cached error (nil if lookup succeeded)
	expiresAt time.Time
}

// defaultManifestCacheTTL is how long manifest options are cached before re-fetching.
const defaultManifestCacheTTL = 30 * time.Second

// NewAdapterBridge creates a bridge that connects adapter bindings to the intent router.
// The controller is used for initial sync with current adapter state.
func NewAdapterBridge(bus *eventbus.Bus, service *Service, controller AdaptersController) *AdapterBridge {
	return &AdapterBridge{
		bus:              bus,
		service:          service,
		controller:       controller,
		embeddingBridge:  NewEmbeddingBridge(),
		manifestCache:    make(map[string]*manifestCacheEntry),
		manifestCacheTTL: defaultManifestCacheTTL,
	}
}

// getCachedManifestOptions returns manifest options from cache or fetches them if expired.
// This reduces I/O overhead during adapter flapping (multiple READY events).
func (b *AdapterBridge) getCachedManifestOptions(ctx context.Context, adapterID string) (map[string]manifest.AdapterOption, error) {
	if b.controller == nil {
		return nil, nil
	}

	// Check cache (must be called with b.mu held)
	now := time.Now()
	if entry, ok := b.manifestCache[adapterID]; ok && now.Before(entry.expiresAt) {
		return entry.options, entry.err
	}

	// Cache miss or expired - fetch from controller
	options, err := b.controller.ManifestOptions(ctx, adapterID)

	// Cache the result (including errors to avoid repeated failed lookups)
	b.manifestCache[adapterID] = &manifestCacheEntry{
		options:   options,
		err:       err,
		expiresAt: now.Add(b.manifestCacheTTL),
	}

	return options, err
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
	b.sub = eventbus.Subscribe[eventbus.AdapterStatusEvent](b.bus,
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
// adapters. For mock adapters, creates directly. For NAP adapters, uses
// runtime status to get endpoint info.
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
		config, err := b.parseConfig(status.Config)
		if err != nil {
			log.Printf("[IntentRouter/Bridge] Initial sync: invalid config for %s: %v", adapterID, err)
			// Publish error so UI/CLI knows about the configuration problem
			b.publishBridgeDiagnostic(syncCtx, adapterID, fmt.Sprintf("invalid config: %v", err), eventbus.BridgeDiagnosticConfigInvalid)
			// Don't proceed with invalid config - this is a configuration error
			// that needs to be fixed by the user
			return
		}

		// For builtin mock adapters, create directly without waiting for Ready event
		if adapters.IsBuiltinMockAdapter(adapterID) {
			b.mu.Lock()
			ok := b.configureAdapter(syncCtx, adapterID, nil, config)
			b.mu.Unlock()
			if ok {
				log.Printf("[IntentRouter/Bridge] Initial sync: configured mock adapter %s", adapterID)
			} else {
				b.publishBridgeDiagnostic(syncCtx, adapterID, "failed to create mock adapter", eventbus.BridgeDiagnosticConnectionFailed)
			}
			return
		}

		// For non-mock adapters, check if already ready from runtime status
		if status.Runtime != nil && status.Runtime.Health == eventbus.AdapterHealthReady {
			b.mu.Lock()
			ok := b.configureAdapter(syncCtx, adapterID, status.Runtime.Extra, config)
			b.mu.Unlock()
			if ok {
				log.Printf("[IntentRouter/Bridge] Initial sync: configured NAP adapter %s", adapterID)
			} else {
				b.publishBridgeDiagnostic(syncCtx, adapterID, "failed to create NAP adapter", eventbus.BridgeDiagnosticConnectionFailed)
			}
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

	// Clean up NAP adapter connection
	b.mu.Lock()
	if b.napAdapter != nil {
		if err := b.napAdapter.Close(); err != nil {
			log.Printf("[IntentRouter/Bridge] Error closing NAP adapter: %v", err)
		}
		b.napAdapter = nil
	}
	b.mu.Unlock()

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
	eventbus.Consume(ctx, b.sub, &b.wg, func(event eventbus.AdapterStatusEvent) {
		b.handleAdapterStatus(ctx, event)
	})
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
		b.handleAdapterReady(ctx, evt)

	case eventbus.AdapterHealthStopped:
		b.handleAdapterStopped(evt)

	case eventbus.AdapterHealthError, eventbus.AdapterHealthDegraded:
		b.handleAdapterError(evt)
	}
}

func (b *AdapterBridge) handleAdapterReady(ctx context.Context, evt eventbus.AdapterStatusEvent) {
	// Extract address from event
	newAddress := ""
	if evt.Extra != nil {
		newAddress = strings.TrimSpace(evt.Extra[adapters.RuntimeExtraAddress])
	}

	// Look up config from controller for this adapter.
	// We do this early to get UpdatedAt for circuit breaker reset detection.
	configInfo, err := b.lookupAdapterConfig(ctx, evt.AdapterID)
	if err != nil {
		// Config lookup/parse/validation error - don't proceed with broken config.
		// Keep previous adapter (if any) working, but signal the error.
		// This is a config error (not connection error) since validation failed.
		log.Printf("[IntentRouter/Bridge] Config error for adapter %s: %v", evt.AdapterID, err)

		// Determine error type based on error content
		diagnosticType := eventbus.BridgeDiagnosticLookupFailed
		if errors.Is(err, ErrInvalidConfig) {
			diagnosticType = eventbus.BridgeDiagnosticConfigInvalid
		}

		b.recordError(evt.AdapterID, newAddress, "")
		b.publishBridgeDiagnostic(ctx, evt.AdapterID, fmt.Sprintf("config error: %v", err), diagnosticType)
		return
	}

	// Reset circuit breaker if adapter, address, or config changed from the failing one.
	// This allows immediate processing when:
	// - User switches to a different adapter after failure
	// - Adapter restarts on a different port (address change)
	// - Config was fixed (version changed) - fast recovery without waiting for backoff
	// We compare against lastFailed* values, not current adapter, because
	// the failing adapter may never have been successfully configured.
	if b.consecutiveErrors > 0 {
		adapterChanged := b.lastFailedAdapter != evt.AdapterID
		addressChanged := b.lastFailedAddress != newAddress
		// Config change detection: compare versions if both are non-empty.
		// Note: "empty" is a valid version (empty config with no UpdatedAt).
		// We only skip config change detection if we couldn't get version info at all.
		configChanged := configInfo != nil && configInfo.version != "" && b.lastConfigVersion != "" && b.lastConfigVersion != configInfo.version

		if adapterChanged || addressChanged || configChanged {
			reason := "adapter/address changed"
			if configChanged && !adapterChanged && !addressChanged {
				reason = "config updated"
			}
			log.Printf("[IntentRouter/Bridge] Circuit breaker reset: %s", reason)
			b.consecutiveErrors = 0
			b.lastFailedAdapter = ""
			b.lastFailedAddress = ""
			b.lastConfigVersion = ""
		}
	}

	// Circuit-breaker: if we've had too many consecutive errors, apply backoff.
	// This prevents resource exhaustion from unstable adapters (restart loops).
	// Only applies when same adapter+address+config keeps failing.
	if b.consecutiveErrors >= maxConsecutiveErrors {
		backoff := b.calculateBackoff()
		elapsed := time.Since(b.lastErrorTime)
		if elapsed < backoff {
			log.Printf("[IntentRouter/Bridge] AI adapter ready (backoff %v remaining): %s", backoff-elapsed, evt.AdapterID)
			return
		}
		log.Printf("[IntentRouter/Bridge] AI adapter ready (backoff expired, retrying): %s", evt.AdapterID)
	}

	log.Printf("[IntentRouter/Bridge] AI adapter ready: %s (address: %s)", evt.AdapterID, newAddress)

	// Get config from the lookup result
	var config map[string]any
	var configVersion string
	if configInfo != nil {
		config = configInfo.config
		configVersion = configInfo.version
	}

	if !b.configureAdapter(ctx, evt.AdapterID, evt.Extra, config) {
		// Adapter creation failed - error already logged, publish diagnostic event
		b.recordError(evt.AdapterID, newAddress, configVersion)
		b.publishBridgeDiagnostic(ctx, evt.AdapterID, "failed to create adapter", eventbus.BridgeDiagnosticConnectionFailed)
		return
	}

	// Success - reset circuit breaker
	b.consecutiveErrors = 0
	b.lastFailedAdapter = ""
	b.lastFailedAddress = ""
	b.lastConfigVersion = ""
}

func (b *AdapterBridge) handleAdapterStopped(evt eventbus.AdapterStatusEvent) {
	if b.current != evt.AdapterID {
		// Not our current adapter
		return
	}

	log.Printf("[IntentRouter/Bridge] AI adapter stopped: %s", evt.AdapterID)
	b.clearAdapter()

	// Reset circuit breaker - adapter has definitively stopped, not flapping.
	// Next READY should be processed immediately without backoff.
	b.consecutiveErrors = 0
	b.lastFailedAdapter = ""
	b.lastFailedAddress = ""
	b.lastConfigVersion = ""
}

func (b *AdapterBridge) handleAdapterError(evt eventbus.AdapterStatusEvent) {
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

	// Reset circuit breaker - error from adapters.Service indicates adapter
	// has crashed/failed (not just bridge connection issue). Next READY
	// should be processed immediately to allow fast recovery.
	b.consecutiveErrors = 0
	b.lastFailedAdapter = ""
	b.lastFailedAddress = ""
	b.lastConfigVersion = ""
}

// configureAdapter creates and sets the appropriate IntentAdapter.
// For NAP adapters, runtime contains endpoint info (address, transport).
// Returns true if adapter was successfully configured, false otherwise.
// On failure, the previous adapter (if any) is preserved to maintain service continuity.
// Must be called with b.mu held.
func (b *AdapterBridge) configureAdapter(ctx context.Context, adapterID string, runtime map[string]string, config map[string]any) bool {
	// Try to create new adapter FIRST, before closing the old one.
	// This ensures we don't lose a working adapter if the new one fails.
	newAdapter := b.createAdapter(ctx, adapterID, runtime, config)
	if newAdapter == nil {
		log.Printf("[IntentRouter/Bridge] Failed to create adapter for %s, keeping previous adapter", adapterID)
		return false
	}

	// Success - now safe to close previous NAP adapter
	if b.napAdapter != nil {
		if err := b.napAdapter.Close(); err != nil {
			log.Printf("[IntentRouter/Bridge] Error closing previous NAP adapter: %v", err)
		}
		b.napAdapter = nil
	}

	// Track NAP adapter reference for cleanup and embedding bridge (if it's a NAP adapter)
	if nap, ok := newAdapter.(*NAPAdapter); ok {
		b.napAdapter = nap
		if b.embeddingBridge != nil {
			b.embeddingBridge.SetClient(nap.client)
		}
	} else {
		if b.embeddingBridge != nil {
			b.embeddingBridge.SetClient(nil)
		}
	}

	// Extract and store current address for endpoint change detection
	address := ""
	if runtime != nil {
		address = strings.TrimSpace(runtime[adapters.RuntimeExtraAddress])
	}

	b.current = adapterID
	b.currentAddress = address
	b.adapter = newAdapter
	b.service.SetAdapter(newAdapter)

	log.Printf("[IntentRouter/Bridge] Intent router configured with adapter: %s", adapterID)
	return true
}

// clearAdapter removes the current adapter.
// Must be called with b.mu held.
func (b *AdapterBridge) clearAdapter() {
	if b.current == "" {
		return
	}

	// Clean up NAP adapter connection
	if b.napAdapter != nil {
		if err := b.napAdapter.Close(); err != nil {
			log.Printf("[IntentRouter/Bridge] Error closing NAP adapter: %v", err)
		}
		b.napAdapter = nil
	}

	b.current = ""
	b.currentAddress = ""
	b.adapter = nil
	b.service.SetAdapter(nil)
	if b.embeddingBridge != nil {
		b.embeddingBridge.SetClient(nil)
	}

	log.Printf("[IntentRouter/Bridge] Intent router adapter cleared")
}

// createAdapter constructs the appropriate IntentAdapter for the given adapter ID.
// For builtin mocks, creates MockAdapter directly.
// For external adapters, creates NAPAdapter using the gRPC endpoint from runtime.
// Note: This is only called for AI slot adapters (bridge filters by SlotAI).
func (b *AdapterBridge) createAdapter(ctx context.Context, adapterID string, runtime map[string]string, config map[string]any) IntentAdapter {
	// Handle all builtin mock adapters (currently only AI mock, but future-proof).
	// The bridge only handles AI slot, so non-AI mocks won't reach this code.
	if adapters.IsBuiltinMockAdapter(adapterID) {
		return NewMockAdapter(
			WithMockName(adapterID),
			WithMockParseCommands(true),
			WithMockEchoMode(false),
		)
	}

	// For external adapters, create NAP AI client using gRPC
	address := ""
	transport := "process"
	if runtime != nil {
		address = strings.TrimSpace(runtime[adapters.RuntimeExtraAddress])
		if t := strings.TrimSpace(runtime[adapters.RuntimeExtraTransport]); t != "" {
			transport = t
		}
	}

	if address == "" {
		log.Printf("[IntentRouter/Bridge] NAP AI adapter %s has no endpoint address", adapterID)
		return nil
	}

	endpoint := configstore.AdapterEndpoint{
		AdapterID: adapterID,
		Transport: transport,
		Address:   address,
	}
	if runtime != nil {
		endpoint.TLSCertPath = strings.TrimSpace(runtime[adapters.RuntimeExtraTLSCertPath])
		endpoint.TLSKeyPath = strings.TrimSpace(runtime[adapters.RuntimeExtraTLSKeyPath])
		endpoint.TLSCACertPath = strings.TrimSpace(runtime[adapters.RuntimeExtraTLSCACertPath])
		endpoint.TLSInsecure = strings.TrimSpace(runtime[adapters.RuntimeExtraTLSInsecure]) == "true"
	}

	napAdapter, err := NewNAPAdapter(ctx, NAPAdapterParams{
		AdapterID: adapterID,
		Endpoint:  endpoint,
		Config:    config,
	})
	if err != nil {
		log.Printf("[IntentRouter/Bridge] Failed to create NAP AI adapter %s: %v", adapterID, err)
		return nil
	}

	log.Printf("[IntentRouter/Bridge] Created NAP AI adapter: %s (address: %s)", adapterID, address)
	return napAdapter
}

// parseConfig parses JSON config string into a map.
// Returns ErrInvalidConfig if the JSON is malformed.
// Empty config string returns nil config without error (config is optional).
// Note: Schema validation against manifest options is done separately in lookupAdapterConfig.
func (b *AdapterBridge) parseConfig(configJSON string) (map[string]any, error) {
	if configJSON == "" {
		return nil, nil
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	return config, nil
}

// adapterConfigInfo contains configuration details fetched from the controller.
type adapterConfigInfo struct {
	config  map[string]any
	version string // UpdatedAt if available, otherwise sha256 hash of config JSON
}

// computeConfigVersion returns a version string for change detection.
// Uses UpdatedAt if available, otherwise computes sha256 hash of config JSON.
// Always returns a deterministic version to ensure circuit breaker can detect changes.
func computeConfigVersion(updatedAt, configJSON string) string {
	if updatedAt = strings.TrimSpace(updatedAt); updatedAt != "" {
		return updatedAt
	}
	// Fallback to config hash when UpdatedAt is not available.
	// Always compute hash (even for empty config) to ensure consistent change detection.
	// This prevents false "no change" detection when config is actually modified.
	configJSON = strings.TrimSpace(configJSON)
	hash := sha256.Sum256([]byte(configJSON))
	return "hash:" + hex.EncodeToString(hash[:8]) // First 8 bytes is enough for change detection
}

// lookupAdapterConfig fetches the current config for an adapter from the controller.
// This is used when handling READY events where config isn't included in the event.
// Returns config info (including version for change detection) and any error.
// A nil config with nil error means no config is available (which is valid).
func (b *AdapterBridge) lookupAdapterConfig(ctx context.Context, adapterID string) (*adapterConfigInfo, error) {
	if b.controller == nil {
		return &adapterConfigInfo{version: computeConfigVersion("", "")}, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	statuses, err := b.controller.Overview(lookupCtx)
	if err != nil {
		return nil, fmt.Errorf("lookup adapter config: %w", err)
	}

	for _, status := range statuses {
		if status.Slot != adapters.SlotAI {
			continue
		}
		if status.AdapterID == nil || *status.AdapterID != adapterID {
			continue
		}

		config, err := b.parseConfig(status.Config)
		if err != nil {
			return nil, err
		}

		// Validate config against manifest options (manifest as source of truth).
		// If ManifestOptions returns error or nil, we skip validation:
		// - Builtin/mock adapters may not have manifests
		// - Controller may not have options registered yet
		// In these cases, we accept any config to avoid blocking READY.
		// Note: We use cached options to reduce I/O during adapter flapping.
		options, err := b.getCachedManifestOptions(lookupCtx, adapterID)
		if err != nil {
			// Log warning but don't block - adapter may work without manifest validation
			log.Printf("[IntentRouter/Bridge] Warning: manifest options lookup failed for %s: %v (skipping validation)", adapterID, err)
		} else if len(options) > 0 {
			// Only validate if manifest declares options.
			// Empty options (nil or empty map) means "no options declared" - accept any config.
			// This supports legacy adapters without manifests and builtin mocks.
			if err := manifest.ValidateConfigAgainstOptions(options, config); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
			}
		}
		// If options is empty (no error), adapter has no declared options - accept any config

		return &adapterConfigInfo{
			config:  config,
			version: computeConfigVersion(status.UpdatedAt, status.Config),
		}, nil
	}

	return &adapterConfigInfo{version: computeConfigVersion("", "")}, nil
}

// publishBridgeDiagnostic publishes a diagnostic event to the dedicated bridge topic.
// This allows UI/CLI to be notified when the bridge encounters configuration
// or adapter creation errors that prevent the AI slot from working properly.
//
// Bridge events are published on TopicIntentRouterDiagnostics, separate from
// adapter process lifecycle events (TopicAdaptersStatus). This ensures:
// - Clear separation of concerns (bridge config vs adapter process)
// - Consistent event format (BridgeDiagnosticEvent vs AdapterStatusEvent)
// - Easier filtering/routing for UI/CLI
//
// The diagnosticType parameter indicates the nature of the event:
//   - BridgeDiagnosticConfigInvalid: config validation failed (not recoverable)
//   - BridgeDiagnosticConnectionFailed: connection to adapter failed (recoverable)
//   - BridgeDiagnosticLookupFailed: controller/DB lookup failed (recoverable)
//   - BridgeDiagnosticConfigured: adapter successfully configured (informational)
//   - BridgeDiagnosticCleared: adapter cleared/disconnected (informational)
func (b *AdapterBridge) publishBridgeDiagnostic(ctx context.Context, adapterID, message string, diagnosticType eventbus.BridgeDiagnosticType) {
	if b.bus == nil {
		return
	}

	// Determine if error is recoverable based on type
	recoverable := diagnosticType != eventbus.BridgeDiagnosticConfigInvalid

	eventbus.Publish(ctx, b.bus, eventbus.IntentRouter.Diagnostics, eventbus.SourceIntentRouterBridge, eventbus.BridgeDiagnosticEvent{
		AdapterID:   adapterID,
		Type:        diagnosticType,
		Message:     message,
		Recoverable: recoverable,
		Timestamp:   time.Now(),
	})
}

// recordError increments the consecutive error counter and records the time.
// Also tracks which adapter/address/config caused the error for circuit breaker reset detection.
//
// When configVersion is empty (e.g., lookup failed), we preserve the existing lastConfigVersion
// to avoid false circuit breaker resets. Only update lastConfigVersion when we have valid info.
//
// Must be called with b.mu held.
func (b *AdapterBridge) recordError(adapterID, address, configVersion string) {
	b.consecutiveErrors++
	b.lastErrorTime = time.Now()
	b.lastFailedAdapter = adapterID
	b.lastFailedAddress = address
	// Only update config version if we have valid info (non-empty).
	// When lookup fails, we don't know the config, so we keep the previous value.
	if configVersion != "" {
		b.lastConfigVersion = configVersion
	}
}

// calculateBackoff returns the current backoff duration based on error count.
// Uses exponential backoff: baseBackoffDelay * 2^(errors - maxConsecutiveErrors)
// Capped at maxBackoffDelay.
func (b *AdapterBridge) calculateBackoff() time.Duration {
	if b.consecutiveErrors < maxConsecutiveErrors {
		return 0
	}

	// Exponential backoff: 500ms, 1s, 2s, 4s, 8s, 16s, 30s (capped)
	exponent := b.consecutiveErrors - maxConsecutiveErrors
	backoff := baseBackoffDelay * time.Duration(1<<exponent)
	if backoff > maxBackoffDelay {
		backoff = maxBackoffDelay
	}
	return backoff
}

// ResetCircuitBreaker resets the error counter, allowing immediate retry.
// This is useful when external conditions change (e.g., new adapter binding).
func (b *AdapterBridge) ResetCircuitBreaker() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveErrors = 0
}

// EmbeddingBridge returns the embedding provider backed by the current NAP adapter.
// Returns nil if no embedding bridge is available.
func (b *AdapterBridge) EmbeddingBridge() *EmbeddingBridge {
	return b.embeddingBridge
}
