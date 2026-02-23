package vad

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	"github.com/nupi-ai/nupi/internal/audio/adapterutil"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/napdial"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
	testutil "github.com/nupi-ai/nupi/internal/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestAdapterFactoryReturnsMockAnalyzer(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.05,
		"min_frames": 2,
	}); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	factory := NewAdapterFactory(store, nil)
	analyzer, err := factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  "mic",
	})
	if err != nil {
		t.Fatalf("expected analyzer, got error: %v", err)
	}
	mock, ok := analyzer.(*mockAnalyzer)
	if !ok {
		t.Fatalf("expected *mockAnalyzer, got %T", analyzer)
	}
	if mock.threshold != 0.05 {
		t.Fatalf("expected threshold 0.05, got %f", mock.threshold)
	}
	if mock.minFrames != 2 {
		t.Fatalf("expected minFrames 2, got %d", mock.minFrames)
	}
}

func TestAdapterFactoryReturnsErrorOnConfigParseFailure(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	_, err := store.DB().ExecContext(ctx, `
        UPDATE adapter_bindings
        SET config = '{invalid'
        WHERE slot = ? AND instance_name = ? AND profile_name = ?
    `, string(adapters.SlotVAD), store.InstanceName(), store.ProfileName())
	if err != nil {
		t.Fatalf("corrupt config: %v", err)
	}

	factory := NewAdapterFactory(store, nil)
	_, err = factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  "mic",
	})
	if err == nil {
		t.Fatalf("expected error due to invalid config")
	}
}

func TestAdapterFactoryReturnsUnavailableWhenAdapterMissing(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	factory := NewAdapterFactory(store, nil)
	_, err := factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  "mic",
	})
	if !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("expected ErrAdapterUnavailable, got %v", err)
	}
}

func TestAdapterFactoryCreatesNAPAnalyzer(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	const adapterID = "adapter.vad.test"
	if err := store.UpsertAdapter(ctx, configstore.Adapter{
		ID:      adapterID,
		Source:  "local",
		Type:    "vad",
		Name:    "Test VAD",
		Version: "dev",
	}); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	bufListener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	napv1.RegisterVoiceActivityDetectionServiceServer(server, &mockVADServer{})
	go server.Serve(bufListener)
	t.Cleanup(func() {
		server.GracefulStop()
	})

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapterID,
		Transport: "grpc",
		Address:   "bufconn",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return bufListener.DialContext(ctx)
	}

	factory := NewAdapterFactory(store, nil)
	ctx = napdial.ContextWithDialer(ctx, dialer)
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

	analyzer, err := factory.Create(ctx, params)
	if err != nil {
		t.Fatalf("create nap analyzer: %v", err)
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

	detections, err := analyzer.OnSegment(ctx, segment)
	if err != nil {
		t.Fatalf("analyze segment: %v", err)
	}
	final, err := analyzer.Close(ctx)
	if err != nil {
		t.Fatalf("close analyzer: %v", err)
	}

	combined := append([]Detection{}, detections...)
	combined = append(combined, final...)

	var foundStart bool
	for _, det := range combined {
		if det.Active {
			foundStart = true
		}
	}
	if !foundStart {
		t.Fatalf("expected at least one active detection, combined=%+v", combined)
	}
}

func TestAdapterFactoryProcessTransportUsesRuntimeAddress(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	const adapterID = "adapter.vad.process"
	if err := store.UpsertAdapter(ctx, configstore.Adapter{
		ID:      adapterID,
		Source:  "local",
		Type:    "vad",
		Name:    "Process VAD",
		Version: "dev",
	}); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("tcp listen unavailable (sandboxed environment?): %v", err)
	}
	server := grpc.NewServer()
	napv1.RegisterVoiceActivityDetectionServiceServer(server, &mockVADServer{})
	go server.Serve(listener)
	t.Cleanup(func() {
		server.GracefulStop()
		listener.Close()
	})

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapterID,
		Transport: "process",
		Command:   "serve",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	addr := listener.Addr().String()
	runtime := runtimeSourceStub{
		statuses: []adapters.BindingStatus{
			{
				Slot:      adapters.SlotVAD,
				AdapterID: strPtr(adapterID),
				Status:    configstore.BindingStatusActive,
				Runtime: &adapters.RuntimeStatus{
					AdapterID: adapterID,
					Health:    eventbus.AdapterHealthReady,
					Extra: map[string]string{
						adapters.RuntimeExtraAddress:   addr,
						adapters.RuntimeExtraTransport: "process",
					},
				},
			},
		},
	}

	factory := NewAdapterFactory(store, runtime)
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

	analyzer, err := factory.Create(ctx, params)
	if err != nil {
		t.Fatalf("create analyzer: %v", err)
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

	detections, err := analyzer.OnSegment(ctx, segment)
	if err != nil {
		t.Fatalf("analyze segment: %v", err)
	}
	final, err := analyzer.Close(ctx)
	if err != nil {
		t.Fatalf("close analyzer: %v", err)
	}

	combined := append([]Detection{}, detections...)
	combined = append(combined, final...)

	var gotStart bool
	for _, det := range combined {
		if det.Active {
			gotStart = true
		}
	}
	if !gotStart {
		t.Fatalf("missing expected active detection: %+v", combined)
	}
}

