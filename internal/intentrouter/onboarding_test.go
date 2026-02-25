package intentrouter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/awareness"
	"github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/prompts"
)

// mockOnboardingProvider implements OnboardingProvider for testing.
type mockOnboardingProvider struct {
	onboarding      bool
	lockedToSession string // if set, only this session gets onboarding; empty = accept all sessions
	bootstrap       string
}

func (m *mockOnboardingProvider) IsOnboardingSession(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	if !m.onboarding {
		return false
	}
	if m.lockedToSession == "" {
		return true
	}
	return m.lockedToSession == sessionID
}
func (m *mockOnboardingProvider) BootstrapContent() string { return m.bootstrap }

func TestOnboardingEventTypeOverride(t *testing.T) {
	svc := NewService(nil,
		WithOnboardingProvider(&mockOnboardingProvider{onboarding: true, bootstrap: "test"}),
	)

	prompt := eventbus.ConversationPromptEvent{
		PromptID:  "p1",
		SessionID: "s1",
		NewMessage: eventbus.ConversationMessage{
			Text:   "hello",
			Origin: eventbus.OriginUser,
		},
		Metadata: map[string]string{
			constants.MetadataKeyEventType: constants.PromptEventUserIntent,
		},
	}

	req := svc.buildIntentRequest(prompt, nil, nil, svc.onboardingProvider)

	if req.EventType != EventTypeOnboarding {
		t.Fatalf("EventType = %q, want %q", req.EventType, EventTypeOnboarding)
	}
}

func TestNoOverrideWhenNotOnboarding(t *testing.T) {
	svc := NewService(nil,
		WithOnboardingProvider(&mockOnboardingProvider{onboarding: false}),
	)

	prompt := eventbus.ConversationPromptEvent{
		PromptID:  "p1",
		SessionID: "s1",
		NewMessage: eventbus.ConversationMessage{
			Text:   "hello",
			Origin: eventbus.OriginUser,
		},
		Metadata: map[string]string{
			constants.MetadataKeyEventType: constants.PromptEventUserIntent,
		},
	}

	req := svc.buildIntentRequest(prompt, nil, nil, svc.onboardingProvider)

	if req.EventType != EventTypeUserIntent {
		t.Fatalf("EventType = %q, want %q (no override when not onboarding)", req.EventType, EventTypeUserIntent)
	}
}

func TestNoOverrideForNonUserIntentEvents(t *testing.T) {
	svc := NewService(nil,
		WithOnboardingProvider(&mockOnboardingProvider{onboarding: true, bootstrap: "test"}),
	)

	for _, et := range []string{
		constants.PromptEventMemoryFlush,
		constants.PromptEventSessionSlug,
		constants.PromptEventSessionOutput,
		string(EventTypeHeartbeat),
	} {
		prompt := eventbus.ConversationPromptEvent{
			PromptID:  "p1",
			SessionID: "s1",
			NewMessage: eventbus.ConversationMessage{
				Text:   "hello",
				Origin: eventbus.OriginUser,
			},
			Metadata: map[string]string{
				constants.MetadataKeyEventType: et,
			},
		}

		req := svc.buildIntentRequest(prompt, nil, nil, svc.onboardingProvider)

		if req.EventType == EventTypeOnboarding {
			t.Fatalf("event_type %q was overridden to onboarding, but only user_intent should be overridden", et)
		}
	}
}

func TestBootstrapContentInjectedInSystemPrompt(t *testing.T) {
	provider := &mockOnboardingProvider{
		onboarding: true,
		bootstrap:  "# Welcome\nTest bootstrap instructions",
	}

	result := injectBootstrapContent("base prompt", provider)

	if !strings.Contains(result, "<onboarding>") {
		t.Fatal("should contain <onboarding> tag")
	}
	if !strings.Contains(result, "</onboarding>") {
		t.Fatal("should contain </onboarding> closing tag")
	}
	if !strings.Contains(result, "# Welcome") {
		t.Fatal("should preserve H1 heading inside tag (no stripping)")
	}
	if !strings.Contains(result, "Test bootstrap instructions") {
		t.Fatal("should contain bootstrap body content")
	}
	if !strings.HasPrefix(result, "base prompt") {
		t.Fatal("should preserve original system prompt")
	}
}

func TestInjectBootstrapContentNilProvider(t *testing.T) {
	result := injectBootstrapContent("base prompt", nil)

	if result != "base prompt" {
		t.Fatalf("expected unchanged prompt with nil provider, got %q", result)
	}
}

