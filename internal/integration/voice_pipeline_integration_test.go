package integration

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	"github.com/nupi-ai/nupi/internal/audio/barge"
	"github.com/nupi-ai/nupi/internal/audio/egress"
	"github.com/nupi-ai/nupi/internal/audio/ingress"
	"github.com/nupi-ai/nupi/internal/audio/stt"
	"github.com/nupi-ai/nupi/internal/audio/vad"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/contentpipeline"
	"github.com/nupi-ai/nupi/internal/conversation"
	"github.com/nupi-ai/nupi/internal/eventbus"
	adapters "github.com/nupi-ai/nupi/internal/plugins/adapters"
	testutil "github.com/nupi-ai/nupi/internal/testutil"
	"github.com/nupi-ai/nupi/internal/voice/slots"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

func TestVoicePipelineEndToEndWithBarge(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapters.MockSTTAdapterID, map[string]any{
		"phrases": []any{"hello voice", "please respond"},
	}); err != nil {
		t.Fatalf("activate stt adapter: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, map[string]any{
		"duration_ms": 600,
	}); err != nil {
		t.Fatalf("activate tts adapter: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.2,
		"min_frames": 2,
	}); err != nil {
		t.Fatalf("activate vad adapter: %v", err)
	}

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	vadSvc := vad.New(bus,
		vad.WithFactory(vad.NewAdapterFactory(store)),
		vad.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	bargeSvc := barge.New(bus,
		barge.WithConfidenceThreshold(0.1),
		barge.WithCooldown(50*time.Millisecond),
		barge.WithQuietPeriod(0),
	)

	ttsFactory := egress.NewAdapterFactory(store)
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

	pipelineSvc := contentpipeline.NewService(bus, nil)

	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)

	services := []startStopper{
		pipelineSvc,
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
		streamID = slots.TTS
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

func TestAudioIngressToSTTGRPCPipeline(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	const adapterID = "adapter.stt.grpc.test"
	adapter := configstore.Adapter{
		ID:      adapterID,
		Source:  "local",
		Type:    "stt",
		Name:    "Test gRPC STT",
		Version: "dev",
	}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapterID, nil); err != nil {
		t.Fatalf("activate stt adapter: %v", err)
	}

	const bufSize = 1024 * 1024
	lis := bufconn.Listen(bufSize)
	grpcServer := grpc.NewServer()
	napv1.RegisterSpeechToTextServiceServer(grpcServer, &grpcTestSTTServer{})
	go func() {
		if err := grpcServer.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Logf("grpc server stopped: %v", err)
		}
	}()
	t.Cleanup(func() {
		grpcServer.GracefulStop()
		_ = lis.Close()
	})
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}

	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapterID,
		Transport: "grpc",
		Address:   "bufconn",
	}); err != nil {
		t.Fatalf("register adapter endpoint: %v", err)
	}

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(
		bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus, conversation.WithHistoryLimit(8), conversation.WithDetachTTL(time.Second))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := ingressSvc.Start(runCtx); err != nil {
		t.Fatalf("start ingress service: %v", err)
	}
	defer ingressSvc.Shutdown(context.Background())

	if err := sttSvc.Start(stt.ContextWithDialer(runCtx, dialer)); err != nil {
		t.Fatalf("start stt service: %v", err)
	}
	defer sttSvc.Shutdown(context.Background())

	if err := pipelineSvc.Start(runCtx); err != nil {
		t.Fatalf("start pipeline service: %v", err)
	}
	defer pipelineSvc.Shutdown(context.Background())

	if err := conversationSvc.Start(runCtx); err != nil {
		t.Fatalf("start conversation service: %v", err)
	}
	defer conversationSvc.Shutdown(context.Background())

	finalSub := bus.Subscribe(eventbus.TopicSpeechTranscriptFinal, eventbus.WithSubscriptionName("test_final_transcripts"))
	defer finalSub.Close()
	promptSub := bus.Subscribe(eventbus.TopicConversationPrompt, eventbus.WithSubscriptionName("test_conversation_prompt"))
	defer promptSub.Close()

	finalCh := make(chan eventbus.SpeechTranscriptEvent, 1)
	pipelineCh := make(chan eventbus.PipelineMessageEvent, 1)
	promptCh := make(chan eventbus.ConversationPromptEvent, 1)

	bridgeCtx, bridgeCancel := context.WithCancel(runCtx)
	defer bridgeCancel()

	go func() {
		for {
			select {
			case <-bridgeCtx.Done():
				return
			case env, ok := <-finalSub.C():
				if !ok {
					return
				}
				tr, ok := env.Payload.(eventbus.SpeechTranscriptEvent)
				if !ok {
					continue
				}
				select {
				case finalCh <- tr:
				default:
				}
			}
		}
	}()

	pipelineSub := bus.Subscribe(eventbus.TopicPipelineCleaned, eventbus.WithSubscriptionName("test_pipeline_cleaned"))
	defer pipelineSub.Close()

	go func() {
		for {
			select {
			case <-bridgeCtx.Done():
				return
			case env, ok := <-pipelineSub.C():
				if !ok {
					return
				}
				evt, ok := env.Payload.(eventbus.PipelineMessageEvent)
				if !ok {
					continue
				}
				select {
				case pipelineCh <- evt:
				default:
				}
			}
		}
	}()
	go func() {
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
				select {
				case promptCh <- prompt:
				default:
				}
			}
		}
	}()

	sessionID := "grpc-session"
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:  eventbus.TopicSessionsLifecycle,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionLifecycleEvent{
			SessionID: sessionID,
			State:     eventbus.SessionStateRunning,
		},
	})

	format := eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}

	stream, err := ingressSvc.OpenStream(sessionID, "mic", format, map[string]string{
		"locale": "en-US",
	})
	if err != nil {
		t.Fatalf("open ingress stream: %v", err)
	}

	segment := make([]byte, 640)
	for i := 0; i < 2; i++ {
		if err := stream.Write(segment); err != nil {
			t.Fatalf("write segment %d: %v", i+1, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	finalEvent := waitFor(t, finalCh, 2*time.Second)
	if !finalEvent.Final {
		t.Fatalf("expected final transcript, got %+v", finalEvent)
	}
	if finalEvent.Text != fmt.Sprintf("final-%d", finalEvent.Sequence) {
		t.Fatalf("unexpected final transcript text: %+v", finalEvent)
	}

	pipelineEvent := waitFor(t, pipelineCh, time.Second)
	if pipelineEvent.Text != finalEvent.Text {
		t.Fatalf("pipeline text mismatch: got %q want %q", pipelineEvent.Text, finalEvent.Text)
	}

	promptEvent := waitFor(t, promptCh, time.Second)
	if promptEvent.NewMessage.Text != finalEvent.Text {
		t.Fatalf("prompt text mismatch: got %q want %q", promptEvent.NewMessage.Text, finalEvent.Text)
	}
	if promptEvent.NewMessage.Origin != eventbus.OriginUser {
		t.Fatalf("unexpected prompt origin: %s", promptEvent.NewMessage.Origin)
	}
}

func waitFor[T any](t *testing.T, ch <-chan T, timeout time.Duration) T {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case v := <-ch:
		return v
	case <-timer.C:
		t.Fatalf("timeout waiting for value (%s)", timeout)
	}
	var zero T
	return zero
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

type grpcTestSTTServer struct {
	napv1.UnimplementedSpeechToTextServiceServer
}

func (s *grpcTestSTTServer) StreamTranscription(stream napv1.SpeechToTextService_StreamTranscriptionServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		if req.GetFlush() {
			return nil
		}

		segment := req.GetSegment()
		if segment == nil {
			continue
		}

		text := fmt.Sprintf("segment-%d", segment.GetSequence())
		if segment.GetLast() {
			text = fmt.Sprintf("final-%d", segment.GetSequence())
		}

		resp := &napv1.Transcript{
			Sequence:   segment.GetSequence(),
			Text:       text,
			Confidence: 0.8,
			Final:      segment.GetLast(),
			Metadata:   segment.GetMetadata(),
			StartedAt:  segment.GetStartedAt(),
			EndedAt:    segment.GetEndedAt(),
		}

		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}
