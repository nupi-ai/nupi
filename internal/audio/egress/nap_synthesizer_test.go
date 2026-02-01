package egress

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
	testutil "github.com/nupi-ai/nupi/internal/testutil"
	"github.com/nupi-ai/nupi/internal/voice/slots"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// mockTTSServer implements TextToSpeechServiceServer for testing.
type mockTTSServer struct {
	napv1.UnimplementedTextToSpeechServiceServer
	// chunks to stream back for each call
	chunks []*napv1.SynthesisResponse
	// if set, return this error during streaming
	streamErr error
}

func (s *mockTTSServer) StreamSynthesis(req *napv1.StreamSynthesisRequest, stream napv1.TextToSpeechService_StreamSynthesisServer) error {
	if s.streamErr != nil {
		return s.streamErr
	}
	for _, chunk := range s.chunks {
		if err := stream.Send(chunk); err != nil {
			return err
		}
	}
	return nil
}

func startMockTTSServer(t *testing.T, srv napv1.TextToSpeechServiceServer) (*bufconn.Listener, func(context.Context, string) (net.Conn, error)) {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	napv1.RegisterTextToSpeechServiceServer(server, srv)
	go server.Serve(lis)
	t.Cleanup(func() {
		server.GracefulStop()
	})
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
	return lis, dialer
}

func TestNAPSynthesizerSpeakStreamsChunks(t *testing.T) {
	srv := &mockTTSServer{
		chunks: []*napv1.SynthesisResponse{
			{
				Status: napv1.SynthesisStatus_SYNTHESIS_STATUS_PLAYING,
				Chunk: &napv1.AudioChunk{
					Data:       []byte{1, 2, 3, 4},
					Sequence:   1,
					DurationMs: 100,
					First:      true,
					Last:       false,
					Metadata:   map[string]string{"part": "1"},
				},
				Timestamp: timestamppb.Now(),
			},
			{
				Status: napv1.SynthesisStatus_SYNTHESIS_STATUS_FINISHED,
				Chunk: &napv1.AudioChunk{
					Data:       []byte{5, 6, 7, 8},
					Sequence:   2,
					DurationMs: 100,
					First:      false,
					Last:       true,
				},
			},
		},
	}

	_, dialer := startMockTTSServer(t, srv)
	ctx := ContextWithDialer(context.Background(), dialer)

	endpoint := configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.test",
		Transport: "grpc",
		Address:   "bufconn",
	}
	synth, err := newNAPSynthesizer(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
	}, endpoint)
	if err != nil {
		t.Fatalf("create synthesizer: %v", err)
	}
	defer synth.Close(context.Background())

	chunks, err := synth.Speak(ctx, SpeakRequest{
		SessionID: "sess",
		StreamID:  slots.TTS,
		Text:      "Hello world",
	})
	if err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if string(chunks[0].Data) != string([]byte{1, 2, 3, 4}) {
		t.Fatalf("unexpected first chunk data: %v", chunks[0].Data)
	}
	if chunks[0].Duration != 100*time.Millisecond {
		t.Fatalf("expected 100ms duration, got %v", chunks[0].Duration)
	}
	if chunks[0].Metadata["part"] != "1" {
		t.Fatalf("expected metadata part=1, got %+v", chunks[0].Metadata)
	}
	if !chunks[1].Final {
		t.Fatalf("expected last chunk to be final")
	}
}

func TestNAPSynthesizerSpeakHandlesAdapterError(t *testing.T) {
	srv := &mockTTSServer{
		chunks: []*napv1.SynthesisResponse{
			{
				Status:       napv1.SynthesisStatus_SYNTHESIS_STATUS_ERROR,
				ErrorMessage: "voice model not found",
			},
		},
	}

	_, dialer := startMockTTSServer(t, srv)
	ctx := ContextWithDialer(context.Background(), dialer)

	synth, err := newNAPSynthesizer(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
	}, configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.test",
		Transport: "grpc",
		Address:   "bufconn",
	})
	if err != nil {
		t.Fatalf("create synthesizer: %v", err)
	}
	defer synth.Close(context.Background())

	_, err = synth.Speak(ctx, SpeakRequest{Text: "test"})
	if err == nil {
		t.Fatalf("expected error from adapter")
	}
	if !strings.Contains(err.Error(), "voice model not found") {
		t.Fatalf("expected adapter error message, got: %v", err)
	}
}

