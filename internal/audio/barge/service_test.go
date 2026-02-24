package barge

import (
	"context"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/voice/slots"
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

	bargeSub := eventbus.SubscribeTo(bus, eventbus.Speech.BargeIn)
	defer bargeSub.Close()

	sessionID := "sess-1"
	ttsStream := slots.TTS
	vadStream := "mic"

	eventbus.Publish(context.Background(), bus, eventbus.Audio.EgressPlayback, "", eventbus.AudioEgressPlaybackEvent{
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
	})
	time.Sleep(10 * time.Millisecond)

	eventbus.Publish(context.Background(), bus, eventbus.Speech.VADDetected, "", eventbus.SpeechVADEvent{
		SessionID:  sessionID,
		StreamID:   vadStream,
		Active:     true,
		Confidence: 0.8,
		Timestamp:  time.Now().UTC(),
		Metadata: map[string]string{
			"vad": "mock",
		},
	})

	select {
	case env := <-bargeSub.C():
		if env.Source != eventbus.SourceSpeechBarge {
			t.Fatalf("unexpected source %s", env.Source)
		}
		event := env.Payload
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

	bargeSub := eventbus.SubscribeTo(bus, eventbus.Speech.BargeIn)
	defer bargeSub.Close()

	eventbus.Publish(context.Background(), bus, eventbus.Audio.EgressPlayback, "", eventbus.AudioEgressPlaybackEvent{
		SessionID: "sess-dup",
		StreamID:  slots.TTS,
		Sequence:  1,
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
		Data:  []byte{0, 1},
		Final: false,
	})
	time.Sleep(10 * time.Millisecond)

	event := eventbus.SpeechVADEvent{
		SessionID:  "sess-dup",
		StreamID:   "mic",
		Active:     true,
		Confidence: 0.9,
		Timestamp:  time.Now().UTC(),
	}

	eventbus.Publish(context.Background(), bus, eventbus.Speech.VADDetected, "", event)

	select {
	case env := <-bargeSub.C():
		evt := env.Payload
		if evt.StreamID != slots.TTS {
			t.Fatalf("expected barge for tts stream, got %s", evt.StreamID)
		}
	case <-time.After(time.Second):
		t.Fatal("first barge event missing")
	}

	// Second event inside cooldown window should be ignored.
	eventbus.Publish(context.Background(), bus, eventbus.Speech.VADDetected, "", event)

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

	bargeSub := eventbus.SubscribeTo(bus, eventbus.Speech.BargeIn)
	defer bargeSub.Close()

	sessionID := "sess-client"
	ttsStream := slots.TTS
	eventbus.Publish(context.Background(), bus, eventbus.Audio.EgressPlayback, "", eventbus.AudioEgressPlaybackEvent{
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
	})

	meta := map[string]string{"device": "mobile", "note": "urgent"}
	eventbus.Publish(context.Background(), bus, eventbus.Audio.Interrupt, "", eventbus.AudioInterruptEvent{
		SessionID: sessionID,
		StreamID:  ttsStream,
		Reason:    "manual",
		Timestamp: time.Now().UTC(),
		Metadata:  meta,
	})

	select {
	case env := <-bargeSub.C():
		if env.Source != eventbus.SourceSpeechBarge {
			t.Fatalf("unexpected source %s", env.Source)
		}
		event := env.Payload
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

	bargeSub := eventbus.SubscribeTo(bus, eventbus.Speech.BargeIn)
	defer bargeSub.Close()

	now := time.Now().UTC()

	eventbus.Publish(context.Background(), bus, eventbus.Audio.EgressPlayback, "", eventbus.AudioEgressPlaybackEvent{
		SessionID: "sess-quiet",
		StreamID:  slots.TTS,
		Sequence:  1,
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
		Data:  []byte{1, 2},
		Final: false,
	})
	time.Sleep(10 * time.Millisecond)

	firstVAD := eventbus.SpeechVADEvent{
		SessionID:  "sess-quiet",
		StreamID:   "mic",
		Active:     true,
		Confidence: 0.9,
		Timestamp:  now.Add(20 * time.Millisecond),
	}
	eventbus.Publish(context.Background(), bus, eventbus.Speech.VADDetected, "", firstVAD)

	select {
	case env := <-bargeSub.C():
		event := env.Payload
		if event.SessionID != "sess-quiet" || event.StreamID != slots.TTS {
			t.Fatalf("unexpected event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("expected barge event while playback active")
	}

	eventbus.PublishWithOpts(context.Background(), bus, eventbus.Audio.EgressPlayback, "", eventbus.AudioEgressPlaybackEvent{
		SessionID: "sess-quiet",
		StreamID:  slots.TTS,
		Sequence:  2,
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
		Data:  []byte{0, 0},
		Final: true,
	}, eventbus.WithTimestamp(now.Add(40*time.Millisecond)))
	time.Sleep(10 * time.Millisecond)

	secondVAD := eventbus.SpeechVADEvent{
		SessionID:  "sess-quiet",
		StreamID:   "mic",
		Active:     true,
		Confidence: 0.9,
		Timestamp:  now.Add(60 * time.Millisecond),
	}
	eventbus.Publish(context.Background(), bus, eventbus.Speech.VADDetected, "", secondVAD)

	select {
	case env := <-bargeSub.C():
		t.Fatalf("unexpected barge event during quiet period: %+v", env.Payload)
	case <-time.After(150 * time.Millisecond):
	}
}

