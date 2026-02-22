package eventbus

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestConsumeForwardsPayload(t *testing.T) {
	bus := New()
	defer bus.Shutdown()

	sub := Subscribe[SpeechVADEvent](bus, TopicSpeechVADDetected)
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	received := make(chan SpeechVADEvent, 1)

	wg.Add(1)
	go Consume(ctx, sub, &wg, func(evt SpeechVADEvent) {
		received <- evt
	})

	bus.publish(context.Background(), Envelope{
		Topic:   TopicSpeechVADDetected,
		Payload: SpeechVADEvent{SessionID: "s1", StreamID: "st1", Active: true},
	})

	select {
	case got := <-received:
		if got.SessionID != "s1" || got.StreamID != "st1" || !got.Active {
			t.Fatalf("unexpected payload: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for consume payload")
	}

	cancel()
	waitGroupWithTimeout(t, &wg)
}

func TestConsumeEnvelopeForwardsEnvelope(t *testing.T) {
	bus := New()
	defer bus.Shutdown()

	sub := Subscribe[SpeechVADEvent](bus, TopicSpeechVADDetected)
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ts := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	received := make(chan TypedEnvelope[SpeechVADEvent], 1)

	wg.Add(1)
	go ConsumeEnvelope(ctx, sub, &wg, func(env TypedEnvelope[SpeechVADEvent]) {
		received <- env
	})

	bus.publish(context.Background(), Envelope{
		Topic:     TopicSpeechVADDetected,
		Timestamp: ts,
		Source:    SourceSpeechVAD,
		Payload:   SpeechVADEvent{SessionID: "s1", Active: true},
	})

	select {
	case got := <-received:
		if got.Timestamp != ts {
			t.Fatalf("unexpected timestamp: got %v want %v", got.Timestamp, ts)
		}
		if got.Source != SourceSpeechVAD {
			t.Fatalf("unexpected source: got %v want %v", got.Source, SourceSpeechVAD)
		}
		if got.Payload.SessionID != "s1" {
			t.Fatalf("unexpected payload: %+v", got.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for consume envelope")
	}

	cancel()
	waitGroupWithTimeout(t, &wg)
}

func TestConsumeReturnsWhenSubscriptionClosed(t *testing.T) {
	bus := New()
	defer bus.Shutdown()

	sub := Subscribe[SpeechVADEvent](bus, TopicSpeechVADDetected)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go Consume(ctx, sub, &wg, func(SpeechVADEvent) {})

	sub.Close()
	waitGroupWithTimeout(t, &wg)
}

func TestConsumeWithNilSubscription(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go Consume(ctx, nil, &wg, func(SpeechVADEvent) {})

	waitGroupWithTimeout(t, &wg)
}

func waitGroupWithTimeout(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitgroup timed out")
	}
}
