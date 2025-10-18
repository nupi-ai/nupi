package egress

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/modules"
)

func TestServicePublishesPlayback(t *testing.T) {
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
	if err := store.SetActiveAdapter(ctx, string(modules.SlotTTS), modules.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	svc := New(bus,
		WithFactory(NewModuleFactory(store)),
		WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	t.Cleanup(func() {
		svc.Shutdown(context.Background())
	})

	playbackSub := bus.Subscribe(eventbus.TopicAudioEgressPlayback)
	defer playbackSub.Close()

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicConversationSpeak,
		Payload: eventbus.ConversationSpeakEvent{
			SessionID: "sess-tts",
			PromptID:  "prompt-1",
			Text:      "Hello audio",
		},
	})

	evt := receivePlayback(t, playbackSub)
	if evt.SessionID != "sess-tts" {
		t.Fatalf("unexpected session id: %s", evt.SessionID)
	}
	if len(evt.Data) == 0 {
		t.Fatalf("expected audio data")
	}
	if !evt.Final {
		t.Fatalf("expected final flag")
	}
}

func TestServiceBuffersUntilAdapterAvailable(t *testing.T) {
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

	svc := New(bus,
		WithFactory(NewModuleFactory(store)),
		WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	t.Cleanup(func() {
		svc.Shutdown(context.Background())
	})

	playbackSub := bus.Subscribe(eventbus.TopicAudioEgressPlayback)
	defer playbackSub.Close()

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicConversationSpeak,
		Payload: eventbus.ConversationSpeakEvent{
			SessionID: "sess-buffered",
			Text:      "buffered",
		},
	})

	select {
	case <-playbackSub.C():
		t.Fatalf("unexpected playback before adapter active")
	case <-time.After(30 * time.Millisecond):
	}

	if err := store.SetActiveAdapter(ctx, string(modules.SlotTTS), modules.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("activate adapter: %v", err)
	}

	evt := receivePlayback(t, playbackSub)
	if evt.SessionID != "sess-buffered" {
		t.Fatalf("unexpected session id: %s", evt.SessionID)
	}
}

func receivePlayback(t *testing.T, sub *eventbus.Subscription) eventbus.AudioEgressPlaybackEvent {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	for {
		select {
		case env := <-sub.C():
			payload, ok := env.Payload.(eventbus.AudioEgressPlaybackEvent)
			if !ok {
				continue
			}
			return payload
		case <-timer.C:
			t.Fatalf("timeout waiting for playback event")
		}
	}
}
