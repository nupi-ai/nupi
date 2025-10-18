package vad

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"testing"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/modules"
)

func TestVADServiceEmitsDetections(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, err := configstore.Open(configstore.Options{DBPath: filepath.Join(t.TempDir(), "config.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	if err := modules.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(modules.SlotVAD), modules.MockVADAdapterID, map[string]any{
		"threshold":  0.01,
		"min_frames": 1,
	}); err != nil {
		t.Fatalf("activate vad adapter: %v", err)
	}

	svc := New(bus, WithFactory(NewModuleFactory(store)), WithRetryDelays(10*time.Millisecond, 50*time.Millisecond))
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer svc.Shutdown(context.Background())

	vadSub := bus.Subscribe(eventbus.TopicSpeechVADDetected)
	defer vadSub.Close()

	segment := eventbus.AudioIngressSegmentEvent{
		SessionID: "sess-vad",
		StreamID:  "mic",
		Sequence:  1,
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
		Data:      loudPCM(),
		Duration:  20 * time.Millisecond,
		First:     true,
		Last:      false,
		StartedAt: time.Now().UTC(),
		EndedAt:   time.Now().UTC().Add(20 * time.Millisecond),
	}

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicAudioIngressSegment,
		Payload: segment,
	})

	event := receiveVADEvent(t, vadSub)
	if event.SessionID != "sess-vad" {
		t.Fatalf("unexpected session id: %s", event.SessionID)
	}
	if !event.Active {
		t.Fatalf("expected active detection")
	}
}

func TestVADServiceBuffersUntilAdapterAvailable(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, err := configstore.Open(configstore.Options{DBPath: filepath.Join(t.TempDir(), "config.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	if err := modules.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure adapters: %v", err)
	}

	svc := New(bus, WithFactory(NewModuleFactory(store)), WithRetryDelays(10*time.Millisecond, 50*time.Millisecond))
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer svc.Shutdown(context.Background())

	vadSub := bus.Subscribe(eventbus.TopicSpeechVADDetected)
	defer vadSub.Close()

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicAudioIngressSegment,
		Payload: eventbus.AudioIngressSegmentEvent{
			SessionID: "sess-wait",
			StreamID:  "mic",
			Sequence:  1,
			Format: eventbus.AudioFormat{
				Encoding:   eventbus.AudioEncodingPCM16,
				SampleRate: 16000,
				Channels:   1,
				BitDepth:   16,
			},
			Data:      loudPCM(),
			Duration:  20 * time.Millisecond,
			First:     true,
			Last:      false,
			StartedAt: time.Now().UTC(),
			EndedAt:   time.Now().UTC().Add(20 * time.Millisecond),
		},
	})

	select {
	case <-vadSub.C():
		t.Fatalf("unexpected detection before adapter active")
	case <-time.After(30 * time.Millisecond):
	}

	if err := store.SetActiveAdapter(ctx, string(modules.SlotVAD), modules.MockVADAdapterID, map[string]any{
		"threshold":  0.01,
		"min_frames": 1,
	}); err != nil {
		t.Fatalf("activate adapter: %v", err)
	}

	event := receiveVADEvent(t, vadSub)
	if event.SessionID != "sess-wait" {
		t.Fatalf("unexpected session id: %s", event.SessionID)
	}
}

func loudPCM() []byte {
	data := make([]byte, 640)
	for i := 0; i < len(data); i += 2 {
		binary.LittleEndian.PutUint16(data[i:], uint16(25000))
	}
	return data
}

func receiveVADEvent(t *testing.T, sub *eventbus.Subscription) eventbus.SpeechVADEvent {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case env := <-sub.C():
			event, ok := env.Payload.(eventbus.SpeechVADEvent)
			if !ok {
				continue
			}
			return event
		case <-timer.C:
			t.Fatalf("timeout waiting for VAD event")
		}
	}
}