func TestInjectBootstrapContentEmptyContent(t *testing.T) {
	provider := &mockOnboardingProvider{
		onboarding: true,
		bootstrap:  "",
	}

	result := injectBootstrapContent("base prompt", provider)

	if result != "base prompt" {
		t.Fatalf("expected unchanged prompt with empty bootstrap, got %q", result)
	}
}

func TestInjectBootstrapContentWhitespaceOnly(t *testing.T) {
	provider := &mockOnboardingProvider{
		onboarding: true,
		bootstrap:  "   \n  \n  ",
	}

	result := injectBootstrapContent("base prompt", provider)

	if result != "base prompt" {
		t.Fatalf("expected unchanged prompt with whitespace-only bootstrap, got %q", result)
	}
}

func TestPromptAdapterMapsOnboardingEventType(t *testing.T) {
	// Regression test: verify PromptEngineAdapter maps EventTypeOnboarding →
	// prompts.EventTypeOnboarding and returns the onboarding template content
	// (not user_intent fallback).
	//
	// Uses store.Open() which has a 5s internal timeout for keychain probing.
	// Under -race detector overhead this can exceed the default deadline, so
	// a generous 30s context is used. If this test flakes, increase the timeout
	// or refactor to mock the prompts engine.
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

	// Build with onboarding event type.
	resp, err := adapter.Build(PromptBuildRequest{
		EventType:  EventTypeOnboarding,
		Transcript: "hello",
	})
	if err != nil {
		t.Fatalf("Build(onboarding): %v", err)
	}
	if resp == nil {
		t.Fatal("Build(onboarding) returned nil response")
	}

	// The onboarding template mentions onboarding_complete and core_memory_update.
	// The user_intent template does NOT. This distinguishes correct mapping from fallback.
	if !strings.Contains(resp.SystemPrompt, "onboarding_complete") {
		t.Fatalf("onboarding SystemPrompt should contain 'onboarding_complete' (got user_intent fallback?)\nSystemPrompt: %s", resp.SystemPrompt)
	}
}

func TestOnboardingToolFiltering(t *testing.T) {
	reg := NewToolRegistry()

	// Register all expected tools.
	coreUpdate := newMockHandler("core_memory_update", "Update core memory")
	onboardingComplete := newMockHandler("onboarding_complete", "Complete onboarding")
	memSearch := newMockHandler("memory_search", "Search memory")

	reg.Register(coreUpdate)
	reg.Register(onboardingComplete)
	reg.Register(memSearch)

	defs := reg.GetToolsForEventType(EventTypeOnboarding)

	// Only core_memory_update and onboarding_complete should be returned.
	if len(defs) != 2 {
		t.Fatalf("Expected 2 tools for onboarding, got %d", len(defs))
	}

	names := make(map[string]bool)
	for _, d := range defs {
		names[d.Name] = true
	}

	if !names["core_memory_update"] {
		t.Fatal("Expected core_memory_update in onboarding tools")
	}
	if !names["onboarding_complete"] {
		t.Fatal("Expected onboarding_complete in onboarding tools")
	}
	if names["memory_search"] {
		t.Fatal("memory_search should NOT be in onboarding tools")
	}
}

func TestEmptySessionIDDoesNotGetOnboarding(t *testing.T) {
	svc := NewService(nil,
		WithOnboardingProvider(&mockOnboardingProvider{onboarding: true, bootstrap: "test"}),
	)

	prompt := eventbus.ConversationPromptEvent{
		PromptID:  "p1",
		SessionID: "", // empty — must NOT claim onboarding lock
		NewMessage: eventbus.ConversationMessage{
			Text:   "hello",
			Origin: eventbus.OriginUser,
		},
		Metadata: map[string]string{
			constants.MetadataKeyEventType: constants.PromptEventUserIntent,
		},
	}

	req := svc.buildIntentRequest(prompt, nil, nil, svc.onboardingProvider)

	if req.EventType != EventTypeUserIntent {
		t.Fatalf("EventType = %q, want %q (empty session ID must not trigger onboarding)", req.EventType, EventTypeUserIntent)
	}
}

