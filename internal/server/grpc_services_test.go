package server

import (
	"context"
	"fmt"
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
	"github.com/nupi-ai/nupi/internal/language"
	adapters "github.com/nupi-ai/nupi/internal/plugins/adapters"
	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/recording"
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

// stubSessionManager satisfies SessionManager for tests that need session
// validation to pass without creating real PTY sessions.
type stubSessionManager struct {
	sessions map[string]*session.Session
}

func (s *stubSessionManager) ListSessions() []*session.Session                       { return nil }
func (s *stubSessionManager) CreateSession(pty.StartOptions, bool) (*session.Session, error) {
	return nil, fmt.Errorf("not implemented")
}
func (s *stubSessionManager) GetSession(id string) (*session.Session, error) {
	if s.sessions != nil {
		if sess, ok := s.sessions[id]; ok {
			return sess, nil
		}
	}
	// Return a zero-value session for any ID to pass validation.
	return &session.Session{}, nil
}
func (s *stubSessionManager) KillSession(string) error                            { return nil }
func (s *stubSessionManager) WriteToSession(string, []byte) error                 { return nil }
func (s *stubSessionManager) ResizeSession(string, int, int) error                { return nil }
func (s *stubSessionManager) GetRecordingStore() *recording.Store                 { return nil }
func (s *stubSessionManager) AddEventListener(session.SessionEventListener)       {}
func (s *stubSessionManager) AttachToSession(string, pty.OutputSink, bool) error  { return nil }
func (s *stubSessionManager) DetachFromSession(string, pty.OutputSink) error      { return nil }

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

func TestSessionsServiceGetGlobalConversation(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	now := time.Now().UTC()
	store := &mockConversationStore{
		globalTurns: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "global hello", At: now, Meta: map[string]string{"alpha": "1"}},
			{Origin: eventbus.OriginAI, Text: "global reply", At: now.Add(10 * time.Millisecond), Meta: map[string]string{"beta": "2"}},
		},
	}
	apiServer.SetConversationStore(store)

	service := newSessionsService(apiServer)

	resp, err := service.GetGlobalConversation(context.Background(), &apiv1.GetGlobalConversationRequest{})
	if err != nil {
		t.Fatalf("GetGlobalConversation returned error: %v", err)
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
	if resp.GetTurns()[0].GetOrigin() != string(eventbus.OriginUser) || resp.GetTurns()[0].GetText() != "global hello" {
		t.Fatalf("unexpected first turn: %+v", resp.GetTurns()[0])
	}
	if resp.GetTurns()[1].GetOrigin() != string(eventbus.OriginAI) || resp.GetTurns()[1].GetText() != "global reply" {
		t.Fatalf("unexpected second turn: %+v", resp.GetTurns()[1])
	}
	if len(resp.GetTurns()[1].GetMetadata()) != 1 || resp.GetTurns()[1].GetMetadata()[0].GetKey() != "beta" || resp.GetTurns()[1].GetMetadata()[0].GetValue() != "2" {
		t.Fatalf("unexpected metadata: %+v", resp.GetTurns()[1].GetMetadata())
	}
}

