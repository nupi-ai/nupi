package server

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/audio/ingress"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/pty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
)

func TestSessionsServiceGetConversation(t *testing.T) {
	apiServer, sessionManager := newTestAPIServer(t)

	opts := pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 1"},
		Rows:    24,
		Cols:    80,
	}
	sess, err := sessionManager.CreateSession(opts, false)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
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
	apiServer, sessionManager := newTestAPIServer(t)

	opts := pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 1"},
		Rows:    24,
		Cols:    80,
	}
	sess, err := sessionManager.CreateSession(opts, false)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
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

func TestModulesServiceOverview(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	store := openTestStore(t)
	modulesSvc := newTestModulesService(t, store)
	apiServer.SetModulesService(modulesSvc)

	ctx := context.Background()
	adapter := configstore.Adapter{ID: "adapter.ai.primary", Source: "builtin", Type: "ai", Name: "Primary AI"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, "ai.primary", adapter.ID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	service := newModulesService(apiServer)

	resp, err := service.Overview(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("modules overview: %v", err)
	}
	if len(resp.GetModules()) == 0 {
		t.Fatalf("expected modules in overview")
	}

	var entry *apiv1.ModuleEntry
	for _, module := range resp.GetModules() {
		if module.GetSlot() == "ai.primary" {
			entry = module
			break
		}
	}
	if entry == nil {
		t.Fatalf("ai.primary slot not found in overview")
	}
	if entry.AdapterId == nil || entry.GetAdapterId() != adapter.ID {
		t.Fatalf("expected adapter %s, got %v", adapter.ID, entry.AdapterId)
	}
	if entry.GetStatus() == "" {
		t.Fatalf("expected status to be populated")
	}
}

func TestModulesServiceBindStartStop(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	store := openTestStore(t)
	modulesSvc := newTestModulesService(t, store)
	apiServer.SetModulesService(modulesSvc)

	ctx := context.Background()
	adapter := configstore.Adapter{ID: "adapter.ai.bind", Source: "builtin", Type: "ai", Name: "Bind AI"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	service := newModulesService(apiServer)

	bindResp, err := service.BindModule(ctx, &apiv1.BindModuleRequest{
		Slot:      "ai.primary",
		AdapterId: adapter.ID,
	})
	if err != nil {
		t.Fatalf("BindModule error: %v", err)
	}
	if bindResp.GetModule().AdapterId == nil || bindResp.GetModule().GetAdapterId() != adapter.ID {
		t.Fatalf("expected bound adapter %s, got %v", adapter.ID, bindResp.GetModule().AdapterId)
	}
	if bindResp.GetModule().GetStatus() != configstore.BindingStatusActive {
		t.Fatalf("expected status %s, got %s", configstore.BindingStatusActive, bindResp.GetModule().GetStatus())
	}

	startResp, err := service.StartModule(ctx, &apiv1.ModuleSlotRequest{Slot: "ai.primary"})
	if err != nil {
		t.Fatalf("StartModule error: %v", err)
	}
	if startResp.GetModule().GetStatus() != configstore.BindingStatusActive {
		t.Fatalf("expected active status after start, got %s", startResp.GetModule().GetStatus())
	}
	if startResp.GetModule().GetRuntime() == nil || startResp.GetModule().GetRuntime().GetHealth() == "" {
		t.Fatalf("expected runtime health after start")
	}

	stopResp, err := service.StopModule(ctx, &apiv1.ModuleSlotRequest{Slot: "ai.primary"})
	if err != nil {
		t.Fatalf("StopModule error: %v", err)
	}
	if stopResp.GetModule().GetStatus() != configstore.BindingStatusInactive {
		t.Fatalf("expected inactive status after stop, got %s", stopResp.GetModule().GetStatus())
	}
	if stopResp.GetModule().GetRuntime() == nil || !strings.EqualFold(stopResp.GetModule().GetRuntime().GetHealth(), string(eventbus.ModuleHealthStopped)) {
		t.Fatalf("expected runtime health 'stopped', got %+v", stopResp.GetModule().GetRuntime())
	}
}

func TestQuickstartServiceIncludesModules(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	store := apiServer.configStore
	modulesSvc := newTestModulesService(t, store)
	apiServer.SetModulesService(modulesSvc)

	ctx := context.Background()
	adapter := configstore.Adapter{ID: "adapter.ai.quickstart", Source: "builtin", Type: "ai", Name: "Quickstart AI"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, "ai.primary", adapter.ID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	service := newQuickstartService(apiServer)
	resp, err := service.GetStatus(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("QuickstartService.GetStatus error: %v", err)
	}
	if len(resp.GetModules()) == 0 {
		t.Fatalf("expected modules list in quickstart response")
	}
	var found bool
	for _, entry := range resp.GetModules() {
		if entry.GetSlot() == "ai.primary" {
			found = true
			if entry.GetAdapterId() != adapter.ID {
				t.Fatalf("expected adapter %s, got %s", adapter.ID, entry.GetAdapterId())
			}
			break
		}
	}
	if !found {
		t.Fatalf("ai.primary slot not present in quickstart modules")
	}
}

func TestQuickstartServiceWithoutModulesService(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	service := newQuickstartService(apiServer)
	_, err := service.GetStatus(context.Background(), &emptypb.Empty{})
	if err == nil {
		t.Fatalf("expected error when modules service unavailable")
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected error codes.Unavailable, got %v", status.Code(err))
	}
}

func TestAudioServiceStreamAudioIn(t *testing.T) {
	apiServer, sessionManager := newTestAPIServer(t)
	bus := eventbus.New()
	ingressSvc := ingress.New(bus)
	apiServer.SetAudioIngressService(ingressSvc)
	sessionManager.UseEventBus(bus)

	opts := pty.StartOptions{Command: "/bin/sh", Args: []string{"-c", "sleep 1"}, Rows: 24, Cols: 80}
	sess, err := sessionManager.CreateSession(opts, false)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
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
