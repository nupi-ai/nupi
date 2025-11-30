package server

import (
	"context"
	"io"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/audio/egress"
	"github.com/nupi-ai/nupi/internal/audio/ingress"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	adapters "github.com/nupi-ai/nupi/internal/plugins/adapters"
	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/session"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// skipIfNoPTY skips the test if PTY operations are not available
func skipIfNoPTY(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests not supported on Windows")
	}
	p := pty.New()
	err := p.Start(pty.StartOptions{Command: "/bin/sh", Args: []string{"-c", "exit 0"}})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "operation not permitted") ||
			strings.Contains(msg, "permission denied") ||
			strings.Contains(msg, "no such file or directory") {
			t.Skipf("PTY not available: %v", err)
		}
	}
	if p != nil {
		p.Stop(100 * time.Millisecond)
	}
}

// createSessionOrSkip creates a session or skips the test if PTY is not available
func createSessionOrSkip(t *testing.T, sm *session.Manager, opts pty.StartOptions, attached bool) *session.Session {
	t.Helper()
	sess, err := sm.CreateSession(opts, attached)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "operation not permitted") ||
			strings.Contains(msg, "permission denied") ||
			strings.Contains(msg, "no such file or directory") ||
			strings.Contains(msg, "failed to start PTY") {
			t.Skipf("PTY not available: %v", err)
		}
		t.Fatalf("create session: %v", err)
	}
	return sess
}

func TestSessionsServiceGetConversation(t *testing.T) {
	skipIfNoPTY(t)
	apiServer, sessionManager := newTestAPIServer(t)

	opts := pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 1"},
		Rows:    24,
		Cols:    80,
	}
	sess := createSessionOrSkip(t, sessionManager, opts, false)
	defer sessionManager.KillSession(sess.ID)

	now := time.Now().UTC()
	store := &mockConversationStore{
		turns: map[string][]eventbus.ConversationTurn{
			sess.ID: {
				{Origin: eventbus.OriginUser, Text: "hello", At: now, Meta: map[string]string{"alpha": "1"}},
				{Origin: eventbus.OriginAI, Text: "hi there", At: now.Add(10 * time.Millisecond), Meta: map[string]string{"beta": "2"}},
			},
		},
	}
	apiServer.SetConversationStore(store)

	service := newSessionsService(apiServer)

	resp, err := service.GetConversation(context.Background(), &apiv1.GetConversationRequest{SessionId: sess.ID})
	if err != nil {
		t.Fatalf("GetConversation returned error: %v", err)
	}

	if resp.GetSessionId() != sess.ID {
		t.Fatalf("unexpected session id %q", resp.GetSessionId())
	}
	if resp.GetOffset() != 0 || resp.GetLimit() != 2 || resp.GetTotal() != 2 {
		t.Fatalf("unexpected pagination metadata: offset=%d limit=%d total=%d", resp.GetOffset(), resp.GetLimit(), resp.GetTotal())
	}
	if resp.GetHasMore() {
		t.Fatalf("expected has_more=false")
	}
	if resp.GetNextOffset() != 0 {
		t.Fatalf("expected next_offset=0, got %d", resp.GetNextOffset())
	}
	if len(resp.GetTurns()) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(resp.GetTurns()))
	}
	if resp.GetTurns()[0].GetOrigin() != string(eventbus.OriginUser) || resp.GetTurns()[0].GetText() != "hello" {
		t.Fatalf("unexpected first turn: %+v", resp.GetTurns()[0])
	}
	if resp.GetTurns()[1].GetOrigin() != string(eventbus.OriginAI) || resp.GetTurns()[1].GetText() != "hi there" {
		t.Fatalf("unexpected second turn: %+v", resp.GetTurns()[1])
	}
	if len(resp.GetTurns()[1].GetMetadata()) != 1 || resp.GetTurns()[1].GetMetadata()[0].GetKey() != "beta" || resp.GetTurns()[1].GetMetadata()[0].GetValue() != "2" {
		t.Fatalf("unexpected metadata: %+v", resp.GetTurns()[1].GetMetadata())
	}
}