func TestHandlePromptOnboardingFullFlow(t *testing.T) {
	// Integration test: verifies the full handlePrompt path for onboarding:
	// 1. Event type override from user_intent → onboarding
	// 2. Bootstrap content injected in system prompt via <onboarding> tag
	// 3. Tool filtering limits tools to onboarding set
	// 4. Adapter receives correct request
	bus := eventbus.New()
	defer bus.Shutdown()

	provider := &mockOnboardingProvider{
		onboarding: true,
		bootstrap:  "# Welcome to Nupi\nAsk the user their name and preferences.",
	}

	adapter := &testAdapter{
		name:  "test",
		ready: true,
		response: &IntentResponse{
			PromptID: "p-onboard",
			Actions:  []IntentAction{{Type: ActionSpeak, Text: "Hello! What's your name?"}},
		},
	}

	coreMemProvider := &mockCoreMemoryProvider{memory: "## SOUL\nBe helpful."}

	reg := NewToolRegistry()
	reg.Register(newMockHandler("core_memory_update", "Update core memory"))
	reg.Register(newMockHandler("onboarding_complete", "Complete onboarding"))
	reg.Register(newMockHandler("memory_search", "Search memory"))

	svc := NewService(bus,
		WithAdapter(adapter),
		WithOnboardingProvider(provider),
		WithCoreMemoryProvider(coreMemProvider),
		WithToolRegistry(reg),
	)

	replySub := eventbus.SubscribeTo(bus, eventbus.Conversation.Reply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Publish a user_intent prompt — should be overridden to onboarding.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Prompt, eventbus.SourceConversation, eventbus.ConversationPromptEvent{
		PromptID:  "p-onboard",
		SessionID: "s1",
		NewMessage: eventbus.ConversationMessage{
			Text:   "hi there",
			Origin: eventbus.OriginUser,
		},
		Metadata: map[string]string{
			constants.MetadataKeyEventType: constants.PromptEventUserIntent,
		},
	})

	// Wait for reply.
	select {
	case <-replySub.C():
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for reply")
	}

	// Verify adapter received onboarding event type.
	req := adapter.GetLastRequest()
	if req.EventType != EventTypeOnboarding {
		t.Fatalf("adapter EventType = %q, want %q", req.EventType, EventTypeOnboarding)
	}

	// Verify bootstrap content was injected via <onboarding> tag.
	if !strings.Contains(req.SystemPrompt, "<onboarding>") {
		t.Fatal("system prompt should contain <onboarding> tag")
	}
	if !strings.Contains(req.SystemPrompt, "# Welcome to Nupi") {
		t.Fatal("system prompt should preserve H1 inside <onboarding> tag")
	}
	if !strings.Contains(req.SystemPrompt, "Ask the user their name") {
		t.Fatal("system prompt should contain bootstrap body content")
	}

	// Verify core memory was prepended via <core-memory> tag.
	if !strings.Contains(req.SystemPrompt, "<core-memory>") {
		t.Fatal("system prompt should contain <core-memory> tag")
	}
	if !strings.Contains(req.SystemPrompt, "Be helpful.") {
		t.Fatal("system prompt should contain core memory")
	}

	// Verify only onboarding tools were offered (not memory_search).
	toolNames := make(map[string]bool)
	for _, td := range req.AvailableTools {
		toolNames[td.Name] = true
	}
	if !toolNames["core_memory_update"] || !toolNames["onboarding_complete"] {
		t.Fatalf("expected onboarding tools, got %v", toolNames)
	}
	if toolNames["memory_search"] {
		t.Fatal("memory_search should NOT be available during onboarding")
	}
}

func TestOnboardingSessionLockInBuildRequest(t *testing.T) {
	// Session A owns onboarding; session B should stay as user_intent.
	provider := &mockOnboardingProvider{
		onboarding:      true,
		lockedToSession: "session-A",
		bootstrap:       "test",
	}

	svc := NewService(nil, WithOnboardingProvider(provider))

	// Session A should get onboarding.
	promptA := eventbus.ConversationPromptEvent{
		PromptID:  "p1",
		SessionID: "session-A",
		NewMessage: eventbus.ConversationMessage{
			Text:   "hello",
			Origin: eventbus.OriginUser,
		},
		Metadata: map[string]string{constants.MetadataKeyEventType: constants.PromptEventUserIntent},
	}
	reqA := svc.buildIntentRequest(promptA, nil, nil, svc.onboardingProvider)
	if reqA.EventType != EventTypeOnboarding {
		t.Fatalf("session A: EventType = %q, want %q", reqA.EventType, EventTypeOnboarding)
	}

	// Session B should stay as user_intent.
	promptB := eventbus.ConversationPromptEvent{
		PromptID:  "p2",
		SessionID: "session-B",
		NewMessage: eventbus.ConversationMessage{
			Text:   "hello",
			Origin: eventbus.OriginUser,
		},
		Metadata: map[string]string{constants.MetadataKeyEventType: constants.PromptEventUserIntent},
	}
	reqB := svc.buildIntentRequest(promptB, nil, nil, svc.onboardingProvider)
	if reqB.EventType != EventTypeUserIntent {
		t.Fatalf("session B: EventType = %q, want %q (should NOT get onboarding)", reqB.EventType, EventTypeUserIntent)
	}
}

