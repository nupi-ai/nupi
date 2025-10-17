package server

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/pty"
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