func TestSessionsServiceGetConversationPagination(t *testing.T) {
	skipIfNoPTY(t)
	apiServer, sessionManager := newTestAPIServer(t)

	opts := pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 1"},
		Rows:    24,
		Cols:    80,
	}
	sess := createSessionOrSkip(t, sessionManager, opts, false)
	defer sessionManager.KillSession(sess.ID)

	now := time.Now().UTC()
	store := &mockConversationStore{
		turns: map[string][]eventbus.ConversationTurn{
			sess.ID: {
				{Origin: eventbus.OriginUser, Text: "A", At: now},
				{Origin: eventbus.OriginAI, Text: "B", At: now.Add(10 * time.Millisecond)},
				{Origin: eventbus.OriginUser, Text: "C", At: now.Add(20 * time.Millisecond)},
				{Origin: eventbus.OriginAI, Text: "D", At: now.Add(30 * time.Millisecond)},
			},
		},
	}
	apiServer.SetConversationStore(store)

	service := newSessionsService(apiServer)

	resp, err := service.GetConversation(context.Background(), &apiv1.GetConversationRequest{
		SessionId: sess.ID,
		Offset:    1,
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("GetConversation returned error: %v", err)
	}

	if resp.GetOffset() != 1 || resp.GetLimit() != 2 || resp.GetTotal() != 4 {
		t.Fatalf("unexpected pagination metadata: offset=%d limit=%d total=%d", resp.GetOffset(), resp.GetLimit(), resp.GetTotal())
	}
	if !resp.GetHasMore() {
		t.Fatalf("expected has_more=true")
	}
	if resp.GetNextOffset() != 3 {
		t.Fatalf("expected next_offset=3, got %d", resp.GetNextOffset())
	}
	if len(resp.GetTurns()) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(resp.GetTurns()))
	}
	if resp.GetTurns()[0].GetText() != "B" || resp.GetTurns()[1].GetText() != "C" {
		t.Fatalf("unexpected turns: %+v", resp.GetTurns())
	}
}

func TestAdaptersServiceOverview(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	store := openTestStore(t)
	adaptersSvc := newTestAdaptersService(t, store)
	apiServer.SetAdaptersController(adaptersSvc)

	ctx := context.Background()
	adapter := configstore.Adapter{ID: "adapter.ai", Source: "builtin", Type: "ai", Name: "Primary AI"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "grpc",
		Address:   "127.0.0.1:9930",
	}); err != nil {
		t.Fatalf("upsert adapter endpoint: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, "ai", adapter.ID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	service := newAdapterRuntimeService(apiServer)

	resp, err := service.Overview(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("adapters overview: %v", err)
	}
	if len(resp.GetAdapters()) == 0 {
		t.Fatalf("expected adapters in overview")
	}

	var entry *apiv1.AdapterEntry
	for _, adapter := range resp.GetAdapters() {
		if adapter.GetSlot() == "ai" {
			entry = adapter
			break
		}
	}
	if entry == nil {
		t.Fatalf("ai slot not found in overview")
	}
	if entry.AdapterId == nil || entry.GetAdapterId() != adapter.ID {
		t.Fatalf("expected adapter %s, got %v", adapter.ID, entry.AdapterId)
	}
	if entry.GetStatus() == "" {
		t.Fatalf("expected status to be populated")
	}
}

func TestAdaptersServiceBindStartStop(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	store := openTestStore(t)
	adaptersSvc := newTestAdaptersService(t, store)
	apiServer.SetAdaptersController(adaptersSvc)

	ctx := context.Background()
	adapter := configstore.Adapter{ID: "adapter.ai.bind", Source: "builtin", Type: "ai", Name: "Bind AI"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "grpc",
		Address:   "127.0.0.1:9940",
	}); err != nil {
		t.Fatalf("upsert adapter endpoint: %v", err)
	}

	service := newAdapterRuntimeService(apiServer)

	bindResp, err := service.BindAdapter(ctx, &apiv1.BindAdapterRequest{
		Slot:      "ai",
		AdapterId: adapter.ID,
	})
	if err != nil {
		t.Fatalf("BindAdapter error: %v", err)
	}
	if bindResp.GetAdapter().AdapterId == nil || bindResp.GetAdapter().GetAdapterId() != adapter.ID {
		t.Fatalf("expected bound adapter %s, got %v", adapter.ID, bindResp.GetAdapter().AdapterId)
	}
	if bindResp.GetAdapter().GetStatus() != configstore.BindingStatusActive {
		t.Fatalf("expected status %s, got %s", configstore.BindingStatusActive, bindResp.GetAdapter().GetStatus())
	}

	startResp, err := service.StartAdapter(ctx, &apiv1.AdapterSlotRequest{Slot: "ai"})
	if err != nil {
		t.Fatalf("StartAdapter error: %v", err)
	}
	if startResp.GetAdapter().GetStatus() != configstore.BindingStatusActive {
		t.Fatalf("expected active status after start, got %s", startResp.GetAdapter().GetStatus())
	}
	if startResp.GetAdapter().GetRuntime() == nil || startResp.GetAdapter().GetRuntime().GetHealth() == "" {
		t.Fatalf("expected runtime health after start")
	}

	stopResp, err := service.StopAdapter(ctx, &apiv1.AdapterSlotRequest{Slot: "ai"})
	if err != nil {
		t.Fatalf("StopAdapter error: %v", err)
	}
	if stopResp.GetAdapter().GetStatus() != configstore.BindingStatusInactive {
		t.Fatalf("expected inactive status after stop, got %s", stopResp.GetAdapter().GetStatus())
	}
	if stopResp.GetAdapter().GetRuntime() == nil || !strings.EqualFold(stopResp.GetAdapter().GetRuntime().GetHealth(), string(eventbus.AdapterHealthStopped)) {
		t.Fatalf("expected runtime health 'stopped', got %+v", stopResp.GetAdapter().GetRuntime())
	}
}

