package egress

import (
	"context"
	"path/filepath"
	"sync"
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

func TestServiceInterruptStopsPlayback(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	synth := newInterruptSynth()
	svc := New(bus,
		WithFactory(FactoryFunc(func(context.Context, SessionParams) (Synthesizer, error) {
			return synth, nil
		})),
		WithStreamID("tts.primary"),
	)
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	t.Cleanup(func() {
		svc.Shutdown(context.Background())
	})

	playbackSub := bus.Subscribe(eventbus.TopicAudioEgressPlayback)
	defer playbackSub.Close()

	sessionID := "sess-interrupt"
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicConversationSpeak,
		Payload: eventbus.ConversationSpeakEvent{
			SessionID: sessionID,
			Text:      "interrupted message",
		},
	})

	first := receivePlayback(t, playbackSub)
	if first.Final {
		t.Fatalf("expected non-final chunk before interrupt")
	}
	if first.Metadata["barge_in"] == "true" {
		t.Fatalf("unexpected barge metadata on initial chunk: %+v", first.Metadata)
	}

	svc.Interrupt(sessionID, "", "test_barge", map[string]string{"origin": "test"})

	final := receivePlayback(t, playbackSub)
	if !final.Final {
		t.Fatalf("expected final chunk after interrupt")
	}
	if final.Metadata["barge_in"] != "true" {
		t.Fatalf("expected barge_in metadata on final chunk: %+v", final.Metadata)
	}
	if final.Metadata["barge_in_reason"] != "test_barge" {
		t.Fatalf("unexpected barge_in_reason: %s", final.Metadata["barge_in_reason"])
	}
	if final.Metadata["origin"] != "test" {
		t.Fatalf("expected interrupt metadata to propagate, got %+v", final.Metadata)
	}

	synth.waitForClose(t)

	select {
	case env := <-playbackSub.C():
		t.Fatalf("unexpected extra playback event after interrupt: %+v", env.Payload)
	case <-time.After(100 * time.Millisecond):
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

type interruptSynth struct {
	closeOnce sync.Once
	closeCh   chan struct{}
}

func newInterruptSynth() *interruptSynth {
	return &interruptSynth{
		closeCh: make(chan struct{}),
	}
}

func (s *interruptSynth) Speak(ctx context.Context, req SpeakRequest) ([]SynthesisChunk, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Millisecond):
	}
	return []SynthesisChunk{{
		Data:     []byte{0, 1},
		Duration: 10 * time.Millisecond,
		Final:    false,
		Metadata: map[string]string{"phase": "speak"},
	}}, nil
}

func (s *interruptSynth) Close(context.Context) ([]SynthesisChunk, error) {
	s.closeOnce.Do(func() {
		close(s.closeCh)
	})
	return []SynthesisChunk{{
		Duration: 0,
		Final:    true,
		Metadata: map[string]string{"phase": "close"},
	}}, nil
}

func (s *interruptSynth) waitForClose(t *testing.T) {
	t.Helper()
	select {
	case <-s.closeCh:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for synthesizer close")
	}
}
