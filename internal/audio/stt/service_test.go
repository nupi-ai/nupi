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

	partialSub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptPartial)
	defer partialSub.Close()
	finalSub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptFinal)
	defer finalSub.Close()

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

	eventbus.Publish(context.Background(), bus, eventbus.Audio.IngressSegment, "", segment1)

	first := receiveTranscript(t, partialSub, 1*time.Second)
	if first.Sequence != 1 {
		t.Fatalf("unexpected sequence: got %d want 1", first.Sequence)
	}
	if first.Text != "hello" || first.Final {
		t.Fatalf("unexpected transcript contents: %+v", first)
	}
	if first.Metadata["locale"] != "en-US" {
		t.Fatalf("metadata not propagated: %+v", first.Metadata)
	}

	if first.Final {
		t.Fatalf("expected non-final transcript on partial topic")
	}

	eventbus.Publish(context.Background(), bus, eventbus.Audio.IngressSegment, "", segment2)

	finalTranscript := receiveTranscript(t, finalSub, 1*time.Second)
	if finalTranscript.Sequence != 2 || !finalTranscript.Final {
		t.Fatalf("unexpected final transcript on dedicated topic: %+v", finalTranscript)
	}
	if finalTranscript.Text != "hello world" {
		t.Fatalf("unexpected final transcript contents: %+v", finalTranscript)
	}

	expectNoEvent(t, partialSub, 50*time.Millisecond)

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

	sub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptPartial)
	defer sub.Close()

	eventbus.Publish(context.Background(), bus, eventbus.Audio.IngressSegment, "", eventbus.AudioIngressSegmentEvent{
		SessionID: "sess-err",
		StreamID:  "mic",
		Sequence:  1,
	})

	select {
	case <-sub.C():
		t.Fatalf("unexpected transcript event published")
	case <-time.After(100 * time.Millisecond):
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

	sub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptFinal)
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

	eventbus.Publish(context.Background(), bus, eventbus.Audio.IngressSegment, "", segment)

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

	sub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptFinal)
	defer sub.Close()

	for i := 0; i < 10; i++ {
		eventbus.Publish(context.Background(), bus, eventbus.Audio.IngressSegment, "", eventbus.AudioIngressSegmentEvent{
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
		})
	}

	select {
	case <-sub.C():
		t.Fatalf("unexpected transcript when factory unavailable")
	case <-time.After(50 * time.Millisecond):
	}

	if svc.manager.PendingCount() != 0 {
		t.Fatalf("expected no pending entries when factory unavailable, got %d", svc.manager.PendingCount())
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

	sub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptFinal)
	defer sub.Close()

	for i := 0; i < totalSegments; i++ {
		seq := uint64(i)
		eventbus.Publish(context.Background(), bus, eventbus.Audio.IngressSegment, "", eventbus.AudioIngressSegmentEvent{
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
		})
	}

	transcript := receiveTranscript(t, sub, time.Second)
	if transcript.Text != "kept" {
		t.Fatalf("expected only last buffered transcript; got %q", transcript.Text)
	}
}

