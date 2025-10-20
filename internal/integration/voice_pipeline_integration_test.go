package integration

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/audio/barge"
	"github.com/nupi-ai/nupi/internal/audio/egress"
	"github.com/nupi-ai/nupi/internal/audio/ingress"
	"github.com/nupi-ai/nupi/internal/audio/stt"
	"github.com/nupi-ai/nupi/internal/audio/vad"
	"github.com/nupi-ai/nupi/internal/conversation"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/modules"
	testutil "github.com/nupi-ai/nupi/internal/testutil"
)

func TestVoicePipelineEndToEndWithBarge(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := modules.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, string(modules.SlotSTTPrimary), modules.MockSTTAdapterID, map[string]any{
		"phrases": []any{"hello voice", "please respond"},
	}); err != nil {
		t.Fatalf("activate stt adapter: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, string(modules.SlotTTS), modules.MockTTSAdapterID, map[string]any{
		"duration_ms": 600,
	}); err != nil {
		t.Fatalf("activate tts adapter: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, string(modules.SlotVAD), modules.MockVADAdapterID, map[string]any{
		"threshold":  0.2,
		"min_frames": 2,
	}); err != nil {
		t.Fatalf("activate vad adapter: %v", err)
	}

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewModuleFactory(store)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	vadSvc := vad.New(bus,
		vad.WithFactory(vad.NewModuleFactory(store)),
		vad.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	bargeSvc := barge.New(bus,
		barge.WithConfidenceThreshold(0.1),
		barge.WithCooldown(50*time.Millisecond),
		barge.WithQuietPeriod(0),
	)

	ttsFactory := egress.NewModuleFactory(store)
	streamingFactory := egress.FactoryFunc(func(ctx context.Context, params egress.SessionParams) (egress.Synthesizer, error) {
		synth, err := ttsFactory.Create(ctx, params)
		if err != nil {
			return nil, err
		}
		return newStreamingSynth(synth), nil
	})

	egressSvc := egress.New(bus,
		egress.WithFactory(streamingFactory),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)

	services := []startStopper{
		ingressSvc,
		sttSvc,
		vadSvc,
		bargeSvc,
		egressSvc,
		conversationSvc,
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start service %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	transcriptSub := bus.Subscribe(eventbus.TopicSpeechTranscript)
	defer transcriptSub.Close()
	promptSub := bus.Subscribe(eventbus.TopicConversationPrompt)
	defer promptSub.Close()
	playbackSub := bus.Subscribe(eventbus.TopicAudioEgressPlayback)
	defer playbackSub.Close()
	bargeSub := bus.Subscribe(eventbus.TopicSpeechBargeIn)
	defer bargeSub.Close()

	var bridgeWG sync.WaitGroup
	bridgeCtx, bridgeCancel := context.WithCancel(runCtx)
	defer func() {
		bridgeCancel()
		bridgeWG.Wait()
	}()

	bridgeWG.Add(1)
	go func() {
		defer bridgeWG.Done()
		for {
			select {
			case <-bridgeCtx.Done():
				return
			case env, ok := <-transcriptSub.C():
				if !ok {
					return
				}
				transcript, ok := env.Payload.(eventbus.SpeechTranscriptEvent)
				if !ok || !transcript.Final {
					continue
				}
				bus.Publish(context.Background(), eventbus.Envelope{
					Topic:  eventbus.TopicPipelineCleaned,
					Source: eventbus.SourceContentPipeline,
					Payload: eventbus.PipelineMessageEvent{
						SessionID:   transcript.SessionID,
						Origin:      eventbus.OriginUser,
						Text:        transcript.Text,
						Annotations: transcript.Metadata,
						Sequence:    transcript.Sequence,
					},
				})
			}
		}
	}()

	bridgeWG.Add(1)
	go func() {
		defer bridgeWG.Done()
		for {
			select {
			case <-bridgeCtx.Done():
				return
			case env, ok := <-promptSub.C():
				if !ok {
					return
				}
				prompt, ok := env.Payload.(eventbus.ConversationPromptEvent)
				if !ok {
					continue
				}
				replyText := "Acknowledged: " + prompt.NewMessage.Text
				bus.Publish(context.Background(), eventbus.Envelope{
					Topic:  eventbus.TopicConversationReply,
					Source: eventbus.SourceConversation,
					Payload: eventbus.ConversationReplyEvent{
						SessionID: prompt.SessionID,
						PromptID:  prompt.PromptID,
						Text:      replyText,
						Metadata: map[string]string{
							"adapter": "mock.ai",
						},
					},
				})
			}
		}
	}()

	format := eventbus.AudioFormat{
		Encoding:   eventbus.AudioEncodingPCM16,
		SampleRate: 16000,
		Channels:   1,
		BitDepth:   16,
	}

	const sessionID = "voice-session"
	stream, err := ingressSvc.OpenStream(sessionID, "mic", format, map[string]string{
		"locale": "en-US",
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer stream.Close()

	quietChunk := pcmBuffer(200, 320)
	if err := stream.Write(quietChunk); err != nil {
		t.Fatalf("write chunk 1: %v", err)
	}
	if err := stream.Write(quietChunk); err != nil {
		t.Fatalf("write chunk 2: %v", err)
	}

	firstPlayback := waitForPlayback(t, playbackSub, 2*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return !evt.Final
	})
	if firstPlayback.SessionID != sessionID {
		t.Fatalf("unexpected playback session: %s", firstPlayback.SessionID)
	}
	if firstPlayback.Final {
		t.Fatalf("expected streaming playback chunk, got final")
	}

	streamID := firstPlayback.StreamID
	if streamID == "" {
		streamID = "tts.primary"
	}

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:  eventbus.TopicAudioInterrupt,
		Source: eventbus.SourceClient,
		Payload: eventbus.AudioInterruptEvent{
			SessionID: sessionID,
			StreamID:  streamID,
			Reason:    "manual",
			Metadata: map[string]string{
				"origin": "integration-test",
			},
			Timestamp: time.Now().UTC(),
		},
	})

	bargeEvent := waitForBargeEvent(t, bargeSub, time.Second)
	if bargeEvent.SessionID != sessionID {
		t.Fatalf("unexpected barge session: %s", bargeEvent.SessionID)
	}
	if bargeEvent.Reason != "manual" {
		t.Fatalf("unexpected barge reason: %s", bargeEvent.Reason)
	}

	finalPlayback := waitForPlayback(t, playbackSub, time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return evt.Final
	})
	if finalPlayback.Metadata["barge_in"] != "true" {
		t.Fatalf("expected barge metadata on final playback, got %+v", finalPlayback.Metadata)
	}
	if finalPlayback.Metadata["barge_in_reason"] != "manual" {
		t.Fatalf("unexpected barge_in_reason: %s", finalPlayback.Metadata["barge_in_reason"])
	}

	waitForConversationMeta(t, conversationSvc, sessionID, time.Second, func(meta map[string]string) bool {
		return meta["barge_in"] == "true" && meta["barge_in_reason"] == "manual"
	})
}

