package egress

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/audio/streammanager"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
	"github.com/nupi-ai/nupi/internal/voice/slots"
)

func TestServicePublishesPlayback(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, err := configstore.Open(configstore.Options{DBPath: filepath.Join(t.TempDir(), "config.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	svc := New(bus,
		WithFactory(NewAdapterFactory(store, nil)),
		WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	t.Cleanup(func() {
		svc.Shutdown(context.Background())
	})

	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback)
	defer playbackSub.Close()

	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Speak, "", eventbus.ConversationSpeakEvent{
		SessionID: "sess-tts",
		PromptID:  "prompt-1",
		Text:      "Hello audio",
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

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure adapters: %v", err)
	}

	svc := New(bus,
		WithFactory(NewAdapterFactory(store, nil)),
		WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	t.Cleanup(func() {
		svc.Shutdown(context.Background())
	})

	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback)
	defer playbackSub.Close()

	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Speak, "", eventbus.ConversationSpeakEvent{
		SessionID: "sess-buffered",
		Text:      "buffered",
	})

	select {
	case <-playbackSub.C():
		t.Fatalf("unexpected playback before adapter active")
	case <-time.After(30 * time.Millisecond):
	}

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, nil); err != nil {
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
		WithStreamID(slots.TTS),
	)
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	t.Cleanup(func() {
		svc.Shutdown(context.Background())
	})

	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback)
	defer playbackSub.Close()

	sessionID := "sess-interrupt"
	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Speak, "", eventbus.ConversationSpeakEvent{
		SessionID: sessionID,
		Text:      "interrupted message",
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

func receivePlayback(t *testing.T, sub *eventbus.TypedSubscription[eventbus.AudioEgressPlaybackEvent]) eventbus.AudioEgressPlaybackEvent {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	select {
	case env := <-sub.C():
		return env.Payload
	case <-timer.C:
		t.Fatalf("timeout waiting for playback event")
	}
	return eventbus.AudioEgressPlaybackEvent{}
}

func TestServiceRebuffersOnSpeakUnavailable(t *testing.T) {
	bus := eventbus.New()

	var (
		mu       sync.Mutex
		attempts int
	)

	factory := FactoryFunc(func(ctx context.Context, params SessionParams) (Synthesizer, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 1 {
			return &unavailableSynth{}, nil
		}
		return &noopSynth{}, nil
	})

	svc := New(
		bus,
		WithFactory(factory),
		WithRetryDelays(5*time.Millisecond, 20*time.Millisecond),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer svc.Shutdown(context.Background())

	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback)
	defer playbackSub.Close()

	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Speak, "", eventbus.ConversationSpeakEvent{
		SessionID: "sess-rebuffer",
		Text:      "hello rebuffered",
	})

	evt := receivePlayback(t, playbackSub)
	if evt.SessionID != "sess-rebuffer" {
		t.Fatalf("unexpected session id: %s", evt.SessionID)
	}

	mu.Lock()
	a := attempts
	mu.Unlock()
	if a < 2 {
		t.Fatalf("expected at least 2 factory attempts, got %d", a)
	}
}

