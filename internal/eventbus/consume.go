package eventbus

import (
	"context"
	"sync"
)

// Consume reads typed events from sub until context cancellation or
// subscription closure, then forwards payloads to handler.
func Consume[T any](ctx context.Context, sub *TypedSubscription[T], wg *sync.WaitGroup, handler func(T)) {
	ConsumeEnvelope(ctx, sub, wg, func(env TypedEnvelope[T]) {
		handler(env.Payload)
	})
}

// ConsumeEnvelope reads typed events from sub until context cancellation or
// subscription closure, then forwards full envelopes to handler.
func ConsumeEnvelope[T any](ctx context.Context, sub *TypedSubscription[T], wg *sync.WaitGroup, handler func(TypedEnvelope[T])) {
	if wg != nil {
		defer wg.Done()
	}
	if sub == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			handler(env)
		}
	}
}