func TestQuickstartServiceIncludesAdapters(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	store := apiServer.configStore
	adaptersSvc := newTestAdaptersService(t, store)
	apiServer.SetAdaptersController(adaptersSvc)

	ctx := context.Background()
	adapter := configstore.Adapter{ID: "adapter.ai.quickstart", Source: "builtin", Type: "ai", Name: "Quickstart AI"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapter.ID,
		Transport: "grpc",
		Address:   "127.0.0.1:9950",
	}); err != nil {
		t.Fatalf("upsert adapter endpoint: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, "ai", adapter.ID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	service := newQuickstartService(apiServer)
	resp, err := service.GetStatus(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("QuickstartService.GetStatus error: %v", err)
	}
	if len(resp.GetAdapters()) == 0 {
		t.Fatalf("expected adapters list in quickstart response")
	}
	var found bool
	for _, entry := range resp.GetAdapters() {
		if entry.GetSlot() == "ai" {
			found = true
			if entry.GetAdapterId() != adapter.ID {
				t.Fatalf("expected adapter %s, got %s", adapter.ID, entry.GetAdapterId())
			}
			break
		}
	}
	if !found {
		t.Fatalf("ai slot not present in quickstart adapters")
	}
	if got := resp.GetMissingReferenceAdapters(); !reflect.DeepEqual(got, adapters.RequiredReferenceAdapters) {
		t.Fatalf("expected missing reference adapters %v, got %v", adapters.RequiredReferenceAdapters, got)
	}
}

func TestQuickstartServiceWithoutAdaptersService(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	service := newQuickstartService(apiServer)
	_, err := service.GetStatus(context.Background(), &emptypb.Empty{})
	if err == nil {
		t.Fatalf("expected error when adapter service unavailable")
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected error codes.Unavailable, got %v", status.Code(err))
	}
}

func TestQuickstartServiceUpdateFailsWhenReferenceMissing(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	store := apiServer.configStore
	service := newQuickstartService(apiServer)

	ctx := context.WithValue(context.Background(), authContextKey{}, storedToken{Role: string(roleAdmin)})

	adapterList := []configstore.Adapter{
		{ID: "adapter.ai.qs", Source: "builtin", Type: "ai", Name: "AI"},
		{ID: "adapter.stt.custom", Source: "builtin", Type: "stt", Name: "STT"},
		{ID: "adapter.tts.custom", Source: "builtin", Type: "tts", Name: "TTS"},
		{ID: "adapter.vad.custom", Source: "builtin", Type: "vad", Name: "VAD"},
		{ID: "adapter.tunnel.custom", Source: "builtin", Type: "tunnel", Name: "Tunnel"},
	}
	for _, adapter := range adapterList {
		if err := store.UpsertAdapter(ctx, adapter); err != nil {
			t.Fatalf("upsert adapter: %v", err)
		}
		if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
			AdapterID: adapter.ID,
			Transport: "grpc",
			Address:   "127.0.0.1:0",
		}); err != nil {
			t.Fatalf("upsert adapter endpoint %s: %v", adapter.ID, err)
		}
	}

	bindings := map[string]string{
		string(adapters.SlotAI):     "adapter.ai.qs",
		string(adapters.SlotSTT):    "adapter.stt.custom",
		string(adapters.SlotTTS):    "adapter.tts.custom",
		string(adapters.SlotVAD):    "adapter.vad.custom",
		string(adapters.SlotTunnel): "adapter.tunnel.custom",
	}
	for slot, adapterID := range bindings {
		if err := store.SetActiveAdapter(ctx, slot, adapterID, nil); err != nil {
			t.Fatalf("bind slot %s: %v", slot, err)
		}
	}

	_, err := service.Update(ctx, &apiv1.UpdateQuickstartRequest{
		Complete: wrapperspb.Bool(true),
	})
	if err == nil {
		t.Fatalf("expected update to fail when reference adapters missing")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", status.Code(err))
	}
	if !strings.Contains(err.Error(), "reference adapters missing") {
		t.Fatalf("expected reference missing message, got %v", err)
	}
}

