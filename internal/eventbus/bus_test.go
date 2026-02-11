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

func TestBusOverflowDelivery(t *testing.T) {
	// Critical topics route ALL events through the overflow buffer to preserve FIFO ordering.
	// The drain goroutine moves events from overflow → channel asynchronously.
	bus := eventbus.New(eventbus.WithTopicBuffer(eventbus.TopicConversationPrompt, 1))
	sub := bus.Subscribe(eventbus.TopicConversationPrompt, eventbus.WithSubscriptionBuffer(1))
	defer sub.Close()

	ctx := context.Background()

	// Both events go through overflow buffer → drain goroutine → channel.
	bus.Publish(ctx, eventbus.Envelope{
		Topic:   eventbus.TopicConversationPrompt,
		Source:  eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{PromptID: "p1"},
	})

	bus.Publish(ctx, eventbus.Envelope{
		Topic:   eventbus.TopicConversationPrompt,
		Source:  eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{PromptID: "p2"},
	})

	// We should receive both events in FIFO order via the drain goroutine.
	var received []string
	timeout := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case env := <-sub.C():
			msg := env.Payload.(eventbus.ConversationPromptEvent)
			received = append(received, msg.PromptID)
		case <-timeout:
			t.Fatalf("timed out, only received %d events: %v", len(received), received)
		}
	}

	if len(received) != 2 || received[0] != "p1" || received[1] != "p2" {
		t.Fatalf("expected [p1, p2], got %v", received)
	}

	metrics := bus.Metrics()
	// With overflow strategy, all events route through the overflow buffer.
	if metrics.OverflowTotal < 2 {
		t.Fatalf("expected OverflowTotal >= 2, got %d", metrics.OverflowTotal)
	}
	if metrics.DroppedTotal != 0 {
		t.Fatalf("expected no drops, got %d", metrics.DroppedTotal)
	}
}

func TestBusDropNewest(t *testing.T) {
	// Low-priority topics use drop-newest.
	bus := eventbus.New(eventbus.WithTopicBuffer(eventbus.TopicAdaptersLog, 1))
	sub := bus.Subscribe(eventbus.TopicAdaptersLog, eventbus.WithSubscriptionBuffer(1))
	defer sub.Close()

	ctx := context.Background()

	bus.Publish(ctx, eventbus.Envelope{
		Topic:   eventbus.TopicAdaptersLog,
		Source:  eventbus.SourceAdaptersService,
		Payload: eventbus.AdapterLogEvent{Message: "first"},
	})

	bus.Publish(ctx, eventbus.Envelope{
		Topic:   eventbus.TopicAdaptersLog,
		Source:  eventbus.SourceAdaptersService,
		Payload: eventbus.AdapterLogEvent{Message: "second"},
	})

	// With drop-newest, the first event should be retained.
	select {
	case env := <-sub.C():
		msg := env.Payload.(eventbus.AdapterLogEvent)
		if msg.Message != "first" {
			t.Fatalf("expected first event to survive, got %q", msg.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}

	metrics := bus.Metrics()
	if metrics.DroppedTotal == 0 {
		t.Fatal("expected dropped events for drop-newest")
	}
}

func TestBusOverflowFallback(t *testing.T) {
	// When the overflow buffer is full, deliver() falls back to drop-oldest on the channel.
	// With overflow=2 and channel=1, rapid publishes fill both overflow and channel via
	// the drain goroutine, eventually triggering the fallback drop path.
	bus := eventbus.New(
		eventbus.WithTopicBuffer(eventbus.TopicConversationPrompt, 1),
		eventbus.WithTopicPolicy(eventbus.TopicConversationPrompt, eventbus.DeliveryPolicy{
			Strategy:    eventbus.StrategyOverflow,
			Priority:    eventbus.PriorityCritical,
			MaxOverflow: 2, // small overflow buffer
		}),
	)
	sub := bus.Subscribe(eventbus.TopicConversationPrompt, eventbus.WithSubscriptionBuffer(1))
	defer sub.Close()

	ctx := context.Background()

	// Publish enough events to guarantee the overflow is full. With overflow=2 and channel=1,
	// we need at least 4 publishes. Publish extra to be sure at least one drops.
	for i := 0; i < 10; i++ {
		bus.Publish(ctx, eventbus.Envelope{
			Topic:   eventbus.TopicConversationPrompt,
			Source:  eventbus.SourceConversation,
			Payload: eventbus.ConversationPromptEvent{PromptID: "p"},
		})
	}

	metrics := bus.Metrics()
	if metrics.DroppedTotal == 0 {
		t.Fatal("expected a drop when overflow buffer is full")
	}
}

func TestBusOverflowMetrics(t *testing.T) {
	bus := eventbus.New(eventbus.WithTopicBuffer(eventbus.TopicSessionsLifecycle, 1))
	sub := bus.Subscribe(eventbus.TopicSessionsLifecycle, eventbus.WithSubscriptionBuffer(1))
	defer sub.Close()

	ctx := context.Background()

	// Both events route through the overflow buffer (critical topic uses StrategyOverflow).
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsLifecycle,
		Source: eventbus.SourceSessionManager,
	})
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsLifecycle,
		Source: eventbus.SourceSessionManager,
	})

	metrics := bus.Metrics()
	if metrics.OverflowTotal < 2 {
		t.Fatalf("expected OverflowTotal >= 2, got %d", metrics.OverflowTotal)
	}
}

func TestBusShutdownWithOverflow(t *testing.T) {
	bus := eventbus.New()

	// Subscribe to a critical topic (overflow strategy) and a normal topic.
	sub1 := bus.Subscribe(eventbus.TopicConversationPrompt)
	sub2 := bus.Subscribe(eventbus.TopicSessionsOutput)

	// Publish a few events.
	ctx := context.Background()
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
	})
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsOutput,
		Source: eventbus.SourceSessionManager,
	})

	// Shutdown should not deadlock or panic, and should clean up drain goroutines.
	done := make(chan struct{})
	go func() {
		bus.Shutdown()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown() deadlocked with overflow subscriptions")
	}

	// Drain any buffered events, then verify channels are closed.
	for range sub1.C() {
	}
	for range sub2.C() {
	}
}

func TestBusWithTopicPolicy(t *testing.T) {
	// Override a normally drop-oldest topic to use drop-newest.
	bus := eventbus.New(
		eventbus.WithTopicBuffer(eventbus.TopicSessionsOutput, 1),
		eventbus.WithTopicPolicy(eventbus.TopicSessionsOutput, eventbus.DeliveryPolicy{
			Strategy: eventbus.StrategyDropNewest,
			Priority: eventbus.PriorityLow,
		}),
	)
	sub := bus.Subscribe(eventbus.TopicSessionsOutput, eventbus.WithSubscriptionBuffer(1))
	defer sub.Close()

	ctx := context.Background()

	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsOutput,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionOutputEvent{Sequence: 1},
	})
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsOutput,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionOutputEvent{Sequence: 2},
	})

	select {
	case env := <-sub.C():
		msg := env.Payload.(eventbus.SessionOutputEvent)
		if msg.Sequence != 1 {
			t.Fatalf("expected first event to survive with drop-newest, got sequence %d", msg.Sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}
