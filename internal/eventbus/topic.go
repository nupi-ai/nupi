package eventbus

import (
	"context"
	"time"
)

// TopicDef binds a Topic string to a payload type T at compile time.
// Use with Publish, PublishWithOpts, and SubscribeTo for type-safe messaging.
type TopicDef[T any] struct{ topic Topic }

// NewTopicDef creates a typed topic descriptor for the given topic string.
func NewTopicDef[T any](topic Topic) TopicDef[T] { return TopicDef[T]{topic: topic} }

// Topic returns the underlying topic string.
func (d TopicDef[T]) Topic() Topic { return d.topic }

// Publish sends a typed payload on the bus using the topic descriptor.
// The compiler enforces that payload matches the type bound to the descriptor.
// If bus is nil the call is a no-op.
func Publish[T any](ctx context.Context, bus *Bus, td TopicDef[T], source Source, payload T) {
	if bus == nil {
		return
	}
	bus.publish(ctx, Envelope{
		Topic:   td.topic,
		Source:  source,
		Payload: payload,
	})
}

// PublishOption customises the Envelope built by PublishWithOpts.
type PublishOption func(*Envelope)

// WithTimestamp overrides the envelope timestamp (default is time.Now().UTC()).
func WithTimestamp(ts time.Time) PublishOption {
	return func(env *Envelope) {
		env.Timestamp = ts
	}
}

// WithCorrelationID sets the envelope correlation ID.
func WithCorrelationID(id string) PublishOption {
	return func(env *Envelope) {
		env.CorrelationID = id
	}
}

// PublishWithOpts is like Publish but accepts options to customise the envelope.
// If bus is nil the call is a no-op.
func PublishWithOpts[T any](ctx context.Context, bus *Bus, td TopicDef[T], source Source, payload T, opts ...PublishOption) {
	if bus == nil {
		return
	}
	env := Envelope{
		Topic:   td.topic,
		Source:  source,
		Payload: payload,
	}
	for _, opt := range opts {
		opt(&env)
	}
	bus.publish(ctx, env)
}

// SubscribeTo creates a typed subscription using a topic descriptor.
// It is equivalent to Subscribe[T](bus, td.Topic(), opts...) but ensures
// the subscription type matches the descriptor's payload type.
func SubscribeTo[T any](bus *Bus, td TopicDef[T], opts ...SubscriptionOption) *TypedSubscription[T] {
	return Subscribe[T](bus, td.topic, opts...)
}
