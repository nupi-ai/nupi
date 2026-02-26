package intentrouter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/prompts"
)

func TestPromptAdapterUnknownEventTypeFallback(t *testing.T) {
	// Verify that an unrecognized EventType falls back to user_intent
	// and produces a valid response (not an error).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := store.Open(store.Options{
		DBPath: t.TempDir() + "/test.db",
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	for eventType, content := range store.DefaultPromptTemplates() {
		if err := s.SeedPromptTemplate(ctx, eventType, content); err != nil {
			t.Fatalf("seed template %s: %v", eventType, err)
		}
	}

	engine := prompts.New(s)
	if err := engine.LoadTemplates(ctx); err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}

	adapter := NewPromptEngineAdapter(engine)

	resp, err := adapter.Build(PromptBuildRequest{
		EventType:  "totally_unknown_type",
		SessionID:  "sess-fallback",
		Transcript: "hello",
	})
	if err != nil {
		t.Fatalf("Build(unknown): %v", err)
	}
	if resp == nil {
		t.Fatal("Build(unknown) returned nil response")
	}

	// user_intent template mentions "Nupi"; unknown type should fall back to it.
	if !strings.Contains(resp.SystemPrompt, "Nupi") {
		t.Fatalf("unknown event type should fall back to user_intent template, got: %s", resp.SystemPrompt)
	}
}

func TestPromptAdapterNilEngine(t *testing.T) {
	adapter := NewPromptEngineAdapter(nil)

	resp, err := adapter.Build(PromptBuildRequest{
		EventType:  EventTypeUserIntent,
		Transcript: "hello",
	})
	if err != nil {
		t.Fatalf("Build with nil engine should not error, got: %v", err)
	}
	if resp != nil {
		t.Fatalf("Build with nil engine should return nil response, got: %+v", resp)
	}
}

// Compile-time verification that intentrouter and prompts EventType constants
// resolve to the same underlying strings. If any mapping drifts, this fails to compile.
var _ = [7]struct{}{
	assertSameStr(string(EventTypeUserIntent), string(prompts.EventTypeUserIntent)),
	assertSameStr(string(EventTypeSessionOutput), string(prompts.EventTypeSessionOutput)),
	assertSameStr(string(EventTypeClarification), string(prompts.EventTypeClarification)),
	assertSameStr(string(EventTypeOnboarding), string(prompts.EventTypeOnboarding)),
	assertSameStr(string(EventTypeHeartbeat), string(prompts.EventTypeHeartbeat)),
	assertSameStr(string(EventTypeJournalCompaction), string(prompts.EventTypeJournalCompaction)),
	assertSameStr(string(EventTypeConversationCompaction), string(prompts.EventTypeConversationCompaction)),
}

func assertSameStr(a, b string) struct{} {
	if a != b {
		panic("event type constant mismatch: " + a + " != " + b)
	}
	return struct{}{}
}