func TestNAPSynthesizerSpeakHandlesServerStreamError(t *testing.T) {
	srv := &mockTTSServer{
		streamErr: grpcstatus.Error(codes.Internal, "synthesis engine crashed"),
	}

	_, dialer := startMockTTSServer(t, srv)
	ctx := ContextWithDialer(context.Background(), dialer)

	synth, err := newNAPSynthesizer(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
	}, configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.test",
		Transport: "grpc",
		Address:   "bufconn",
	})
	if err != nil {
		t.Fatalf("create synthesizer: %v", err)
	}
	defer synth.Close(context.Background())

	_, err = synth.Speak(ctx, SpeakRequest{Text: "test"})
	if err == nil {
		t.Fatalf("expected error from stream")
	}
	if !strings.Contains(err.Error(), "receive synthesis chunk") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNAPSynthesizerSpeakClientCancel(t *testing.T) {
	srv := &mockTTSServer{
		streamErr: grpcstatus.Error(codes.Canceled, "cancelled"),
	}

	_, dialer := startMockTTSServer(t, srv)
	ctx := ContextWithDialer(context.Background(), dialer)

	synth, err := newNAPSynthesizer(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
	}, configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.test",
		Transport: "grpc",
		Address:   "bufconn",
	})
	if err != nil {
		t.Fatalf("create synthesizer: %v", err)
	}
	defer synth.Close(context.Background())

	// Server returns Canceled but our ctx is not cancelled — should be treated as error.
	_, err = synth.Speak(ctx, SpeakRequest{Text: "test"})
	if err == nil {
		t.Fatalf("expected error when server cancels but client ctx is active")
	}
}

func TestNAPSynthesizerSpeakContextCancelled(t *testing.T) {
	srv := &mockTTSServer{
		streamErr: grpcstatus.Error(codes.Canceled, "cancelled"),
	}

	_, dialer := startMockTTSServer(t, srv)
	ctx := ContextWithDialer(context.Background(), dialer)

	synth, err := newNAPSynthesizer(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
	}, configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.test",
		Transport: "grpc",
		Address:   "bufconn",
	})
	if err != nil {
		t.Fatalf("create synthesizer: %v", err)
	}
	defer synth.Close(context.Background())

	// Cancel our context before calling Speak — should return context.Canceled.
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	_, err = synth.Speak(cancelCtx, SpeakRequest{Text: "test"})
	if err == nil {
		t.Fatalf("expected error when context is cancelled")
	}
}

