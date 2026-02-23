package intentrouter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/prompts"
)

// mockOnboardingProvider implements OnboardingProvider for testing.
type mockOnboardingProvider struct {
	onboarding        bool
	onboardingSession string // if set, only this session gets onboarding
	bootstrap         string
}

func (m *mockOnboardingProvider) IsOnboardingSession(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	if !m.onboarding {
		return false
	}
	if m.onboardingSession == "" {
		// No lock configured — accept all (for existing tests).
		return true
	}
	return m.onboardingSession == sessionID
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
		string(EventTypeScheduledTask),
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

	if !strings.Contains(result, "## ONBOARDING") {
		t.Fatal("should contain '## ONBOARDING' section")
	}
	if strings.Contains(result, "# Welcome") {
		t.Fatal("should NOT contain H1 heading (should be stripped)")
	}
	if !strings.Contains(result, "Test bootstrap instructions") {
		t.Fatal("should contain bootstrap body content")
	}
	if !strings.HasPrefix(result, "base prompt") {
		t.Fatal("should preserve original system prompt")
	}
}

func TestInjectBootstrapContentH1Only(t *testing.T) {
	// BOOTSTRAP.md with only an H1 heading and no body — injection should be skipped.
	provider := &mockOnboardingProvider{
		onboarding: true,
		bootstrap:  "# Welcome\n",
	}

	result := injectBootstrapContent("base prompt", provider)

	if strings.Contains(result, "## ONBOARDING") {
		t.Fatal("should NOT inject ONBOARDING section when content is H1-only")
	}
	if result != "base prompt" {
		t.Fatalf("expected unchanged prompt, got %q", result)
	}
}

func TestInjectBootstrapContentNilProvider(t *testing.T) {
	result := injectBootstrapContent("base prompt", nil)

	if result != "base prompt" {
		t.Fatalf("expected unchanged prompt with nil provider, got %q", result)
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

func TestInjectBootstrapContentNoH1(t *testing.T) {
	// Content without an H1 heading — should be injected as-is.
	provider := &mockOnboardingProvider{
		onboarding: true,
		bootstrap:  "Just plain instructions without a heading.",
	}

	result := injectBootstrapContent("base prompt", provider)

	if !strings.Contains(result, "## ONBOARDING") {
		t.Fatal("should contain '## ONBOARDING' section")
	}
	if !strings.Contains(result, "Just plain instructions without a heading.") {
		t.Fatal("should contain the bootstrap content verbatim")
	}
}

func TestInjectBootstrapContentH1NoNewline(t *testing.T) {
	// H1-only without trailing newline — must not cause hierarchy inversion.
	provider := &mockOnboardingProvider{
		onboarding: true,
		bootstrap:  "# Welcome",
	}

	result := injectBootstrapContent("base prompt", provider)

	if strings.Contains(result, "# Welcome") {
		t.Fatal("should NOT inject H1 heading without body (hierarchy inversion)")
	}
	if result != "base prompt" {
		t.Fatalf("expected unchanged prompt, got %q", result)
	}
}

func TestInjectBootstrapContentBareHash(t *testing.T) {
	// Bare "#" (empty H1 per CommonMark) should be stripped.
	provider := &mockOnboardingProvider{
		onboarding: true,
		bootstrap:  "#\nContent after empty heading",
	}

	result := injectBootstrapContent("base prompt", provider)

	if strings.Contains(result, "\n#\n") {
		t.Fatal("bare '#' should be stripped as empty H1")
	}
	if !strings.Contains(result, "Content after empty heading") {
		t.Fatal("should contain body content after bare H1")
	}
	if !strings.Contains(result, "## ONBOARDING") {
		t.Fatal("should contain ONBOARDING section header")
	}
}

func TestInjectBootstrapContentMultipleH1(t *testing.T) {
	// Multiple H1 headings — ALL must be stripped to prevent hierarchy inversion.
	provider := &mockOnboardingProvider{
		onboarding: true,
		bootstrap:  "# Welcome\nContent here\n# Another Section\nMore content",
	}

	result := injectBootstrapContent("base prompt", provider)

	if strings.Contains(result, "# Welcome") {
		t.Fatal("should NOT contain first H1 heading")
	}
	if strings.Contains(result, "# Another Section") {
		t.Fatal("should NOT contain second H1 heading")
	}
	if !strings.Contains(result, "Content here") {
		t.Fatal("should contain body after first H1")
	}
	if !strings.Contains(result, "More content") {
		t.Fatal("should contain body after second H1")
	}
	if !strings.Contains(result, "## ONBOARDING") {
		t.Fatal("should contain ONBOARDING section header")
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

func TestOnboardingSessionLockInBuildRequest(t *testing.T) {
	// Session A owns onboarding; session B should stay as user_intent.
	provider := &mockOnboardingProvider{
		onboarding:        true,
		onboardingSession: "session-A",
		bootstrap:         "test",
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