func TestSessionsServiceGetGlobalConversationPagination(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	now := time.Now().UTC()
	store := &mockConversationStore{
		globalTurns: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "A", At: now},
			{Origin: eventbus.OriginAI, Text: "B", At: now.Add(10 * time.Millisecond)},
			{Origin: eventbus.OriginUser, Text: "C", At: now.Add(20 * time.Millisecond)},
			{Origin: eventbus.OriginAI, Text: "D", At: now.Add(30 * time.Millisecond)},
		},
	}
	apiServer.SetConversationStore(store)

	service := newSessionsService(apiServer)

	resp, err := service.GetGlobalConversation(context.Background(), &apiv1.GetGlobalConversationRequest{
		Offset: 1,
		Limit:  2,
	})
	if err != nil {
		t.Fatalf("GetGlobalConversation returned error: %v", err)
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

func TestSessionsServiceGetGlobalConversationEmpty(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	store := &mockConversationStore{}
	apiServer.SetConversationStore(store)

	service := newSessionsService(apiServer)

	resp, err := service.GetGlobalConversation(context.Background(), &apiv1.GetGlobalConversationRequest{})
	if err != nil {
		t.Fatalf("GetGlobalConversation returned error: %v", err)
	}

	if resp.GetTotal() != 0 {
		t.Fatalf("expected total=0, got %d", resp.GetTotal())
	}
	if len(resp.GetTurns()) != 0 {
		t.Fatalf("expected 0 turns, got %d", len(resp.GetTurns()))
	}
}

func TestSessionsServiceGetGlobalConversationNoService(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	// conversation store is nil by default

	service := newSessionsService(apiServer)

	_, err := service.GetGlobalConversation(context.Background(), &apiv1.GetGlobalConversationRequest{})
	if err == nil {
		t.Fatalf("expected error when conversation service unavailable")
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected codes.Unavailable, got %v", status.Code(err))
	}
}

func TestAdaptersServiceOverview(t *testing.T) {
	apiServer, _, store := newTestAPIServerWithStore(t)
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
	apiServer, _, store := newTestAPIServerWithStore(t)
	adaptersSvc := newTestAdaptersService(t, store)
	apiServer.SetAdaptersController(adaptersSvc)

	ctx := context.Background()
	// Use builtin mock adapter so that Bind+Start runs in-process without network.
	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	adapterID := adapters.MockAIAdapterID

	service := newAdapterRuntimeService(apiServer)

	bindResp, err := service.BindAdapter(ctx, &apiv1.BindAdapterRequest{
		Slot:      "ai",
		AdapterId: adapterID,
	})
	if err != nil {
		t.Fatalf("BindAdapter error: %v", err)
	}
	if bindResp.GetAdapter().AdapterId == nil || bindResp.GetAdapter().GetAdapterId() != adapterID {
		t.Fatalf("expected bound adapter %s, got %v", adapterID, bindResp.GetAdapter().AdapterId)
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
	apiServer, _, store := newTestAPIServerWithStore(t)
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

	rawSub := eventbus.SubscribeTo(bus, eventbus.Audio.IngressRaw)
	defer rawSub.Close()
	segSub := eventbus.SubscribeTo(bus, eventbus.Audio.IngressSegment)
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

	raw1 := recvEnvelope(t, rawSub).Payload
	if raw1.Sequence != 1 || len(raw1.Data) != 640 {
		t.Fatalf("unexpected raw event #1: %+v", raw1)
	}
	raw2 := recvEnvelope(t, rawSub).Payload
	if raw2.Sequence != 2 || len(raw2.Data) != 320 {
		t.Fatalf("unexpected raw event #2: %+v", raw2)
	}

	seg1 := recvEnvelope(t, segSub).Payload
	if !seg1.First || seg1.Last || len(seg1.Data) != 640 || seg1.Duration != 20*time.Millisecond {
		t.Fatalf("unexpected segment #1: %+v", seg1)
	}
	seg2 := recvEnvelope(t, segSub).Payload
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
	// Use stub session manager so audio tests pass session validation
	// without requiring real PTY sessions.
	apiServer.sessionManager = &stubSessionManager{}

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

	eventbus.Publish(context.Background(), bus, eventbus.Audio.EgressPlayback, "", eventbus.AudioEgressPlaybackEvent{
		SessionID: "sess-audio",
		StreamID:  streamID,
		Sequence:  1,
		Format:    format,
		Duration:  150 * time.Millisecond,
		Data:      []byte{1, 2, 3, 4},
		Final:     false,
		Metadata:  map[string]string{"phase": "speak"},
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

	eventbus.Publish(context.Background(), bus, eventbus.Audio.EgressPlayback, "", eventbus.AudioEgressPlaybackEvent{
		SessionID: "sess-audio",
		StreamID:  streamID,
		Sequence:  2,
		Format:    format,
		Data:      []byte{},
		Final:     true,
		Metadata:  map[string]string{"barge_in": "true"},
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
	// Use stub session manager so capabilities test passes session validation
	// without requiring real PTY sessions.
	apiServer.sessionManager = &stubSessionManager{}

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

func recvEnvelope[T any](t *testing.T, sub *eventbus.TypedSubscription[T]) eventbus.TypedEnvelope[T] {
	t.Helper()
	select {
	case env := <-sub.C():
		return env
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for event")
	}
	return eventbus.TypedEnvelope[T]{}
}

func expectNoEnvelope[T any](t *testing.T, sub *eventbus.TypedSubscription[T]) {
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

func TestConfigServiceTransportAuthRequired(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	// Enable auth so that requireRoleGRPC actually checks credentials.
	apiServer.authMu.Lock()
	apiServer.authRequired = true
	apiServer.authMu.Unlock()

	service := newConfigService(apiServer)
	ctx := context.Background() // no auth token

	t.Run("GetTransportConfig", func(t *testing.T) {
		_, err := service.GetTransportConfig(ctx, &emptypb.Empty{})
		if err == nil {
			t.Fatal("expected auth error, got nil")
		}
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
		}
	})

	t.Run("UpdateTransportConfig", func(t *testing.T) {
		_, err := service.UpdateTransportConfig(ctx, &apiv1.UpdateTransportConfigRequest{
			Config: &apiv1.TransportConfig{Binding: "loopback"},
		})
		if err == nil {
			t.Fatal("expected auth error, got nil")
		}
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
		}
	})
}

func TestSessionsServiceSendVoiceCommand(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.sessionManager = &stubSessionManager{}

	service := newSessionsService(apiServer)

	sub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned)
	defer sub.Close()

	resp, err := service.SendVoiceCommand(context.Background(), &apiv1.SendVoiceCommandRequest{
		SessionId: "test-session",
		Text:      "check status",
		Metadata:  map[string]string{"source": "whisper", "client_type": "mobile"},
	})
	if err != nil {
		t.Fatalf("SendVoiceCommand returned error: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected accepted=true")
	}
	if resp.GetMessage() != "voice command queued" {
		t.Fatalf("expected message 'voice command queued', got %q", resp.GetMessage())
	}

	env := recvEnvelope(t, sub)
	evt := env.Payload
	if evt.SessionID != "test-session" {
		t.Fatalf("expected session_id 'test-session', got %q", evt.SessionID)
	}
	if evt.Origin != eventbus.OriginUser {
		t.Fatalf("expected origin 'user', got %q", evt.Origin)
	}
	if evt.Text != "check status" {
		t.Fatalf("expected text 'check status', got %q", evt.Text)
	}
	if evt.Annotations["input_source"] != "voice" {
		t.Fatalf("expected input_source=voice, got %q", evt.Annotations["input_source"])
	}
	if evt.Annotations["client_type"] != "mobile" {
		t.Fatalf("expected client_type=mobile from caller metadata, got %q", evt.Annotations["client_type"])
	}
	if evt.Annotations["source"] != "whisper" {
		t.Fatalf("expected source=whisper from caller metadata, got %q", evt.Annotations["source"])
	}
}

func TestSessionsServiceSendVoiceCommandEmptyText(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.sessionManager = &stubSessionManager{}

	service := newSessionsService(apiServer)

	_, err := service.SendVoiceCommand(context.Background(), &apiv1.SendVoiceCommandRequest{
		SessionId: "test-session",
		Text:      "",
	})
	if err == nil {
		t.Fatal("expected error for empty text")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestSessionsServiceSendVoiceCommandNoEventBus(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	apiServer.sessionManager = &stubSessionManager{}
	// eventBus is nil by default

	service := newSessionsService(apiServer)

	_, err := service.SendVoiceCommand(context.Background(), &apiv1.SendVoiceCommandRequest{
		Text: "hello",
	})
	if err == nil {
		t.Fatal("expected error when event bus unavailable")
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %v", status.Code(err))
	}
}

func TestSessionsServiceSendVoiceCommandGlobalNoSession(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)

	service := newSessionsService(apiServer)

	sub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned)
	defer sub.Close()

	// Empty session_id should work (global/sessionless)
	resp, err := service.SendVoiceCommand(context.Background(), &apiv1.SendVoiceCommandRequest{
		Text: "what sessions do I have",
	})
	if err != nil {
		t.Fatalf("SendVoiceCommand returned error: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected accepted=true")
	}

	env := recvEnvelope(t, sub)
	if env.Payload.SessionID != "" {
		t.Fatalf("expected empty session_id, got %q", env.Payload.SessionID)
	}
}

// notFoundSessionManager returns errors for all GetSession calls,
// used to test the NotFound code path in SendVoiceCommand.
type notFoundSessionManager struct {
	stubSessionManager
}

func (s *notFoundSessionManager) GetSession(string) (*session.Session, error) {
	return nil, fmt.Errorf("session not found")
}

func TestSessionsServiceSendVoiceCommandWhitespaceOnlyText(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.sessionManager = &stubSessionManager{}

	service := newSessionsService(apiServer)

	_, err := service.SendVoiceCommand(context.Background(), &apiv1.SendVoiceCommandRequest{
		Text: "   \t\n  ",
	})
	if err == nil {
		t.Fatal("expected error for whitespace-only text")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestSessionsServiceSendVoiceCommandReservedAnnotationKeys(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.sessionManager = &stubSessionManager{}

	service := newSessionsService(apiServer)

	sub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned)
	defer sub.Close()

	// Send metadata that attempts to override reserved annotation keys.
	// Only input_source is reserved; client_type and confidence are caller-provided.
	resp, err := service.SendVoiceCommand(context.Background(), &apiv1.SendVoiceCommandRequest{
		Text: "test",
		Metadata: map[string]string{
			"input_source": "hacked",
			"client_type":  "desktop",
			"confidence":   "0.95",
			"custom_key":   "allowed",
		},
	})
	if err != nil {
		t.Fatalf("SendVoiceCommand returned error: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected accepted=true")
	}

	env := recvEnvelope(t, sub)
	annotations := env.Payload.Annotations

	// input_source is reserved — must retain "voice".
	if annotations["input_source"] != "voice" {
		t.Fatalf("reserved input_source overridden: got %q, want %q", annotations["input_source"], "voice")
	}
	// client_type and confidence are caller-provided — should pass through.
	if annotations["client_type"] != "desktop" {
		t.Fatalf("expected client_type=desktop from caller, got %q", annotations["client_type"])
	}
	if annotations["confidence"] != "0.95" {
		t.Fatalf("expected confidence=0.95 from caller, got %q", annotations["confidence"])
	}
	// Other non-reserved keys must pass through.
	if annotations["custom_key"] != "allowed" {
		t.Fatalf("custom metadata not passed through: got %q, want %q", annotations["custom_key"], "allowed")
	}
}

func TestSessionsServiceSendVoiceCommandTextTooLong(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.sessionManager = &stubSessionManager{}

	service := newSessionsService(apiServer)

	longText := strings.Repeat("a", 10001)
	_, err := service.SendVoiceCommand(context.Background(), &apiv1.SendVoiceCommandRequest{
		Text: longText,
	})
	if err == nil {
		t.Fatal("expected error for text exceeding max length")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
	if !strings.Contains(err.Error(), "exceeds maximum length") {
		t.Fatalf("expected max length error message, got %v", err)
	}
}

// M1 fix test: verify length is counted in runes, not bytes.
// A string of 10000 multi-byte runes (ą = 2 bytes each = 20000 bytes)
// should be accepted because it's exactly 10000 characters.
func TestSessionsServiceSendVoiceCommandMultibyteWithinLimit(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.sessionManager = &stubSessionManager{}

	service := newSessionsService(apiServer)

	sub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned)
	defer sub.Close()

	// 10000 Polish "ą" characters = 20000 bytes but 10000 runes — within limit.
	multibyteText := strings.Repeat("ą", maxVoiceCommandTextLen)
	resp, err := service.SendVoiceCommand(context.Background(), &apiv1.SendVoiceCommandRequest{
		Text: multibyteText,
	})
	if err != nil {
		t.Fatalf("expected success for %d-rune multi-byte text, got error: %v", maxVoiceCommandTextLen, err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected accepted=true")
	}

	env := recvEnvelope(t, sub)
	if env.Payload.Text != multibyteText {
		t.Fatalf("text mismatch")
	}
}

func TestSessionsServiceSendVoiceCommandInvalidSession(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.sessionManager = &notFoundSessionManager{}

	service := newSessionsService(apiServer)

	_, err := service.SendVoiceCommand(context.Background(), &apiv1.SendVoiceCommandRequest{
		SessionId: "nonexistent-session",
		Text:      "hello",
	})
	if err == nil {
		t.Fatal("expected error for non-existent session")
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", status.Code(err))
	}
}

func TestSessionsServiceSendVoiceCommandLanguagePropagation(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.sessionManager = &stubSessionManager{}

	service := newSessionsService(apiServer)

	sub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned)
	defer sub.Close()

	// Set language in context via language.WithLanguage (simulates gRPC
	// middleware that parses the nupi-language header).
	ctx := language.WithLanguage(context.Background(), language.Language{
		ISO1:        "pl",
		BCP47:       "pl-PL",
		EnglishName: "Polish",
		NativeName:  "Polski",
	})

	resp, err := service.SendVoiceCommand(ctx, &apiv1.SendVoiceCommandRequest{
		Text: "sprawdź status",
	})
	if err != nil {
		t.Fatalf("SendVoiceCommand returned error: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected accepted=true")
	}

	env := recvEnvelope(t, sub)
	annotations := env.Payload.Annotations

	if annotations[language.MetaISO1] != "pl" {
		t.Fatalf("expected nupi.lang.iso1=pl, got %q", annotations[language.MetaISO1])
	}
	if annotations[language.MetaBCP47] != "pl-PL" {
		t.Fatalf("expected nupi.lang.bcp47=pl-PL, got %q", annotations[language.MetaBCP47])
	}
	if annotations[language.MetaEnglish] != "Polish" {
		t.Fatalf("expected nupi.lang.english=Polish, got %q", annotations[language.MetaEnglish])
	}
	if annotations[language.MetaNative] != "Polski" {
		t.Fatalf("expected nupi.lang.native=Polski, got %q", annotations[language.MetaNative])
	}
}

func TestSessionsServiceSendVoiceCommandNilMetadata(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.sessionManager = &stubSessionManager{}

	service := newSessionsService(apiServer)

	sub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned)
	defer sub.Close()

	// Nil metadata map should work — annotations get only the default "input_source".
	resp, err := service.SendVoiceCommand(context.Background(), &apiv1.SendVoiceCommandRequest{
		Text:     "hello",
		Metadata: nil,
	})
	if err != nil {
		t.Fatalf("SendVoiceCommand returned error: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected accepted=true")
	}

	env := recvEnvelope(t, sub)
	if env.Payload.Annotations["input_source"] != "voice" {
		t.Fatalf("expected input_source=voice, got %q", env.Payload.Annotations["input_source"])
	}
}

func TestSessionsServiceSendVoiceCommandMetadataTooMany(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.sessionManager = &stubSessionManager{}

	service := newSessionsService(apiServer)

	// Exceed max metadata entry count.
	bigMeta := make(map[string]string, 51)
	for i := range 51 {
		bigMeta[fmt.Sprintf("key_%d", i)] = "value"
	}
	_, err := service.SendVoiceCommand(context.Background(), &apiv1.SendVoiceCommandRequest{
		Text:     "hello",
		Metadata: bigMeta,
	})
	if err == nil {
		t.Fatal("expected error for too many metadata entries")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
	if !strings.Contains(err.Error(), "metadata exceeds maximum") {
		t.Fatalf("expected metadata limit error message, got %v", err)
	}
}

func TestSessionsServiceSendVoiceCommandMetadataValueTooLong(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.sessionManager = &stubSessionManager{}

	service := newSessionsService(apiServer)

	_, err := service.SendVoiceCommand(context.Background(), &apiv1.SendVoiceCommandRequest{
		Text:     "hello",
		Metadata: map[string]string{"key": strings.Repeat("x", 1025)},
	})
	if err == nil {
		t.Fatal("expected error for metadata value too long")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestSessionsServiceSendVoiceCommandMetadataKeyTooLong(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.sessionManager = &stubSessionManager{}

	service := newSessionsService(apiServer)

	_, err := service.SendVoiceCommand(context.Background(), &apiv1.SendVoiceCommandRequest{
		Text:     "hello",
		Metadata: map[string]string{strings.Repeat("k", 257): "value"},
	})
	if err == nil {
		t.Fatal("expected error for metadata key too long")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
	if !strings.Contains(err.Error(), "exceeds limits") {
		t.Fatalf("expected limits error message, got %v", err)
	}
}

// Verify metadata length is counted in runes, not bytes — same fix as text field (Review 5).
func TestSessionsServiceSendVoiceCommandMetadataMultibyteWithinLimit(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.sessionManager = &stubSessionManager{}

	service := newSessionsService(apiServer)

	sub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned)
	defer sub.Close()

	// 1024 Polish "ź" characters = 2048 bytes but 1024 runes — within value limit.
	multibyteValue := strings.Repeat("ź", maxVoiceCommandMetadataValueLen)
	resp, err := service.SendVoiceCommand(context.Background(), &apiv1.SendVoiceCommandRequest{
		Text:     "hello",
		Metadata: map[string]string{"lang_note": multibyteValue},
	})
	if err != nil {
		t.Fatalf("expected success for %d-rune multi-byte metadata value, got error: %v", maxVoiceCommandMetadataValueLen, err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected accepted=true")
	}

	env := recvEnvelope(t, sub)
	if env.Payload.Annotations["lang_note"] != multibyteValue {
		t.Fatal("metadata value mismatch")
	}
}

func TestSessionsServiceNilManagerReturnsUnavailable(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	apiServer.sessionManager = nil

	service := newSessionsService(apiServer)
	ctx := context.WithValue(context.Background(), authContextKey{}, storedToken{Role: string(roleAdmin)})

	t.Run("ListSessions", func(t *testing.T) {
		_, err := service.ListSessions(ctx, &apiv1.ListSessionsRequest{})
		if err == nil || status.Code(err) != codes.Unavailable {
			t.Fatalf("expected Unavailable, got %v", err)
		}
	})

	t.Run("CreateSession", func(t *testing.T) {
		_, err := service.CreateSession(ctx, &apiv1.CreateSessionRequest{Command: "echo"})
		if err == nil || status.Code(err) != codes.Unavailable {
			t.Fatalf("expected Unavailable, got %v", err)
		}
	})

	t.Run("KillSession", func(t *testing.T) {
		_, err := service.KillSession(ctx, &apiv1.KillSessionRequest{SessionId: "fake"})
		if err == nil || status.Code(err) != codes.Unavailable {
			t.Fatalf("expected Unavailable, got %v", err)
		}
	})

	t.Run("GetConversation", func(t *testing.T) {
		// GetConversation requires conversation store first, so set it to something
		apiServer.conversation = &mockConversationStore{}
		_, err := service.GetConversation(ctx, &apiv1.GetConversationRequest{SessionId: "fake"})
		if err == nil || status.Code(err) != codes.Unavailable {
			t.Fatalf("expected Unavailable, got %v", err)
		}
	})

	t.Run("SendVoiceCommand", func(t *testing.T) {
		// M3 fix (Review 6): verify SendVoiceCommand with non-empty session_id
		// returns Unavailable when session manager is nil.
		bus := eventbus.New()
		apiServer.SetEventBus(bus)
		_, err := service.SendVoiceCommand(ctx, &apiv1.SendVoiceCommandRequest{
			SessionId: "some-session",
			Text:      "hello",
		})
		if err == nil || status.Code(err) != codes.Unavailable {
			t.Fatalf("expected Unavailable, got %v", err)
		}
	})
}