func TestNAPSynthesizerUnsupportedTransport(t *testing.T) {
	_, err := newNAPSynthesizer(context.Background(), SessionParams{}, configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.test",
		Transport: "websocket",
		Address:   "localhost:1234",
	})
	if err == nil {
		t.Fatalf("expected error for unsupported transport")
	}
	if !strings.Contains(err.Error(), "unsupported transport") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNAPSynthesizerMissingAddress(t *testing.T) {
	_, err := newNAPSynthesizer(context.Background(), SessionParams{}, configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.test",
		Transport: "grpc",
		Address:   "",
	})
	if err == nil {
		t.Fatalf("expected error for missing address")
	}
	if !strings.Contains(err.Error(), "missing address") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAdapterFactoryCreatesNAPSynthesizer(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	const adapterID = "adapter.tts.test"
	if err := store.UpsertAdapter(ctx, configstore.Adapter{
		ID:      adapterID,
		Source:  "local",
		Type:    "tts",
		Name:    "Test TTS",
		Version: "dev",
	}); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	srv := &mockTTSServer{
		chunks: []*napv1.SynthesisResponse{
			{
				Status: napv1.SynthesisStatus_SYNTHESIS_STATUS_FINISHED,
				Chunk: &napv1.AudioChunk{
					Data:       []byte{0xAA, 0xBB},
					Sequence:   1,
					DurationMs: 50,
					First:      true,
					Last:       true,
				},
			},
		},
	}

	_, dialer := startMockTTSServer(t, srv)

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapterID,
		Transport: "grpc",
		Address:   "bufconn",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	factory := NewAdapterFactory(store, nil)
	ctx = ContextWithDialer(ctx, dialer)

	synth, err := factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
	})
	if err != nil {
		t.Fatalf("create synthesizer: %v", err)
	}

	chunks, err := synth.Speak(ctx, SpeakRequest{
		SessionID: "sess",
		StreamID:  slots.TTS,
		Text:      "hello",
	})
	if err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(chunks) != 1 || !chunks[0].Final {
		t.Fatalf("expected 1 final chunk, got %+v", chunks)
	}
	if _, err := synth.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestAdapterFactoryProcessTransportUsesRuntimeAddress(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	const adapterID = "adapter.tts.process"
	if err := store.UpsertAdapter(ctx, configstore.Adapter{
		ID:      adapterID,
		Source:  "local",
		Type:    "tts",
		Name:    "Process TTS",
		Version: "dev",
	}); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	srv := &mockTTSServer{
		chunks: []*napv1.SynthesisResponse{
			{
				Status: napv1.SynthesisStatus_SYNTHESIS_STATUS_FINISHED,
				Chunk: &napv1.AudioChunk{
					Data:       []byte{0xFF},
					Sequence:   1,
					DurationMs: 10,
					First:      true,
					Last:       true,
				},
			},
		},
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	napv1.RegisterTextToSpeechServiceServer(server, srv)
	go server.Serve(listener)
	t.Cleanup(func() {
		server.GracefulStop()
		listener.Close()
	})

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapterID,
		Transport: "process",
		Command:   "serve",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	runtime := runtimeSourceStub{
		statuses: []adapters.BindingStatus{
			{
				Slot:      adapters.SlotTTS,
				AdapterID: strPtr(adapterID),
				Status:    configstore.BindingStatusActive,
				Runtime: &adapters.RuntimeStatus{
					AdapterID: adapterID,
					Health:    eventbus.AdapterHealthReady,
					Extra: map[string]string{
						adapters.RuntimeExtraAddress:   listener.Addr().String(),
						adapters.RuntimeExtraTransport: "process",
					},
				},
			},
		},
	}

	factory := NewAdapterFactory(store, runtime)
	synth, err := factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
	})
	if err != nil {
		t.Fatalf("create synthesizer: %v", err)
	}

	chunks, err := synth.Speak(ctx, SpeakRequest{Text: "test"})
	if err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	synth.Close(context.Background())
}

func TestAdapterFactoryRuntimeLookupErrors(t *testing.T) {
	t.Run("awaiting address returns ErrAdapterUnavailable", func(t *testing.T) {
		factory := adapterFactory{
			runtime: runtimeSourceStub{statuses: []adapters.BindingStatus{
				{
					AdapterID: strPtr("adapter.tts"),
					Status:    configstore.BindingStatusActive,
					Runtime:   nil,
				},
			}},
		}
		_, err := factory.lookupRuntimeAddress(context.Background(), "adapter.tts")
		if err == nil {
			t.Fatalf("expected error")
		}
		if !errors.Is(err, ErrAdapterUnavailable) {
			t.Fatalf("expected ErrAdapterUnavailable, got: %v", err)
		}
		if !strings.Contains(err.Error(), "awaiting runtime address") {
			t.Fatalf("expected descriptive message, got: %v", err)
		}
	})

	t.Run("not running returns ErrAdapterUnavailable", func(t *testing.T) {
		factory := adapterFactory{
			runtime: runtimeSourceStub{statuses: []adapters.BindingStatus{
				{
					AdapterID: strPtr("another.adapter"),
				},
			}},
		}
		_, err := factory.lookupRuntimeAddress(context.Background(), "adapter.tts")
		if err == nil {
			t.Fatalf("expected error")
		}
		if !errors.Is(err, ErrAdapterUnavailable) {
			t.Fatalf("expected ErrAdapterUnavailable, got: %v", err)
		}
		if !strings.Contains(err.Error(), "not running") {
			t.Fatalf("expected descriptive message, got: %v", err)
		}
	})

	t.Run("duplicate entries is not ErrAdapterUnavailable", func(t *testing.T) {
		factory := adapterFactory{
			runtime: runtimeSourceStub{statuses: []adapters.BindingStatus{
				{AdapterID: strPtr("adapter.tts")},
				{AdapterID: strPtr("adapter.tts")},
			}},
		}
		_, err := factory.lookupRuntimeAddress(context.Background(), "adapter.tts")
		if err == nil {
			t.Fatalf("expected error")
		}
		if errors.Is(err, ErrAdapterUnavailable) {
			t.Fatalf("duplicate entries should NOT be ErrAdapterUnavailable")
		}
	})

	t.Run("nil runtime", func(t *testing.T) {
		factory := adapterFactory{runtime: nil}
		_, err := factory.lookupRuntimeAddress(context.Background(), "adapter.tts")
		if err == nil {
			t.Fatalf("expected error")
		}
	})
}

