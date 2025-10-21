package stt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/modules"
	testutil "github.com/nupi-ai/nupi/internal/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

func TestModuleFactoryCreatesMockTranscriber(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := modules.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, string(modules.SlotSTTPrimary), modules.MockSTTAdapterID, map[string]any{
		"text": "factory",
	}); err != nil {
		t.Fatalf("activate mock adapter: %v", err)
	}

	factory := NewModuleFactory(store)

	params := SessionParams{
		SessionID: "sess",
		StreamID:  "mic",
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
	}

	transcriber, err := factory.Create(ctx, params)
	if err != nil {
		t.Fatalf("create transcriber: %v", err)
	}

	segment := eventbus.AudioIngressSegmentEvent{
		SessionID: "sess",
		StreamID:  "mic",
		Sequence:  1,
		Format:    params.Format,
		Data:      make([]byte, 640),
		Duration:  20 * time.Millisecond,
		First:     true,
		Last:      true,
		StartedAt: time.Now().UTC(),
		EndedAt:   time.Now().UTC().Add(20 * time.Millisecond),
	}

	transcripts, err := transcriber.OnSegment(ctx, segment)
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if len(transcripts) == 0 || transcripts[0].Text != "factory" {
		t.Fatalf("unexpected transcript: %+v", transcripts)
	}
}

func TestModuleFactoryReturnsErrorOnConfigParseFailure(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := modules.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(modules.SlotSTTPrimary), modules.MockSTTAdapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	_, err := store.DB().ExecContext(ctx, `
        UPDATE adapter_bindings
        SET config = '{invalid'
        WHERE slot = ? AND instance_name = ? AND profile_name = ?
    `, string(modules.SlotSTTPrimary), store.InstanceName(), store.ProfileName())
	if err != nil {
		t.Fatalf("corrupt config: %v", err)
	}

	factory := NewModuleFactory(store)
	_, err = factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  "mic",
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
	})
	if err == nil {
		t.Fatalf("expected error due to invalid config")
	}
}

func TestModuleFactoryReturnsUnavailableWhenAdapterMissing(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	factory := NewModuleFactory(store)
	_, err := factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  "mic",
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
	})
	if !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("expected ErrAdapterUnavailable, got %v", err)
	}
}

func TestModuleFactoryCreatesNAPTranscriber(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	const adapterID = "adapter.stt.test"
	if err := store.UpsertAdapter(ctx, configstore.Adapter{
		ID:      adapterID,
		Source:  "local",
		Type:    "stt",
		Name:    "Test STT",
		Version: "dev",
	}); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	bufListener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	napv1.RegisterSpeechToTextServiceServer(server, &mockSTTServer{})
	go server.Serve(bufListener)
	t.Cleanup(func() {
		server.GracefulStop()
	})

	if err := store.SetActiveAdapter(ctx, string(modules.SlotSTTPrimary), adapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}
	if err := store.UpsertModuleEndpoint(ctx, configstore.ModuleEndpoint{
		AdapterID: adapterID,
		Transport: "grpc",
		Address:   "bufconn",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return bufListener.DialContext(ctx)
	}

	factory := NewModuleFactory(store)
	ctx = ContextWithDialer(ctx, dialer)
	params := SessionParams{
		SessionID: "sess",
		StreamID:  "mic",
		Format: eventbus.AudioFormat{
			Encoding:      eventbus.AudioEncodingPCM16,
			SampleRate:    16000,
			Channels:      1,
			BitDepth:      16,
			FrameDuration: 20 * time.Millisecond,
		},
	}

	transcriber, err := factory.Create(ctx, params)
	if err != nil {
		t.Fatalf("create nap transcriber: %v", err)
	}

	segment := eventbus.AudioIngressSegmentEvent{
		SessionID: "sess",
		StreamID:  "mic",
		Sequence:  1,
		Format:    params.Format,
		Data:      make([]byte, 640),
		Duration:  20 * time.Millisecond,
		First:     true,
		Last:      false,
		StartedAt: time.Now().UTC(),
		EndedAt:   time.Now().UTC().Add(20 * time.Millisecond),
	}

	transcripts, err := transcriber.OnSegment(ctx, segment)
	if err != nil {
		t.Fatalf("transcribe segment: %v", err)
	}
	final, err := transcriber.Close(ctx)
	if err != nil {
		t.Fatalf("close transcriber: %v", err)
	}

	combined := append([]Transcription{}, transcripts...)
	combined = append(combined, final...)

	foundSeq := false
	foundFinal := false
	for _, tr := range combined {
		if tr.Text == "seq-1" && !tr.Final {
			foundSeq = true
		}
		if tr.Text == "final" && tr.Final {
			foundFinal = true
		}
	}
	if !foundSeq || !foundFinal {
		t.Fatalf("missing expected transcripts, combined=%+v", combined)
	}
}

type mockSTTServer struct {
	napv1.UnimplementedSpeechToTextServiceServer
}

func (s *mockSTTServer) StreamTranscription(stream napv1.SpeechToTextService_StreamTranscriptionServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		if req.GetFlush() {
			if err := stream.Send(&napv1.Transcript{
				Sequence: 999,
				Text:     "final",
				Final:    true,
			}); err != nil {
				return err
			}
			return nil
		}

		if seg := req.GetSegment(); seg != nil {
			resp := &napv1.Transcript{
				Sequence:   seg.GetSequence(),
				Text:       fmt.Sprintf("seq-%d", seg.GetSequence()),
				Confidence: 0.9,
				Final:      false,
				StartedAt:  seg.GetStartedAt(),
				EndedAt:    seg.GetEndedAt(),
			}
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
	}
}
