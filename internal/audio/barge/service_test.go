package barge

import (
	"context"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

func TestCoordinatorPublishesBargeInOnVAD(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus, WithConfidenceThreshold(0.1), WithCooldown(100*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	defer svc.Shutdown(context.Background())

	bargeSub := bus.Subscribe(eventbus.TopicSpeechBargeIn)
	defer bargeSub.Close()

	sessionID := "sess-1"
	ttsStream := "tts.primary"
	vadStream := "mic"

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicAudioEgressPlayback,
		Payload: eventbus.AudioEgressPlaybackEvent{
			SessionID: sessionID,
			StreamID:  ttsStream,
			Sequence:  1,
			Format: eventbus.AudioFormat{
				Encoding:   eventbus.AudioEncodingPCM16,
				SampleRate: 16000,
				Channels:   1,
				BitDepth:   16,
			},
			Data:  []byte{0, 1},
			Final: false,
		},
	})
	time.Sleep(10 * time.Millisecond)

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicSpeechVADDetected,
		Payload: eventbus.SpeechVADEvent{
			SessionID:  sessionID,
			StreamID:   vadStream,
			Active:     true,
			Confidence: 0.8,
			Timestamp:  time.Now().UTC(),
			Metadata: map[string]string{
				"vad": "mock",
			},
		},
	})

	select {
	case env := <-bargeSub.C():
		if env.Source != eventbus.SourceSpeechBarge {
			t.Fatalf("unexpected source %s", env.Source)
		}
		event, ok := env.Payload.(eventbus.SpeechBargeInEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", env.Payload)
		}
		if event.SessionID != sessionID || event.StreamID != ttsStream {
			t.Fatalf("unexpected event payload: %+v", event)
		}
		if event.Reason != "vad_detected" {
			t.Fatalf("unexpected reason %q", event.Reason)
		}
		if event.Metadata["trigger"] != "vad_detected" {
			t.Fatalf("missing trigger metadata: %+v", event.Metadata)
		}
		if event.Metadata["vad_stream_id"] != vadStream {
			t.Fatalf("missing vad stream metadata: %+v", event.Metadata)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for barge-in event")
	}
}

func TestCoordinatorCooldownPreventsRapidDuplicates(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus, WithConfidenceThreshold(0.1), WithCooldown(500*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	defer svc.Shutdown(context.Background())

	bargeSub := bus.Subscribe(eventbus.TopicSpeechBargeIn)
	defer bargeSub.Close()

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicAudioEgressPlayback,
		Payload: eventbus.AudioEgressPlaybackEvent{
			SessionID: "sess-dup",
			StreamID:  "tts.primary",
			Sequence:  1,
			Format: eventbus.AudioFormat{
				Encoding:   eventbus.AudioEncodingPCM16,
				SampleRate: 16000,
				Channels:   1,
				BitDepth:   16,
			},
			Data:  []byte{0, 1},
			Final: false,
		},
	})
	time.Sleep(10 * time.Millisecond)

	event := eventbus.SpeechVADEvent{
		SessionID:  "sess-dup",
		StreamID:   "mic",
		Active:     true,
		Confidence: 0.9,
		Timestamp:  time.Now().UTC(),
	}

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicSpeechVADDetected,
		Payload: event,
	})

	select {
	case env := <-bargeSub.C():
		evt, ok := env.Payload.(eventbus.SpeechBargeInEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", env.Payload)
		}
		if evt.StreamID != "tts.primary" {
			t.Fatalf("expected barge for tts stream, got %s", evt.StreamID)
		}
	case <-time.After(time.Second):
		t.Fatal("first barge event missing")
	}

	// Second event inside cooldown window should be ignored.
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicSpeechVADDetected,
		Payload: event,
	})

	select {
	case env := <-bargeSub.C():
		t.Fatalf("unexpected duplicate barge event: %+v", env.Payload)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestCoordinatorPublishesBargeInOnClientInterrupt(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus, WithCooldown(0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	defer svc.Shutdown(context.Background())

	bargeSub := bus.Subscribe(eventbus.TopicSpeechBargeIn)
	defer bargeSub.Close()

	sessionID := "sess-client"
	ttsStream := "tts.primary"
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicAudioEgressPlayback,
		Payload: eventbus.AudioEgressPlaybackEvent{
			SessionID: sessionID,
			StreamID:  ttsStream,
			Sequence:  1,
			Format: eventbus.AudioFormat{
				Encoding:   eventbus.AudioEncodingPCM16,
				SampleRate: 16000,
				Channels:   1,
				BitDepth:   16,
			},
			Data:  []byte{0, 1},
			Final: false,
		},
	})

	meta := map[string]string{"device": "mobile", "note": "urgent"}
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicAudioInterrupt,
		Payload: eventbus.AudioInterruptEvent{
			SessionID: sessionID,
			StreamID:  ttsStream,
			Reason:    "manual",
			Timestamp: time.Now().UTC(),
			Metadata:  meta,
		},
	})

	select {
	case env := <-bargeSub.C():
		if env.Source != eventbus.SourceSpeechBarge {
			t.Fatalf("unexpected source %s", env.Source)
		}
		event, ok := env.Payload.(eventbus.SpeechBargeInEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", env.Payload)
		}
		if event.SessionID != sessionID || event.StreamID != ttsStream {
			t.Fatalf("unexpected event payload: %+v", event)
		}
		if event.Reason != "manual" {
			t.Fatalf("unexpected reason %q", event.Reason)
		}
		if event.Metadata["trigger"] != "manual" {
			t.Fatalf("expected metadata trigger to mirror reason, got %+v", event.Metadata)
		}
		if event.Metadata["confidence"] != "1" {
			t.Fatalf("unexpected confidence metadata: %v", event.Metadata["confidence"])
		}
		if event.Metadata["device"] != "mobile" {
			t.Fatalf("metadata not propagated: %+v", event.Metadata)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for barge-in event from client interrupt")
	}
}