func TestAdapterFactoryReturnsUnavailableForMissingEndpoint(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	const adapterID = "adapter.vad.noendpoint"
	if err := store.UpsertAdapter(ctx, configstore.Adapter{
		ID:      adapterID,
		Source:  "local",
		Type:    "vad",
		Name:    "No Endpoint VAD",
		Version: "dev",
	}); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	factory := NewAdapterFactory(store, nil)
	_, err := factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  "mic",
	})
	if !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("expected ErrAdapterUnavailable for missing endpoint, got: %v", err)
	}
}

func TestNAPAnalyzerMapsAllEventTypes(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	const adapterID = "adapter.vad.allevents"
	if err := store.UpsertAdapter(ctx, configstore.Adapter{
		ID:      adapterID,
		Source:  "local",
		Type:    "vad",
		Name:    "AllEvents VAD",
		Version: "dev",
	}); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	bufListener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	napv1.RegisterVoiceActivityDetectionServiceServer(server, &mockVADServerAllEvents{})
	go server.Serve(bufListener)
	t.Cleanup(func() {
		server.GracefulStop()
	})

	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapterID,
		Transport: "grpc",
		Address:   "bufconn",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return bufListener.DialContext(ctx)
	}

	factory := NewAdapterFactory(store, nil)
	ctx = napdial.ContextWithDialer(ctx, dialer)
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

	analyzer, err := factory.Create(ctx, params)
	if err != nil {
		t.Fatalf("create nap analyzer: %v", err)
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

	detections, err := analyzer.OnSegment(ctx, segment)
	if err != nil {
		t.Fatalf("analyze segment: %v", err)
	}
	final, err := analyzer.Close(ctx)
	if err != nil {
		t.Fatalf("close analyzer: %v", err)
	}

	combined := append([]Detection{}, detections...)
	combined = append(combined, final...)

	// The mock sends START (Active=true), ONGOING (Active=true), END (Active=false).
	var gotActive, gotInactive bool
	for _, det := range combined {
		if det.Active {
			gotActive = true
		} else {
			gotInactive = true
		}
	}
	if !gotActive {
		t.Fatalf("expected Active=true detection (from START or ONGOING), got: %+v", combined)
	}
	if !gotInactive {
		t.Fatalf("expected Active=false detection (from END), got: %+v", combined)
	}
}

func TestAdapterFactoryProcessTransportPendingReturnsUnavailable(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	const adapterID = "adapter.vad.pending"
	if err := store.UpsertAdapter(ctx, configstore.Adapter{
		ID:      adapterID,
		Source:  "local",
		Type:    "vad",
		Name:    "Pending VAD",
		Version: "dev",
	}); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapterID,
		Transport: "process",
		Command:   "serve",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	// Runtime reports adapter present but no address yet (still starting).
	runtime := runtimeSourceStub{statuses: []adapters.BindingStatus{
		{
			AdapterID: strPtr(adapterID),
			Status:    configstore.BindingStatusActive,
			Runtime:   nil,
		},
	}}

	factory := NewAdapterFactory(store, runtime)
	_, err := factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  "mic",
	})
	if !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("expected ErrAdapterUnavailable for pending process adapter, got: %v", err)
	}
}

func TestAdapterFactoryLookupRuntimeWithoutSource(t *testing.T) {
	_, err := adapterutil.LookupRuntimeAddress(context.Background(), nil, "adapter.process", "vad", ErrAdapterUnavailable)
	if err == nil {
		t.Fatalf("expected error due to missing runtime metadata")
	}
	if errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("missing runtime metadata should NOT be ErrAdapterUnavailable")
	}
}