// TestRealAwarenessOnboardingIntegration uses a real awareness.Service (not mock)
// to verify the full onboarding lifecycle through the intent router:
// scaffold → onboarding detected → event type override → complete → normal flow.
func TestRealAwarenessOnboardingIntegration(t *testing.T) {
	dir := t.TempDir()
	awarenessService := awareness.NewService(dir)
	ctx := context.Background()

	if err := awarenessService.Start(ctx); err != nil {
		t.Fatalf("awareness Start: %v", err)
	}
	defer awarenessService.Shutdown(ctx)

	// Verify BOOTSTRAP.md was scaffolded (onboarding active).
	bootstrapPath := filepath.Join(dir, "awareness", "BOOTSTRAP.md")
	if _, err := os.Stat(bootstrapPath); err != nil {
		t.Fatalf("expected BOOTSTRAP.md to exist after fresh Start: %v", err)
	}

	routerSvc := NewService(nil, WithOnboardingProvider(awarenessService))

	makePrompt := func(sessionID string) eventbus.ConversationPromptEvent {
		return eventbus.ConversationPromptEvent{
			PromptID:  "p-" + sessionID,
			SessionID: sessionID,
			NewMessage: eventbus.ConversationMessage{
				Text:   "hello",
				Origin: eventbus.OriginUser,
			},
			Metadata: map[string]string{constants.MetadataKeyEventType: constants.PromptEventUserIntent},
		}
	}

	// Phase 1: First prompt should be overridden to onboarding.
	req1 := routerSvc.buildIntentRequest(makePrompt("sess-1"), nil, nil, routerSvc.onboardingProvider)
	if req1.EventType != EventTypeOnboarding {
		t.Fatalf("phase 1: EventType = %q, want %q", req1.EventType, EventTypeOnboarding)
	}

	// Phase 2: Bootstrap content should be injectable via <onboarding> tag.
	bc := awarenessService.BootstrapContent()
	if bc == "" {
		t.Fatal("phase 2: BootstrapContent() should be non-empty")
	}

	injected := injectBootstrapContent("base prompt", awarenessService)
	if !strings.Contains(injected, "<onboarding>") {
		t.Fatal("phase 2: injected content should contain <onboarding> tag")
	}

	// Phase 3: Complete onboarding.
	removed, err := awarenessService.CompleteOnboarding()
	if err != nil {
		t.Fatalf("phase 3: CompleteOnboarding: %v", err)
	}
	if !removed {
		t.Fatal("phase 3: expected removed=true")
	}

	// Verify BOOTSTRAP.md is gone.
	if _, err := os.Stat(bootstrapPath); err == nil {
		t.Fatal("phase 3: BOOTSTRAP.md should be deleted after CompleteOnboarding")
	}

	// Phase 4: Next prompt should stay as user_intent (onboarding done).
	req2 := routerSvc.buildIntentRequest(makePrompt("sess-1"), nil, nil, routerSvc.onboardingProvider)
	if req2.EventType != EventTypeUserIntent {
		t.Fatalf("phase 4: EventType = %q, want %q (onboarding should be done)", req2.EventType, EventTypeUserIntent)
	}

	// Phase 5: Bootstrap injection should be no-op after completion.
	notInjected := injectBootstrapContent("base prompt", awarenessService)
	if strings.Contains(notInjected, "<onboarding>") {
		t.Fatal("phase 5: should not inject bootstrap after completion")
	}

	// Phase 6: Restart awareness — BOOTSTRAP.md should NOT be recreated.
	awarenessService.Shutdown(ctx)
	awarenessService2 := awareness.NewService(dir)
	if err := awarenessService2.Start(ctx); err != nil {
		t.Fatalf("phase 6: second Start: %v", err)
	}
	defer awarenessService2.Shutdown(ctx)

	if _, err := os.Stat(bootstrapPath); err == nil {
		t.Fatal("phase 6: BOOTSTRAP.md should not be recreated after restart (marker prevents recreation)")
	}
}
