package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/nupi-ai/nupi/internal/intentrouter"
	"github.com/nupi-ai/nupi/internal/napdial"
	adapters "github.com/nupi-ai/nupi/internal/plugins/adapters"
	testutil "github.com/nupi-ai/nupi/internal/testutil"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
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
		vad.WithFactory(vad.NewAdapterFactory(store, nil)),
		vad.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	bargeSvc := barge.New(bus,
		barge.WithConfidenceThreshold(0.1),
		barge.WithCooldown(50*time.Millisecond),
		barge.WithQuietPeriod(0),
	)

	ttsFactory := egress.NewAdapterFactory(store, nil)
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

	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt)
	defer promptSub.Close()
	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback)
	defer playbackSub.Close()
	bargeSub := eventbus.SubscribeTo(bus, eventbus.Speech.BargeIn)
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
				prompt := env.Payload
				replyText := "Acknowledged: " + prompt.NewMessage.Text
				eventbus.Publish(context.Background(), bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
					SessionID: prompt.SessionID,
					PromptID:  prompt.PromptID,
					Text:      replyText,
					Metadata: map[string]string{
						"adapter": "mock.ai",
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

	eventbus.Publish(context.Background(), bus, eventbus.Audio.Interrupt, eventbus.SourceClient, eventbus.AudioInterruptEvent{
		SessionID: sessionID,
		StreamID:  streamID,
		Reason:    "manual",
		Metadata: map[string]string{
			"origin": "integration-test",
		},
		Timestamp: time.Now().UTC(),
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

// syncBuffer is a goroutine-safe bytes.Buffer wrapper for capturing log output
// in tests where service goroutines write concurrently with test assertions.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (sb *syncBuffer) Write(p []byte) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Write(p)
}

func (sb *syncBuffer) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.String()
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

func waitForPlayback(t *testing.T, sub *eventbus.TypedSubscription[eventbus.AudioEgressPlaybackEvent], timeout time.Duration, predicate func(eventbus.AudioEgressPlaybackEvent) bool) eventbus.AudioEgressPlaybackEvent {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case env := <-sub.C():
			if predicate == nil || predicate(env.Payload) {
				return env.Payload
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

	if err := sttSvc.Start(napdial.ContextWithDialer(runCtx, dialer)); err != nil {
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

	finalSub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptFinal, eventbus.WithSubscriptionName("test_final_transcripts"))
	defer finalSub.Close()
	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt, eventbus.WithSubscriptionName("test_conversation_prompt"))
	defer promptSub.Close()

	finalCh := make(chan eventbus.SpeechTranscriptEvent, 1)
	pipelineCh := make(chan eventbus.PipelineMessageEvent, 1)
	promptCh := make(chan eventbus.ConversationPromptEvent, 1)

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
			case env, ok := <-finalSub.C():
				if !ok {
					return
				}
				select {
				case finalCh <- env.Payload:
				default:
				}
			}
		}
	}()

	pipelineSub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned, eventbus.WithSubscriptionName("test_pipeline_cleaned"))
	defer pipelineSub.Close()

	bridgeWG.Add(1)
	go func() {
		defer bridgeWG.Done()
		for {
			select {
			case <-bridgeCtx.Done():
				return
			case env, ok := <-pipelineSub.C():
				if !ok {
					return
				}
				select {
				case pipelineCh <- env.Payload:
				default:
				}
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
				select {
				case promptCh <- env.Payload:
				default:
				}
			}
		}
	}()

	sessionID := "grpc-session"
	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
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
		panic("unreachable")
	}
}

func waitForBargeEvent(t *testing.T, sub *eventbus.TypedSubscription[eventbus.SpeechBargeInEvent], timeout time.Duration) eventbus.SpeechBargeInEvent {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case env := <-sub.C():
			return env.Payload
		case <-timer.C:
			t.Fatalf("timeout waiting for barge event (%s)", timeout)
		}
	}
}