func TestServiceRetryDropsPendingOnPermanentError(t *testing.T) {
	bus := eventbus.New()

	var (
		mu       sync.Mutex
		attempts int
	)

	permanentErr := errors.New("duplicate runtime entries")
	factory := FactoryFunc(func(ctx context.Context, params SessionParams) (Synthesizer, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 1 {
			return nil, ErrAdapterUnavailable
		}
		return nil, permanentErr
	})

	svc := New(
		bus,
		WithFactory(factory),
		WithRetryDelays(5*time.Millisecond, 20*time.Millisecond),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer svc.Shutdown(context.Background())

	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Speak, "", eventbus.ConversationSpeakEvent{
		SessionID: "sess-perm",
		Text:      "permanent fail",
	})

	// Poll until the retry fires and hits the permanent error.
	deadline := time.After(time.Second)
	for {
		pending := svc.manager.PendingCount()
		if pending == 0 {
			mu.Lock()
			a := attempts
			mu.Unlock()
			if a >= 2 {
				break
			}
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for pending queue to be dropped on permanent error")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

type unavailableSynth struct{}

func (u *unavailableSynth) Speak(context.Context, SpeakRequest) ([]SynthesisChunk, error) {
	return nil, fmt.Errorf("tts: open synthesis stream: %w: %w", errors.New("connection refused"), ErrAdapterUnavailable)
}

func (u *unavailableSynth) Close(context.Context) ([]SynthesisChunk, error) {
	return nil, nil
}

type noopSynth struct{}

func (n *noopSynth) Speak(context.Context, SpeakRequest) ([]SynthesisChunk, error) {
	return []SynthesisChunk{{Final: true}}, nil
}

func (n *noopSynth) Close(context.Context) ([]SynthesisChunk, error) {
	return nil, nil
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

func TestEnqueueRejectsAfterRebufferStarted(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus, WithFactory(FactoryFunc(func(context.Context, SessionParams) (Synthesizer, error) {
		return &noopSynth{}, nil
	})))
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start service: %v", err)
	}

	params := SessionParams{
		SessionID: "sess-rej",
		StreamID:  "tts",
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
	}

	key := streammanager.StreamKey(params.SessionID, params.StreamID)
	h, err := svc.manager.CreateStream(key, params)
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}
	st := h.(*stream)
	t.Cleanup(func() {
		svc.Shutdown(context.Background())
		st.Wait(context.Background())
	})

	// Simulate rebuffer: set stopped = true under lock.
	st.mu.Lock()
	st.stopped = true
	st.mu.Unlock()

	err = st.Enqueue(speakRequest{
		SessionID: "sess-rej",
		StreamID:  "tts",
		Text:      "should be rejected",
	})
	if !errors.Is(err, errStreamRebuffering) {
		t.Fatalf("expected errStreamRebuffering, got %v", err)
	}
}

func TestInterruptBeforeStartDoesNotPanic(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus, WithFactory(FactoryFunc(func(context.Context, SessionParams) (Synthesizer, error) {
		return &noopSynth{}, nil
	})))
	// Do NOT call Start() — manager is nil.
	// Interrupt should be a safe no-op.
	svc.Interrupt("sess-nostart", "", "test", nil)
}

func TestLifecycleStopRemovesStream(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus, WithFactory(FactoryFunc(func(context.Context, SessionParams) (Synthesizer, error) {
		return &noopSynth{}, nil
	})))
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start service: %v", err)
	}
	t.Cleanup(func() { svc.Shutdown(context.Background()) })

	sessionID := "sess-lifecycle"
	key := streammanager.StreamKey(sessionID, svc.DefaultStreamID())

	// Create a stream.
	h, err := svc.manager.CreateStream(key, SessionParams{
		SessionID: sessionID,
		StreamID:  svc.DefaultStreamID(),
		Format:    svc.PlaybackFormat(),
	})
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}
	if h == nil {
		t.Fatal("expected non-nil handle")
	}
	if _, ok := svc.manager.Stream(key); !ok {
		t.Fatal("expected stream to exist after create")
	}

	// Publish lifecycle stopped event.
	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Lifecycle, "", eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateStopped,
	})

	// Wait for the lifecycle handler to process.
	deadline := time.After(time.Second)
	for {
		if _, ok := svc.manager.Stream(key); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for lifecycle handler to remove stream")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestHandleSpeakRequestBuffersOnRebuffering(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus, WithFactory(FactoryFunc(func(context.Context, SessionParams) (Synthesizer, error) {
		return &noopSynth{}, nil
	})))
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start service: %v", err)
	}

	params := SessionParams{
		SessionID: "sess-buf",
		StreamID:  "tts",
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
	}
	key := streammanager.StreamKey(params.SessionID, params.StreamID)

	h, err := svc.manager.CreateStream(key, params)
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}
	st := h.(*stream)
	t.Cleanup(func() {
		svc.Shutdown(context.Background())
		st.Wait(context.Background())
	})

	// Simulate rebuffer state.
	st.mu.Lock()
	st.stopped = true
	st.mu.Unlock()

	// handleSpeakRequest should detect enqueue failure and buffer the request.
	svc.handleSpeakRequest(speakRequest{
		SessionID: "sess-buf",
		StreamID:  "tts",
		Text:      "should be buffered",
	})

	pq, ok := svc.manager.GetPendingQueue(key)
	if !ok || len(pq.Items) == 0 {
		t.Fatalf("expected request to be buffered in pending queue, found=%t", ok)
	}
}
