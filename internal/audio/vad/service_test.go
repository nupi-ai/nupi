package vad

import (
	"context"
	"encoding/binary"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
)

func TestVADServiceEmitsDetections(t *testing.T) {
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
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.01,
		"min_frames": 1,
	}); err != nil {
		t.Fatalf("activate vad adapter: %v", err)
	}

	svc := New(bus, WithFactory(NewAdapterFactory(store, nil)), WithRetryDelays(10*time.Millisecond, 50*time.Millisecond))
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

	metrics := svc.Metrics()
	if metrics.DetectionsTotal == 0 {
		t.Fatalf("expected detections total to be incremented")
	}
	if metrics.RetryAttemptsTotal != 0 {
		t.Fatalf("expected retry attempts to remain zero, got %d", metrics.RetryAttemptsTotal)
	}
	if metrics.RetryAbandonedTotal != 0 {
		t.Fatalf("expected retry failures to remain zero, got %d", metrics.RetryAbandonedTotal)
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

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure adapters: %v", err)
	}

	svc := New(bus, WithFactory(NewAdapterFactory(store, nil)), WithRetryDelays(10*time.Millisecond, 50*time.Millisecond))
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

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.01,
		"min_frames": 1,
	}); err != nil {
		t.Fatalf("activate adapter: %v", err)
	}

	event := receiveVADEvent(t, vadSub)
	if event.SessionID != "sess-wait" {
		t.Fatalf("unexpected session id: %s", event.SessionID)
	}

	metrics := svc.Metrics()
	if metrics.DetectionsTotal == 0 {
		t.Fatalf("expected detections after adapter activation")
	}
	if metrics.RetryAttemptsTotal == 0 {
		t.Fatalf("expected retry attempts to be recorded")
	}
	if metrics.RetryAbandonedTotal != 0 {
		t.Fatalf("expected retry failures to remain zero during adapter wait")
	}
}

