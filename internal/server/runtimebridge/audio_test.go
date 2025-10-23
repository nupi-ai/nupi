package runtimebridge

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/audio/egress"
	"github.com/nupi-ai/nupi/internal/audio/ingress"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/server"
)

func TestAudioIngressProvider(t *testing.T) {
	if AudioIngressProvider(nil) != nil {
		t.Fatalf("expected nil provider for nil service")
	}

	bus := eventbus.New()
	svc := ingress.New(bus)
	provider := AudioIngressProvider(svc)
	if provider == nil {
		t.Fatalf("expected provider instance")
	}

	format := eventbus.AudioFormat{
		Encoding:   eventbus.AudioEncodingPCM16,
		SampleRate: 16000,
		Channels:   1,
		BitDepth:   16,
	}

	stream, err := provider.OpenStream("session", "primary", format, map[string]string{"origin": "test"})
	if err != nil {
		t.Fatalf("OpenStream returned error: %v", err)
	}

	if err := stream.Write([]byte{1, 2, 3}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	stream, err = provider.OpenStream("session", "primary", format, nil)
	if err != nil {
		t.Fatalf("OpenStream second attempt returned error: %v", err)
	}
	defer stream.Close()

	if _, err := provider.OpenStream("session", "primary", format, nil); !errors.Is(err, server.ErrAudioStreamExists) {
		t.Fatalf("expected ErrAudioStreamExists, got %v", err)
	}
}

func TestAudioEgressController(t *testing.T) {
	if AudioEgressController(nil) != nil {
		t.Fatalf("expected nil controller for nil service")
	}

	bus := eventbus.New()
	format := eventbus.AudioFormat{
		Encoding:   eventbus.AudioEncodingPCM16,
		SampleRate: 44100,
		Channels:   2,
		BitDepth:   16,
	}

	speakCh := make(chan egress.SpeakRequest, 1)
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)

	factory := egress.FactoryFunc(func(ctx context.Context, params egress.SessionParams) (egress.Synthesizer, error) {
		return &stubSynth{requests: speakCh}, nil
	})

	svc := egress.New(
		bus,
		egress.WithFactory(factory),
		egress.WithLogger(logger),
		egress.WithAudioFormat(format),
		egress.WithStreamID("tts.custom"),
	)

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("tts service start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	controller := AudioEgressController(svc)
	if controller.DefaultStreamID() != "tts.custom" {
		t.Fatalf("unexpected stream id: %s", controller.DefaultStreamID())
	}
	if controller.PlaybackFormat() != format {
		t.Fatalf("unexpected playback format: %+v", controller.PlaybackFormat())
	}

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicConversationSpeak,
		Payload: eventbus.ConversationSpeakEvent{
			SessionID: "sess",
			PromptID:  "prompt",
			Text:      "hello",
		},
	})

	select {
	case req := <-speakCh:
		if req.SessionID != "sess" {
			t.Fatalf("unexpected session id in synth request: %+v", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for synthesizer invocation")
	}

	controller.Interrupt("sess", "", "barge", map[string]string{"reason": "test"})

	deadline := time.Now().Add(2 * time.Second)
	for {
		if strings.Contains(logBuf.String(), "barge-in interrupt session=sess") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected interrupt log, got %q", logBuf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type stubSynth struct {
	requests chan<- egress.SpeakRequest
}

func (s *stubSynth) Speak(ctx context.Context, req egress.SpeakRequest) ([]egress.SynthesisChunk, error) {
	select {
	case s.requests <- req:
	default:
	}
	return []egress.SynthesisChunk{{
		Data:   []byte("audio"),
		Final:  true,
		Format: nil,
	}}, nil
}

func (s *stubSynth) Close(ctx context.Context) ([]egress.SynthesisChunk, error) {
	return nil, nil
}
