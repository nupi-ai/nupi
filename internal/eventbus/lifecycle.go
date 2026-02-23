package eventbus

import (
	"context"
	"reflect"
	"sync"
)

// SubscriptionCloser is the minimal contract required to close a subscription.
type SubscriptionCloser interface {
	Close()
}

// SubscriptionGroup tracks subscriptions that should be closed together.
type SubscriptionGroup struct {
	mu   sync.Mutex
	subs []SubscriptionCloser
}

// Add registers subscriptions for bulk shutdown. Nil values are ignored.
func (g *SubscriptionGroup) Add(subs ...SubscriptionCloser) {
	if g == nil || len(subs) == 0 {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	for _, sub := range subs {
		if !isNilSubscription(sub) {
			g.subs = append(g.subs, sub)
		}
	}
}

// CloseAll closes all tracked subscriptions and clears the group.
func (g *SubscriptionGroup) CloseAll() {
	if g == nil {
		return
	}

	g.mu.Lock()
	subs := g.subs
	g.subs = nil
	g.mu.Unlock()

	for _, sub := range subs {
		sub.Close()
	}
}

func isNilSubscription(sub SubscriptionCloser) bool {
	if sub == nil {
		return true
	}
	v := reflect.ValueOf(sub)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// ServiceLifecycle centralises common service lifecycle plumbing:
// start context, track subscriptions, run workers, and wait for shutdown.
type ServiceLifecycle struct {
	ctx    context.Context
	cancel context.CancelFunc
	subs   SubscriptionGroup
	wg     sync.WaitGroup
}

// Start initialises the service context using the provided parent context.
func (l *ServiceLifecycle) Start(ctx context.Context) {
	l.ctx, l.cancel = context.WithCancel(ctx)
}

// Context returns the active service context.
func (l *ServiceLifecycle) Context() context.Context {
	return l.ctx
}

// AddSubscriptions registers subscriptions that should be closed on shutdown.
func (l *ServiceLifecycle) AddSubscriptions(subs ...SubscriptionCloser) {
	l.subs.Add(subs...)
}

// Go runs a worker goroutine tracked by the lifecycle wait group.
func (l *ServiceLifecycle) Go(worker func(ctx context.Context)) {
	if worker == nil {
		return
	}
	l.wg.Add(1)
	go func(ctx context.Context) {
		defer l.wg.Done()
		worker(ctx)
	}(l.ctx)
}

// Stop cancels the service context and closes tracked subscriptions.
func (l *ServiceLifecycle) Stop() {
	if l.cancel != nil {
		l.cancel()
	}
	l.subs.CloseAll()
}

// Wait blocks until all lifecycle workers complete or ctx is done.
func (l *ServiceLifecycle) Wait(ctx context.Context) error {
	return WaitForWorkers(ctx, &l.wg)
}

// Shutdown combines Stop and Wait for convenience.
func (l *ServiceLifecycle) Shutdown(ctx context.Context) error {
	l.Stop()
	return l.Wait(ctx)
}

// WaitForWorkers waits for the provided wait group or returns when ctx is done.
func WaitForWorkers(ctx context.Context, wg *sync.WaitGroup) error {
	if wg == nil {
		return nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