type startStopper interface {
	Start(context.Context) error
	Shutdown(context.Context) error
}

type streamingSynth struct {
	inner egress.Synthesizer

	mu   sync.Mutex
	tail []egress.SynthesisChunk
}

func newStreamingSynth(inner egress.Synthesizer) *streamingSynth {
	return &streamingSynth{inner: inner}
}

func (s *streamingSynth) Speak(ctx context.Context, req egress.SpeakRequest) ([]egress.SynthesisChunk, error) {
	chunks, err := s.inner.Speak(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var head egress.SynthesisChunk
	var remainder []egress.SynthesisChunk

	if len(chunks) > 1 {
		head = copyChunk(chunks[0])
		head.Final = false
		remainder = copyChunks(chunks[1:])
	} else {
		head, remainder = splitChunk(chunks[0])
	}

	s.tail = remainder
	return []egress.SynthesisChunk{head}, nil
}

func (s *streamingSynth) Close(ctx context.Context) ([]egress.SynthesisChunk, error) {
	s.mu.Lock()
	out := copyChunks(s.tail)
	s.tail = nil
	s.mu.Unlock()

	extra, err := s.inner.Close(ctx)
	if len(extra) > 0 {
		out = append(out, copyChunks(extra)...)
	}
	return out, err
}

func waitForPlayback(t *testing.T, sub *eventbus.Subscription, timeout time.Duration, predicate func(eventbus.AudioEgressPlaybackEvent) bool) eventbus.AudioEgressPlaybackEvent {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case env := <-sub.C():
			evt, ok := env.Payload.(eventbus.AudioEgressPlaybackEvent)
			if !ok {
				continue
			}
			if predicate == nil || predicate(evt) {
				return evt
			}
		case <-timer.C:
			t.Fatalf("timeout waiting for playback event (%s)", timeout)
		}
	}
}

func waitForBargeEvent(t *testing.T, sub *eventbus.Subscription, timeout time.Duration) eventbus.SpeechBargeInEvent {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case env := <-sub.C():
			evt, ok := env.Payload.(eventbus.SpeechBargeInEvent)
			if !ok {
				continue
			}
			return evt
		case <-timer.C:
			t.Fatalf("timeout waiting for barge event (%s)", timeout)
		}
	}
}

func waitForConversationMeta(t *testing.T, svc *conversation.Service, sessionID string, timeout time.Duration, predicate func(map[string]string) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		turns := svc.Context(sessionID)
		if len(turns) >= 2 {
			meta := turns[len(turns)-1].Meta
			if predicate(meta) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("conversation metadata not updated within %s", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func pcmBuffer(amplitude int16, samples int) []byte {
	buf := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(amplitude))
	}
	return buf
}

func copyChunks(chunks []egress.SynthesisChunk) []egress.SynthesisChunk {
	if len(chunks) == 0 {
		return nil
	}
	out := make([]egress.SynthesisChunk, len(chunks))
	for i, chunk := range chunks {
		out[i] = copyChunk(chunk)
	}
	return out
}

func copyChunk(chunk egress.SynthesisChunk) egress.SynthesisChunk {
	c := egress.SynthesisChunk{
		Data:     append([]byte(nil), chunk.Data...),
		Duration: chunk.Duration,
		Final:    chunk.Final,
		Metadata: copyStringMap(chunk.Metadata),
	}
	if chunk.Format != nil {
		format := *chunk.Format
		c.Format = &format
	}
	return c
}

func splitChunk(chunk egress.SynthesisChunk) (egress.SynthesisChunk, []egress.SynthesisChunk) {
	first := copyChunk(chunk)
	second := copyChunk(chunk)

	first.Final = false
	if first.Duration > 0 {
		first.Duration /= 2
		second.Duration -= first.Duration
		if second.Duration < 0 {
			second.Duration = 0
		}
	}

	if len(chunk.Data) > 0 {
		mid := len(chunk.Data) / 2
		if mid == 0 {
			mid = len(chunk.Data)
		}
		first.Data = append([]byte(nil), chunk.Data[:mid]...)
		second.Data = append([]byte(nil), chunk.Data[mid:]...)
	}

	second.Final = true
	return first, []egress.SynthesisChunk{second}
}

func copyStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
