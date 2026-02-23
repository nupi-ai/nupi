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