func TestAudioServiceStreamAudioIn(t *testing.T) {
	skipIfNoPTY(t)
	apiServer, sessionManager := newTestAPIServer(t)
	enableVoiceAdapters(t, apiServer.configStore)
	bus := eventbus.New()
	ingressSvc := ingress.New(bus)
	apiServer.SetAudioIngress(newTestAudioIngressProvider(ingressSvc))
	sessionManager.UseEventBus(bus)

	opts := pty.StartOptions{Command: "/bin/sh", Args: []string{"-c", "sleep 1"}, Rows: 24, Cols: 80}
	sess := createSessionOrSkip(t, sessionManager, opts, false)
	defer sessionManager.KillSession(sess.ID)

	service := newAudioService(apiServer)

	rawSub := bus.Subscribe(eventbus.TopicAudioIngressRaw)
	defer rawSub.Close()
	segSub := bus.Subscribe(eventbus.TopicAudioIngressSegment)
	defer segSub.Close()

	stream := &fakeAudioInStream{
		ctx: context.WithValue(context.Background(), authContextKey{}, storedToken{Role: string(roleAdmin)}),
		reqs: []*apiv1.StreamAudioInRequest{
			{
				SessionId: sess.ID,
				StreamId:  "mic",
				Format: &apiv1.AudioFormat{
					Encoding:        "pcm_s16le",
					SampleRate:      16000,
					Channels:        1,
					BitDepth:        16,
					FrameDurationMs: 20,
				},
				Chunk: &apiv1.AudioChunk{
					Data:     make([]byte, 640),
					Sequence: 1,
					Metadata: map[string]string{"client": "test"},
				},
			},
			{
				SessionId: sess.ID,
				StreamId:  "mic",
				Chunk: &apiv1.AudioChunk{
					Data:     make([]byte, 320),
					Sequence: 2,
					Last:     true,
				},
			},
		},
	}

	if err := service.StreamAudioIn(stream); err != nil {
		t.Fatalf("StreamAudioIn returned error: %v", err)
	}
	if stream.resp == nil || !stream.resp.GetReady() || stream.resp.GetAckSequence() != 2 {
		t.Fatalf("unexpected response: %+v", stream.resp)
	}

	raw1 := recvEnvelope(t, rawSub).Payload.(eventbus.AudioIngressRawEvent)
	if raw1.Sequence != 1 || len(raw1.Data) != 640 {
		t.Fatalf("unexpected raw event #1: %+v", raw1)
	}
	raw2 := recvEnvelope(t, rawSub).Payload.(eventbus.AudioIngressRawEvent)
	if raw2.Sequence != 2 || len(raw2.Data) != 320 {
		t.Fatalf("unexpected raw event #2: %+v", raw2)
	}

	seg1 := recvEnvelope(t, segSub).Payload.(eventbus.AudioIngressSegmentEvent)
	if !seg1.First || seg1.Last || len(seg1.Data) != 640 || seg1.Duration != 20*time.Millisecond {
		t.Fatalf("unexpected segment #1: %+v", seg1)
	}
	seg2 := recvEnvelope(t, segSub).Payload.(eventbus.AudioIngressSegmentEvent)
	if seg2.First || !seg2.Last || len(seg2.Data) != 320 {
		t.Fatalf("unexpected segment #2: %+v", seg2)
	}

	if _, ok := ingressSvc.Stream(sess.ID, "mic"); ok {
		t.Fatalf("stream should be removed after close")
	}

	expectNoEnvelope(t, rawSub)
	expectNoEnvelope(t, segSub)
}