func TestNAPSynthesizerMergesResponseMetadata(t *testing.T) {
	srv := &mockTTSServer{
		chunks: []*napv1.SynthesisResponse{
			{
				Status:   napv1.SynthesisStatus_SYNTHESIS_STATUS_PLAYING,
				Metadata: map[string]string{"voice": "alloy", "model": "v2"},
				Chunk: &napv1.AudioChunk{
					Data:       []byte{1, 2},
					Sequence:   1,
					DurationMs: 50,
					First:      true,
					Last:       false,
					Metadata:   map[string]string{"model": "v3", "chunk_id": "c1"},
				},
			},
			{
				Status:   napv1.SynthesisStatus_SYNTHESIS_STATUS_FINISHED,
				Metadata: map[string]string{"voice": "alloy"},
				Chunk: &napv1.AudioChunk{
					Data:       []byte{3, 4},
					Sequence:   2,
					DurationMs: 50,
					First:      false,
					Last:       true,
				},
			},
		},
	}

	_, dialer := startMockTTSServer(t, srv)
	ctx := ContextWithDialer(context.Background(), dialer)

	synth, err := newNAPSynthesizer(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
	}, configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.test",
		Transport: "grpc",
		Address:   "bufconn",
	})
	if err != nil {
		t.Fatalf("create synthesizer: %v", err)
	}
	defer synth.Close(context.Background())

	chunks, err := synth.Speak(ctx, SpeakRequest{Text: "test"})
	if err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}

	// Chunk-level "model" should override response-level "model".
	if chunks[0].Metadata["model"] != "v3" {
		t.Fatalf("expected chunk-level model=v3 to win, got %s", chunks[0].Metadata["model"])
	}
	// Response-level "voice" should be present.
	if chunks[0].Metadata["voice"] != "alloy" {
		t.Fatalf("expected response-level voice=alloy, got %s", chunks[0].Metadata["voice"])
	}
	// Chunk-level "chunk_id" should be preserved.
	if chunks[0].Metadata["chunk_id"] != "c1" {
		t.Fatalf("expected chunk_id=c1, got %s", chunks[0].Metadata["chunk_id"])
	}

	// Second chunk: response metadata merged (no chunk metadata).
	if chunks[1].Metadata["voice"] != "alloy" {
		t.Fatalf("expected response-level voice on second chunk, got %+v", chunks[1].Metadata)
	}
}

func TestNAPSynthesizerStatusOnlyResponsePropagatesMetadata(t *testing.T) {
	srv := &mockTTSServer{
		chunks: []*napv1.SynthesisResponse{
			{
				Status: napv1.SynthesisStatus_SYNTHESIS_STATUS_PLAYING,
				Chunk: &napv1.AudioChunk{
					Data:       []byte{1, 2},
					Sequence:   1,
					DurationMs: 50,
					First:      true,
					Last:       true,
				},
			},
			{
				// Status-only response with metadata, no chunk.
				Status:   napv1.SynthesisStatus_SYNTHESIS_STATUS_FINISHED,
				Metadata: map[string]string{"synthesis_time_ms": "320"},
			},
		},
	}

	_, dialer := startMockTTSServer(t, srv)
	ctx := ContextWithDialer(context.Background(), dialer)

	synth, err := newNAPSynthesizer(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
	}, configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.test",
		Transport: "grpc",
		Address:   "bufconn",
	})
	if err != nil {
		t.Fatalf("create synthesizer: %v", err)
	}
	defer synth.Close(context.Background())

	chunks, err := synth.Speak(ctx, SpeakRequest{Text: "test"})
	if err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	// Status-only metadata should be merged into the last chunk.
	if chunks[0].Metadata["synthesis_time_ms"] != "320" {
		t.Fatalf("expected status-only metadata to be merged, got %+v", chunks[0].Metadata)
	}
}

