package eventbus

import (
	"sync"
	"time"
)

// TypedEnvelope is a generic wrapper around Envelope with a typed payload.
type TypedEnvelope[T any] struct {
	Topic         Topic
	Timestamp     time.Time
	Source        Source
	CorrelationID string
	Payload       T
}

// TypedSubscription wraps a raw Subscription and delivers only payloads
// that match the type parameter T. Mismatched payloads are silently skipped.
type TypedSubscription[T any] struct {
	raw       *Subscription
	ch        chan TypedEnvelope[T]
	done      chan struct{}
	quit      chan struct{}
	closeOnce sync.Once
}

// Subscribe creates a typed subscription on the given bus and topic.
// A bridge goroutine reads from the underlying Subscription, performs a
// type assertion on each Envelope.Payload, and forwards matching events
// to the typed channel. Payloads that don't match T are silently dropped.
//
// The typed channel is unbuffered — backpressure is handled by the raw
// subscription's existing buffer.
func Subscribe[T any](bus *Bus, topic Topic, opts ...SubscriptionOption) *TypedSubscription[T] {
	raw := bus.Subscribe(topic, opts...)

	ts := &TypedSubscription[T]{
		raw:  raw,
		ch:   make(chan TypedEnvelope[T]),
		done: make(chan struct{}),
		quit: make(chan struct{}),
	}

	go ts.bridge()
	return ts
}

// C returns the typed event channel.
func (ts *TypedSubscription[T]) C() <-chan TypedEnvelope[T] {
	return ts.ch
}

// Close stops the bridge goroutine and closes the underlying subscription.
// It is safe to call Close multiple times.
func (ts *TypedSubscription[T]) Close() {
	ts.closeOnce.Do(func() {
		close(ts.quit)
	})
	ts.raw.Close()
	<-ts.done
}

func (ts *TypedSubscription[T]) bridge() {
	defer close(ts.done)
	defer close(ts.ch)

	for env := range ts.raw.C() {
		payload, ok := env.Payload.(T)
		if !ok {
			continue
		}
		typed := TypedEnvelope[T]{
			Topic:         env.Topic,
			Timestamp:     env.Timestamp,
			Source:        env.Source,
			CorrelationID: env.CorrelationID,
			Payload:       payload,
		}
		select {
		case ts.ch <- typed:
		case <-ts.quit:
			return
		}
	}
}
