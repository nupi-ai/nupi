package stt

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

func TestServicePublishesTranscripts(t *testing.T) {
	bus := eventbus.New()

	transcriber := &scriptedTranscriber{
		outputs: map[uint64][]Transcription{
			1: {
				{
					Text:       "hello",
					Confidence: 0.8,
					Final:      false,
					StartedAt:  time.Unix(1, 0).UTC(),
					EndedAt:    time.Unix(1, int64(200*time.Millisecond)).UTC(),
					Metadata: map[string]string{
						"locale": "en-US",
					},
				},
			},
		},
		closeOutputs: []Transcription{
			{
				Text:       "hello world",
				Confidence: 0.92,
				Final:      true,
				StartedAt:  time.Unix(1, 0).UTC(),
				EndedAt:    time.Unix(1, int64(500*time.Millisecond)).UTC(),
			},
		},
	}

	factory := FactoryFunc(func(ctx context.Context, params SessionParams) (Transcriber, error) {
		if params.SessionID != "sess-1" || params.StreamID != "mic" {
			t.Fatalf("unexpected params: %+v", params)
		}
		return transcriber, nil
	})

	svc := New(bus, WithFactory(factory))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer svc.Shutdown(context.Background())

	sub := bus.Subscribe(eventbus.TopicSpeechTranscript)
	defer sub.Close()

	segment1 := eventbus.AudioIngressSegmentEvent{
		SessionID: "sess-1",
		StreamID:  "mic",
		Sequence:  1,
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
		Data:      make([]byte, 640),
		Duration:  20 * time.Millisecond,
		First:     true,
		Last:      false,
		StartedAt: time.Unix(1, 0).UTC(),
		EndedAt:   time.Unix(1, int64(200*time.Millisecond)).UTC(),
	}

	segment2 := eventbus.AudioIngressSegmentEvent{
		SessionID: "sess-1",
		StreamID:  "mic",
		Sequence:  2,
		Format:    segment1.Format,
		Data:      make([]byte, 640),
		Duration:  20 * time.Millisecond,
		First:     false,
		Last:      true,
		StartedAt: time.Unix(1, int64(200*time.Millisecond)).UTC(),
		EndedAt:   time.Unix(1, int64(400*time.Millisecond)).UTC(),
	}

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicAudioIngressSegment,
		Payload: segment1,
	})

	first := receiveTranscript(t, sub, 1*time.Second)
	if first.Sequence != 1 {
		t.Fatalf("unexpected sequence: got %d want 1", first.Sequence)
	}
	if first.Text != "hello" || first.Final {
		t.Fatalf("unexpected transcript contents: %+v", first)
	}
	if first.Metadata["locale"] != "en-US" {
		t.Fatalf("metadata not propagated: %+v", first.Metadata)
	}

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicAudioIngressSegment,
		Payload: segment2,
	})

	second := receiveTranscript(t, sub, 1*time.Second)
	if second.Sequence != 2 {
		t.Fatalf("unexpected second sequence: %d", second.Sequence)
	}
	if second.Text != "hello world" || !second.Final {
		t.Fatalf("unexpected final transcript: %+v", second)
	}

	if !transcriber.closed() {
		t.Fatalf("expected transcriber Close to be invoked")
	}
}

func TestServiceFactoryError(t *testing.T) {
	bus := eventbus.New()

	factory := FactoryFunc(func(context.Context, SessionParams) (Transcriber, error) {
		return nil, errors.New("failed to create transcriber")
	})

	svc := New(bus, WithFactory(factory))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer svc.Shutdown(context.Background())

	sub := bus.Subscribe(eventbus.TopicSpeechTranscript)
	defer sub.Close()

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicAudioIngressSegment,
		Payload: eventbus.AudioIngressSegmentEvent{
			SessionID: "sess-err",
			StreamID:  "mic",
			Sequence:  1,
		},
	})

	select {
	case <-sub.C():
		t.Fatalf("unexpected transcript event published")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestServiceMetricsSegments(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus, WithFactory(FactoryFunc(func(context.Context, SessionParams) (Transcriber, error) {
		return nil, ErrAdapterUnavailable
	})))

	if metrics := svc.Metrics(); metrics.SegmentsTotal != 0 {
		t.Fatalf("expected initial SegmentsTotal to be 0, got %d", metrics.SegmentsTotal)
	}

	segment := eventbus.AudioIngressSegmentEvent{
		SessionID: "metrics-session",
		StreamID:  "mic",
		Sequence:  1,
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
		Data:     make([]byte, 640),
		Duration: 20 * time.Millisecond,
	}

	svc.handleSegment(segment)
	svc.handleSegment(segment)

	if metrics := svc.Metrics(); metrics.SegmentsTotal != 2 {
		t.Fatalf("expected SegmentsTotal 2, got %d", metrics.SegmentsTotal)
	}
}