func TestAudioServiceStreamAudioOut(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	enableVoiceAdapters(t, apiServer.configStore)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)

	egressSvc := egress.New(bus)
	apiServer.SetAudioEgress(newTestAudioEgressController(egressSvc))
	apiServer.sessionManager = nil

	service := newAudioService(apiServer)

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), authContextKey{}, storedToken{Role: string(roleAdmin)}))
	defer cancel()

	srv := newFakeAudioOutServer(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- service.StreamAudioOut(&apiv1.StreamAudioOutRequest{
			SessionId: "sess-audio",
		}, srv)
	}()

	time.Sleep(20 * time.Millisecond)

	format := egressSvc.PlaybackFormat()
	streamID := egressSvc.DefaultStreamID()

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicAudioEgressPlayback,
		Payload: eventbus.AudioEgressPlaybackEvent{
			SessionID: "sess-audio",
			StreamID:  streamID,
			Sequence:  1,
			Format:    format,
			Duration:  150 * time.Millisecond,
			Data:      []byte{1, 2, 3, 4},
			Final:     false,
			Metadata:  map[string]string{"phase": "speak"},
		},
	})

	first := srv.waitForResponses(t, 1, time.Second)[0]
	if first.GetChunk().GetSequence() != 1 || !first.GetChunk().GetFirst() || first.GetChunk().GetLast() {
		t.Fatalf("unexpected first chunk: %+v", first.GetChunk())
	}
	if len(first.GetChunk().GetData()) != 4 {
		t.Fatalf("unexpected chunk data length: %d", len(first.GetChunk().GetData()))
	}
	if got := first.GetChunk().GetMetadata()["phase"]; got != "speak" {
		t.Fatalf("metadata not propagated: %+v", first.GetChunk().GetMetadata())
	}
	if first.GetChunk().GetDurationMs() != 150 {
		t.Fatalf("unexpected duration ms: %d", first.GetChunk().GetDurationMs())
	}

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicAudioEgressPlayback,
		Payload: eventbus.AudioEgressPlaybackEvent{
			SessionID: "sess-audio",
			StreamID:  streamID,
			Sequence:  2,
			Format:    format,
			Data:      []byte{},
			Final:     true,
			Metadata:  map[string]string{"barge_in": "true"},
		},
	})

	second := srv.waitForResponses(t, 2, time.Second)[1]
	if !second.GetChunk().GetLast() {
		t.Fatalf("expected final chunk, got %+v", second.GetChunk())
	}
	if second.GetChunk().GetMetadata()["barge_in"] != "true" {
		t.Fatalf("expected barge metadata on final chunk, got %+v", second.GetChunk().GetMetadata())
	}
	if second.GetChunk().GetDurationMs() != 0 {
		t.Fatalf("expected zero duration for final chunk, got %d", second.GetChunk().GetDurationMs())
	}

	cancel()
	select {
	case err := <-errCh:
		if status.Code(err) != codes.Canceled {
			t.Fatalf("expected canceled error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for stream to finish")
	}
}