func TestAdapterFactoryLookupRuntimeTimeout(t *testing.T) {
	block := make(chan struct{})
	runtime := blockingRuntimeSource{block: block}

	ctx := context.Background()
	start := time.Now()
	_, err := adapterutil.LookupRuntimeAddress(ctx, runtime, "adapter.process", "vad", ErrAdapterUnavailable)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
	if !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("timeout should be ErrAdapterUnavailable (transient), got %v", err)
	}
	if elapsed := time.Since(start); elapsed < adapterutil.RuntimeLookupTimeout {
		t.Fatalf("expected lookup to last at least %v, got %v", adapterutil.RuntimeLookupTimeout, elapsed)
	}
}

func TestAdapterFactoryLookupRuntimePendingAddress(t *testing.T) {
	runtime := runtimeSourceStub{statuses: []adapters.BindingStatus{
		{
			AdapterID: strPtr("adapter.process"),
			Status:    configstore.BindingStatusActive,
			Runtime:   nil,
		},
	}}

	_, err := adapterutil.LookupRuntimeAddress(context.Background(), runtime, "adapter.process", "vad", ErrAdapterUnavailable)
	if err == nil {
		t.Fatalf("expected error when runtime address missing")
	}
	if !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("awaiting address should be ErrAdapterUnavailable (transient), got %v", err)
	}
	if !strings.Contains(err.Error(), "awaiting runtime address") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAdapterFactoryLookupRuntimeAdapterNotRunning(t *testing.T) {
	runtime := runtimeSourceStub{statuses: []adapters.BindingStatus{
		{
			AdapterID: strPtr("another.adapter"),
		},
	}}

	_, err := adapterutil.LookupRuntimeAddress(context.Background(), runtime, "adapter.process", "vad", ErrAdapterUnavailable)
	if err == nil {
		t.Fatalf("expected error when adapter not running")
	}
	if !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("not running should be ErrAdapterUnavailable (transient), got %v", err)
	}
	if !strings.Contains(err.Error(), "not running") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAdapterFactoryLookupRuntimeDuplicateStatuses(t *testing.T) {
	runtime := runtimeSourceStub{statuses: []adapters.BindingStatus{
		{
			AdapterID: strPtr("adapter.process"),
		},
		{
			AdapterID: strPtr("adapter.process"),
		},
	}}

	_, err := adapterutil.LookupRuntimeAddress(context.Background(), runtime, "adapter.process", "vad", ErrAdapterUnavailable)
	if err == nil {
		t.Fatalf("expected error due to duplicate runtime entries")
	}
	if errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("duplicate entries should NOT be ErrAdapterUnavailable")
	}
	if !strings.Contains(err.Error(), "duplicate runtime entries") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAdapterFactoryLookupRuntimeDuplicateWithAddress(t *testing.T) {
	runtime := runtimeSourceStub{statuses: []adapters.BindingStatus{
		{
			AdapterID: strPtr("adapter.process"),
			Runtime: &adapters.RuntimeStatus{
				Extra: map[string]string{adapters.RuntimeExtraAddress: "127.0.0.1:9000"},
			},
		},
		{
			AdapterID: strPtr("adapter.process"),
		},
	}}

	_, err := adapterutil.LookupRuntimeAddress(context.Background(), runtime, "adapter.process", "vad", ErrAdapterUnavailable)
	if err == nil {
		t.Fatalf("expected error due to duplicate runtime entries even when address present")
	}
	if errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("duplicate entries should NOT be ErrAdapterUnavailable")
	}
	if !strings.Contains(err.Error(), "duplicate runtime entries") {
		t.Fatalf("unexpected error: %v", err)
	}
}

type runtimeSourceStub struct {
	statuses []adapters.BindingStatus
	err      error
}

type blockingRuntimeSource struct {
	block chan struct{}
}

func (s runtimeSourceStub) Overview(context.Context) ([]adapters.BindingStatus, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.statuses, nil
}

func (b blockingRuntimeSource) Overview(ctx context.Context) ([]adapters.BindingStatus, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.block:
		return nil, nil
	}
}

func strPtr(value string) *string {
	return &value
}