func TestNAPSynthesizerStatusOnlyBeforeFirstChunkBuffersMetadata(t *testing.T) {
	srv := &mockTTSServer{
		chunks: []*napv1.SynthesisResponse{
			{
				// Status-only before any chunk — metadata should be buffered.
				Status:   napv1.SynthesisStatus_SYNTHESIS_STATUS_STARTED,
				Metadata: map[string]string{"voice_id": "EXAVITQu4vr4xnSDxMaL", "latency": "fast"},
			},
			{
				Status: napv1.SynthesisStatus_SYNTHESIS_STATUS_PLAYING,
				Chunk: &napv1.AudioChunk{
					Data:       []byte{10, 20},
					Sequence:   1,
					DurationMs: 50,
					First:      true,
					Last:       true,
					Metadata:   map[string]string{"latency": "normal"},
				},
			},
		},
	}

	_, dialer := startMockTTSServer(t, srv)
	ctx := ContextWithDialer(context.Background(), dialer)

	synth, err := newNAPSynthesizer(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
	}, configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.test",
		Transport: "grpc",
		Address:   "bufconn",
	})
	if err != nil {
		t.Fatalf("create synthesizer: %v", err)
	}
	defer synth.Close(context.Background())

	chunks, err := synth.Speak(ctx, SpeakRequest{Text: "test"})
	if err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	// Buffered metadata "voice_id" should be present on the first chunk.
	if chunks[0].Metadata["voice_id"] != "EXAVITQu4vr4xnSDxMaL" {
		t.Fatalf("expected buffered voice_id, got %+v", chunks[0].Metadata)
	}
	// Chunk-level "latency" should override buffered "latency".
	if chunks[0].Metadata["latency"] != "normal" {
		t.Fatalf("expected chunk-level latency=normal to win over buffered, got %s", chunks[0].Metadata["latency"])
	}
}

func TestNAPSynthesizerStatusOnlyUpdatesOverridePrevious(t *testing.T) {
	srv := &mockTTSServer{
		chunks: []*napv1.SynthesisResponse{
			{
				Status:   napv1.SynthesisStatus_SYNTHESIS_STATUS_STARTED,
				Metadata: map[string]string{"progress": "init", "voice": "v1"},
			},
			{
				// Second status-only should override "progress" from first.
				Status:   napv1.SynthesisStatus_SYNTHESIS_STATUS_STARTED,
				Metadata: map[string]string{"progress": "ready"},
			},
			{
				Status: napv1.SynthesisStatus_SYNTHESIS_STATUS_PLAYING,
				Chunk: &napv1.AudioChunk{
					Data:       []byte{1},
					Sequence:   1,
					DurationMs: 10,
					First:      true,
					Last:       false,
				},
			},
			{
				// Status-only after chunk — should update metadata.
				Status:   napv1.SynthesisStatus_SYNTHESIS_STATUS_PLAYING,
				Metadata: map[string]string{"progress": "streaming"},
			},
			{
				// Another status-only — "progress" should be overridden again.
				Status:   napv1.SynthesisStatus_SYNTHESIS_STATUS_FINISHED,
				Metadata: map[string]string{"progress": "done"},
			},
		},
	}

	_, dialer := startMockTTSServer(t, srv)
	ctx := ContextWithDialer(context.Background(), dialer)

	synth, err := newNAPSynthesizer(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
	}, configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.test",
		Transport: "grpc",
		Address:   "bufconn",
	})
	if err != nil {
		t.Fatalf("create synthesizer: %v", err)
	}
	defer synth.Close(context.Background())

	chunks, err := synth.Speak(ctx, SpeakRequest{Text: "test"})
	if err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}

	// Pre-chunk: second status-only "progress=ready" should override first "progress=init".
	// Then chunk arrives → pendingMeta merged. voice=v1 should survive (no conflict from chunk).
	if chunks[0].Metadata["voice"] != "v1" {
		t.Fatalf("expected buffered voice=v1, got %+v", chunks[0].Metadata)
	}

	// Post-chunk: two status-only updates. Last one wins: "progress=done".
	if chunks[0].Metadata["progress"] != "done" {
		t.Fatalf("expected latest status-only progress=done, got %s", chunks[0].Metadata["progress"])
	}
}