func TestAudioServiceGetAudioCapabilities(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	enableVoiceAdapters(t, apiServer.configStore)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	ingressSvc := ingress.New(bus)
	apiServer.SetAudioIngress(newTestAudioIngressProvider(ingressSvc))
	egressSvc := egress.New(bus)
	apiServer.SetAudioEgress(newTestAudioEgressController(egressSvc))
	apiServer.sessionManager = nil

	service := newAudioService(apiServer)
	ctx := context.WithValue(context.Background(), authContextKey{}, storedToken{Role: string(roleAdmin)})

	resp, err := service.GetAudioCapabilities(ctx, &apiv1.GetAudioCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetAudioCapabilities returned error: %v", err)
	}
	if len(resp.GetCapture()) == 0 {
		t.Fatalf("expected capture capabilities")
	}
	if capture := resp.GetCapture()[0]; capture.GetStreamId() != "mic" || capture.GetFormat().GetSampleRate() != 16000 {
		t.Fatalf("unexpected capture capability: %+v", capture)
	} else {
		if capture.GetMetadata()["ready"] != "true" {
			t.Fatalf("expected capture ready=true, got %+v", capture.GetMetadata())
		}
	}
	if len(resp.GetPlayback()) == 0 {
		t.Fatalf("expected playback capabilities")
	}
	playback := resp.GetPlayback()[0]
	if playback.GetStreamId() != egressSvc.DefaultStreamID() {
		t.Fatalf("unexpected playback stream id: %s", playback.GetStreamId())
	}
	if playback.GetFormat().GetSampleRate() != uint32(egressSvc.PlaybackFormat().SampleRate) {
		t.Fatalf("playback format mismatch: %+v", playback.GetFormat())
	}
	if playback.GetMetadata()["ready"] != "true" {
		t.Fatalf("expected playback ready=true, got %+v", playback.GetMetadata())
	}

	if _, err := service.GetAudioCapabilities(ctx, &apiv1.GetAudioCapabilitiesRequest{SessionId: "sess"}); err != nil {
		t.Fatalf("GetAudioCapabilities with session returned error: %v", err)
	}
}

func recvEnvelope(t *testing.T, sub *eventbus.Subscription) eventbus.Envelope {
	t.Helper()
	select {
	case env := <-sub.C():
		return env
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for event")
	}
	return eventbus.Envelope{}
}

func expectNoEnvelope(t *testing.T, sub *eventbus.Subscription) {
	t.Helper()
	select {
	case env := <-sub.C():
		t.Fatalf("unexpected event: %+v", env)
	case <-time.After(20 * time.Millisecond):
	}
}

type fakeAudioInStream struct {
	ctx  context.Context
	reqs []*apiv1.StreamAudioInRequest
	idx  int
	resp *apiv1.StreamAudioInResponse
}

func (f *fakeAudioInStream) Context() context.Context {
	if f.ctx != nil {
		return f.ctx
	}
	return context.Background()
}

func (f *fakeAudioInStream) Recv() (*apiv1.StreamAudioInRequest, error) {
	if f.idx >= len(f.reqs) {
		return nil, io.EOF
	}
	req := f.reqs[f.idx]
	f.idx++
	return req, nil
}

func (f *fakeAudioInStream) SendAndClose(resp *apiv1.StreamAudioInResponse) error {
	f.resp = resp
	return nil
}

func (f *fakeAudioInStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeAudioInStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeAudioInStream) SetTrailer(metadata.MD)       {}
func (f *fakeAudioInStream) SendMsg(interface{}) error    { return nil }
func (f *fakeAudioInStream) RecvMsg(interface{}) error    { return nil }

type fakeAudioOutServer struct {
	ctx       context.Context
	mu        sync.Mutex
	responses []*apiv1.StreamAudioOutResponse
}

func newFakeAudioOutServer(ctx context.Context) *fakeAudioOutServer {
	return &fakeAudioOutServer{ctx: ctx}
}

func (f *fakeAudioOutServer) Context() context.Context {
	if f.ctx != nil {
		return f.ctx
	}
	return context.Background()
}

func (f *fakeAudioOutServer) Send(resp *apiv1.StreamAudioOutResponse) error {
	f.mu.Lock()
	f.responses = append(f.responses, resp)
	f.mu.Unlock()
	return nil
}

func (f *fakeAudioOutServer) waitForResponses(t *testing.T, count int, timeout time.Duration) []*apiv1.StreamAudioOutResponse {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		f.mu.Lock()
		if len(f.responses) >= count {
			out := append([]*apiv1.StreamAudioOutResponse(nil), f.responses...)
			f.mu.Unlock()
			return out
		}
		f.mu.Unlock()

		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline.C:
			t.Fatalf("timeout waiting for %d responses, got %d", count, len(f.responses))
		}
	}
}

func (f *fakeAudioOutServer) SetHeader(metadata.MD) error  { return nil }
func (f *fakeAudioOutServer) SendHeader(metadata.MD) error { return nil }
func (f *fakeAudioOutServer) SetTrailer(metadata.MD)       {}
func (f *fakeAudioOutServer) SendMsg(interface{}) error    { return nil }
func (f *fakeAudioOutServer) RecvMsg(interface{}) error    { return nil }
