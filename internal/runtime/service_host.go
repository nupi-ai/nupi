package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
)

// ServiceFactory constructs a service instance. It is invoked on every start or restart.
type ServiceFactory func(ctx context.Context) (Service, error)

// ServiceHost orchestrates the lifecycle of runtime services.
type ServiceHost struct {
	mu        sync.Mutex
	order     []string
	entries   map[string]*serviceRegistration
	started   bool
	errors    chan error
	cancel    context.CancelFunc
	parentCtx context.Context
}

// Option configures a service registration.
type Option func(*serviceRegistration)

type serviceRegistration struct {
	name            string
	factory         ServiceFactory
	service         Service
	shutdownTimeout time.Duration
	errWatch        chan error
}

// WithShutdownTimeout customises the shutdown timeout for a service.
func WithShutdownTimeout(timeout time.Duration) Option {
	return func(reg *serviceRegistration) {
		reg.shutdownTimeout = timeout
	}
}

// NewServiceHost creates a new service host.
func NewServiceHost() *ServiceHost {
	return &ServiceHost{
		entries: make(map[string]*serviceRegistration),
		errors:  make(chan error, 1),
	}
}

// Register registers a service factory under the provided name.
func (h *ServiceHost) Register(name string, factory ServiceFactory, opts ...Option) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.started {
		return fmt.Errorf("runtime: cannot register service %q after start", name)
	}
	if _, exists := h.entries[name]; exists {
		return fmt.Errorf("runtime: service %q already registered", name)
	}

	reg := &serviceRegistration{
		name:            name,
		factory:         factory,
		shutdownTimeout: 5 * time.Second,
	}
	for _, opt := range opts {
		opt(reg)
	}

	h.entries[name] = reg
	h.order = append(h.order, name)
	return nil
}

// Start initialises and starts all registered services in registration order.
func (h *ServiceHost) Start(ctx context.Context) error {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return fmt.Errorf("runtime: service host already started")
	}
	h.started = true
	h.parentCtx, h.cancel = context.WithCancel(ctx)
	h.mu.Unlock()

	started := make([]*serviceRegistration, 0, len(h.order))

	for _, name := range h.order {
		reg := h.getRegistration(name)
		if reg == nil {
			continue
		}

		svc, err := reg.factory(h.parentCtx)
		if err != nil {
			h.stopStarted(started)
			return fmt.Errorf("runtime: create service %q: %w", name, err)
		}

		if err := svc.Start(h.parentCtx); err != nil {
			h.stopStarted(started)
			return fmt.Errorf("runtime: start service %q: %w", name, err)
		}

		reg.service = svc
		h.watchErrors(reg)
		started = append(started, reg)
	}

	return nil
}

// Stop gracefully stops all services in reverse registration order.
func (h *ServiceHost) Stop(ctx context.Context) error {
	h.mu.Lock()
	if !h.started {
		h.mu.Unlock()
		return nil
	}
	h.started = false
	cancel := h.cancel
	h.cancel = nil
	h.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	var stopErr error
	for i := len(h.order) - 1; i >= 0; i-- {
		name := h.order[i]
		reg := h.getRegistration(name)
		if reg == nil || reg.service == nil {
			continue
		}

		timeout := reg.shutdownTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}

		stopCtx, cancel := context.WithTimeout(ctx, timeout)
		if err := reg.service.Shutdown(stopCtx); err != nil && err != context.Canceled {
			stopErr = fmt.Errorf("runtime: shutdown service %q: %w", name, err)
		}
		cancel()
		reg.service = nil
		reg.errWatch = nil
	}

	return stopErr
}

// Restart stops and starts the given service.
func (h *ServiceHost) Restart(ctx context.Context, name string) error {
	h.mu.Lock()
	if !h.started {
		h.mu.Unlock()
		return fmt.Errorf("runtime: host not started")
	}
	reg := h.entries[name]
	h.mu.Unlock()

	if reg == nil {
		return fmt.Errorf("runtime: service %q not registered", name)
	}

	if reg.service != nil {
		timeout := reg.shutdownTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}

		stopCtx, cancel := context.WithTimeout(ctx, timeout)
		err := reg.service.Shutdown(stopCtx)
		cancel()
		if err != nil && err != context.Canceled {
			return fmt.Errorf("runtime: shutdown service %q: %w", name, err)
		}
		reg.service = nil
		reg.errWatch = nil
	}

	svc, err := reg.factory(h.parentCtx)
	if err != nil {
		return fmt.Errorf("runtime: recreate service %q: %w", name, err)
	}

	if err := svc.Start(h.parentCtx); err != nil {
		return fmt.Errorf("runtime: restart service %q: %w", name, err)
	}

	reg.errWatch = nil
	reg.service = svc
	h.watchErrors(reg)
	return nil
}

// Errors returns a channel receiving fatal service errors.
func (h *ServiceHost) Errors() <-chan error {
	return h.errors
}

// WatchConfig starts polling the configuration store and invokes handler on change events.
// The returned cancel function stops the watcher. The watcher runs only while the host is started.
func (h *ServiceHost) WatchConfig(ctx context.Context, store *configstore.Store, interval time.Duration, handler func(configstore.ChangeEvent)) (func(), error) {
	h.mu.Lock()
	if !h.started {
		h.mu.Unlock()
		return nil, fmt.Errorf("runtime: cannot watch config before host is started")
	}
	parentCtx := h.parentCtx
	h.mu.Unlock()

	watchCtx, cancel := context.WithCancel(parentCtx)
	events, err := store.Watch(watchCtx, interval)
	if err != nil {
		cancel()
		return nil, err
	}

	go func() {
		for {
			select {
			case <-watchCtx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				if handler != nil {
					handler(ev)
				}
			}
		}
	}()

	return cancel, nil
}

func (h *ServiceHost) getRegistration(name string) *serviceRegistration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.entries[name]
}

func (h *ServiceHost) watchErrors(reg *serviceRegistration) {
	if reg.service == nil {
		return
	}

	observable, ok := reg.service.(interface{ Errors() <-chan error })
	if !ok {
		return
	}

	if reg.errWatch != nil {
		return
	}

	reg.errWatch = make(chan error, 1)

	go func(name string, ch <-chan error) {
		for err := range ch {
			if err == nil {
				continue
			}
			select {
			case h.errors <- fmt.Errorf("%s service error: %w", name, err):
			default:
			}
		}
	}(reg.name, observable.Errors())
}

func (h *ServiceHost) stopStarted(started []*serviceRegistration) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := len(started) - 1; i >= 0; i-- {
		if started[i].service != nil {
			started[i].service.Shutdown(ctx) // ignore errors during rollback
			started[i].service = nil
			started[i].errWatch = nil
		}
	}
}