func TestServiceBuffersUntilAdapterAvailable(t *testing.T) {
	bus := eventbus.New()

	var (
		mu    sync.Mutex
		ready bool
	)

	factory := FactoryFunc(func(ctx context.Context, params SessionParams) (Transcriber, error) {
		mu.Lock()
		defer mu.Unlock()
		if !ready {
			return nil, ErrAdapterUnavailable
		}
		return &scriptedTranscriber{
			outputs: map[uint64][]Transcription{
				1: {
					{
						Text:       "hello buffered",
						Confidence: 0.9,
						Final:      true,
						StartedAt:  time.Unix(1, 0).UTC(),
						EndedAt:    time.Unix(1, int64(200*time.Millisecond)).UTC(),
					},
				},
			},
		}, nil
	})

	svc := New(
		bus,
		WithFactory(factory),
		WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer svc.Shutdown(context.Background())

	sub := bus.Subscribe(eventbus.TopicSpeechTranscript)
	defer sub.Close()

	segment := eventbus.AudioIngressSegmentEvent{
		SessionID: "sess-pending",
		StreamID:  "mic",
		Sequence:  1,
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
		Data:      make([]byte, 640),
		Duration:  20 * time.Millisecond,
		First:     true,
		Last:      true,
		StartedAt: time.Unix(1, 0).UTC(),
		EndedAt:   time.Unix(1, int64(200*time.Millisecond)).UTC(),
	}

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicAudioIngressSegment,
		Payload: segment,
	})

	select {
	case evt := <-sub.C():
		t.Fatalf("unexpected transcript before adapter ready: %+v", evt.Payload)
	case <-time.After(25 * time.Millisecond):
	}

	mu.Lock()
	ready = true
	mu.Unlock()

	transcript := receiveTranscript(t, sub, 500*time.Millisecond)
	if transcript.Text != "hello buffered" {
		t.Fatalf("unexpected transcript text: %q", transcript.Text)
	}
	if !transcript.Final {
		t.Fatalf("expected final transcript flag")
	}
}

func TestServiceDoesNotBufferWhenFactoryUnavailable(t *testing.T) {
	bus := eventbus.New()

	svc := New(bus)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer svc.Shutdown(context.Background())

	sub := bus.Subscribe(eventbus.TopicSpeechTranscript)
	defer sub.Close()

	for i := 0; i < 10; i++ {
		bus.Publish(context.Background(), eventbus.Envelope{
			Topic: eventbus.TopicAudioIngressSegment,
			Payload: eventbus.AudioIngressSegmentEvent{
				SessionID: "sess-none",
				StreamID:  "mic",
				Sequence:  uint64(i),
				Format: eventbus.AudioFormat{
					Encoding:   eventbus.AudioEncodingPCM16,
					SampleRate: 16000,
					Channels:   1,
					BitDepth:   16,
				},
				Data:     make([]byte, 640),
				Duration: 20 * time.Millisecond,
				First:    i == 0,
				Last:     i == 9,
			},
		})
	}

	select {
	case <-sub.C():
		t.Fatalf("unexpected transcript when factory unavailable")
	case <-time.After(50 * time.Millisecond):
	}

	svc.pendingMu.Lock()
	defer svc.pendingMu.Unlock()
	if len(svc.pending) != 0 {
		t.Fatalf("expected no pending entries when factory unavailable, got %d", len(svc.pending))
	}
}

func TestPendingBufferDropsOldestWhenFull(t *testing.T) {
	bus := eventbus.New()

	var (
		mu       sync.Mutex
		attempts int
	)

	totalSegments := maxPendingSegments + 1
	targetSequence := uint64(totalSegments - 1)

	factory := FactoryFunc(func(ctx context.Context, params SessionParams) (Transcriber, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts < 3 {
			return nil, ErrAdapterUnavailable
		}
		return &scriptedTranscriber{
			outputs: map[uint64][]Transcription{
				targetSequence: {
					{
						Text:       "kept",
						Confidence: 0.7,
						Final:      true,
						StartedAt:  time.Unix(1, 0).UTC(),
						EndedAt:    time.Unix(1, int64(200*time.Millisecond)).UTC(),
					},
				},
			},
		}, nil
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

	sub := bus.Subscribe(eventbus.TopicSpeechTranscript)
	defer sub.Close()

	for i := 0; i < totalSegments; i++ {
		seq := uint64(i)
		bus.Publish(context.Background(), eventbus.Envelope{
			Topic: eventbus.TopicAudioIngressSegment,
			Payload: eventbus.AudioIngressSegmentEvent{
				SessionID: "sess-drop",
				StreamID:  "mic",
				Sequence:  seq,
				Format: eventbus.AudioFormat{
					Encoding:   eventbus.AudioEncodingPCM16,
					SampleRate: 16000,
					Channels:   1,
					BitDepth:   16,
				},
				Data:      make([]byte, 640),
				Duration:  20 * time.Millisecond,
				First:     seq == 0,
				Last:      seq == targetSequence,
				StartedAt: time.Unix(1, 0).UTC(),
				EndedAt:   time.Unix(1, int64(200*time.Millisecond)).UTC(),
			},
		})
	}

	transcript := receiveTranscript(t, sub, time.Second)
	if transcript.Text != "kept" {
		t.Fatalf("expected only last buffered transcript; got %q", transcript.Text)
	}
}

func receiveTranscript(t *testing.T, sub *eventbus.Subscription, timeout time.Duration) eventbus.SpeechTranscriptEvent {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case env := <-sub.C():
		payload, ok := env.Payload.(eventbus.SpeechTranscriptEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", env.Payload)
		}
		return payload
	case <-timer.C:
		t.Fatalf("timeout waiting for transcript")
	}
	return eventbus.SpeechTranscriptEvent{}
}

type scriptedTranscriber struct {
	mu           sync.Mutex
	outputs      map[uint64][]Transcription
	closeOutputs []Transcription
	closeCalled  bool
}

func (s *scriptedTranscriber) OnSegment(_ context.Context, segment eventbus.AudioIngressSegmentEvent) ([]Transcription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outputs == nil {
		return nil, nil
	}
	return append([]Transcription(nil), s.outputs[segment.Sequence]...), nil
}

func (s *scriptedTranscriber) Close(context.Context) ([]Transcription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalled = true
	return append([]Transcription(nil), s.closeOutputs...), nil
}

func (s *scriptedTranscriber) closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCalled
}
