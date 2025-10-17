package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

// Service orchestrates adapter modules and publishes status updates on the event bus.
type Service struct {
	manager *Manager
	store   *configstore.Store
	bus     *eventbus.Bus

	watchInterval  time.Duration
	ensureInterval time.Duration

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	state   map[Slot]moduleState
	errMu   sync.Mutex
	lastErr string
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
	// ErrManagerNotConfigured indicates the service is missing the modules manager dependency.
	ErrManagerNotConfigured = errors.New("modules: manager is required")
	// ErrEventBusNotConfigured indicates an event bus must be provided.
	ErrEventBusNotConfigured = errors.New("modules: event bus is required")
)

type moduleState struct {
	adapter     string
	fingerprint string
	startedAt   time.Time
}

// NewService constructs a modules service responsible for driving module processes.
func NewService(manager *Manager, store *configstore.Store, bus *eventbus.Bus, opts ...ServiceOption) *Service {
	svc := &Service{
		manager:        manager,
		store:          store,
		bus:            bus,
		watchInterval:  time.Second,
		ensureInterval: 15 * time.Second,
		state:          make(map[Slot]moduleState),
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// Start launches watchers that keep module processes in sync with configuration.
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
			return fmt.Errorf("modules: start config watcher: %w", err)
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
				if ev.AdaptersChanged || ev.AdapterBindingsChanged || ev.ModuleEndpointsChanged {
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

	current := make(map[Slot]moduleState, len(s.state))
	for slot, state := range s.state {
		current[slot] = state
	}

	// Emit stop events for modules that disappeared or will be restarted.
	for slot, state := range current {
		binding, ok := runningBySlot[slot]
		if !ok {
			s.publishStatus(ctx, eventbus.ModuleStatusEvent{
				ModuleID: state.adapter,
				Slot:     string(slot),
				Status:   eventbus.ModuleHealthStopped,
				Message:  "module stopped",
			})
			delete(s.state, slot)
			continue
		}

		fingerprint := bindingFingerprint(binding)
		if state.fingerprint != fingerprint {
			s.publishStatus(ctx, eventbus.ModuleStatusEvent{
				ModuleID: state.adapter,
				Slot:     string(slot),
				Status:   eventbus.ModuleHealthStopped,
				Message:  "module restarting",
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

		newState := moduleState{
			adapter:     binding.AdapterID,
			fingerprint: fingerprint,
			startedAt:   now,
		}
		s.state[slot] = newState

		s.publishStatus(ctx, eventbus.ModuleStatusEvent{
			ModuleID:  binding.AdapterID,
			Slot:      string(slot),
			Status:    eventbus.ModuleHealthReady,
			Message:   "module ready",
			StartedAt: now,
		})
	}
}

func (s *Service) publishStatus(ctx context.Context, evt eventbus.ModuleStatusEvent) {
	if s.bus == nil {
		return
	}
	if evt.Status != eventbus.ModuleHealthError {
		s.clearLastError()
	}
	s.bus.Publish(ctx, eventbus.Envelope{
		Topic:   eventbus.TopicModulesStatus,
		Source:  eventbus.SourceModulesService,
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
		Topic:  eventbus.TopicModulesStatus,
		Source: eventbus.SourceModulesService,
		Payload: eventbus.ModuleStatusEvent{
			Status:  eventbus.ModuleHealthError,
			Message: msg,
		},
	})
}

// Shutdown stops background goroutines and terminates managed module processes.
func (s *Service) Shutdown(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	return s.manager.StopAll(ctx)
}

func bindingFingerprint(binding Binding) string {
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
