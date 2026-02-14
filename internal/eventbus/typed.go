package eventbus

import (
	"log"
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
// that match the type parameter T. Mismatched payloads are logged and skipped.
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
// to the typed channel. Payloads that don't match T are logged and skipped.
//
// If bus is nil the returned subscription's channel is immediately closed
// and Close is a no-op — symmetric with Publish's nil-bus handling.
//
// The typed channel is unbuffered — backpressure is handled by the raw
// subscription's existing buffer.
func Subscribe[T any](bus *Bus, topic Topic, opts ...SubscriptionOption) *TypedSubscription[T] {
	if bus == nil {
		ch := make(chan TypedEnvelope[T])
		done := make(chan struct{})
		close(ch)
		close(done)
		return &TypedSubscription[T]{
			ch:   ch,
			done: done,
			quit: make(chan struct{}),
		}
	}

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
		if ts.raw != nil {
			ts.raw.Close()
		}
		<-ts.done
	})
}

// logger returns the bus logger if available, nil otherwise.
func (ts *TypedSubscription[T]) logger() *log.Logger {
	if ts.raw != nil && ts.raw.bus != nil {
		return ts.raw.bus.logger
	}
	return nil
}

func (ts *TypedSubscription[T]) bridge() {
	defer close(ts.done)
	defer close(ts.ch)

	for env := range ts.raw.C() {
		payload, ok := env.Payload.(T)
		if !ok {
			var zero T
			logger := ts.logger()
			if logger != nil {
				logger.Printf("[eventbus] typed subscription: type mismatch on topic %s: expected %T, got %T", env.Topic, zero, env.Payload)
			}
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