func TestNAPSynthesizerMetadataOnlyWhenNoAudioChunks(t *testing.T) {
	srv := &mockTTSServer{
		chunks: []*napv1.SynthesisResponse{
			{
				Status:   napv1.SynthesisStatus_SYNTHESIS_STATUS_STARTED,
				Metadata: map[string]string{"adapter": "elevenlabs", "model": "v2"},
			},
			{
				Status:   napv1.SynthesisStatus_SYNTHESIS_STATUS_FINISHED,
				Metadata: map[string]string{"reason": "empty_text"},
			},
		},
	}

	_, dialer := startMockTTSServer(t, srv)
	ctx := ContextWithDialer(context.Background(), dialer)

	synth, err := newNAPSynthesizer(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
	}, configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.test",
		Transport: "grpc",
		Address:   "bufconn",
	})
	if err != nil {
		t.Fatalf("create synthesizer: %v", err)
	}
	defer synth.Close(context.Background())

	chunks, err := synth.Speak(ctx, SpeakRequest{Text: ""})
	if err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 metadata-only chunk, got %d", len(chunks))
	}
	if !chunks[0].Final {
		t.Fatalf("expected final flag on metadata-only chunk")
	}
	if len(chunks[0].Data) != 0 {
		t.Fatalf("expected no audio data, got %d bytes", len(chunks[0].Data))
	}
	if chunks[0].Metadata["adapter"] != "elevenlabs" {
		t.Fatalf("expected adapter metadata, got %+v", chunks[0].Metadata)
	}
	if chunks[0].Metadata["reason"] != "empty_text" {
		t.Fatalf("expected reason=empty_text (later update), got %+v", chunks[0].Metadata)
	}
}

func TestNAPSynthesizerEnforceFinalOnStreamEnd(t *testing.T) {
	srv := &mockTTSServer{
		chunks: []*napv1.SynthesisResponse{
			{
				Status: napv1.SynthesisStatus_SYNTHESIS_STATUS_PLAYING,
				Chunk: &napv1.AudioChunk{
					Data:       []byte{1, 2},
					Sequence:   1,
					DurationMs: 50,
					First:      true,
					Last:       false, // not final
				},
			},
			{
				Status: napv1.SynthesisStatus_SYNTHESIS_STATUS_PLAYING,
				Chunk: &napv1.AudioChunk{
					Data:       []byte{3, 4},
					Sequence:   2,
					DurationMs: 50,
					First:      false,
					Last:       false, // also not final — stream ends via EOF
				},
			},
			{
				// Terminal status-only, no chunk.
				Status: napv1.SynthesisStatus_SYNTHESIS_STATUS_FINISHED,
			},
		},
	}

	_, dialer := startMockTTSServer(t, srv)
	ctx := ContextWithDialer(context.Background(), dialer)

	synth, err := newNAPSynthesizer(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
	}, configstore.AdapterEndpoint{
		AdapterID: "adapter.tts.test",
		Transport: "grpc",
		Address:   "bufconn",
	})
	if err != nil {
		t.Fatalf("create synthesizer: %v", err)
	}
	defer synth.Close(context.Background())

	chunks, err := synth.Speak(ctx, SpeakRequest{Text: "test"})
	if err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	// First chunk should NOT be final.
	if chunks[0].Final {
		t.Fatalf("first chunk should not be final")
	}
	// Last chunk must be forced to Final even though chunk.last was false.
	if !chunks[1].Final {
		t.Fatalf("last chunk must be Final after stream ends")
	}
}

func TestAdapterFactoryRuntimeOverviewErrorMapsToUnavailable(t *testing.T) {
	factory := adapterFactory{
		runtime: runtimeSourceStub{err: errors.New("service temporarily unavailable")},
	}
	_, err := factory.lookupRuntimeAddress(context.Background(), "adapter.tts")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("expected ErrAdapterUnavailable for Overview failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "temporarily unavailable") {
		t.Fatalf("expected original error message preserved, got: %v", err)
	}
}

func TestAdapterFactoryRuntimeLookupTimeout(t *testing.T) {
	block := make(chan struct{})
	factory := adapterFactory{
		runtime: blockingRuntimeSource{block: block},
	}

	start := time.Now()
	_, err := factory.lookupRuntimeAddress(context.Background(), "adapter.tts")
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("expected ErrAdapterUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("expected deadline info in error message, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < runtimeLookupTimeout {
		t.Fatalf("expected lookup to last at least %v, got %v", runtimeLookupTimeout, elapsed)
	}
}

type blockingRuntimeSource struct {
	block chan struct{}
}

func (b blockingRuntimeSource) Overview(ctx context.Context) ([]adapters.BindingStatus, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.block:
		return nil, nil
	}
}

type runtimeSourceStub struct {
	statuses []adapters.BindingStatus
	err      error
}

func (s runtimeSourceStub) Overview(context.Context) ([]adapters.BindingStatus, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.statuses, nil
}

func strPtr(v string) *string {
	return &v
}