func TestCoordinatorQuietPeriodBlocksVAD(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus, WithCooldown(0), WithQuietPeriod(250*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	defer svc.Shutdown(context.Background())

	bargeSub := bus.Subscribe(eventbus.TopicSpeechBargeIn)
	defer bargeSub.Close()

	now := time.Now().UTC()

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicAudioEgressPlayback,
		Payload: eventbus.AudioEgressPlaybackEvent{
			SessionID: "sess-quiet",
			StreamID:  "tts.primary",
			Sequence:  1,
			Format: eventbus.AudioFormat{
				Encoding:   eventbus.AudioEncodingPCM16,
				SampleRate: 16000,
				Channels:   1,
				BitDepth:   16,
			},
			Data:  []byte{1, 2},
			Final: false,
		},
	})
	time.Sleep(10 * time.Millisecond)

	firstVAD := eventbus.SpeechVADEvent{
		SessionID:  "sess-quiet",
		StreamID:   "mic",
		Active:     true,
		Confidence: 0.9,
		Timestamp:  now.Add(20 * time.Millisecond),
	}
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicSpeechVADDetected,
		Payload: firstVAD,
	})

	select {
	case env := <-bargeSub.C():
		event, ok := env.Payload.(eventbus.SpeechBargeInEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", env.Payload)
		}
		if event.SessionID != "sess-quiet" || event.StreamID != "tts.primary" {
			t.Fatalf("unexpected event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("expected barge event while playback active")
	}

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicAudioEgressPlayback,
		Payload: eventbus.AudioEgressPlaybackEvent{
			SessionID: "sess-quiet",
			StreamID:  "tts.primary",
			Sequence:  2,
			Format: eventbus.AudioFormat{
				Encoding:   eventbus.AudioEncodingPCM16,
				SampleRate: 16000,
				Channels:   1,
				BitDepth:   16,
			},
			Data:  []byte{0, 0},
			Final: true,
		},
		Timestamp: now.Add(40 * time.Millisecond),
	})
	time.Sleep(10 * time.Millisecond)

	secondVAD := eventbus.SpeechVADEvent{
		SessionID:  "sess-quiet",
		StreamID:   "mic",
		Active:     true,
		Confidence: 0.9,
		Timestamp:  now.Add(60 * time.Millisecond),
	}
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicSpeechVADDetected,
		Payload: secondVAD,
	})

	select {
	case env := <-bargeSub.C():
		t.Fatalf("unexpected barge event during quiet period: %+v", env.Payload)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestServiceMetricsBargeInTotal(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus)

	if metrics := svc.Metrics(); metrics.BargeInTotal != 0 {
		t.Fatalf("expected initial BargeInTotal = 0, got %d", metrics.BargeInTotal)
	}

	svc.publishBargeIn("metrics-session", "tts.primary", time.Now().UTC(), "test", 0.5, nil)
	svc.publishBargeIn("metrics-session", "tts.primary", time.Now().UTC(), "test", 0.5, nil)

	if metrics := svc.Metrics(); metrics.BargeInTotal != 2 {
		t.Fatalf("expected BargeInTotal = 2, got %d", metrics.BargeInTotal)
	}
}