func TestNAPAnalyzerUnavailableWrapsError(t *testing.T) {
	// Dialer that refuses connections, simulating an unreachable adapter process.
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return nil, fmt.Errorf("connection refused")
	}
	ctx := napdial.ContextWithDialer(context.Background(), dialer)

	_, err := newNAPAnalyzer(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  "mic",
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
	}, configstore.AdapterEndpoint{
		AdapterID: "adapter.vad.test",
		Transport: "grpc",
		Address:   "bufconn",
	})
	if err == nil {
		t.Fatalf("expected error from unavailable adapter")
	}
	if !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("expected ErrAdapterUnavailable, got: %v", err)
	}
}

type mockVADServer struct {
	napv1.UnimplementedVoiceActivityDetectionServiceServer
}

func (s *mockVADServer) DetectSpeech(stream napv1.VoiceActivityDetectionService_DetectSpeechServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		// Skip init request (no pcm_data).
		if len(req.GetPcmData()) == 0 {
			continue
		}

		// Echo back a SPEECH_START event for each data request.
		if err := stream.Send(&napv1.SpeechEvent{
			Type:       napv1.SpeechEventType_SPEECH_EVENT_TYPE_START,
			Confidence: 0.95,
			Metadata: map[string]string{
				"seq": fmt.Sprintf("%s:%s", req.GetSessionId(), req.GetStreamId()),
			},
		}); err != nil {
			return err
		}
	}
}

// mockVADServerAllEvents sends START, ONGOING and END for each data chunk,
// exercising all three SpeechEventType mappings through the gRPC layer.
type mockVADServerAllEvents struct {
	napv1.UnimplementedVoiceActivityDetectionServiceServer
}

func (s *mockVADServerAllEvents) DetectSpeech(stream napv1.VoiceActivityDetectionService_DetectSpeechServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if len(req.GetPcmData()) == 0 {
			continue
		}

		events := []napv1.SpeechEventType{
			napv1.SpeechEventType_SPEECH_EVENT_TYPE_START,
			napv1.SpeechEventType_SPEECH_EVENT_TYPE_ONGOING,
			napv1.SpeechEventType_SPEECH_EVENT_TYPE_END,
		}
		for _, evtType := range events {
			if err := stream.Send(&napv1.SpeechEvent{
				Type:       evtType,
				Confidence: 0.85,
			}); err != nil {
				return err
			}
		}
	}
}

// mockVADServerPartialCrash sends one START event for the first data chunk then
// returns codes.Unavailable, simulating an adapter crash mid-stream.
type mockVADServerPartialCrash struct {
	napv1.UnimplementedVoiceActivityDetectionServiceServer
}

func (s *mockVADServerPartialCrash) DetectSpeech(stream napv1.VoiceActivityDetectionService_DetectSpeechServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if len(req.GetPcmData()) == 0 {
			continue
		}

		// Send one START event then crash.
		_ = stream.Send(&napv1.SpeechEvent{
			Type:       napv1.SpeechEventType_SPEECH_EVENT_TYPE_START,
			Confidence: 0.9,
		})
		return grpcstatus.Error(codes.Unavailable, "adapter process crashed")
	}
}

func TestNAPAnalyzerMidStreamUnavailableReportsError(t *testing.T) {
	bufListener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	napv1.RegisterVoiceActivityDetectionServiceServer(server, &mockVADServerPartialCrash{})
	go server.Serve(bufListener)
	t.Cleanup(func() {
		server.GracefulStop()
	})

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return bufListener.DialContext(ctx)
	}

	ctx := napdial.ContextWithDialer(context.Background(), dialer)
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

	analyzer, err := newNAPAnalyzer(ctx, params, configstore.AdapterEndpoint{
		AdapterID: "adapter.vad.crash",
		Transport: "grpc",
		Address:   "bufconn",
	})
	if err != nil {
		t.Fatalf("create analyzer: %v", err)
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

	// Send a segment; the mock replies with one START then crashes.
	// With Last=true, the grace-period wait gives the receiver time to capture
	// the Unavailable error in errCh before drainError checks it.
	_, segErr := analyzer.OnSegment(ctx, segment)
	if segErr == nil {
		// If timing prevented detection, a brief sleep + retry should surface it.
		time.Sleep(30 * time.Millisecond)
		_, segErr = analyzer.OnSegment(ctx, segment)
	}
	if !errors.Is(segErr, ErrAdapterUnavailable) {
		t.Fatalf("expected ErrAdapterUnavailable from mid-stream crash, got: %v", segErr)
	}

	// Close suppresses ErrAdapterUnavailable during intentional teardown.
	_, closeErr := analyzer.Close(ctx)
	if closeErr != nil {
		t.Fatalf("unexpected close error: %v", closeErr)
	}
}
