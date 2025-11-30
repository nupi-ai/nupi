package prompts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

// setupTestEngine creates a test store, seeds default templates, and returns a new engine.
func setupTestEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()

	// Create test store with temp database
	s, err := store.Open(store.Options{
		DBPath: t.TempDir() + "/test.db",
	})
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}

	// Seed default templates
	ctx := context.Background()
	for eventType, content := range store.DefaultPromptTemplates() {
		if err := s.SeedPromptTemplate(ctx, eventType, content); err != nil {
			t.Fatalf("failed to seed template %s: %v", eventType, err)
		}
	}

	// Create engine and load templates
	e := New(s)
	if err := e.LoadTemplates(ctx); err != nil {
		t.Fatalf("failed to load templates: %v", err)
	}

	return e, s
}

func TestNew(t *testing.T) {
	e, s := setupTestEngine(t)
	defer s.Close()

	if e == nil {
		t.Fatal("New() returned nil")
	}

	// Should have all default templates loaded
	for _, eventType := range []EventType{
		EventTypeUserIntent,
		EventTypeSessionOutput,
		EventTypeHistorySummary,
		EventTypeClarification,
	} {
		if _, ok := e.templates[eventType]; !ok {
			t.Errorf("missing default template for %s", eventType)
		}
	}
}

func TestBuildPrompt_UserIntent(t *testing.T) {
	e, s := setupTestEngine(t)
	defer s.Close()

	req := BuildRequest{
		EventType:   EventTypeUserIntent,
		SessionID:   "session-123",
		Transcript:  "run the tests",
		CurrentTool: "go",
		AvailableSessions: []SessionInfo{
			{
				ID:      "session-123",
				Command: "go test ./...",
				Tool:    "go",
				Status:  "running",
			},
		},
		History: []eventbus.ConversationTurn{
			{
				Origin: eventbus.OriginUser,
				Text:   "let's run some tests",
				At:     time.Now(),
			},
		},
	}

	resp, err := e.BuildPrompt(req)
	if err != nil {
		t.Fatalf("BuildPrompt failed: %v", err)
	}

	if resp.SystemPrompt == "" {
		t.Error("SystemPrompt is empty")
	}
	if resp.UserPrompt == "" {
		t.Error("UserPrompt is empty")
	}

	// Check that key elements are present
	if !strings.Contains(resp.SystemPrompt, "Nupi") {
		t.Error("SystemPrompt should mention Nupi")
	}
	if !strings.Contains(resp.SystemPrompt, "session-123") {
		t.Error("SystemPrompt should contain session ID")
	}
	if !strings.Contains(resp.SystemPrompt, "go") {
		t.Error("SystemPrompt should contain tool name")
	}
	if !strings.Contains(resp.UserPrompt, "run the tests") {
		t.Error("UserPrompt should contain transcript")
	}
}

func TestBuildPrompt_SessionOutput(t *testing.T) {
	e, s := setupTestEngine(t)
	defer s.Close()

	req := BuildRequest{
		EventType:     EventTypeSessionOutput,
		SessionID:     "session-456",
		CurrentTool:   "npm",
		SessionOutput: "npm ERR! code ENOENT\nnpm ERR! syscall open",
	}

	resp, err := e.BuildPrompt(req)
	if err != nil {
		t.Fatalf("BuildPrompt failed: %v", err)
	}

	if !strings.Contains(resp.UserPrompt, "npm ERR!") {
		t.Error("UserPrompt should contain session output")
	}
}

func TestBuildPrompt_HistorySummary(t *testing.T) {
	e, s := setupTestEngine(t)
	defer s.Close()

	req := BuildRequest{
		EventType: EventTypeHistorySummary,
		History: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "start a new project", At: time.Now()},
			{Origin: eventbus.OriginAI, Text: "I'll create a new project for you", At: time.Now()},
			{Origin: eventbus.OriginTool, Text: "mkdir my-project && cd my-project", At: time.Now()},
		},
	}

	resp, err := e.BuildPrompt(req)
	if err != nil {
		t.Fatalf("BuildPrompt failed: %v", err)
	}

	if !strings.Contains(resp.UserPrompt, "[user]") {
		t.Error("UserPrompt should contain formatted history")
	}
	if !strings.Contains(resp.UserPrompt, "[assistant]") {
		t.Error("UserPrompt should contain AI turn")
	}
}

func TestBuildPrompt_Clarification(t *testing.T) {
	e, s := setupTestEngine(t)
	defer s.Close()

	req := BuildRequest{
		EventType:             EventTypeClarification,
		SessionID:             "session-789",
		Transcript:            "yes, delete it",
		ClarificationQuestion: "Are you sure you want to delete this file?",
	}

	resp, err := e.BuildPrompt(req)
	if err != nil {
		t.Fatalf("BuildPrompt failed: %v", err)
	}

	if !strings.Contains(resp.SystemPrompt, "Are you sure you want to delete this file?") {
		t.Error("SystemPrompt should contain original question")
	}
	if !strings.Contains(resp.UserPrompt, "yes, delete it") {
		t.Error("UserPrompt should contain user's response")
	}
}

func TestBuildPrompt_UnknownEventType(t *testing.T) {
	e, s := setupTestEngine(t)
	defer s.Close()

	req := BuildRequest{
		EventType:  EventType("unknown"),
		Transcript: "hello",
	}

	_, err := e.BuildPrompt(req)
	if err == nil {
		t.Error("expected error for unknown event type")
	}
}

