package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/plugins/manifest"
)

// Service orchestrates adapters and publishes status updates on the event bus.
type Service struct {
	manager *Manager
	store   *configstore.Store
	bus     *eventbus.Bus

	watchInterval  time.Duration
	ensureInterval time.Duration

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	state    map[Slot]adapterState
	errMu    sync.Mutex
	lastErr  string
	statusMu sync.RWMutex
	statuses map[Slot]runtimeStatus
}

// ServiceOption configures the service behaviour.
type ServiceOption func(*Service)

// WithWatchInterval overrides how frequently the service polls the configuration store.
func WithWatchInterval(d time.Duration) ServiceOption {
	return func(s *Service) {
		if d > 0 {
			s.watchInterval = d
		}
	}
}

// WithEnsureInterval sets the reconciliation interval used in addition to configuration watches.
func WithEnsureInterval(d time.Duration) ServiceOption {
	return func(s *Service) {
		if d >= 0 {
			s.ensureInterval = d
		}
	}
}

var (
	// ErrManagerNotConfigured indicates the service is missing the adapters manager dependency.
	ErrManagerNotConfigured = errors.New("adapters: manager is required")
	// ErrEventBusNotConfigured indicates an event bus must be provided.
	ErrEventBusNotConfigured = errors.New("adapters: event bus is required")
)

type adapterState struct {
	adapter     string
	fingerprint string
	startedAt   time.Time
}

type runtimeStatus struct {
	event    eventbus.AdapterStatusEvent
	recorded time.Time
}

// RuntimeStatus describes the last known runtime state of a adapter process.
type RuntimeStatus struct {
	AdapterID string
	Health    eventbus.AdapterHealth
	Message   string
	StartedAt *time.Time
	UpdatedAt time.Time
	Extra     map[string]string
}

// BindingStatus aggregates configuration binding metadata with runtime status.
type BindingStatus struct {
	Slot      Slot
	AdapterID *string
	Status    string
	Config    string
	UpdatedAt string
	Runtime   *RuntimeStatus
}

// NewService constructs a adapters service responsible for driving adapter processes.
func NewService(manager *Manager, store *configstore.Store, bus *eventbus.Bus, opts ...ServiceOption) *Service {
	svc := &Service{
		manager:        manager,
		store:          store,
		bus:            bus,
		watchInterval:  time.Second,
		ensureInterval: 15 * time.Second,
		state:          make(map[Slot]adapterState),
		statuses:       make(map[Slot]runtimeStatus),
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// Start launches watchers that keep adapter processes in sync with configuration.
func (s *Service) Start(ctx context.Context) error {
	if s.manager == nil {
		return ErrManagerNotConfigured
	}
	if s.bus == nil {
		return ErrEventBusNotConfigured
	}

	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	if err := s.reconcile(runCtx); err != nil && !errors.Is(err, ErrBindingSourceNotConfigured) && !errors.Is(err, ErrRunnerManagerNotConfigured) {
		// Non-fatal errors are emitted on the bus; continue running to allow retries.
	}

	if s.store != nil {
		if err := s.startWatcher(runCtx); err != nil {
			return fmt.Errorf("adapters: start config watcher: %w", err)
		}
	}

	if s.ensureInterval > 0 {
		s.startTicker(runCtx)
	}

	return nil
}

func (s *Service) startWatcher(ctx context.Context) error {
	events, err := s.store.Watch(ctx, s.watchInterval)
	if err != nil {
		return err
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				if ev.AdaptersChanged || ev.AdapterBindingsChanged || ev.AdapterEndpointsChanged {
					s.reconcile(ctx)
				}
			}
		}
	}()
	return nil
}

func (s *Service) startTicker(ctx context.Context) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.ensureInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.reconcile(ctx)
			}
		}
	}()
}

func (s *Service) reconcile(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	err := s.manager.Ensure(ctx)
	if err != nil {
		s.publishError(ctx, err)
	} else {
		s.clearLastError()
	}

	running := s.manager.Running()
	s.updateState(ctx, running)
	return err
}