func TestServiceRetryDropsPendingOnPermanentError(t *testing.T) {
	bus := eventbus.New()

	var (
		mu       sync.Mutex
		attempts int
	)

	permanentErr := errors.New("duplicate runtime entries")
	factory := FactoryFunc(func(ctx context.Context, params SessionParams) (Transcriber, error) {
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

	eventbus.Publish(context.Background(), bus, eventbus.Audio.IngressSegment, "", eventbus.AudioIngressSegmentEvent{
		SessionID: "sess-perm",
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

func receiveTranscript(t *testing.T, sub *eventbus.TypedSubscription[eventbus.SpeechTranscriptEvent], timeout time.Duration) eventbus.SpeechTranscriptEvent {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case env := <-sub.C():
		return env.Payload
	case <-timer.C:
		t.Fatalf("timeout waiting for transcript")
	}
	return eventbus.SpeechTranscriptEvent{}
}

func expectNoEvent(t *testing.T, sub *eventbus.TypedSubscription[eventbus.SpeechTranscriptEvent], timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case env := <-sub.C():
		t.Fatalf("unexpected event on topic: %+v", env.Payload)
	case <-timer.C:
	}
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

func TestSTTRecoversMidStreamAdapterFailure(t *testing.T) {
	bus := eventbus.New()

	var (
		mu      sync.Mutex
		callNum int
	)

	factory := FactoryFunc(func(ctx context.Context, params SessionParams) (Transcriber, error) {
		mu.Lock()
		defer mu.Unlock()
		callNum++
		switch callNum {
		case 1:
			return &failingTranscriber{failOnSeq: 2}, nil
		case 2:
			return nil, ErrAdapterUnavailable
		default:
			return &scriptedTranscriber{
				outputs: map[uint64][]Transcription{
					3: {
						{
							Text:       "recovered",
							Confidence: 0.85,
							Final:      true,
							StartedAt:  time.Unix(1, 0).UTC(),
							EndedAt:    time.Unix(1, int64(200*time.Millisecond)).UTC(),
						},
					},
				},
			}, nil
		}
	})

	svc := New(bus, WithFactory(factory))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer svc.Shutdown(context.Background())

	finalSub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptFinal)
	defer finalSub.Close()

	baseSegment := eventbus.AudioIngressSegmentEvent{
		SessionID: "sess-recover",
		StreamID:  "mic",
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
		Data:     make([]byte, 640),
		Duration: 20 * time.Millisecond,
	}

	seg1 := baseSegment
	seg1.Sequence = 1
	seg1.First = true
	eventbus.Publish(context.Background(), bus, eventbus.Audio.IngressSegment, "", seg1)
	time.Sleep(20 * time.Millisecond)

	seg2 := baseSegment
	seg2.Sequence = 2
	eventbus.Publish(context.Background(), bus, eventbus.Audio.IngressSegment, "", seg2)
	time.Sleep(20 * time.Millisecond)

	seg3drop := baseSegment
	seg3drop.Sequence = 99
	eventbus.Publish(context.Background(), bus, eventbus.Audio.IngressSegment, "", seg3drop)
	time.Sleep(20 * time.Millisecond)

	seg3 := baseSegment
	seg3.Sequence = 3
	seg3.Last = true
	eventbus.Publish(context.Background(), bus, eventbus.Audio.IngressSegment, "", seg3)

	transcript := receiveTranscript(t, finalSub, time.Second)
	if transcript.Text != "recovered" {
		t.Fatalf("expected recovered transcript, got %q", transcript.Text)
	}
}

type failingTranscriber struct {
	mu            sync.Mutex
	failOnSeq     uint64
	partialOnFail []Transcription
}

func (f *failingTranscriber) OnSegment(_ context.Context, segment eventbus.AudioIngressSegmentEvent) ([]Transcription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if segment.Sequence == f.failOnSeq {
		return append([]Transcription(nil), f.partialOnFail...), ErrAdapterUnavailable
	}
	return nil, nil
}

func (f *failingTranscriber) Close(context.Context) ([]Transcription, error) {
	return nil, nil
}

func TestSTTPublishesPartialResultsBeforeRecovery(t *testing.T) {
	bus := eventbus.New()

	factory := FactoryFunc(func(ctx context.Context, params SessionParams) (Transcriber, error) {
		return &failingTranscriber{
			failOnSeq: 1,
			partialOnFail: []Transcription{
				{
					Text:       "partial before fail",
					Confidence: 0.5,
					Final:      false,
					StartedAt:  time.Unix(1, 0).UTC(),
					EndedAt:    time.Unix(1, int64(100*time.Millisecond)).UTC(),
				},
			},
		}, nil
	})

	svc := New(bus, WithFactory(factory))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer svc.Shutdown(context.Background())

	partialSub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptPartial)
	defer partialSub.Close()

	seg := eventbus.AudioIngressSegmentEvent{
		SessionID: "sess-partial",
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
		First:    true,
	}

	eventbus.Publish(context.Background(), bus, eventbus.Audio.IngressSegment, "", seg)

	tr := receiveTranscript(t, partialSub, time.Second)
	if tr.Text != "partial before fail" {
		t.Fatalf("expected partial transcript to be published before recovery, got %q", tr.Text)
	}
}