func TestVADServiceMetricsRetryAbandoned(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	factory := newTogglingFactory()
	svc := New(bus, WithFactory(factory), WithRetryDelays(5*time.Millisecond, 20*time.Millisecond))
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer svc.Shutdown(context.Background())

	segment := eventbus.AudioIngressSegmentEvent{
		SessionID: "sess-retry",
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

	// Switch from adapter-unavailable to permanent errors so that
	// MaxFailures (10) is eventually exhausted and the queue abandoned.
	time.Sleep(2 * time.Millisecond)
	factory.set(factoryStateError)

	waitFor(t, 2*time.Second, func() bool {
		return svc.Metrics().RetryAbandonedTotal > 0
	})

	metrics := svc.Metrics()
	if metrics.RetryAttemptsTotal == 0 {
		t.Fatalf("expected retry attempts to be recorded")
	}
	if metrics.RetryAbandonedTotal == 0 {
		t.Fatalf("expected retry failures to be recorded")
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

const (
	factoryStateUnavailable int32 = iota
	factoryStateError
	factoryStateSuccess
)

type togglingFactory struct {
	state atomic.Int32
}

func newTogglingFactory() *togglingFactory {
	f := &togglingFactory{}
	f.state.Store(factoryStateUnavailable)
	return f
}

func (f *togglingFactory) set(state int32) {
	f.state.Store(state)
}

func (f *togglingFactory) Create(context.Context, SessionParams) (Analyzer, error) {
	switch f.state.Load() {
	case factoryStateUnavailable:
		return nil, ErrAdapterUnavailable
	case factoryStateError:
		return nil, errors.New("adapter failure")
	default:
		return &staticAnalyzer{}, nil
	}
}

type staticAnalyzer struct{}

func (staticAnalyzer) OnSegment(context.Context, eventbus.AudioIngressSegmentEvent) ([]Detection, error) {
	return []Detection{{
		Active:     true,
		Confidence: 0.9,
	}}, nil
}

func (staticAnalyzer) Close(context.Context) ([]Detection, error) {
	return nil, nil
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for condition")
}

func TestVADRecoversMidStreamAdapterFailure(t *testing.T) {
	bus := eventbus.New()

	var callNum atomic.Int32

	factory := FactoryFunc(func(ctx context.Context, params SessionParams) (Analyzer, error) {
		n := callNum.Add(1)
		switch n {
		case 1:
			// First analyzer: will fail on second segment.
			return &failingAnalyzer{failOnSeq: 2}, nil
		case 2:
			// Recovery attempt: factory still unavailable.
			return nil, ErrAdapterUnavailable
		default:
			// Factory recovers.
			return &staticAnalyzer{}, nil
		}
	})

	svc := New(bus, WithFactory(factory))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer svc.Shutdown(context.Background())

	vadSub := bus.Subscribe(eventbus.TopicSpeechVADDetected)
	defer vadSub.Close()

	baseSegment := eventbus.AudioIngressSegmentEvent{
		SessionID: "sess-vad-recover",
		StreamID:  "mic",
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
		Data:      loudPCM(),
		Duration:  20 * time.Millisecond,
		StartedAt: time.Now().UTC(),
		EndedAt:   time.Now().UTC().Add(20 * time.Millisecond),
	}

	// Segment 1: succeeds on first analyzer.
	seg1 := baseSegment
	seg1.Sequence = 1
	seg1.First = true
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicAudioIngressSegment,
		Payload: seg1,
	})

	event := receiveVADEvent(t, vadSub)
	if event.SessionID != "sess-vad-recover" {
		t.Fatalf("unexpected session: %s", event.SessionID)
	}

	// Segment 2: analyzer returns ErrAdapterUnavailable, triggers recovery.
	seg2 := baseSegment
	seg2.Sequence = 2
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicAudioIngressSegment,
		Payload: seg2,
	})
	time.Sleep(20 * time.Millisecond)

	// Segment 3: factory returns ErrAdapterUnavailable, segment dropped.
	seg3 := baseSegment
	seg3.Sequence = 99
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicAudioIngressSegment,
		Payload: seg3,
	})
	time.Sleep(20 * time.Millisecond)

	// Segment 4: factory succeeds, new analyzer produces output.
	seg4 := baseSegment
	seg4.Sequence = 4
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicAudioIngressSegment,
		Payload: seg4,
	})

	recovered := receiveVADEvent(t, vadSub)
	if recovered.SessionID != "sess-vad-recover" {
		t.Fatalf("unexpected session after recovery: %s", recovered.SessionID)
	}
	if !recovered.Active {
		t.Fatalf("expected active detection after recovery")
	}
	if recovered.Confidence != 0.9 {
		t.Fatalf("expected confidence 0.9 from recovered analyzer, got %v (old analyzer returns 0.5)", recovered.Confidence)
	}
}

// failingAnalyzer returns ErrAdapterUnavailable on a specific sequence,
// optionally returning partial results alongside the error (matching NAP contract).
type failingAnalyzer struct {
	failOnSeq     uint64
	partialOnFail []Detection
}

func (f *failingAnalyzer) OnSegment(_ context.Context, segment eventbus.AudioIngressSegmentEvent) ([]Detection, error) {
	if segment.Sequence == f.failOnSeq {
		return append([]Detection(nil), f.partialOnFail...), ErrAdapterUnavailable
	}
	return []Detection{{Active: true, Confidence: 0.5}}, nil
}

func (f *failingAnalyzer) Close(context.Context) ([]Detection, error) {
	return nil, nil
}

func TestVADPublishesPartialResultsBeforeRecovery(t *testing.T) {
	bus := eventbus.New()

	factory := FactoryFunc(func(ctx context.Context, params SessionParams) (Analyzer, error) {
		return &failingAnalyzer{
			failOnSeq: 1,
			partialOnFail: []Detection{
				{Active: true, Confidence: 0.75},
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

	vadSub := bus.Subscribe(eventbus.TopicSpeechVADDetected)
	defer vadSub.Close()

	seg := eventbus.AudioIngressSegmentEvent{
		SessionID: "sess-vad-partial",
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
		StartedAt: time.Now().UTC(),
		EndedAt:   time.Now().UTC().Add(20 * time.Millisecond),
	}

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicAudioIngressSegment,
		Payload: seg,
	})

	event := receiveVADEvent(t, vadSub)
	if !event.Active {
		t.Fatalf("expected partial detection to be published before recovery")
	}
	if event.SessionID != "sess-vad-partial" {
		t.Fatalf("unexpected session: %s", event.SessionID)
	}
}