func (s *Service) updateState(ctx context.Context, running []Binding) {
	s.mu.Lock()
	defer s.mu.Unlock()

	runningBySlot := make(map[Slot]Binding, len(running))
	for _, binding := range running {
		runningBySlot[binding.Slot] = binding
	}

	current := make(map[Slot]adapterState, len(s.state))
	for slot, state := range s.state {
		current[slot] = state
	}

	// Emit stop events for adapters that disappeared or will be restarted.
	for slot, state := range current {
		binding, ok := runningBySlot[slot]
		if !ok {
			s.publishStatus(ctx, eventbus.AdapterStatusEvent{
				AdapterID: state.adapter,
				Slot:      string(slot),
				Status:    eventbus.AdapterHealthStopped,
				Message:   "adapter stopped",
			})
			delete(s.state, slot)
			continue
		}

		fingerprint := bindingFingerprint(binding)
		if state.fingerprint != fingerprint {
			s.publishStatus(ctx, eventbus.AdapterStatusEvent{
				AdapterID: state.adapter,
				Slot:      string(slot),
				Status:    eventbus.AdapterHealthStopped,
				Message:   "adapter restarting",
			})
			delete(s.state, slot)
		}
	}

	now := time.Now().UTC()
	for slot, binding := range runningBySlot {
		fingerprint := bindingFingerprint(binding)
		if state, ok := s.state[slot]; ok && state.fingerprint == fingerprint {
			continue
		}

		newState := adapterState{
			adapter:     binding.AdapterID,
			fingerprint: fingerprint,
			startedAt:   now,
		}
		s.state[slot] = newState

		extra := cloneStringMap(binding.Runtime)
		s.publishStatus(ctx, eventbus.AdapterStatusEvent{
			AdapterID: binding.AdapterID,
			Slot:      string(slot),
			Status:    eventbus.AdapterHealthReady,
			Message:   "adapter ready",
			StartedAt: now,
			Extra:     extra,
		})
	}
}

func (s *Service) publishStatus(ctx context.Context, evt eventbus.AdapterStatusEvent) {
	slot := Slot(strings.TrimSpace(evt.Slot))
	if slot != "" {
		s.recordRuntimeStatus(slot, evt)
	}
	if evt.Status != eventbus.AdapterHealthError {
		s.clearLastError()
	}
	if s.bus == nil {
		return
	}
	s.bus.Publish(ctx, eventbus.Envelope{
		Topic:   eventbus.TopicAdaptersStatus,
		Source:  eventbus.SourceAdaptersService,
		Payload: evt,
	})
}

func (s *Service) publishError(ctx context.Context, err error) {
	if err == nil || s.bus == nil {
		return
	}

	msg := err.Error()

	s.errMu.Lock()
	if msg == s.lastErr {
		s.errMu.Unlock()
		return
	}
	s.lastErr = msg
	s.errMu.Unlock()

	s.bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicAdaptersStatus,
		Source: eventbus.SourceAdaptersService,
		Payload: eventbus.AdapterStatusEvent{
			Status:  eventbus.AdapterHealthError,
			Message: msg,
		},
	})
}

// Overview returns configuration bindings enriched with runtime status.
func (s *Service) Overview(ctx context.Context) ([]BindingStatus, error) {
	if s.store == nil {
		return nil, errors.New("adapters: configuration store unavailable")
	}

	records, err := s.store.ListAdapterBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("adapters: list adapter bindings: %w", err)
	}

	runtime := s.runtimeSnapshot()
	out := make([]BindingStatus, 0, len(records))
	for _, record := range records {
		slot := Slot(record.Slot)
		status := BindingStatus{
			Slot:      slot,
			Status:    record.Status,
			Config:    record.Config,
			UpdatedAt: record.UpdatedAt,
		}
		if record.AdapterID != nil {
			id := strings.TrimSpace(*record.AdapterID)
			if id != "" {
				copyID := id
				status.AdapterID = &copyID
			}
		}
		if rt, ok := runtime[slot]; ok {
			copyRT := rt
			status.Runtime = &copyRT
		}
		out = append(out, status)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Slot < out[j].Slot
	})

	return out, nil
}

// StartSlot marks the slot as active and reconciles adapter processes.
func (s *Service) StartSlot(ctx context.Context, slot Slot) (*BindingStatus, error) {
	if s.store == nil {
		return nil, errors.New("adapters: configuration store unavailable")
	}

	binding, err := s.bindingRecord(ctx, slot)
	if err != nil {
		return nil, err
	}
	if binding.AdapterID == nil || strings.TrimSpace(*binding.AdapterID) == "" {
		return nil, fmt.Errorf("adapters: slot %s has no adapter bound", slot)
	}

	if err := s.store.UpdateAdapterBindingStatus(ctx, string(slot), configstore.BindingStatusActive); err != nil {
		return nil, fmt.Errorf("adapters: activate binding %s: %w", slot, err)
	}

	if err := s.reconcile(ctx); err != nil {
		return nil, err
	}

	return s.bindingStatusForSlot(ctx, slot)
}

