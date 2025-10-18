package eventbus_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

func TestBusPublishDeliver(t *testing.T) {
	bus := eventbus.New()
	sub := bus.Subscribe(eventbus.TopicSessionsOutput)
	defer sub.Close()

	payload := eventbus.SessionOutputEvent{
		SessionID: "sess-1",
		Sequence:  1,
		Data:      []byte("hello"),
		Origin:    eventbus.OriginTool,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	bus.Publish(ctx, eventbus.Envelope{
		Topic:   eventbus.TopicSessionsOutput,
		Source:  eventbus.SourceSessionManager,
		Payload: payload,
	})

	select {
	case env := <-sub.C():
		msg, ok := env.Payload.(eventbus.SessionOutputEvent)
		if !ok {
			t.Fatalf("expected SessionOutputEvent payload, got %T", env.Payload)
		}
		if msg.Sequence != payload.Sequence {
			t.Fatalf("expected sequence %d, got %d", payload.Sequence, msg.Sequence)
		}
		if string(msg.Data) != "hello" {
			t.Fatalf("unexpected payload data: %q", string(msg.Data))
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}

	metrics := bus.Metrics()
	if metrics.PublishTotal != 1 {
		t.Fatalf("expected PublishTotal 1, got %d", metrics.PublishTotal)
	}
}

func TestBusDropOldest(t *testing.T) {
	bus := eventbus.New(eventbus.WithTopicBuffer(eventbus.TopicSessionsOutput, 1))
	sub := bus.Subscribe(eventbus.TopicSessionsOutput, eventbus.WithSubscriptionBuffer(1))
	defer sub.Close()

	ctx := context.Background()

	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsOutput,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionOutputEvent{
			SessionID: "sess-drop",
			Sequence:  1,
		},
	})

	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsOutput,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionOutputEvent{
			SessionID: "sess-drop",
			Sequence:  2,
		},
	})

	select {
	case env := <-sub.C():
		msg, ok := env.Payload.(eventbus.SessionOutputEvent)
		if !ok {
			t.Fatalf("expected SessionOutputEvent payload, got %T", env.Payload)
		}
		if msg.Sequence != 2 {
			t.Fatalf("expected sequence 2 after drop-oldest, got %d", msg.Sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event after drops")
	}

	metrics := bus.Metrics()
	if metrics.DroppedTotal == 0 {
		t.Fatal("expected dropped events to be recorded")
	}
}

func TestBusObserver(t *testing.T) {
	var count atomic.Uint64

	observer := eventbus.ObserverFunc(func(env eventbus.Envelope) {
		if env.Topic == eventbus.TopicPipelineCleaned {
			count.Add(1)
		}
	})

	bus := eventbus.New(eventbus.WithObserver(observer))
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:  eventbus.TopicPipelineCleaned,
		Source: eventbus.SourceContentPipeline,
	})

	if got := count.Load(); got != 1 {
		t.Fatalf("expected observer to be invoked once, got %d", got)
	}

	// Adding observer after construction should also work.
	bus.AddObserver(observer)
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:  eventbus.TopicPipelineCleaned,
		Source: eventbus.SourceContentPipeline,
	})

	if got := count.Load(); got != 3 {
		t.Fatalf("expected observer to be invoked three times, got %d", got)
	}
}