func TestBuildPrompt_NoSession(t *testing.T) {
	e, s := setupTestEngine(t)
	defer s.Close()

	req := BuildRequest{
		EventType:  EventTypeUserIntent,
		SessionID:  "", // No session
		Transcript: "what sessions are available?",
		AvailableSessions: []SessionInfo{
			{ID: "sess-1", Command: "bash", Status: "running"},
			{ID: "sess-2", Command: "python", Status: "detached"},
		},
	}

	resp, err := e.BuildPrompt(req)
	if err != nil {
		t.Fatalf("BuildPrompt failed: %v", err)
	}

	// Should list available sessions
	if !strings.Contains(resp.SystemPrompt, "sess-1") {
		t.Error("SystemPrompt should list available sessions")
	}
	if !strings.Contains(resp.SystemPrompt, "sess-2") {
		t.Error("SystemPrompt should list all sessions")
	}
}

func TestSetTemplate(t *testing.T) {
	e, s := setupTestEngine(t)
	defer s.Close()

	customTemplate := `Custom system prompt for {{.current_tool}}
---USER---
Custom user: {{.transcript}}`

	ctx := context.Background()
	err := e.SetTemplate(ctx, EventTypeUserIntent, customTemplate)
	if err != nil {
		t.Fatalf("SetTemplate failed: %v", err)
	}

	req := BuildRequest{
		EventType:   EventTypeUserIntent,
		CurrentTool: "custom-tool",
		Transcript:  "test message",
	}

	resp, err := e.BuildPrompt(req)
	if err != nil {
		t.Fatalf("BuildPrompt failed: %v", err)
	}

	if !strings.Contains(resp.SystemPrompt, "Custom system prompt for custom-tool") {
		t.Error("should use custom template")
	}
	if !strings.Contains(resp.UserPrompt, "Custom user: test message") {
		t.Error("should use custom user prompt")
	}
}

func TestSetTemplate_Invalid(t *testing.T) {
	e, s := setupTestEngine(t)
	defer s.Close()

	invalidTemplate := `{{.invalid_func unclosed`

	ctx := context.Background()
	err := e.SetTemplate(ctx, EventTypeUserIntent, invalidTemplate)
	if err == nil {
		t.Error("expected error for invalid template")
	}
}

func TestTruncateFunc(t *testing.T) {
	e, s := setupTestEngine(t)
	defer s.Close()

	// Create a template that uses truncate
	tmpl := `{{truncate 10 .session_output}}
---USER---
test`

	ctx := context.Background()
	err := e.SetTemplate(ctx, EventTypeSessionOutput, tmpl)
	if err != nil {
		t.Fatalf("SetTemplate failed: %v", err)
	}

	req := BuildRequest{
		EventType:     EventTypeSessionOutput,
		SessionOutput: "This is a very long output that should be truncated",
	}

	resp, err := e.BuildPrompt(req)
	if err != nil {
		t.Fatalf("BuildPrompt failed: %v", err)
	}

	if len(resp.SystemPrompt) > 20 { // 10 chars + "..."
		t.Errorf("truncate should limit output, got: %s", resp.SystemPrompt)
	}
}

func TestResetTemplate(t *testing.T) {
	e, s := setupTestEngine(t)
	defer s.Close()

	ctx := context.Background()

	// First, set a custom template
	customTemplate := `Custom template
---USER---
Custom`

	if err := e.SetTemplate(ctx, EventTypeUserIntent, customTemplate); err != nil {
		t.Fatalf("SetTemplate failed: %v", err)
	}

	// Reset it
	if err := e.ResetTemplate(ctx, EventTypeUserIntent); err != nil {
		t.Fatalf("ResetTemplate failed: %v", err)
	}

	// Build and check it's back to default
	req := BuildRequest{
		EventType:  EventTypeUserIntent,
		Transcript: "test",
	}

	resp, err := e.BuildPrompt(req)
	if err != nil {
		t.Fatalf("BuildPrompt failed: %v", err)
	}

	if strings.Contains(resp.SystemPrompt, "Custom template") {
		t.Error("should have reset to default template")
	}
	if !strings.Contains(resp.SystemPrompt, "Nupi") {
		t.Error("should have default template mentioning Nupi")
	}
}

func TestGetTemplate(t *testing.T) {
	e, s := setupTestEngine(t)
	defer s.Close()

	ctx := context.Background()

	// Get default template
	content, isCustom, err := e.GetTemplate(ctx, EventTypeUserIntent)
	if err != nil {
		t.Fatalf("GetTemplate failed: %v", err)
	}
	if isCustom {
		t.Error("default template should not be marked as custom")
	}
	if !strings.Contains(content, "Nupi") {
		t.Error("content should contain default template")
	}

	// Set custom template
	customTemplate := `Custom`
	if err := s.SetPromptTemplate(ctx, string(EventTypeUserIntent), customTemplate); err != nil {
		t.Fatalf("SetPromptTemplate failed: %v", err)
	}

	content, isCustom, err = e.GetTemplate(ctx, EventTypeUserIntent)
	if err != nil {
		t.Fatalf("GetTemplate failed: %v", err)
	}
	if !isCustom {
		t.Error("should be marked as custom after modification")
	}
	if content != customTemplate {
		t.Errorf("content mismatch: got %q, want %q", content, customTemplate)
	}
}
