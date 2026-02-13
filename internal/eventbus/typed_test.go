package eventbus

import (
	"context"
	"testing"
	"time"
)

func TestTypedSubscribeDeliverPayload(t *testing.T) {
	bus := New()
	defer bus.Shutdown()

	sub := Subscribe[SpeechVADEvent](bus, TopicSpeechVADDetected)
	defer sub.Close()

	want := SpeechVADEvent{
		SessionID:  "s1",
		StreamID:   "str1",
		Active:     true,
		Confidence: 0.9,
	}
	bus.Publish(context.Background(), Envelope{
		Topic:   TopicSpeechVADDetected,
		Payload: want,
	})

	select {
	case got := <-sub.C():
		if got.Payload.SessionID != want.SessionID || got.Payload.StreamID != want.StreamID ||
			got.Payload.Active != want.Active || got.Payload.Confidence != want.Confidence {
			t.Fatalf("payload mismatch: got %+v, want %+v", got.Payload, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for typed event")
	}
}

func TestTypedSubscribePreservesMetadata(t *testing.T) {
	bus := New()
	defer bus.Shutdown()

	sub := Subscribe[SpeechVADEvent](bus, TopicSpeechVADDetected)
	defer sub.Close()

	ts := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	bus.Publish(context.Background(), Envelope{
		Topic:         TopicSpeechVADDetected,
		Timestamp:     ts,
		Source:        SourceSpeechVAD,
		CorrelationID: "corr-123",
		Payload:       SpeechVADEvent{SessionID: "s1", Active: true},
	})

	select {
	case got := <-sub.C():
		if got.Timestamp != ts {
			t.Errorf("Timestamp: got %v, want %v", got.Timestamp, ts)
		}
		if got.Source != SourceSpeechVAD {
			t.Errorf("Source: got %v, want %v", got.Source, SourceSpeechVAD)
		}
		if got.CorrelationID != "corr-123" {
			t.Errorf("CorrelationID: got %q, want %q", got.CorrelationID, "corr-123")
		}
		if got.Topic != TopicSpeechVADDetected {
			t.Errorf("Topic: got %v, want %v", got.Topic, TopicSpeechVADDetected)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for typed event")
	}
}

func TestTypedSubscribeSkipsMismatchedPayloads(t *testing.T) {
	bus := New()
	defer bus.Shutdown()

	sub := Subscribe[SpeechVADEvent](bus, TopicSpeechVADDetected)
	defer sub.Close()

	// Publish a mismatched payload type on the same topic.
	bus.Publish(context.Background(), Envelope{
		Topic:   TopicSpeechVADDetected,
		Payload: "not a SpeechVADEvent",
	})

	// Then publish a correct one.
	bus.Publish(context.Background(), Envelope{
		Topic:   TopicSpeechVADDetected,
		Payload: SpeechVADEvent{SessionID: "s1", Active: true},
	})

	select {
	case got := <-sub.C():
		if got.Payload.SessionID != "s1" || !got.Payload.Active {
			t.Fatalf("expected the correct event, got %+v", got.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out — mismatched payload may have blocked delivery")
	}
}

func TestTypedSubscribeClose(t *testing.T) {
	bus := New()
	defer bus.Shutdown()

	sub := Subscribe[SpeechVADEvent](bus, TopicSpeechVADDetected)
	sub.Close()

	// Channel should be closed after Close returns.
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("expected channel to be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel close")
	}
}

func TestTypedSubscribeCloseWhileBridgeBlocked(t *testing.T) {
	bus := New()
	defer bus.Shutdown()

	sub := Subscribe[SpeechVADEvent](bus, TopicSpeechVADDetected)

	// Publish an event but do NOT read from sub.C().
	// The bridge goroutine will be blocked trying to send on the unbuffered channel.
	bus.Publish(context.Background(), Envelope{
		Topic:   TopicSpeechVADDetected,
		Payload: SpeechVADEvent{SessionID: "s1", Active: true},
	})

	// Give the bridge goroutine time to pick up the event and block on send.
	time.Sleep(50 * time.Millisecond)

	// Close must not deadlock — the quit channel should unblock the bridge.
	done := make(chan struct{})
	go func() {
		sub.Close()
		close(done)
	}()

	select {
	case <-done:
		// Close returned without deadlock.
	case <-time.After(2 * time.Second):
		t.Fatal("Close() deadlocked — bridge goroutine stuck on send")
	}
}

func TestSubscribeNilBusReturnsClosedChannel(t *testing.T) {
	sub := Subscribe[SpeechVADEvent](nil, TopicSpeechVADDetected)
	// Channel should already be closed.
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("expected channel to be closed for nil bus")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for closed channel on nil bus")
	}
	// Close should be a no-op and not panic.
	sub.Close()
}

func TestTypedSubscribeBusShutdown(t *testing.T) {
	bus := New()

	sub := Subscribe[SpeechVADEvent](bus, TopicSpeechVADDetected)

	bus.Shutdown()

	// Bridge goroutine should exit and close the typed channel.
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("expected channel to be closed after bus shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel close after shutdown")
	}

	// Wait for bridge done to ensure no goroutine leak.
	select {
	case <-sub.done:
	case <-time.After(time.Second):
		t.Fatal("bridge goroutine did not exit after bus shutdown")
	}
}