// StopSlot marks the slot as inactive and stops the running adapter process.
func (s *Service) StopSlot(ctx context.Context, slot Slot) (*BindingStatus, error) {
	if s.store == nil {
		return nil, errors.New("adapters: configuration store unavailable")
	}

	if _, err := s.bindingRecord(ctx, slot); err != nil {
		return nil, err
	}

	if err := s.store.UpdateAdapterBindingStatus(ctx, string(slot), configstore.BindingStatusInactive); err != nil {
		return nil, fmt.Errorf("adapters: deactivate binding %s: %w", slot, err)
	}

	if err := s.manager.StopSlot(ctx, slot); err != nil {
		return nil, fmt.Errorf("adapters: stop %s: %w", slot, err)
	}

	if err := s.reconcile(ctx); err != nil {
		return nil, err
	}

	return s.bindingStatusForSlot(ctx, slot)
}

func (s *Service) bindingStatusForSlot(ctx context.Context, slot Slot) (*BindingStatus, error) {
	overview, err := s.Overview(ctx)
	if err != nil {
		return nil, err
	}
	for _, status := range overview {
		if status.Slot == slot {
			copyStatus := status
			return &copyStatus, nil
		}
	}
	return nil, fmt.Errorf("adapters: slot %s not found", slot)
}

func (s *Service) bindingRecord(ctx context.Context, slot Slot) (*configstore.AdapterBinding, error) {
	if s.store == nil {
		return nil, errors.New("adapters: configuration store unavailable")
	}

	records, err := s.store.ListAdapterBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("adapters: list adapter bindings: %w", err)
	}
	for _, record := range records {
		if record.Slot != string(slot) {
			continue
		}
		copyRecord := record
		return &copyRecord, nil
	}
	return nil, configstore.NotFoundError{Entity: "adapter_binding", Key: string(slot)}
}

func (s *Service) runtimeSnapshot() map[Slot]RuntimeStatus {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()

	out := make(map[Slot]RuntimeStatus, len(s.statuses))
	for slot, st := range s.statuses {
		out[slot] = convertRuntimeStatus(st)
	}
	return out
}

func (s *Service) recordRuntimeStatus(slot Slot, evt eventbus.AdapterStatusEvent) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.statuses[slot] = runtimeStatus{
		event:    cloneAdapterStatusEvent(evt),
		recorded: time.Now().UTC(),
	}
}

func convertRuntimeStatus(in runtimeStatus) RuntimeStatus {
	var started *time.Time
	if !in.event.StartedAt.IsZero() {
		ts := in.event.StartedAt
		started = &ts
	}
	var extra map[string]string
	if len(in.event.Extra) > 0 {
		extra = make(map[string]string, len(in.event.Extra))
		for k, v := range in.event.Extra {
			extra[k] = v
		}
	}
	return RuntimeStatus{
		AdapterID: in.event.AdapterID,
		Health:    in.event.Status,
		Message:   in.event.Message,
		StartedAt: started,
		UpdatedAt: in.recorded,
		Extra:     extra,
	}
}

func cloneAdapterStatusEvent(evt eventbus.AdapterStatusEvent) eventbus.AdapterStatusEvent {
	cloned := evt
	if len(evt.Extra) > 0 {
		extra := make(map[string]string, len(evt.Extra))
		for k, v := range evt.Extra {
			extra[k] = v
		}
		cloned.Extra = extra
	}
	return cloned
}

// Shutdown stops background goroutines and terminates managed adapter processes.
func (s *Service) Shutdown(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	return s.manager.StopAll(ctx)
}

func bindingFingerprint(binding Binding) string {
	if fp := strings.TrimSpace(binding.Fingerprint); fp != "" {
		return fp
	}
	raw := strings.TrimSpace(binding.RawConfig)
	if raw == "" && len(binding.Config) > 0 {
		rawBytes, _ := json.Marshal(binding.Config)
		raw = string(rawBytes)
	}
	return binding.AdapterID + "|" + raw
}

func (s *Service) clearLastError() {
	s.errMu.Lock()
	s.lastErr = ""
	s.errMu.Unlock()
}

// ManifestOptions returns the configuration options declared in an adapter's manifest.
// Returns nil map (not error) for builtin adapters or adapters without manifest options.
// This allows the intent router bridge to validate config against the manifest schema.
func (s *Service) ManifestOptions(ctx context.Context, adapterID string) (map[string]manifest.AdapterOption, error) {
	if s.manager == nil {
		return nil, ErrManagerNotConfigured
	}

	// Builtin mock adapters don't have manifests
	if IsBuiltinMockAdapter(adapterID) {
		return nil, nil
	}

	_, mf, err := s.manager.lookupAdapter(ctx, adapterID)
	if err != nil {
		return nil, fmt.Errorf("adapters: lookup adapter manifest: %w", err)
	}

	if mf == nil || mf.Adapter == nil {
		return nil, nil
	}

	return mf.Adapter.Options, nil
}