// publishVADUntilBarge publishes a VAD event repeatedly (with short intervals)
// until the barge coordinator produces a barge event. This replaces the fragile
// pattern of time.Sleep(100ms) + single publish, and is deterministic regardless
// of scheduling delays. The barge coordinator only fires once per cooldown
// window, so repeated VAD publishes are harmless.
func publishVADUntilBarge(t *testing.T, bus *eventbus.Bus, bargeSub *eventbus.TypedSubscription[eventbus.SpeechBargeInEvent], sessionID, streamID string, timeout time.Duration) eventbus.SpeechBargeInEvent {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	publish := func() {
		eventbus.Publish(context.Background(), bus, eventbus.Speech.VADDetected, eventbus.SourceSpeechVAD, eventbus.SpeechVADEvent{
			SessionID:  sessionID,
			StreamID:   streamID,
			Active:     true,
			Confidence: 0.8,
			Timestamp:  time.Now().UTC(),
		})
	}

	publish() // first attempt immediately
	for {
		select {
		case env := <-bargeSub.C():
			if env.Payload.SessionID == sessionID {
				return env.Payload
			}
		case <-ticker.C:
			publish()
		case <-deadline.C:
			t.Fatalf("timeout waiting for barge event via VAD publish (%s)", timeout)
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
		Metadata: maputil.Clone(chunk.Metadata),
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

// ---------------------------------------------------------------------------
// Full voice pipeline: ingress → STT → pipeline → conversation → intent router → egress
// ---------------------------------------------------------------------------

// TestFullVoicePipelineEndToEnd verifies the complete voice conversation loop:
// audio input → STT transcription → conversation context → AI intent resolution →
// TTS synthesis → audio playback output.
func TestFullVoicePipelineEndToEnd(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	// Activate mock adapters.
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapters.MockSTTAdapterID, map[string]any{
		"phrases":      []any{"hello nupi"},
		"emit_partial": true,
	}); err != nil {
		t.Fatalf("activate stt adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, map[string]any{
		"duration_ms": 200,
	}); err != nil {
		t.Fatalf("activate tts adapter: %v", err)
	}

	// AI mock: always respond with speak action containing the transcript.
	aiAdapter := &speakBackAdapter{prefix: "Response to: "}

	// Build services.
	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	intentSvc := intentrouter.NewService(bus,
		intentrouter.WithAdapter(aiAdapter),
	)
	egressSvc := egress.New(bus,
		egress.WithFactory(egress.NewAdapterFactory(store, nil)),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	services := []startStopper{
		pipelineSvc,
		ingressSvc,
		sttSvc,
		conversationSvc,
		intentSvc,
		egressSvc,
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start service %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	// Subscribe to intermediate topics to verify the chain.
	transcriptSub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptFinal, eventbus.WithSubscriptionName("test_transcript"))
	defer transcriptSub.Close()
	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt, eventbus.WithSubscriptionName("test_prompt"))
	defer promptSub.Close()
	speakSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Speak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()
	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback"))
	defer playbackSub.Close()

	transcriptCh := make(chan eventbus.SpeechTranscriptEvent, 1)
	promptCh := make(chan eventbus.ConversationPromptEvent, 1)
	speakCh := make(chan eventbus.ConversationSpeakEvent, 1)

	bridgeCtx, bridgeCancel := context.WithCancel(runCtx)
	defer bridgeCancel()

	// Forward subscription events to typed channels for easy assertion.
	go forwardEvents(bridgeCtx, transcriptSub, transcriptCh)
	go forwardEvents(bridgeCtx, promptSub, promptCh)
	go forwardEvents(bridgeCtx, speakSub, speakCh)

	// Emit session lifecycle so conversation service tracks the session.
	const sessionID = "e2e-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	// Open ingress stream and write audio.
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

	// Write two 20ms PCM segments then close to trigger final transcript.
	segment := pcmBuffer(200, 320) // 320 samples = 20ms at 16kHz
	for i := 0; i < 2; i++ {
		if err := stream.Write(segment); err != nil {
			t.Fatalf("write segment %d: %v", i+1, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// 1. Verify STT produces a final transcript.
	transcript := waitFor(t, transcriptCh, 2*time.Second)
	if !transcript.Final {
		t.Fatalf("expected final transcript, got partial")
	}

	// 2. Verify conversation engine produces a prompt.
	prompt := waitFor(t, promptCh, 2*time.Second)
	if prompt.SessionID != sessionID {
		t.Fatalf("prompt sessionID: got %q want %q", prompt.SessionID, sessionID)
	}
	if prompt.NewMessage.Text != transcript.Text {
		t.Fatalf("prompt text: got %q want %q", prompt.NewMessage.Text, transcript.Text)
	}
	if prompt.NewMessage.Origin != eventbus.OriginUser {
		t.Fatalf("prompt origin: got %q want %q", prompt.NewMessage.Origin, eventbus.OriginUser)
	}

	// 3. Verify intent router produces a speak event.
	speak := waitFor(t, speakCh, 2*time.Second)
	if speak.SessionID != sessionID {
		t.Fatalf("speak sessionID: got %q want %q", speak.SessionID, sessionID)
	}
	expectedText := "Response to: " + transcript.Text
	if speak.Text != expectedText {
		t.Fatalf("speak text: got %q want %q", speak.Text, expectedText)
	}

	// 4. Verify egress produces playback events with final chunk.
	finalPlayback := waitForPlayback(t, playbackSub, 2*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return evt.Final
	})
	if finalPlayback.SessionID != sessionID {
		t.Fatalf("playback sessionID: got %q want %q", finalPlayback.SessionID, sessionID)
	}
	if len(finalPlayback.Data) == 0 {
		t.Fatalf("expected playback data, got empty")
	}
}

// TestFullVoicePipelineNoopAction verifies that when the AI adapter returns a
// noop action, the egress service is not triggered.
func TestFullVoicePipelineNoopAction(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapters.MockSTTAdapterID, map[string]any{
		"phrases":      []any{"hello nupi"},
		"emit_partial": true,
	}); err != nil {
		t.Fatalf("activate stt: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	aiAdapter := &noopAdapter{}

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	intentSvc := intentrouter.NewService(bus,
		intentrouter.WithAdapter(aiAdapter),
	)
	egressSvc := egress.New(bus,
		egress.WithFactory(egress.NewAdapterFactory(store, nil)),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	services := []startStopper{pipelineSvc, ingressSvc, sttSvc, conversationSvc, intentSvc, egressSvc}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	// Subscribe to transcript to confirm pipeline progresses, and to speak/playback
	// to confirm they are NOT triggered.
	transcriptSub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptFinal, eventbus.WithSubscriptionName("test_transcript"))
	defer transcriptSub.Close()
	speakSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Speak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()
	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback"))
	defer playbackSub.Close()

	transcriptCh := make(chan eventbus.SpeechTranscriptEvent, 4)
	bridgeCtx, bridgeCancel := context.WithCancel(runCtx)
	defer bridgeCancel()
	go forwardEvents(bridgeCtx, transcriptSub, transcriptCh)

	// Emit session lifecycle.
	const sessionID = "noop-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	stream, err := ingressSvc.OpenStream(sessionID, "mic", eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	segment := pcmBuffer(200, 320)
	for i := 0; i < 2; i++ {
		if err := stream.Write(segment); err != nil {
			t.Fatalf("write segment %d: %v", i+1, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// Positive assertion: pipeline must progress through STT to produce a transcript.
	transcript := waitFor(t, transcriptCh, 2*time.Second)
	if transcript.SessionID != sessionID {
		t.Fatalf("transcript sessionID: got %q want %q", transcript.SessionID, sessionID)
	}

	// After confirming upstream completed, drain speak and playback for a bounded
	// window to verify neither is triggered by a noop action.
	noEventTimer := time.NewTimer(time.Second)
	defer noEventTimer.Stop()
	for {
		select {
		case env := <-speakSub.C():
			t.Fatalf("unexpected speak event: %+v", env.Payload)
		case env := <-playbackSub.C():
			t.Fatalf("unexpected playback event: %+v", env.Payload)
		case <-noEventTimer.C:
			// No speak/playback within window — expected for noop.
			return
		}
	}
}

// TestVoicePipelineEventOrdering verifies events flow through all intermediate
// topics in the correct order with consistent payload integrity.
func TestVoicePipelineEventOrdering(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapters.MockSTTAdapterID, map[string]any{
		"phrases":      []any{"hello nupi"},
		"emit_partial": true,
	}); err != nil {
		t.Fatalf("activate stt: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	aiAdapter := &speakBackAdapter{prefix: "Echo: "}

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	intentSvc := intentrouter.NewService(bus,
		intentrouter.WithAdapter(aiAdapter),
	)
	egressSvc := egress.New(bus,
		egress.WithFactory(egress.NewAdapterFactory(store, nil)),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	services := []startStopper{pipelineSvc, ingressSvc, sttSvc, conversationSvc, intentSvc, egressSvc}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	// Track ordering via envelope timestamps set at publish time (bus.go:125).
	// This reflects actual event ordering rather than goroutine observation order.
	type seqEvent struct {
		name string
		ts   time.Time
	}
	var mu sync.Mutex
	var events []seqEvent

	record := func(name string, ts time.Time) {
		mu.Lock()
		events = append(events, seqEvent{name: name, ts: ts})
		mu.Unlock()
	}

	segmentSub := eventbus.SubscribeTo(bus, eventbus.Audio.IngressSegment, eventbus.WithSubscriptionName("test_segment"))
	defer segmentSub.Close()
	transcriptSub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptFinal, eventbus.WithSubscriptionName("test_transcript"))
	defer transcriptSub.Close()
	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt, eventbus.WithSubscriptionName("test_prompt"))
	defer promptSub.Close()
	speakSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Speak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()
	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback"))
	defer playbackSub.Close()

	bridgeCtx, bridgeCancel := context.WithCancel(runCtx)
	defer bridgeCancel()

	// Monitor segment topic.
	segmentCh := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-bridgeCtx.Done():
				return
			case env, ok := <-segmentSub.C():
				if !ok {
					return
				}
				record("segment", env.Timestamp)
				select {
				case segmentCh <- struct{}{}:
				default:
				}
			}
		}
	}()

	// Monitor transcript topic.
	transcriptCh := make(chan eventbus.SpeechTranscriptEvent, 1)
	go func() {
		for {
			select {
			case <-bridgeCtx.Done():
				return
			case env, ok := <-transcriptSub.C():
				if !ok {
					return
				}
				if env.Payload.Final {
					record("transcript", env.Timestamp)
					select {
					case transcriptCh <- env.Payload:
					default:
					}
				}
			}
		}
	}()

	// Monitor prompt topic.
	promptCh := make(chan eventbus.ConversationPromptEvent, 1)
	go func() {
		for {
			select {
			case <-bridgeCtx.Done():
				return
			case env, ok := <-promptSub.C():
				if !ok {
					return
				}
				record("prompt", env.Timestamp)
				select {
				case promptCh <- env.Payload:
				default:
				}
			}
		}
	}()

	// Monitor speak topic.
	speakCh := make(chan eventbus.ConversationSpeakEvent, 1)
	go func() {
		for {
			select {
			case <-bridgeCtx.Done():
				return
			case env, ok := <-speakSub.C():
				if !ok {
					return
				}
				record("speak", env.Timestamp)
				select {
				case speakCh <- env.Payload:
				default:
				}
			}
		}
	}()

	// Monitor playback topic.
	playbackCh := make(chan eventbus.AudioEgressPlaybackEvent, 1)
	go func() {
		for {
			select {
			case <-bridgeCtx.Done():
				return
			case env, ok := <-playbackSub.C():
				if !ok {
					return
				}
				if env.Payload.Final {
					record("playback", env.Timestamp)
					select {
					case playbackCh <- env.Payload:
					default:
					}
				}
			}
		}
	}()

	const sessionID = "ordering-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	stream, err := ingressSvc.OpenStream(sessionID, "mic", eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	segment := pcmBuffer(200, 320)
	for i := 0; i < 2; i++ {
		if err := stream.Write(segment); err != nil {
			t.Fatalf("write segment %d: %v", i+1, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// Wait for each stage.
	waitFor(t, segmentCh, 2*time.Second)
	transcript := waitFor(t, transcriptCh, 2*time.Second)
	prompt := waitFor(t, promptCh, 2*time.Second)
	speak := waitFor(t, speakCh, 2*time.Second)
	playback := waitFor(t, playbackCh, 3*time.Second)

	// Verify sessionID propagation through the chain.
	if prompt.SessionID != sessionID {
		t.Errorf("prompt sessionID: got %q want %q", prompt.SessionID, sessionID)
	}
	if speak.SessionID != sessionID {
		t.Errorf("speak sessionID: got %q want %q", speak.SessionID, sessionID)
	}
	if playback.SessionID != sessionID {
		t.Errorf("playback sessionID: got %q want %q", playback.SessionID, sessionID)
	}

	// Verify text propagation.
	if speak.Text != "Echo: "+transcript.Text {
		t.Errorf("speak text: got %q want %q", speak.Text, "Echo: "+transcript.Text)
	}

	// Stop monitor goroutines before inspecting the shared events slice.
	bridgeCancel()

	// Verify ordering using envelope timestamps assigned at publish time (bus.go:125).
	// This reflects actual event publish order, not goroutine observation order.
	mu.Lock()
	ordered := make([]seqEvent, len(events))
	copy(ordered, events)
	mu.Unlock()

	firstTS := func(name string) time.Time {
		for _, e := range ordered {
			if e.name == name {
				return e.ts
			}
		}
		return time.Time{}
	}

	segTS := firstTS("segment")
	trTS := firstTS("transcript")
	promptTS := firstTS("prompt")
	speakTS := firstTS("speak")
	playTS := firstTS("playback")

	if segTS.IsZero() || trTS.IsZero() || promptTS.IsZero() || speakTS.IsZero() || playTS.IsZero() {
		names := make([]string, len(ordered))
		for i, e := range ordered {
			names[i] = fmt.Sprintf("%s(%s)", e.name, e.ts.Format(time.RFC3339Nano))
		}
		t.Fatalf("missing events: segment=%v transcript=%v prompt=%v speak=%v playback=%v (all=%v)",
			segTS, trTS, promptTS, speakTS, playTS, names)
	}

	if !segTS.Before(trTS) {
		t.Errorf("segment (%s) should precede transcript (%s)", segTS.Format(time.RFC3339Nano), trTS.Format(time.RFC3339Nano))
	}
	if !trTS.Before(promptTS) {
		t.Errorf("transcript (%s) should precede prompt (%s)", trTS.Format(time.RFC3339Nano), promptTS.Format(time.RFC3339Nano))
	}
	if !promptTS.Before(speakTS) {
		t.Errorf("prompt (%s) should precede speak (%s)", promptTS.Format(time.RFC3339Nano), speakTS.Format(time.RFC3339Nano))
	}
	if !speakTS.Before(playTS) {
		t.Errorf("speak (%s) should precede playback (%s)", speakTS.Format(time.RFC3339Nano), playTS.Format(time.RFC3339Nano))
	}
}

// ---------------------------------------------------------------------------
// Mock AI adapters for integration tests
// ---------------------------------------------------------------------------

// speakBackAdapter is an IntentAdapter that always responds with a speak action
// containing the transcript prefixed with the given string.
type speakBackAdapter struct {
	prefix string
}

func (a *speakBackAdapter) ResolveIntent(_ context.Context, req intentrouter.IntentRequest) (*intentrouter.IntentResponse, error) {
	return &intentrouter.IntentResponse{
		PromptID: req.PromptID,
		Actions: []intentrouter.IntentAction{
			{
				Type: intentrouter.ActionSpeak,
				Text: a.prefix + req.Transcript,
			},
		},
		Confidence: 0.95,
	}, nil
}
func (a *speakBackAdapter) Name() string { return "speak-back-test" }
func (a *speakBackAdapter) Ready() bool  { return true }

// noopAdapter always returns a noop action.
type noopAdapter struct{}

func (a *noopAdapter) ResolveIntent(_ context.Context, req intentrouter.IntentRequest) (*intentrouter.IntentResponse, error) {
	return &intentrouter.IntentResponse{
		PromptID: req.PromptID,
		Actions: []intentrouter.IntentAction{
			{Type: intentrouter.ActionNoop},
		},
	}, nil
}
func (a *noopAdapter) Name() string { return "noop-test" }
func (a *noopAdapter) Ready() bool  { return true }

// forwardEvents reads envelopes from a subscription and sends typed payloads to
// the destination channel. T must match the payload type.
func forwardEvents[T any](ctx context.Context, sub *eventbus.TypedSubscription[T], dst chan<- T) {
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			select {
			case dst <- env.Payload:
			default:
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Existing test infrastructure
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// gRPC NAP adapter integration: STT + TTS via bufconn
// ---------------------------------------------------------------------------

// TestGRPCSTTAndTTSNAPPipeline validates the voice pipeline using real gRPC
// NAP adapters for both STT and TTS (via bufconn, no network).
func TestGRPCSTTAndTTSNAPPipeline(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	// --- STT gRPC adapter via bufconn ---
	const sttAdapterID = "adapter.stt.grpc.e2e"
	if err := store.UpsertAdapter(ctx, configstore.Adapter{
		ID: sttAdapterID, Source: "local", Type: "stt", Name: "E2E gRPC STT", Version: "dev",
	}); err != nil {
		t.Fatalf("upsert stt adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), sttAdapterID, nil); err != nil {
		t.Fatalf("activate stt: %v", err)
	}

	const bufSize = 1024 * 1024
	sttLis := bufconn.Listen(bufSize)
	sttServer := grpc.NewServer()
	napv1.RegisterSpeechToTextServiceServer(sttServer, &grpcTestSTTServer{})
	go func() {
		if err := sttServer.Serve(sttLis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Logf("stt grpc server stopped: %v", err)
		}
	}()
	t.Cleanup(func() {
		sttServer.GracefulStop()
		_ = sttLis.Close()
	})
	sttDialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return sttLis.DialContext(ctx)
	}

	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: sttAdapterID, Transport: "grpc", Address: "bufconn-stt",
	}); err != nil {
		t.Fatalf("register stt endpoint: %v", err)
	}

	// --- TTS gRPC adapter via bufconn ---
	const ttsAdapterID = "adapter.tts.grpc.e2e"
	if err := store.UpsertAdapter(ctx, configstore.Adapter{
		ID: ttsAdapterID, Source: "local", Type: "tts", Name: "E2E gRPC TTS", Version: "dev",
	}); err != nil {
		t.Fatalf("upsert tts adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), ttsAdapterID, nil); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	ttsLis := bufconn.Listen(bufSize)
	ttsServer := grpc.NewServer()
	napv1.RegisterTextToSpeechServiceServer(ttsServer, &grpcTestTTSServer{})
	go func() {
		if err := ttsServer.Serve(ttsLis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Logf("tts grpc server stopped: %v", err)
		}
	}()
	t.Cleanup(func() {
		ttsServer.GracefulStop()
		_ = ttsLis.Close()
	})
	ttsDialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return ttsLis.DialContext(ctx)
	}

	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: ttsAdapterID, Transport: "grpc", Address: "bufconn-tts",
	}); err != nil {
		t.Fatalf("register tts endpoint: %v", err)
	}

	// --- Build services ---
	aiAdapter := &speakBackAdapter{prefix: "NAP: "}

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	intentSvc := intentrouter.NewService(bus,
		intentrouter.WithAdapter(aiAdapter),
	)
	egressSvc := egress.New(bus,
		egress.WithFactory(egress.NewAdapterFactory(store, nil)),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Start services — inject dialers for STT and TTS via context.
	if err := ingressSvc.Start(runCtx); err != nil {
		t.Fatalf("start ingress: %v", err)
	}
	defer ingressSvc.Shutdown(context.Background())

	if err := sttSvc.Start(napdial.ContextWithDialer(runCtx, sttDialer)); err != nil {
		t.Fatalf("start stt: %v", err)
	}
	defer sttSvc.Shutdown(context.Background())

	if err := pipelineSvc.Start(runCtx); err != nil {
		t.Fatalf("start pipeline: %v", err)
	}
	defer pipelineSvc.Shutdown(context.Background())

	if err := conversationSvc.Start(runCtx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer conversationSvc.Shutdown(context.Background())

	if err := intentSvc.Start(runCtx); err != nil {
		t.Fatalf("start intent router: %v", err)
	}
	defer intentSvc.Shutdown(context.Background())

	// Egress needs TTS dialer injected.
	if err := egressSvc.Start(napdial.ContextWithDialer(runCtx, ttsDialer)); err != nil {
		t.Fatalf("start egress: %v", err)
	}
	defer egressSvc.Shutdown(context.Background())

	// --- Subscribe to output topics ---
	transcriptSub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptFinal, eventbus.WithSubscriptionName("test_transcript"))
	defer transcriptSub.Close()
	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback"))
	defer playbackSub.Close()

	transcriptCh := make(chan eventbus.SpeechTranscriptEvent, 1)
	bridgeCtx, bridgeCancel := context.WithCancel(runCtx)
	defer bridgeCancel()
	go forwardEvents(bridgeCtx, transcriptSub, transcriptCh)

	// --- Emit session lifecycle ---
	const sessionID = "grpc-e2e-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	// --- Send audio through ingress ---
	stream, err := ingressSvc.OpenStream(sessionID, "mic", eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	segment := make([]byte, 640) // 320 samples * 2 bytes
	for i := 0; i < 2; i++ {
		if err := stream.Write(segment); err != nil {
			t.Fatalf("write segment %d: %v", i+1, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// --- Verify STT produces transcript via gRPC NAP ---
	transcript := waitFor(t, transcriptCh, 2*time.Second)
	if !transcript.Final {
		t.Fatalf("expected final transcript")
	}

	// --- Verify TTS progressive streaming via gRPC NAP (AC#3) ---
	// The gRPC TTS server sends 2 chunks (non-final + final). Receiving a
	// non-final playback event before the final one proves progressive streaming:
	// audio starts before the full TTS response is generated.
	nonFinalPlayback := waitForPlayback(t, playbackSub, 3*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return !evt.Final && len(evt.Data) > 0
	})
	if nonFinalPlayback.SessionID != sessionID {
		t.Fatalf("non-final playback sessionID: got %q want %q", nonFinalPlayback.SessionID, sessionID)
	}

	finalPlayback := waitForPlayback(t, playbackSub, 3*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return evt.Final
	})
	if finalPlayback.SessionID != sessionID {
		t.Fatalf("final playback sessionID: got %q want %q", finalPlayback.SessionID, sessionID)
	}
	if len(finalPlayback.Data) == 0 {
		t.Fatalf("expected playback audio data from gRPC TTS server")
	}
}

// grpcTestTTSServer is a mock TTS gRPC server that returns PCM audio chunks.
type grpcTestTTSServer struct {
	napv1.UnimplementedTextToSpeechServiceServer
}

func (s *grpcTestTTSServer) StreamSynthesis(req *napv1.StreamSynthesisRequest, stream napv1.TextToSpeechService_StreamSynthesisServer) error {
	if req.GetText() == "" {
		return fmt.Errorf("grpcTestTTSServer: empty text in synthesis request")
	}
	// Generate two chunks of PCM audio, then close.
	for i := 0; i < 2; i++ {
		data := make([]byte, 640) // 320 samples * 2 bytes = 20ms at 16kHz
		for j := range data {
			data[j] = byte(i + 1) // non-zero fill to verify data propagation
		}

		resp := &napv1.SynthesisResponse{
			Status: napv1.SynthesisStatus_SYNTHESIS_STATUS_PLAYING,
			Chunk: &napv1.AudioChunk{
				Data:       data,
				DurationMs: 20,
				Last:       i == 1,
			},
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Task 4: Error paths and graceful degradation
// ---------------------------------------------------------------------------

// failingSynthesizer returns an error on every Speak call, simulating a TTS
// adapter that is broken (e.g. connection lost, gRPC stream error).
type failingSynthesizer struct{}

func (s *failingSynthesizer) Speak(_ context.Context, _ egress.SpeakRequest) ([]egress.SynthesisChunk, error) {
	return nil, fmt.Errorf("tts adapter error: connection lost")
}
func (s *failingSynthesizer) Close(_ context.Context) ([]egress.SynthesisChunk, error) {
	return nil, nil
}

// TestVoicePipelineTTSAdapterError verifies that when the TTS synthesizer
// returns an error on Speak(), the pipeline does not crash and upstream
// services (STT, conversation, intent router) continue operating normally.
func TestVoicePipelineTTSAdapterError(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapters.MockSTTAdapterID, map[string]any{
		"phrases":      []any{"hello nupi"},
		"emit_partial": true,
	}); err != nil {
		t.Fatalf("activate stt: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	// AI adapter that always speaks.
	aiAdapter := &speakBackAdapter{prefix: "Fail test: "}

	// TTS factory that returns a synthesizer which errors on every Speak() call,
	// simulating a broken TTS adapter (connection refused, gRPC stream error).
	ttsFactory := egress.FactoryFunc(func(_ context.Context, _ egress.SessionParams) (egress.Synthesizer, error) {
		return &failingSynthesizer{}, nil
	})

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	intentSvc := intentrouter.NewService(bus, intentrouter.WithAdapter(aiAdapter))
	egressSvc := egress.New(bus,
		egress.WithFactory(ttsFactory),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	services := []startStopper{pipelineSvc, ingressSvc, sttSvc, conversationSvc, intentSvc, egressSvc}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	// Subscribe to speak to confirm intent router still works.
	speakSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Speak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()
	speakCh := make(chan eventbus.ConversationSpeakEvent, 4)

	bridgeCtx, bridgeCancel := context.WithCancel(runCtx)
	defer bridgeCancel()
	go forwardEvents(bridgeCtx, speakSub, speakCh)

	// Subscribe to playback — we expect NO final playback since synthesizer errors.
	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback"))
	defer playbackSub.Close()

	const sessionID = "tts-error-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	stream, err := ingressSvc.OpenStream(sessionID, "mic", eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	segment := pcmBuffer(200, 320)
	for i := 0; i < 2; i++ {
		if err := stream.Write(segment); err != nil {
			t.Fatalf("write segment %d: %v", i+1, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// Intent router should still produce speak events even when TTS fails.
	speak := waitFor(t, speakCh, 2*time.Second)
	if speak.SessionID != sessionID {
		t.Fatalf("speak sessionID: got %q want %q", speak.SessionID, sessionID)
	}

	// After confirming upstream completed (speak event arrived), drain playback
	// for a bounded window. Since Speak() always errors, no playback should appear.
	noPlayback := time.NewTimer(time.Second)
	defer noPlayback.Stop()
	for {
		select {
		case evt := <-playbackSub.C():
			t.Fatalf("unexpected playback event after TTS error: final=%v len=%d",
				evt.Payload.Final, len(evt.Payload.Data))
		case <-noPlayback.C:
			// No playback within window — expected when TTS adapter errors.
			return
		}
	}
}

// TestVoicePipelineGoroutineLifecycle verifies that after a full pipeline
// lifecycle (start → process audio → shutdown), no goroutines are leaked
// from the voice services. Uses deterministic service metrics instead of
// global goroutine counts which are sensitive to unrelated runtime goroutines.
func TestVoicePipelineGoroutineLifecycle(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapters.MockSTTAdapterID, map[string]any{
		"phrases":      []any{"hello nupi"},
		"emit_partial": true,
	}); err != nil {
		t.Fatalf("activate stt: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	aiAdapter := &speakBackAdapter{prefix: "Goroutine test: "}

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	intentSvc := intentrouter.NewService(bus, intentrouter.WithAdapter(aiAdapter))
	egressSvc := egress.New(bus,
		egress.WithFactory(egress.NewAdapterFactory(store, nil)),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	services := []startStopper{pipelineSvc, ingressSvc, sttSvc, conversationSvc, intentSvc, egressSvc}

	runCtx, cancel := context.WithCancel(ctx)

	var shutdownOnce sync.Once
	teardown := func() {
		cancel()
		for i := len(services) - 1; i >= 0; i-- {
			services[i].Shutdown(context.Background())
		}
	}
	t.Cleanup(func() { shutdownOnce.Do(teardown) })

	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start %T: %v", svc, err)
		}
	}

	// Process a full audio loop.
	const sessionID = "goroutine-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback"))
	defer playbackSub.Close()

	stream, err := ingressSvc.OpenStream(sessionID, "mic", eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	segment := pcmBuffer(200, 320)
	for i := 0; i < 2; i++ {
		if err := stream.Write(segment); err != nil {
			t.Fatalf("write segment %d: %v", i+1, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// Wait for playback to confirm pipeline processed fully.
	waitForPlayback(t, playbackSub, 3*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return evt.Final
	})
	playbackSub.Close()

	// Shut down all services and verify deterministic metrics.
	shutdownOnce.Do(teardown)

	// Verify egress has no active streams — deterministic, not flaky.
	if count := egressSvc.ActiveStreamCount(); count != 0 {
		t.Errorf("egress active streams after shutdown: %d, want 0", count)
	}
}

// TestVoicePipelinePayloadIntegrity verifies that sessionID, streamID, and
// metadata propagate correctly through every hop of the event bus chain.
func TestVoicePipelinePayloadIntegrity(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapters.MockSTTAdapterID, map[string]any{
		"phrases":      []any{"hello nupi"},
		"emit_partial": true,
	}); err != nil {
		t.Fatalf("activate stt: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	aiAdapter := &speakBackAdapter{prefix: "Integrity: "}

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	intentSvc := intentrouter.NewService(bus, intentrouter.WithAdapter(aiAdapter))
	egressSvc := egress.New(bus,
		egress.WithFactory(egress.NewAdapterFactory(store, nil)),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	services := []startStopper{pipelineSvc, ingressSvc, sttSvc, conversationSvc, intentSvc, egressSvc}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	// Subscribe to all intermediate topics.
	segmentSub := eventbus.SubscribeTo(bus, eventbus.Audio.IngressSegment, eventbus.WithSubscriptionName("test_seg"))
	defer segmentSub.Close()
	transcriptSub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptFinal, eventbus.WithSubscriptionName("test_tr"))
	defer transcriptSub.Close()
	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt, eventbus.WithSubscriptionName("test_pr"))
	defer promptSub.Close()
	speakSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Speak, eventbus.WithSubscriptionName("test_sp"))
	defer speakSub.Close()
	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_pb"))
	defer playbackSub.Close()

	segmentCh := make(chan eventbus.AudioIngressSegmentEvent, 8)
	transcriptCh := make(chan eventbus.SpeechTranscriptEvent, 4)
	promptCh := make(chan eventbus.ConversationPromptEvent, 4)
	speakCh := make(chan eventbus.ConversationSpeakEvent, 4)

	bridgeCtx, bridgeCancel := context.WithCancel(runCtx)
	defer bridgeCancel()
	go forwardEvents(bridgeCtx, segmentSub, segmentCh)
	go forwardEvents(bridgeCtx, transcriptSub, transcriptCh)
	go forwardEvents(bridgeCtx, promptSub, promptCh)
	go forwardEvents(bridgeCtx, speakSub, speakCh)

	const sessionID = "integrity-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	stream, err := ingressSvc.OpenStream(sessionID, "mic", eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}, map[string]string{"locale": "en-US"})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	segment := pcmBuffer(200, 320)
	for i := 0; i < 2; i++ {
		if err := stream.Write(segment); err != nil {
			t.Fatalf("write segment %d: %v", i+1, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// Verify sessionID propagation at each hop.
	seg := waitFor(t, segmentCh, 2*time.Second)
	if seg.SessionID != sessionID {
		t.Errorf("segment sessionID: got %q want %q", seg.SessionID, sessionID)
	}
	if seg.StreamID != "mic" {
		t.Errorf("segment streamID: got %q want %q", seg.StreamID, "mic")
	}

	tr := waitFor(t, transcriptCh, 2*time.Second)
	if tr.SessionID != sessionID {
		t.Errorf("transcript sessionID: got %q want %q", tr.SessionID, sessionID)
	}
	if tr.StreamID != "mic" {
		t.Errorf("transcript streamID: got %q want %q", tr.StreamID, "mic")
	}

	pr := waitFor(t, promptCh, 2*time.Second)
	if pr.SessionID != sessionID {
		t.Errorf("prompt sessionID: got %q want %q", pr.SessionID, sessionID)
	}

	sp := waitFor(t, speakCh, 2*time.Second)
	if sp.SessionID != sessionID {
		t.Errorf("speak sessionID: got %q want %q", sp.SessionID, sessionID)
	}
	if sp.Text == "" {
		t.Error("speak text should not be empty")
	}

	pb := waitForPlayback(t, playbackSub, 3*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return evt.Final
	})
	if pb.SessionID != sessionID {
		t.Errorf("playback sessionID: got %q want %q", pb.SessionID, sessionID)
	}
	if len(pb.Data) == 0 {
		t.Error("playback data should not be empty")
	}
	// Mock synthesizer produces 500 ms of 16 kHz mono PCM16 = 16000 bytes.
	const wantBytes = 16000
	if len(pb.Data) != wantBytes {
		t.Errorf("playback data length: got %d want %d", len(pb.Data), wantBytes)
	}

	// Verify text flows from transcript through speak.
	if sp.Text != "Integrity: "+tr.Text {
		t.Errorf("text propagation: speak=%q expected prefix+transcript=%q",
			sp.Text, "Integrity: "+tr.Text)
	}
}

// ---------------------------------------------------------------------------
// Barge-in with real TTS streaming
// ---------------------------------------------------------------------------

// multiChunkSynthesizer produces N chunks from a single Speak() call. Combined
// with the existing streamingSynth wrapper, it enables realistic multi-chunk
// streaming: Speak() returns the first chunk (head), Close() returns the
// remaining chunks (tail). The egress publishes the head chunk, then on
// barge-in interrupt calls closeSynthesizer() → Close() → tail chunks are
// decorated with barge metadata.
//
// chunkDelay adds a delay between chunk generation in Speak(). With the
// streamingSynth wrapper this only affects total Speak() latency (all chunks
// are generated up front), but the delay between *published* events is
// determined by the egress's post-Speak publish loop.
type multiChunkSynthesizer struct {
	chunkCount int
	chunkDelay time.Duration
	chunkSize  int           // bytes per chunk (640 = 320 samples = 20ms at 16kHz)
	closeDelay time.Duration // optional: delay Close() to keep stream alive during teardown
}

func (s *multiChunkSynthesizer) makeChunk(index int, final bool) egress.SynthesisChunk {
	data := make([]byte, s.chunkSize)
	for j := range data {
		data[j] = byte(index + 1)
	}
	return egress.SynthesisChunk{
		Data:     data,
		Duration: 20 * time.Millisecond,
		Final:    final,
	}
}

func (s *multiChunkSynthesizer) Speak(ctx context.Context, _ egress.SpeakRequest) ([]egress.SynthesisChunk, error) {
	var chunks []egress.SynthesisChunk
	for i := 0; i < s.chunkCount; i++ {
		chunks = append(chunks, s.makeChunk(i, i == s.chunkCount-1))
		if i < s.chunkCount-1 && s.chunkDelay > 0 {
			select {
			case <-ctx.Done():
				return chunks, ctx.Err()
			case <-time.After(s.chunkDelay):
			}
		}
	}
	return chunks, nil
}

func (s *multiChunkSynthesizer) Close(ctx context.Context) ([]egress.SynthesisChunk, error) {
	if s.closeDelay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(s.closeDelay):
		}
	}
	return nil, nil
}

// quietPeriodSynth changes behavior based on Speak() call count. First N calls
// return single-chunk Final=true (playback completes → triggers quiet period).
// Subsequent calls return a single non-final chunk (keeps playing=true so VAD
// can trigger barge). Used by TestVoicePipelineBargeInQuietPeriod to prove
// that quiet period specifically blocks Phase 1 VAD, not just absent playback.
type quietPeriodSynth struct {
	finalUntilCall int32 // calls <= this return Final=true
	calls          int32
}

func (s *quietPeriodSynth) Speak(_ context.Context, _ egress.SpeakRequest) ([]egress.SynthesisChunk, error) {
	n := atomic.AddInt32(&s.calls, 1)
	chunk := egress.SynthesisChunk{
		Data:     make([]byte, 640),
		Duration: 20 * time.Millisecond,
		Final:    n <= s.finalUntilCall,
	}
	return []egress.SynthesisChunk{chunk}, nil
}

func (s *quietPeriodSynth) Close(_ context.Context) ([]egress.SynthesisChunk, error) {
	return nil, nil
}

// TestVoicePipelineBargeInDuringTTSStreaming validates that when the barge
// coordinator publishes a speech.barge_in event (triggered by VAD), the egress
// service cancels the active TTS stream and the final playback event carries
// barge_in metadata. The TTS mock streams multiple chunks with delays so the
// barge interrupt can fire mid-synthesis.
func TestVoicePipelineBargeInDuringTTSStreaming(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	// Activate mock STT and VAD adapters.
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapters.MockSTTAdapterID, map[string]any{
		"phrases":      []any{"interrupt me"},
		"emit_partial": true,
	}); err != nil {
		t.Fatalf("activate stt: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.2,
		"min_frames": 1,
	}); err != nil {
		t.Fatalf("activate vad: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	// multiChunkSynthesizer produces 10 chunks. Wrapped by streamingSynth,
	// Speak() returns 1 head chunk immediately (published by egress — makes
	// barge coordinator see active playback). Close() returns the remaining 9
	// tail chunks when the stream is torn down. On barge-in, closeSynthesizer()
	// calls Close() and decorates tail chunks with barge metadata.
	ttsFactory := egress.FactoryFunc(func(_ context.Context, _ egress.SessionParams) (egress.Synthesizer, error) {
		inner := &multiChunkSynthesizer{
			chunkCount: 10,
			chunkDelay: 0, // No delay needed — streamingSynth splits head/tail
			chunkSize:  640,
		}
		return newStreamingSynth(inner), nil
	})

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	vadSvc := vad.New(bus,
		vad.WithFactory(vad.NewAdapterFactory(store, nil)),
		vad.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	bargeSvc := barge.New(bus,
		barge.WithConfidenceThreshold(0.1),
		barge.WithCooldown(100*time.Millisecond),
		barge.WithQuietPeriod(0), // Disable quiet period for this test.
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	intentSvc := intentrouter.NewService(bus,
		intentrouter.WithAdapter(&speakBackAdapter{prefix: "Barge test: "}),
	)
	egressSvc := egress.New(bus,
		egress.WithFactory(ttsFactory),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	services := []startStopper{
		pipelineSvc, ingressSvc, sttSvc, vadSvc, bargeSvc, conversationSvc, intentSvc, egressSvc,
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start service %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback"))
	defer playbackSub.Close()
	bargeSub := eventbus.SubscribeTo(bus, eventbus.Speech.BargeIn, eventbus.WithSubscriptionName("test_barge"))
	defer bargeSub.Close()

	// Emit session lifecycle so services track the session.
	const sessionID = "barge-streaming-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	format := eventbus.AudioFormat{
		Encoding:   eventbus.AudioEncodingPCM16,
		SampleRate: 16000,
		Channels:   1,
		BitDepth:   16,
	}

	// Feed audio through ingress to trigger the STT → AI → TTS pipeline.
	stream, err := ingressSvc.OpenStream(sessionID, "mic", format, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	segment := pcmBuffer(200, 320)
	for i := 0; i < 3; i++ {
		if err := stream.Write(segment); err != nil {
			t.Fatalf("write segment %d: %v", i, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// Publish VAD events until the barge coordinator fires. This retries
	// deterministically: the first few VADs may be rejected with "no active
	// playback" until the head chunk is published and processed. No Sleep.
	bargeEvt := publishVADUntilBarge(t, bus, bargeSub, sessionID, "mic", 5*time.Second)
	if bargeEvt.SessionID != sessionID {
		t.Errorf("barge sessionID: got %q want %q", bargeEvt.SessionID, sessionID)
	}
	if bargeEvt.Reason != "vad_detected" {
		t.Errorf("barge reason: got %q want %q", bargeEvt.Reason, "vad_detected")
	}

	// Collect remaining playback events. After barge-in, closeSynthesizer()
	// calls Close() on streamingSynth which returns the 9 tail chunks. These
	// are all decorated with barge metadata. The stream is cut short because
	// each tail chunk gets barge_in=true.
	var bargeChunks int
	finalPlayback := waitForPlayback(t, playbackSub, 3*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		if evt.Metadata["barge_in"] == "true" {
			bargeChunks++
		}
		return evt.Final && evt.Metadata["barge_in"] == "true"
	})
	if finalPlayback.Metadata["barge_in"] != "true" {
		t.Errorf("expected barge_in=true on final playback, got metadata: %v", finalPlayback.Metadata)
	}
	if finalPlayback.Metadata["barge_in_reason"] != "vad_detected" {
		t.Errorf("expected barge_in_reason=vad_detected, got: %q", finalPlayback.Metadata["barge_in_reason"])
	}
	if bargeChunks == 0 {
		t.Errorf("expected at least one barge-decorated chunk from tail, got 0")
	}
	t.Logf("barge-decorated chunks: %d (tail chunks from interrupted stream)", bargeChunks)
}

// TestVoicePipelineBargeInRecovery validates that after a barge-in interrupt,
// new audio input flows through the full pipeline (STT → conversation →
// intent router → egress → playback) and the resulting playback does NOT
// carry barge_in metadata.
func TestVoicePipelineBargeInRecovery(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapters.MockSTTAdapterID, map[string]any{
		"phrases":      []any{"interrupt me", "second input"},
		"emit_partial": true,
	}); err != nil {
		t.Fatalf("activate stt: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.2,
		"min_frames": 1,
	}); err != nil {
		t.Fatalf("activate vad: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	const recoveryCooldown = 100 * time.Millisecond

	ttsFactory := egress.FactoryFunc(func(_ context.Context, _ egress.SessionParams) (egress.Synthesizer, error) {
		return newStreamingSynth(&multiChunkSynthesizer{
			chunkCount: 10, chunkSize: 640,
		}), nil
	})

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	vadSvc := vad.New(bus,
		vad.WithFactory(vad.NewAdapterFactory(store, nil)),
		vad.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	bargeSvc := barge.New(bus,
		barge.WithConfidenceThreshold(0.1),
		barge.WithCooldown(recoveryCooldown),
		barge.WithQuietPeriod(0),
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	intentSvc := intentrouter.NewService(bus,
		intentrouter.WithAdapter(&speakBackAdapter{prefix: "Recovery: "}),
	)
	egressSvc := egress.New(bus,
		egress.WithFactory(ttsFactory),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	services := []startStopper{
		pipelineSvc, ingressSvc, sttSvc, vadSvc, bargeSvc, conversationSvc, intentSvc, egressSvc,
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start service %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback"))
	defer playbackSub.Close()
	bargeSub := eventbus.SubscribeTo(bus, eventbus.Speech.BargeIn, eventbus.WithSubscriptionName("test_barge"))
	defer bargeSub.Close()

	const sessionID = "barge-recovery-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	format := eventbus.AudioFormat{
		Encoding:   eventbus.AudioEncodingPCM16,
		SampleRate: 16000,
		Channels:   1,
		BitDepth:   16,
	}

	// --- Phase 1: Start TTS streaming, then barge-in ---
	stream1, err := ingressSvc.OpenStream(sessionID, "mic", format, nil)
	if err != nil {
		t.Fatalf("open stream 1: %v", err)
	}
	segment := pcmBuffer(200, 320)
	for i := 0; i < 3; i++ {
		if err := stream1.Write(segment); err != nil {
			t.Fatalf("write segment %d: %v", i, err)
		}
	}
	if err := stream1.Close(); err != nil {
		t.Fatalf("close stream 1: %v", err)
	}

	// Trigger barge-in — retries until barge coordinator sees active playback.
	bargeEvt := publishVADUntilBarge(t, bus, bargeSub, sessionID, "mic", 5*time.Second)

	// Wait for final playback with barge metadata.
	waitForPlayback(t, playbackSub, 3*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return evt.Final && evt.Metadata["barge_in"] == "true"
	})

	// --- Phase 2: Feed new audio — verify full pipeline processes it ---

	// Wait for cooldown to elapse based on the actual barge-in timestamp,
	// not a fixed sleep. This adapts to actual timing rather than assuming
	// a fixed duration that could be flaky on slow CI.
	if remaining := time.Until(bargeEvt.Timestamp.Add(recoveryCooldown)); remaining > 0 {
		time.Sleep(remaining)
	}

	stream2, err := ingressSvc.OpenStream(sessionID, "mic2", format, nil)
	if err != nil {
		t.Fatalf("open stream 2: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := stream2.Write(segment); err != nil {
			t.Fatalf("write segment 2-%d: %v", i, err)
		}
	}
	if err := stream2.Close(); err != nil {
		t.Fatalf("close stream 2: %v", err)
	}

	// Wait for a new (non-barge) playback from the second input.
	secondPlayback := waitForPlayback(t, playbackSub, 5*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return len(evt.Data) > 0 && evt.Metadata["barge_in"] != "true"
	})
	if secondPlayback.SessionID != sessionID {
		t.Errorf("second playback sessionID: got %q want %q", secondPlayback.SessionID, sessionID)
	}
	if secondPlayback.Metadata["barge_in"] == "true" {
		t.Errorf("second playback should NOT carry barge_in metadata, got: %v", secondPlayback.Metadata)
	}
	t.Log("barge-in recovery: second input processed successfully")
}

// TestVoicePipelineBargeInCooldown validates that rapid VAD detections within
// the cooldown window are suppressed — only one barge event is published.
func TestVoicePipelineBargeInCooldown(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapters.MockSTTAdapterID, map[string]any{
		"phrases":      []any{"cooldown test"},
		"emit_partial": true,
	}); err != nil {
		t.Fatalf("activate stt: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.2,
		"min_frames": 1,
	}); err != nil {
		t.Fatalf("activate vad: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	// closeDelay keeps the stream alive during teardown so the barge
	// coordinator still sees playing=true when the second VAD arrives.
	// Without this, closeSynthesizer() finishes instantly, publishes the
	// final playback event, and the coordinator rejects the second VAD
	// via "no active playback" instead of cooldown — masking regressions.
	const cooldown = 200 * time.Millisecond
	ttsFactory := egress.FactoryFunc(func(_ context.Context, _ egress.SessionParams) (egress.Synthesizer, error) {
		return newStreamingSynth(&multiChunkSynthesizer{
			chunkCount: 10,
			chunkDelay: 100 * time.Millisecond,
			chunkSize:  640,
			closeDelay: 2 * cooldown, // stream stays alive well past cooldown
		}), nil
	})

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	vadSvc := vad.New(bus,
		vad.WithFactory(vad.NewAdapterFactory(store, nil)),
		vad.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	bargeSvc := barge.New(bus,
		barge.WithConfidenceThreshold(0.1),
		barge.WithCooldown(cooldown),
		barge.WithQuietPeriod(0),
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	intentSvc := intentrouter.NewService(bus,
		intentrouter.WithAdapter(&speakBackAdapter{prefix: "Cooldown: "}),
	)
	egressSvc := egress.New(bus,
		egress.WithFactory(ttsFactory),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	services := []startStopper{
		pipelineSvc, ingressSvc, sttSvc, vadSvc, bargeSvc, conversationSvc, intentSvc, egressSvc,
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start service %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	bargeSub := eventbus.SubscribeTo(bus, eventbus.Speech.BargeIn, eventbus.WithSubscriptionName("test_barge"))
	defer bargeSub.Close()

	const sessionID = "barge-cooldown-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	format := eventbus.AudioFormat{
		Encoding:   eventbus.AudioEncodingPCM16,
		SampleRate: 16000,
		Channels:   1,
		BitDepth:   16,
	}

	// Start TTS streaming.
	stream, err := ingressSvc.OpenStream(sessionID, "mic", format, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	segment := pcmBuffer(200, 320)
	for i := 0; i < 3; i++ {
		if err := stream.Write(segment); err != nil {
			t.Fatalf("write segment %d: %v", i, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// Fire first VAD — retries until barge coordinator sees active playback
	// and accepts it. This is deterministic: no Sleep needed.
	bargeEvt := publishVADUntilBarge(t, bus, bargeSub, sessionID, "mic", 5*time.Second)

	// Now fire a second VAD event immediately. closeDelay keeps the stream
	// alive during teardown, so the barge coordinator still sees playing=true.
	// The second VAD reaches registerTrigger which rejects it via cooldown.
	// Timestamp is relative to the first barge event (50ms after) so it is
	// guaranteed to fall within the 200ms cooldown window regardless of
	// wall-clock delays on slow CI.
	eventbus.Publish(ctx, bus, eventbus.Speech.VADDetected, eventbus.SourceSpeechVAD, eventbus.SpeechVADEvent{
		SessionID:  sessionID,
		StreamID:   "mic",
		Active:     true,
		Confidence: 0.9,
		Timestamp:  bargeEvt.Timestamp.Add(50 * time.Millisecond),
	})

	// Wait long enough for the barge coordinator to process the second VAD.
	// Use 250ms > 200ms cooldown so we'd catch a leaked event.
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case env := <-bargeSub.C():
		t.Errorf("expected no second barge event, but got one: session=%s reason=%s", env.Payload.SessionID, env.Payload.Reason)
	case <-timer.C:
		// No second barge event within 250ms — correct.
		t.Log("cooldown: second VAD event correctly suppressed (no duplicate barge)")
	}
}

// TestVoicePipelineBargeInQuietPeriod validates that VAD events during the
// quiet period after TTS playback completes are suppressed.
func TestVoicePipelineBargeInQuietPeriod(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapters.MockSTTAdapterID, map[string]any{
		"phrases":      []any{"quiet period test"},
		"emit_partial": true,
	}); err != nil {
		t.Fatalf("activate stt: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.2,
		"min_frames": 1,
	}); err != nil {
		t.Fatalf("activate vad: %v", err)
	}
	// Use the mock TTS adapter (single chunk, fast) so playback completes quickly.
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, map[string]any{
		"duration_ms": 100,
	}); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	vadSvc := vad.New(bus,
		vad.WithFactory(vad.NewAdapterFactory(store, nil)),
		vad.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	bargeSvc := barge.New(bus,
		barge.WithConfidenceThreshold(0.1),
		barge.WithCooldown(50*time.Millisecond),
		barge.WithQuietPeriod(300*time.Millisecond), // 300ms quiet period.
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	intentSvc := intentrouter.NewService(bus,
		intentrouter.WithAdapter(&speakBackAdapter{prefix: "Quiet: "}),
	)

	// The egress factory is called once per stream, so a single synthesizer
	// handles all speak requests for a session. Phase 1 needs Final=true
	// (playback completes → quiet period). Phase 2 needs non-final head
	// (stays active so barge coordinator sees playing=true for VAD).
	// Intent router publishes duplicate speak requests, so calls 1-2 are
	// Phase 1, calls 3+ are Phase 2.
	ttsFactory := egress.FactoryFunc(func(_ context.Context, _ egress.SessionParams) (egress.Synthesizer, error) {
		return &quietPeriodSynth{finalUntilCall: 2}, nil
	})
	egressSvc := egress.New(bus,
		egress.WithFactory(ttsFactory),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	services := []startStopper{
		pipelineSvc, ingressSvc, sttSvc, vadSvc, bargeSvc, conversationSvc, intentSvc, egressSvc,
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start service %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	bargeSub := eventbus.SubscribeTo(bus, eventbus.Speech.BargeIn, eventbus.WithSubscriptionName("test_barge"))
	defer bargeSub.Close()
	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback"))
	defer playbackSub.Close()

	const sessionID = "barge-quiet-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	format := eventbus.AudioFormat{
		Encoding:   eventbus.AudioEncodingPCM16,
		SampleRate: 16000,
		Channels:   1,
		BitDepth:   16,
	}

	// Feed audio → pipeline produces TTS playback → let it complete.
	stream, err := ingressSvc.OpenStream(sessionID, "mic", format, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	segment := pcmBuffer(200, 320)
	for i := 0; i < 3; i++ {
		if err := stream.Write(segment); err != nil {
			t.Fatalf("write segment %d: %v", i, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// Wait for playback to finish (final=true event).
	waitForPlayback(t, playbackSub, 5*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return evt.Final
	})

	// Small delay to ensure the barge coordinator's consumePlayback goroutine
	// has processed the final playback event (setting quietUntil). Without this,
	// the test's playback subscription may receive the event before the barge
	// coordinator, causing the VAD to be rejected as "no stream found" instead
	// of "quiet period active" — a false positive.
	time.Sleep(50 * time.Millisecond)

	// Publish VAD within the quiet period (300ms).
	// Explicit Timestamp ensures the barge coordinator evaluates quiet period
	// against publish time, not processing time.
	eventbus.Publish(ctx, bus, eventbus.Speech.VADDetected, eventbus.SourceSpeechVAD, eventbus.SpeechVADEvent{
		SessionID:  sessionID,
		StreamID:   "mic",
		Active:     true,
		Confidence: 0.8,
		Timestamp:  time.Now().UTC(),
	})

	// Wait for quiet period + buffer to catch any delayed barge events.
	// The full quiet period window (300ms) plus 100ms processing buffer
	// ensures we'd see a barge event if one leaked through.
	timer := time.NewTimer(400 * time.Millisecond)
	defer timer.Stop()
	select {
	case env := <-bargeSub.C():
		t.Errorf("expected no barge event during quiet period, but got: session=%s reason=%s", env.Payload.SessionID, env.Payload.Reason)
	case <-timer.C:
		t.Log("quiet period: VAD event correctly suppressed after playback")
	}

	// --- Phase 2: After quiet period expires, VAD with new playback triggers barge ---
	//
	// This proves the quiet period was the blocker in Phase 1, not just the
	// absence of an active stream. Without this phase, the test would pass
	// even with WithQuietPeriod(0) because targetStreamForSession returns
	// ("", false) for both "quiet period active" and "no stream found".
	//
	// By feeding new audio AFTER the quiet period expires and showing that
	// barge-in works normally, we prove the quiet period was the specific
	// blocker during Phase 1.
	const quietPeriodLen = 300 * time.Millisecond // matches WithQuietPeriod above
	time.Sleep(quietPeriodLen)

	stream2, err := ingressSvc.OpenStream(sessionID, "mic2", format, nil)
	if err != nil {
		t.Fatalf("open stream 2: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := stream2.Write(segment); err != nil {
			t.Fatalf("write segment 2-%d: %v", i, err)
		}
	}
	if err := stream2.Close(); err != nil {
		t.Fatalf("close stream 2: %v", err)
	}

	// New audio triggers a new TTS cycle. Once the barge coordinator sees
	// active playback, VAD triggers barge normally — proving quiet period
	// was the reason Phase 1's VAD was blocked, not structural absence.
	bargeEvt := publishVADUntilBarge(t, bus, bargeSub, sessionID, "mic2", 5*time.Second)
	if bargeEvt.Reason != "vad_detected" {
		t.Errorf("post-quiet barge reason: got %q want %q", bargeEvt.Reason, "vad_detected")
	}
	t.Log("quiet period: VAD triggers barge normally after quiet period expires with new playback")
}

// ---------------------------------------------------------------------------
// Voice-optional fallback mode
// ---------------------------------------------------------------------------

// TestVoiceFallbackServicesStartWithoutAdapters validates that ALL audio
// services (ingress, STT, VAD, barge, egress) start successfully when no
// adapters are configured. Each service uses its default factory which returns
// ErrFactoryUnavailable. The daemon does not crash or log errors.
//
// NOTE: This tests the default factory (ErrFactoryUnavailable → drop) path.
// The real daemon uses NewAdapterFactory(store, ...) which returns
// ErrAdapterUnavailable → buffer+retry. See TestVoiceFallbackEgressBuffersWithAdapterFactory
// for the real daemon code path.
func TestVoiceFallbackServicesStartWithoutAdapters(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	// No store setup, no adapter activation — default factories only.
	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus) // default factory → ErrFactoryUnavailable
	vadSvc := vad.New(bus) // default factory → ErrFactoryUnavailable
	bargeSvc := barge.New(bus)
	egressSvc := egress.New(bus) // default factory → ErrFactoryUnavailable
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)

	services := []startStopper{
		pipelineSvc, ingressSvc, sttSvc, vadSvc, bargeSvc, conversationSvc, egressSvc,
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// All services must start without error.
	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start service %T: %v", svc, err)
		}
	}

	// Verify services are alive: publish a session lifecycle event and confirm
	// the bus delivers it (proves event bus and services are running).
	lifecycleSub := eventbus.SubscribeTo(bus, eventbus.Sessions.Lifecycle, eventbus.WithSubscriptionName("test_lifecycle"))
	defer lifecycleSub.Close()

	const sessionID = "fallback-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case env := <-lifecycleSub.C():
		if env.Payload.SessionID != sessionID {
			t.Fatalf("lifecycle sessionID: got %q want %q", env.Payload.SessionID, sessionID)
		}
	case <-timer.C:
		t.Fatal("timeout waiting for lifecycle event — bus not delivering")
	}

	// Shutdown all services — must complete without panic or hanging.
	cancel()
	for i := len(services) - 1; i >= 0; i-- {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := services[i].Shutdown(shutCtx); err != nil {
			t.Errorf("shutdown %T: %v", services[i], err)
		}
		shutCancel()
	}

	// Verify egress has no active streams.
	if count := egressSvc.ActiveStreamCount(); count != 0 {
		t.Errorf("egress active streams after shutdown: %d, want 0", count)
	}
}

// TestVoiceFallbackEgressDropsWithoutTTS validates that when no TTS factory
// is configured, the egress service drops speak requests without crashing,
// retrying indefinitely, or logging errors. The conversation reply text
// remains available on the event bus for API/WebSocket clients.
//
// NOTE: This tests the default factory (ErrFactoryUnavailable → drop) path.
// See TestVoiceFallbackEgressBuffersWithAdapterFactory for the adapter factory
// (ErrAdapterUnavailable → buffer+retry) path used by the real daemon.
func TestVoiceFallbackEgressDropsWithoutTTS(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	// Wire conversation + egress with default factory (no TTS adapter).
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	egressSvc := egress.New(bus) // default factory → ErrFactoryUnavailable

	services := []startStopper{pipelineSvc, conversationSvc, egressSvc}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	// Subscribe to conversation reply (proves text is available) and playback
	// (should NOT fire since no TTS factory).
	replySub := eventbus.SubscribeTo(bus, eventbus.Conversation.Reply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()
	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback"))
	defer playbackSub.Close()

	const sessionID = "fallback-egress-session"

	// Emit session lifecycle so conversation service tracks the session.
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	// Publish a conversation reply — this triggers the egress consumeReplies
	// path which calls handleSpeakRequest → createStream → factory returns
	// ErrFactoryUnavailable → request dropped.
	const replyText = "This is the AI response text"
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
		SessionID: sessionID,
		PromptID:  "test-prompt-1",
		Text:      replyText,
		Metadata:  map[string]string{"adapter": "mock.ai"},
	})

	// Verify the reply text is available on the bus (proves text-mode works).
	replyEvt := waitFor(t, func() <-chan eventbus.ConversationReplyEvent {
		ch := make(chan eventbus.ConversationReplyEvent, 1)
		go func() {
			for {
				select {
				case <-runCtx.Done():
					return
				case env, ok := <-replySub.C():
					if !ok {
						return
					}
					ch <- env.Payload
					return
				}
			}
		}()
		return ch
	}(), 2*time.Second)
	if replyEvt.Text != replyText {
		t.Errorf("reply text: got %q want %q", replyEvt.Text, replyText)
	}

	// Wait a bounded window to confirm NO playback event fires.
	noPlayback := time.NewTimer(500 * time.Millisecond)
	defer noPlayback.Stop()
	select {
	case env := <-playbackSub.C():
		t.Fatalf("unexpected playback event without TTS factory: final=%v len=%d", env.Payload.Final, len(env.Payload.Data))
	case <-noPlayback.C:
		// No playback — correct: factory unavailable drops the request.
	}

	// Verify egress did not create any streams.
	if count := egressSvc.ActiveStreamCount(); count != 0 {
		t.Errorf("egress should have 0 active streams, got %d", count)
	}
}

// TestVoiceFallbackSTTDropsWithoutAdapter validates that when no STT factory
// is configured, the STT bridge drops audio segments without crashing or
// buffering indefinitely.
//
// NOTE: This tests the default factory (ErrFactoryUnavailable → drop) path.
// The real daemon uses NewAdapterFactory(store, ...) → ErrAdapterUnavailable → buffer+retry.
func TestVoiceFallbackSTTDropsWithoutAdapter(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	// Wire ingress + STT with default factory (no STT adapter).
	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus) // default factory → ErrFactoryUnavailable

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := ingressSvc.Start(runCtx); err != nil {
		t.Fatalf("start ingress: %v", err)
	}
	defer ingressSvc.Shutdown(context.Background())
	if err := sttSvc.Start(runCtx); err != nil {
		t.Fatalf("start stt: %v", err)
	}
	defer sttSvc.Shutdown(context.Background())

	// Subscribe to transcript topics — should NOT fire since STT drops segments.
	transcriptSub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptFinal, eventbus.WithSubscriptionName("test_transcript"))
	defer transcriptSub.Close()

	const sessionID = "fallback-stt-session"
	format := eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}

	// Open a stream and write audio segments.
	stream, err := ingressSvc.OpenStream(sessionID, "mic", format, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	segment := pcmBuffer(200, 320)
	for i := 0; i < 3; i++ {
		if err := stream.Write(segment); err != nil {
			t.Fatalf("write segment %d: %v", i, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// Allow STT time to process (it should drop immediately, but give margin).
	noTranscript := time.NewTimer(500 * time.Millisecond)
	defer noTranscript.Stop()
	select {
	case env := <-transcriptSub.C():
		t.Fatalf("unexpected transcript without STT adapter: %+v", env.Payload)
	case <-noTranscript.C:
		// No transcript — correct: factory unavailable drops segments.
	}

}

// TestVoiceFallbackBargeInertWithoutVAD validates that the barge coordinator
// starts, runs, and shuts down cleanly with zero VAD input. Client-initiated
// interrupts still work.
func TestVoiceFallbackBargeInertWithoutVAD(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	// Activate only TTS (no STT, no VAD) to prove barge handles client
	// interrupts without VAD.
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, map[string]any{
		"duration_ms": 200,
	}); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	bargeSvc := barge.New(bus,
		barge.WithConfidenceThreshold(0.1),
		barge.WithCooldown(50*time.Millisecond),
		barge.WithQuietPeriod(0),
	)
	egressSvc := egress.New(bus,
		egress.WithFactory(egress.NewAdapterFactory(store, nil)),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)

	services := []startStopper{pipelineSvc, bargeSvc, conversationSvc, egressSvc}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	bargeSub := eventbus.SubscribeTo(bus, eventbus.Speech.BargeIn, eventbus.WithSubscriptionName("test_barge"))
	defer bargeSub.Close()
	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback"))
	defer playbackSub.Close()

	const sessionID = "fallback-barge-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	// Trigger TTS playback via conversation reply (egress has TTS factory).
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
		SessionID: sessionID,
		PromptID:  "test-prompt",
		Text:      "Hello from the AI",
		Metadata:  map[string]string{"adapter": "mock.ai"},
	})

	// Wait for first playback chunk.
	firstPlayback := waitForPlayback(t, playbackSub, 3*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return len(evt.Data) > 0
	})
	streamID := firstPlayback.StreamID
	if streamID == "" {
		streamID = "tts"
	}

	// Verify no VAD-triggered barge events arrive (no VAD configured).
	noVADBarge := time.NewTimer(200 * time.Millisecond)
	defer noVADBarge.Stop()
	select {
	case env := <-bargeSub.C():
		t.Fatalf("unexpected barge event without VAD: %+v", env.Payload)
	case <-noVADBarge.C:
		// No barge — correct: no VAD means no VAD-triggered barge.
	}

	// Client-initiated interrupt still works without VAD.
	eventbus.Publish(ctx, bus, eventbus.Audio.Interrupt, eventbus.SourceClient, eventbus.AudioInterruptEvent{
		SessionID: sessionID,
		StreamID:  streamID,
		Reason:    "manual",
		Metadata:  map[string]string{"origin": "test"},
		Timestamp: time.Now().UTC(),
	})

	bargeEvt := waitForBargeEvent(t, bargeSub, 2*time.Second)
	if bargeEvt.SessionID != sessionID {
		t.Errorf("barge sessionID: got %q want %q", bargeEvt.SessionID, sessionID)
	}
	if bargeEvt.Reason != "manual" {
		t.Errorf("barge reason: got %q want %q", bargeEvt.Reason, "manual")
	}

	// Confirm final playback with barge metadata arrives.
	waitForPlayback(t, playbackSub, 3*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return evt.Final && evt.Metadata["barge_in"] == "true"
	})
}

// TestVoiceFallbackEgressBuffersWithAdapterFactory validates the REAL daemon
// code path: egress created with NewAdapterFactory backed by a config store
// that has NO TTS adapter binding. Unlike TestVoiceFallbackEgressDropsWithoutTTS
// (which tests the default factory → ErrFactoryUnavailable → drop path), this
// tests the adapter factory → ErrAdapterUnavailable → buffer+retry path that
// the actual daemon uses (daemon.go:119).
func TestVoiceFallbackEgressBuffersWithAdapterFactory(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	// Real config store with NO TTS binding — simulates daemon startup without
	// any voice adapter configured.
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	// Real adapter factory backed by config store (same wiring as daemon.go:119).
	// With no TTS binding, Create() returns ErrAdapterUnavailable → buffer+retry.
	egressLog := &syncBuffer{}
	egressSvc := egress.New(bus,
		egress.WithFactory(egress.NewAdapterFactory(store, nil)),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
		egress.WithLogger(log.New(egressLog, "", 0)),
	)

	services := []startStopper{pipelineSvc, conversationSvc, egressSvc}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	// Subscribe to conversation reply (proves text available) and playback
	// (should NOT fire — no TTS adapter bound).
	replySub := eventbus.SubscribeTo(bus, eventbus.Conversation.Reply, eventbus.WithSubscriptionName("test_reply_af"))
	defer replySub.Close()
	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback_af"))
	defer playbackSub.Close()

	const sessionID = "fallback-adapter-factory-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	// Publish conversation reply — egress receives it, calls factory.Create(),
	// gets ErrAdapterUnavailable, buffers the request with retry backoff.
	const replyText = "AI response via adapter factory path"
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
		SessionID: sessionID,
		PromptID:  "test-prompt-af",
		Text:      replyText,
		Metadata:  map[string]string{"adapter": "mock.ai"},
	})

	// Reply text must be on the bus regardless of TTS availability.
	replyEvt := waitFor(t, func() <-chan eventbus.ConversationReplyEvent {
		ch := make(chan eventbus.ConversationReplyEvent, 1)
		go func() {
			for {
				select {
				case <-runCtx.Done():
					return
				case env, ok := <-replySub.C():
					if !ok {
						return
					}
					ch <- env.Payload
					return
				}
			}
		}()
		return ch
	}(), 2*time.Second)
	if replyEvt.Text != replyText {
		t.Errorf("reply text: got %q want %q", replyEvt.Text, replyText)
	}

	// Egress buffers the request (ErrAdapterUnavailable → retry) but never
	// succeeds because no adapter exists. No playback events should fire.
	noPlayback := time.NewTimer(500 * time.Millisecond)
	defer noPlayback.Stop()
	select {
	case env := <-playbackSub.C():
		t.Fatalf("unexpected playback without TTS adapter: final=%v", env.Payload.Final)
	case <-noPlayback.C:
		// No playback — correct: adapter unavailable, request buffered but never fulfilled.
	}

	// Verify the buffer+retry path was taken (not the drop path).
	logOutput := egressLog.String()
	if !strings.Contains(logOutput, "[TTS] buffered item") {
		t.Errorf("expected buffer log from ErrAdapterUnavailable path, got: %s", logOutput)
	}
	if strings.Contains(logOutput, "[TTS] factory unavailable") {
		t.Errorf("unexpected factory-unavailable log — should be buffer path, got: %s", logOutput)
	}

	// Clean shutdown must not panic even with pending buffered requests.
	cancel()
	if count := egressSvc.ActiveStreamCount(); count != 0 {
		t.Errorf("egress should have 0 active streams, got %d", count)
	}
}

// TestVoiceFallbackSTTBuffersWithAdapterFactory validates the REAL daemon code
// path: STT created with NewAdapterFactory backed by a config store that has NO
// STT adapter binding. Unlike TestVoiceFallbackSTTDropsWithoutAdapter (which
// tests the default factory → ErrFactoryUnavailable → drop path), this tests
// the adapter factory → ErrAdapterUnavailable → buffer+retry path that the
// actual daemon uses (daemon.go:116).
func TestVoiceFallbackSTTBuffersWithAdapterFactory(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	// Real config store with NO STT binding — simulates daemon startup without
	// any STT adapter configured.
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	ingressSvc := ingress.New(bus)

	// Real adapter factory backed by config store (same wiring as daemon.go:116).
	// With no STT binding, Create() returns ErrAdapterUnavailable → buffer+retry.
	sttLog := &syncBuffer{}
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
		stt.WithLogger(log.New(sttLog, "", 0)),
	)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := ingressSvc.Start(runCtx); err != nil {
		t.Fatalf("start ingress: %v", err)
	}
	defer ingressSvc.Shutdown(context.Background())
	if err := sttSvc.Start(runCtx); err != nil {
		t.Fatalf("start stt: %v", err)
	}
	defer sttSvc.Shutdown(context.Background())

	// Subscribe to transcript — should NOT fire since no STT adapter is bound.
	transcriptSub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptFinal, eventbus.WithSubscriptionName("test_transcript_af"))
	defer transcriptSub.Close()

	const sessionID = "fallback-stt-af-session"
	format := eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}

	// Open stream and write audio segments.
	stream, err := ingressSvc.OpenStream(sessionID, "mic", format, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	segment := pcmBuffer(200, 320)
	for i := 0; i < 3; i++ {
		if err := stream.Write(segment); err != nil {
			t.Fatalf("write segment %d: %v", i, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// Allow STT time to process — it should buffer, not drop.
	noTranscript := time.NewTimer(500 * time.Millisecond)
	defer noTranscript.Stop()
	select {
	case env := <-transcriptSub.C():
		t.Fatalf("unexpected transcript without STT adapter: %+v", env.Payload)
	case <-noTranscript.C:
		// No transcript — correct: adapter unavailable, segments buffered but never transcribed.
	}

	// Verify the buffer+retry path was taken (not the drop path).
	logOutput := sttLog.String()
	if !strings.Contains(logOutput, "[STT] buffered item") {
		t.Errorf("expected buffer log from ErrAdapterUnavailable path, got: %s", logOutput)
	}
	if strings.Contains(logOutput, "[STT] factory unavailable") {
		t.Errorf("unexpected factory-unavailable log — should be buffer path, got: %s", logOutput)
	}

}

// ---------------------------------------------------------------------------
// End-to-End VAD Integration Validation
// ---------------------------------------------------------------------------

// waitForVADEvent waits for a SpeechVADEvent matching the predicate.
func waitForVADEvent(t *testing.T, sub *eventbus.TypedSubscription[eventbus.SpeechVADEvent], timeout time.Duration, predicate func(eventbus.SpeechVADEvent) bool) eventbus.SpeechVADEvent {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case env := <-sub.C():
			if predicate == nil || predicate(env.Payload) {
				return env.Payload
			}
		case <-timer.C:
			t.Fatalf("timeout waiting for VAD event (%s)", timeout)
		}
	}
}

// phaseAnalyzer wraps a VAD analyzer to fail with ErrAdapterUnavailable when
// the phase flag is set to 1. During failure, it returns a partial detection
// alongside the error to exercise the NAP partial-results contract (the service
// must publish these before triggering recovery). Used by TestVADMidStreamRecovery.
type phaseAnalyzer struct {
	inner vad.Analyzer
	phase *atomic.Int32
}

func (a *phaseAnalyzer) OnSegment(ctx context.Context, seg eventbus.AudioIngressSegmentEvent) ([]vad.Detection, error) {
	if a.phase.Load() == 1 {
		// Return partial detection alongside error — mirrors real adapters that
		// may flush buffered state when they detect their backend is failing.
		return []vad.Detection{{Active: true, Confidence: 0.5}}, vad.ErrAdapterUnavailable
	}
	return a.inner.OnSegment(ctx, seg)
}

func (a *phaseAnalyzer) Close(ctx context.Context) ([]vad.Detection, error) {
	return a.inner.Close(ctx)
}

// TestVADDrivenVoiceLoop validates the complete voice pipeline driven through
// the actual VAD service with mock adapter. Unlike earlier tests that inject
// VAD events directly, this test drives loud PCM audio through ingress so the
// mock VAD analyzer detects speech based on RMS amplitude.
//
// Audio → ingress → VAD detection → STT transcript → conversation → intent → TTS playback.
func TestVADDrivenVoiceLoop(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	// Mock VAD: threshold 0.1 with min_frames=1. A single loud segment
	// (amplitude 5000 → RMS ≈ 0.15) triggers Active detection immediately.
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapters.MockSTTAdapterID, map[string]any{
		"phrases": []any{"hello vad"},
	}); err != nil {
		t.Fatalf("activate stt: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, map[string]any{
		"duration_ms": 200,
	}); err != nil {
		t.Fatalf("activate tts: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.1,
		"min_frames": 1,
	}); err != nil {
		t.Fatalf("activate vad: %v", err)
	}

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	vadSvc := vad.New(bus,
		vad.WithFactory(vad.NewAdapterFactory(store, nil)),
		vad.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	bargeSvc := barge.New(bus,
		barge.WithConfidenceThreshold(0.1),
		barge.WithCooldown(50*time.Millisecond),
		barge.WithQuietPeriod(0),
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	intentSvc := intentrouter.NewService(bus,
		intentrouter.WithAdapter(&speakBackAdapter{prefix: "VAD loop: "}),
	)
	egressSvc := egress.New(bus,
		egress.WithFactory(egress.NewAdapterFactory(store, nil)),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	services := []startStopper{
		pipelineSvc, ingressSvc, sttSvc, vadSvc, bargeSvc, conversationSvc, intentSvc, egressSvc,
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	vadSub := eventbus.SubscribeTo(bus, eventbus.Speech.VADDetected, eventbus.WithSubscriptionName("test_vad"))
	defer vadSub.Close()
	transcriptSub := eventbus.SubscribeTo(bus, eventbus.Speech.TranscriptFinal, eventbus.WithSubscriptionName("test_transcript"))
	defer transcriptSub.Close()
	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt, eventbus.WithSubscriptionName("test_prompt"))
	defer promptSub.Close()
	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback"))
	defer playbackSub.Close()

	transcriptCh := make(chan eventbus.SpeechTranscriptEvent, 4)
	promptCh := make(chan eventbus.ConversationPromptEvent, 4)
	bridgeCtx, bridgeCancel := context.WithCancel(runCtx)
	defer bridgeCancel()
	go forwardEvents(bridgeCtx, transcriptSub, transcriptCh)
	go forwardEvents(bridgeCtx, promptSub, promptCh)

	const sessionID = "vad-loop-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
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
		t.Fatalf("open stream: %v", err)
	}

	// Write loud PCM: amplitude 5000 → RMS ≈ 0.15 exceeds threshold 0.1.
	loudSegment := pcmBuffer(5000, 320) // 320 samples = 20ms at 16kHz
	for i := 0; i < 3; i++ {
		if err := stream.Write(loudSegment); err != nil {
			t.Fatalf("write segment %d: %v", i+1, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// 1. Verify VAD detects speech with correct fields.
	vadEvt := waitForVADEvent(t, vadSub, 2*time.Second, func(evt eventbus.SpeechVADEvent) bool {
		return evt.Active
	})
	if vadEvt.SessionID != sessionID {
		t.Errorf("VAD sessionID: got %q want %q", vadEvt.SessionID, sessionID)
	}
	if vadEvt.StreamID != "mic" {
		t.Errorf("VAD streamID: got %q want %q", vadEvt.StreamID, "mic")
	}
	if vadEvt.Confidence <= 0 {
		t.Errorf("VAD confidence should be > 0, got %f", vadEvt.Confidence)
	}

	// 2. Verify STT produces a final transcript (triggered by stream close).
	transcript := waitFor(t, transcriptCh, 2*time.Second)
	if !transcript.Final {
		t.Fatalf("expected final transcript, got partial")
	}
	if transcript.SessionID != sessionID {
		t.Errorf("transcript sessionID: got %q want %q", transcript.SessionID, sessionID)
	}

	// 3. Verify conversation prompt is generated from the transcript.
	prompt := waitFor(t, promptCh, 2*time.Second)
	if prompt.SessionID != sessionID {
		t.Errorf("prompt sessionID: got %q want %q", prompt.SessionID, sessionID)
	}
	if prompt.NewMessage.Text != transcript.Text {
		t.Errorf("prompt text: got %q want %q", prompt.NewMessage.Text, transcript.Text)
	}
	if prompt.NewMessage.Origin != eventbus.OriginUser {
		t.Errorf("prompt origin: got %q want %q", prompt.NewMessage.Origin, eventbus.OriginUser)
	}

	// 4. Verify TTS produces playback with final chunk.
	finalPlayback := waitForPlayback(t, playbackSub, 3*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return evt.Final
	})
	if finalPlayback.SessionID != sessionID {
		t.Errorf("playback sessionID: got %q want %q", finalPlayback.SessionID, sessionID)
	}
	if len(finalPlayback.Data) == 0 {
		t.Error("expected playback data, got empty")
	}
}

// TestVADSilenceDoesNotTriggerDetection verifies that zero-amplitude PCM data
// does not trigger VAD speech detection. The mock VAD analyzer calculates RMS
// which is 0 for silent data, well below any configured threshold.
func TestVADSilenceDoesNotTriggerDetection(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.1,
		"min_frames": 1,
	}); err != nil {
		t.Fatalf("activate vad: %v", err)
	}

	ingressSvc := ingress.New(bus)
	vadSvc := vad.New(bus,
		vad.WithFactory(vad.NewAdapterFactory(store, nil)),
		vad.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	services := []startStopper{ingressSvc, vadSvc}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	vadSub := eventbus.SubscribeTo(bus, eventbus.Speech.VADDetected, eventbus.WithSubscriptionName("test_vad"))
	defer vadSub.Close()

	format := eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}

	const sessionID = "vad-silence-session"
	stream, err := ingressSvc.OpenStream(sessionID, "mic", format, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	// Write zero-amplitude (silent) PCM segments.
	silentSegment := pcmBuffer(0, 320)
	for i := 0; i < 5; i++ {
		if err := stream.Write(silentSegment); err != nil {
			t.Fatalf("write segment %d: %v", i+1, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	// No Active VAD detection should fire for silent audio.
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case env := <-vadSub.C():
			if env.Payload.Active {
				t.Fatalf("unexpected active VAD detection from silence: confidence=%f", env.Payload.Confidence)
			}
			// Inactive events are fine (mock emits on Close if was active).
		case <-timer.C:
			return
		}
	}
}

// TestVADBargeInDuringPlayback validates that when TTS is actively streaming
// and the VAD service detects speech from real audio (via mock adapter), the
// barge coordinator fires a barge-in event. Unlike TestVoicePipelineBargeInDuringTTSStreaming
// which injects VAD events directly, this test drives audio through the full
// VAD bridge path: ingress → VAD service → mock analyzer → detection → barge.
func TestVADBargeInDuringPlayback(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapters.MockSTTAdapterID, map[string]any{
		"phrases": []any{"barge test"},
	}); err != nil {
		t.Fatalf("activate stt: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.1,
		"min_frames": 1,
	}); err != nil {
		t.Fatalf("activate vad: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	// Multi-chunk streaming synth keeps playback alive long enough for VAD
	// detection to trigger barge. Speak() returns 1 head chunk (published
	// immediately), Close() returns the 9 tail chunks.
	ttsFactory := egress.FactoryFunc(func(_ context.Context, _ egress.SessionParams) (egress.Synthesizer, error) {
		return newStreamingSynth(&multiChunkSynthesizer{
			chunkCount: 10,
			chunkSize:  640,
		}), nil
	})

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	vadSvc := vad.New(bus,
		vad.WithFactory(vad.NewAdapterFactory(store, nil)),
		vad.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	bargeSvc := barge.New(bus,
		barge.WithConfidenceThreshold(0.1),
		barge.WithCooldown(100*time.Millisecond),
		barge.WithQuietPeriod(0),
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	intentSvc := intentrouter.NewService(bus,
		intentrouter.WithAdapter(&speakBackAdapter{prefix: "Barge VAD: "}),
	)
	egressSvc := egress.New(bus,
		egress.WithFactory(ttsFactory),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	services := []startStopper{
		pipelineSvc, ingressSvc, sttSvc, vadSvc, bargeSvc, conversationSvc, intentSvc, egressSvc,
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback"))
	defer playbackSub.Close()
	bargeSub := eventbus.SubscribeTo(bus, eventbus.Speech.BargeIn, eventbus.WithSubscriptionName("test_barge"))
	defer bargeSub.Close()

	const sessionID = "vad-barge-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateRunning,
	})

	format := eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}

	// Phase 1: Feed audio through the pipeline to start TTS playback.
	stream1, err := ingressSvc.OpenStream(sessionID, "mic", format, nil)
	if err != nil {
		t.Fatalf("open stream 1: %v", err)
	}
	loudSegment := pcmBuffer(5000, 320)
	for i := 0; i < 3; i++ {
		if err := stream1.Write(loudSegment); err != nil {
			t.Fatalf("write segment %d: %v", i, err)
		}
	}
	if err := stream1.Close(); err != nil {
		t.Fatalf("close stream 1: %v", err)
	}

	// Wait for TTS to start streaming (non-final playback = active playback).
	waitForPlayback(t, playbackSub, 5*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return !evt.Final && len(evt.Data) > 0
	})

	// Phase 2: Feed loud audio through a new ingress stream. The new stream
	// creates a fresh VAD analyzer (lastState=false) so the first loud
	// segment triggers Active=true detection. With active playback, the
	// barge coordinator fires.
	stream2, err := ingressSvc.OpenStream(sessionID, "mic2", format, nil)
	if err != nil {
		t.Fatalf("open stream 2: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := stream2.Write(loudSegment); err != nil {
			t.Fatalf("write barge segment %d: %v", i, err)
		}
	}
	if err := stream2.Close(); err != nil {
		t.Fatalf("close stream 2: %v", err)
	}

	// Verify barge event from VAD-driven detection.
	bargeEvt := waitForBargeEvent(t, bargeSub, 3*time.Second)
	if bargeEvt.SessionID != sessionID {
		t.Errorf("barge sessionID: got %q want %q", bargeEvt.SessionID, sessionID)
	}
	if bargeEvt.Reason != "vad_detected" {
		t.Errorf("barge reason: got %q want %q", bargeEvt.Reason, "vad_detected")
	}

	// Verify final playback carries barge_in metadata.
	finalPlayback := waitForPlayback(t, playbackSub, 3*time.Second, func(evt eventbus.AudioEgressPlaybackEvent) bool {
		return evt.Final && evt.Metadata["barge_in"] == "true"
	})
	if finalPlayback.Metadata["barge_in_reason"] != "vad_detected" {
		t.Errorf("expected barge_in_reason=vad_detected, got: %q", finalPlayback.Metadata["barge_in_reason"])
	}
}

// TestVADMultiSessionIsolation verifies that VAD detections and barge-in events
// in one session do not affect another session. Session A receives loud audio
// (triggering VAD + barge), while session B has active playback but no VAD
// input — its playback continues uninterrupted.
func TestVADMultiSessionIsolation(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapters.MockSTTAdapterID, map[string]any{
		"phrases": []any{"session test"},
	}); err != nil {
		t.Fatalf("activate stt: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.1,
		"min_frames": 1,
	}); err != nil {
		t.Fatalf("activate vad: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("activate tts: %v", err)
	}

	// Multi-chunk streaming synth for both sessions.
	ttsFactory := egress.FactoryFunc(func(_ context.Context, _ egress.SessionParams) (egress.Synthesizer, error) {
		return newStreamingSynth(&multiChunkSynthesizer{
			chunkCount: 10,
			chunkSize:  640,
		}), nil
	})

	ingressSvc := ingress.New(bus)
	sttSvc := stt.New(bus,
		stt.WithFactory(stt.NewAdapterFactory(store, nil)),
		stt.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	vadSvc := vad.New(bus,
		vad.WithFactory(vad.NewAdapterFactory(store, nil)),
		vad.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)
	bargeSvc := barge.New(bus,
		barge.WithConfidenceThreshold(0.1),
		barge.WithCooldown(100*time.Millisecond),
		barge.WithQuietPeriod(0),
	)
	pipelineSvc := contentpipeline.NewService(bus, nil)
	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)
	intentSvc := intentrouter.NewService(bus,
		intentrouter.WithAdapter(&speakBackAdapter{prefix: "Multi: "}),
	)
	egressSvc := egress.New(bus,
		egress.WithFactory(ttsFactory),
		egress.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	services := []startStopper{
		pipelineSvc, ingressSvc, sttSvc, vadSvc, bargeSvc, conversationSvc, intentSvc, egressSvc,
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, svc := range services {
		if err := svc.Start(runCtx); err != nil {
			t.Fatalf("start %T: %v", svc, err)
		}
		defer svc.Shutdown(context.Background())
	}

	vadSub := eventbus.SubscribeTo(bus, eventbus.Speech.VADDetected, eventbus.WithSubscriptionName("test_vad"))
	defer vadSub.Close()
	bargeSub := eventbus.SubscribeTo(bus, eventbus.Speech.BargeIn, eventbus.WithSubscriptionName("test_barge"))
	defer bargeSub.Close()
	playbackSub := eventbus.SubscribeTo(bus, eventbus.Audio.EgressPlayback, eventbus.WithSubscriptionName("test_playback"))
	defer playbackSub.Close()

	const sessionA = "multi-session-a"
	const sessionB = "multi-session-b"

	for _, sid := range []string{sessionA, sessionB} {
		eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
			SessionID: sid,
			State:     eventbus.SessionStateRunning,
		})
	}

	format := eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}

	// Silent audio for initial seeding: mock STT still produces transcripts
	// (it ignores audio content), but VAD will NOT detect speech (RMS = 0).
	// This avoids an early VAD detection + barge race during pipeline warm-up.
	silentSegment := pcmBuffer(0, 320)
	loudSegment := pcmBuffer(5000, 320)

	// Seed both sessions with silent audio → STT transcribes → conversation → TTS plays.
	for _, sid := range []string{sessionA, sessionB} {
		stream, err := ingressSvc.OpenStream(sid, "mic", format, nil)
		if err != nil {
			t.Fatalf("open stream %s: %v", sid, err)
		}
		for i := 0; i < 3; i++ {
			if err := stream.Write(silentSegment); err != nil {
				t.Fatalf("write %s segment %d: %v", sid, i, err)
			}
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("close %s stream: %v", sid, err)
		}
	}

	// Wait for playback to start in both sessions.
	seenA := false
	seenB := false
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !seenA || !seenB {
		select {
		case env := <-playbackSub.C():
			if env.Payload.SessionID == sessionA && !env.Payload.Final {
				seenA = true
			}
			if env.Payload.SessionID == sessionB && !env.Payload.Final {
				seenB = true
			}
		case <-deadline.C:
			t.Fatalf("timeout waiting for playback in both sessions (A=%v B=%v)", seenA, seenB)
		}
	}

	// Feed loud audio ONLY in session A via new stream → VAD detects → barge in A.
	streamA, err := ingressSvc.OpenStream(sessionA, "mic2", format, nil)
	if err != nil {
		t.Fatalf("open stream A mic2: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := streamA.Write(loudSegment); err != nil {
			t.Fatalf("write A barge segment %d: %v", i, err)
		}
	}
	if err := streamA.Close(); err != nil {
		t.Fatalf("close stream A mic2: %v", err)
	}

	// Verify VAD detection only for session A.
	waitForVADEvent(t, vadSub, 3*time.Second, func(evt eventbus.SpeechVADEvent) bool {
		return evt.Active && evt.SessionID == sessionA && evt.StreamID == "mic2"
	})

	// Verify barge fires only for session A.
	bargeEvt := waitForBargeEvent(t, bargeSub, 3*time.Second)
	if bargeEvt.SessionID != sessionA {
		t.Errorf("barge should be for session A, got %q", bargeEvt.SessionID)
	}

	// Verify no barge event for session B within a bounded window.
	noBarge := time.NewTimer(500 * time.Millisecond)
	defer noBarge.Stop()
	for {
		select {
		case env := <-bargeSub.C():
			if env.Payload.SessionID == sessionB {
				t.Fatalf("unexpected barge event for session B: %+v", env.Payload)
			}
		case <-noBarge.C:
			// No barge for B — correct: session B had no loud audio input.
			return
		}
	}
}

// TestVADMidStreamRecovery validates that when the VAD adapter becomes
// unavailable mid-stream (OnSegment returns ErrAdapterUnavailable), the
// service nils out the broken analyzer and recovers when the factory
// succeeds again on a subsequent segment.
func TestVADMidStreamRecovery(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.1,
		"min_frames": 1,
	}); err != nil {
		t.Fatalf("activate vad: %v", err)
	}

	// Phase-controlled factory: phase 0 = available, phase 1 = unavailable.
	// The factory wraps each analyzer with phaseAnalyzer so that mid-stream
	// OnSegment calls also respect the phase flag.
	// factoryFailCount tracks Create calls during phase=1, used to synchronize
	// the test without time.Sleep — when >= 1, the service has processed the
	// analyzer failure and attempted reconnection.
	var phase atomic.Int32
	var factoryFailCount atomic.Int32
	innerFactory := vad.NewAdapterFactory(store, nil)
	factory := vad.FactoryFunc(func(ctx context.Context, params vad.SessionParams) (vad.Analyzer, error) {
		if phase.Load() == 1 {
			factoryFailCount.Add(1)
			return nil, vad.ErrAdapterUnavailable
		}
		analyzer, err := innerFactory.Create(ctx, params)
		if err != nil {
			return nil, err
		}
		return &phaseAnalyzer{inner: analyzer, phase: &phase}, nil
	})

	ingressSvc := ingress.New(bus)
	vadSvc := vad.New(bus,
		vad.WithFactory(factory),
		vad.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := ingressSvc.Start(runCtx); err != nil {
		t.Fatalf("start ingress: %v", err)
	}
	defer ingressSvc.Shutdown(context.Background())
	if err := vadSvc.Start(runCtx); err != nil {
		t.Fatalf("start vad: %v", err)
	}
	defer vadSvc.Shutdown(context.Background())

	vadSub := eventbus.SubscribeTo(bus, eventbus.Speech.VADDetected, eventbus.WithSubscriptionName("test_vad"))
	defer vadSub.Close()

	format := eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}

	const sessionID = "vad-recovery-session"
	stream, err := ingressSvc.OpenStream(sessionID, "mic", format, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	loudSegment := pcmBuffer(5000, 320)

	// Phase 1: Available. Loud audio → VAD detects Active=true.
	for i := 0; i < 2; i++ {
		if err := stream.Write(loudSegment); err != nil {
			t.Fatalf("phase 1 write %d: %v", i, err)
		}
	}
	vadEvt := waitForVADEvent(t, vadSub, 2*time.Second, func(evt eventbus.SpeechVADEvent) bool {
		return evt.Active && evt.SessionID == sessionID
	})
	t.Logf("phase 1: VAD detected, confidence=%f", vadEvt.Confidence)

	// Phase 2: Unavailable. OnSegment returns partial detection + error.
	// The VAD service MUST publish the partial detection before closing the
	// broken analyzer (AC #6: "partial results from the failing call are
	// published before recovery").
	phase.Store(1)
	for i := 0; i < 3; i++ {
		if err := stream.Write(loudSegment); err != nil {
			t.Fatalf("phase 2 write %d: %v", i, err)
		}
	}

	// Verify partial detection published during failure (confidence=0.5 from phaseAnalyzer).
	partialEvt := waitForVADEvent(t, vadSub, 2*time.Second, func(evt eventbus.SpeechVADEvent) bool {
		return evt.Active && evt.SessionID == sessionID && evt.Confidence > 0.4 && evt.Confidence < 0.6
	})
	t.Logf("phase 2: partial detection published during failure, confidence=%f", partialEvt.Confidence)

	// Wait for the VAD service to process failure and attempt reconnection.
	// factoryFailCount >= 1 means the service has nilled out the broken
	// analyzer and tried factory.Create, confirming the failure path completed.
	{
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		syncDeadline := time.NewTimer(2 * time.Second)
		defer syncDeadline.Stop()
		for factoryFailCount.Load() < 1 {
			select {
			case <-tick.C:
			case <-syncDeadline.C:
				t.Fatal("timeout waiting for VAD failure processing")
			}
		}
	}

	// Phase 3: Available again. Next segment → Create succeeds → new analyzer
	// detects Active=true (fresh analyzer, lastState=false).
	phase.Store(0)
	for i := 0; i < 2; i++ {
		if err := stream.Write(loudSegment); err != nil {
			t.Fatalf("phase 3 write %d: %v", i, err)
		}
	}
	recoveredEvt := waitForVADEvent(t, vadSub, 2*time.Second, func(evt eventbus.SpeechVADEvent) bool {
		return evt.Active && evt.SessionID == sessionID
	})
	t.Logf("phase 3: VAD recovered, confidence=%f", recoveredEvt.Confidence)

	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}
}

// TestVADNoStaleStateAcrossSessions verifies that different sessions get
// independent VAD analyzers — state from session A does not leak to session B.
// The mock VAD adapter only emits Active=true on a false→true transition, so
// if stale "active" state leaked from session A to B, session B's first loud
// audio would NOT trigger a detection. (AC #2: "no stale VAD state leaks
// between sessions")
func TestVADNoStaleStateAcrossSessions(t *testing.T) {
	ctx := context.Background()
	bus := eventbus.New()

	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.1,
		"min_frames": 1,
	}); err != nil {
		t.Fatalf("activate vad: %v", err)
	}

	ingressSvc := ingress.New(bus)
	vadSvc := vad.New(bus,
		vad.WithFactory(vad.NewAdapterFactory(store, nil)),
		vad.WithRetryDelays(10*time.Millisecond, 50*time.Millisecond),
	)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := ingressSvc.Start(runCtx); err != nil {
		t.Fatalf("start ingress: %v", err)
	}
	defer ingressSvc.Shutdown(context.Background())
	if err := vadSvc.Start(runCtx); err != nil {
		t.Fatalf("start vad: %v", err)
	}
	defer vadSvc.Shutdown(context.Background())

	vadSub := eventbus.SubscribeTo(bus, eventbus.Speech.VADDetected, eventbus.WithSubscriptionName("test_vad"))
	defer vadSub.Close()

	format := eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}

	loudSegment := pcmBuffer(5000, 320)

	// Session A: Feed loud audio → VAD detects Active (analyzer's lastState becomes true).
	const sessionA = "vad-stale-a"
	streamA, err := ingressSvc.OpenStream(sessionA, "mic", format, nil)
	if err != nil {
		t.Fatalf("open stream A: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := streamA.Write(loudSegment); err != nil {
			t.Fatalf("session A write %d: %v", i, err)
		}
	}
	waitForVADEvent(t, vadSub, 2*time.Second, func(evt eventbus.SpeechVADEvent) bool {
		return evt.Active && evt.SessionID == sessionA
	})
	t.Log("session A: VAD Active detected")
	if err := streamA.Close(); err != nil {
		t.Fatalf("close stream A: %v", err)
	}

	// Session B: New session, feed loud audio. If session A's active state leaked,
	// session B's analyzer would already have lastState=true and the false→true
	// transition would NOT fire — causing a timeout here.
	const sessionB = "vad-stale-b"
	streamB, err := ingressSvc.OpenStream(sessionB, "mic", format, nil)
	if err != nil {
		t.Fatalf("open stream B: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := streamB.Write(loudSegment); err != nil {
			t.Fatalf("session B write %d: %v", i, err)
		}
	}
	waitForVADEvent(t, vadSub, 2*time.Second, func(evt eventbus.SpeechVADEvent) bool {
		return evt.Active && evt.SessionID == sessionB
	})
	t.Log("session B: VAD Active detected — no stale state from session A")
	if err := streamB.Close(); err != nil {
		t.Fatalf("close stream B: %v", err)
	}
}
