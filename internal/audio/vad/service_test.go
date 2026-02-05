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
	if metrics.RetryFailuresTotal != 0 {
		t.Fatalf("expected retry failures to remain zero, got %d", metrics.RetryFailuresTotal)
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
	if metrics.RetryFailuresTotal != 0 {
		t.Fatalf("expected retry failures to remain zero during adapter wait")
	}
}

func TestVADServiceMetricsRetryFailures(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	factory := newTogglingFactory()
	svc := New(bus, WithFactory(factory), WithRetryDelays(5*time.Millisecond, 20*time.Millisecond))
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer svc.Shutdown(context.Background())

	vadSub := bus.Subscribe(eventbus.TopicSpeechVADDetected)
	defer vadSub.Close()

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

	// First retry attempts should encounter adapter errors.
	time.Sleep(2 * time.Millisecond)
	factory.set(factoryStateError)

	waitFor(t, 500*time.Millisecond, func() bool {
		return svc.Metrics().RetryFailuresTotal > 0
	})

	// Unblock by allowing the factory to succeed and emit detections.
	factory.set(factoryStateSuccess)

	event := receiveVADEvent(t, vadSub)
	if event.SessionID != "sess-retry" {
		t.Fatalf("unexpected session id: %s", event.SessionID)
	}

	metrics := svc.Metrics()
	if metrics.RetryAttemptsTotal == 0 {
		t.Fatalf("expected retry attempts to be recorded")
	}
	if metrics.RetryFailuresTotal == 0 {
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
