package prompts

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

// EventType describes the type of AI request.
type EventType string

const (
	// EventTypeUserIntent is for interpreting user voice/text commands.
	EventTypeUserIntent EventType = "user_intent"

	// EventTypeSessionOutput is for analyzing session output.
	EventTypeSessionOutput EventType = "session_output"

	// EventTypeHistorySummary is for summarizing conversation history.
	EventTypeHistorySummary EventType = "history_summary"

	// EventTypeClarification is for handling user responses to clarification requests.
	EventTypeClarification EventType = "clarification"

	// EventTypeMemoryFlush is for extracting important context before conversation compaction.
	EventTypeMemoryFlush EventType = "memory_flush"

	// EventTypeSessionSlug is for generating a session slug and summary on session close.
	EventTypeSessionSlug EventType = "session_slug"
)

// SessionInfo provides context about an available session.
type SessionInfo struct {
	ID        string            `json:"id"`
	Command   string            `json:"command"`
	Tool      string            `json:"tool,omitempty"`
	Status    string            `json:"status"`
	StartTime time.Time         `json:"start_time"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// BuildRequest contains all context needed to build a prompt.
type BuildRequest struct {
	// EventType determines which prompt template to use.
	EventType EventType

	// SessionID is the target session (empty for sessionless commands).
	SessionID string

	// Transcript is the user's speech or text input.
	Transcript string

	// History contains recent conversation turns for context.
	History []eventbus.ConversationTurn

	// AvailableSessions lists all sessions the user can interact with.
	AvailableSessions []SessionInfo

	// CurrentTool is the detected tool in the current session.
	CurrentTool string

	// SessionOutput is the output from the session (for EventTypeSessionOutput).
	SessionOutput string

	// ClarificationQuestion is the original question (for EventTypeClarification).
	ClarificationQuestion string

	// Metadata contains additional context.
	Metadata map[string]string
}

// BuildResponse contains the generated prompts.
type BuildResponse struct {
	// SystemPrompt is the system-level instruction for the AI.
	SystemPrompt string

	// UserPrompt is the user-level message for the AI.
	UserPrompt string

	// Context contains additional structured data for advanced plugins.
	Context map[string]any
}

// Engine manages prompt templates and builds prompts for AI adapters.
type Engine struct {
	store *store.Store

	mu        sync.RWMutex
	templates map[EventType]*template.Template
}

// New creates a new prompt engine backed by the config store.
func New(s *store.Store) *Engine {
	return &Engine{
		store:     s,
		templates: make(map[EventType]*template.Template),
	}
}

// LoadTemplates loads and parses all templates from the store.
// Should be called after store initialization and prompt seeding.
func (e *Engine) LoadTemplates(ctx context.Context) error {
	templates, err := e.store.ListPromptTemplates(ctx)
	if err != nil {
		return fmt.Errorf("prompts: failed to load templates from store: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for _, pt := range templates {
		eventType := EventType(pt.EventType)
		tmpl, err := template.New(pt.EventType).Funcs(templateFuncs).Parse(pt.Content)
		if err != nil {
			return fmt.Errorf("prompts: failed to parse template %s: %w", pt.EventType, err)
		}
		e.templates[eventType] = tmpl
	}

	return nil
}

// InvalidateCache clears cached templates. Call this after updating a template.
func (e *Engine) InvalidateCache() {
	e.mu.Lock()
	e.templates = make(map[EventType]*template.Template)
	e.mu.Unlock()
}

// Reload reloads templates from the store.
func (e *Engine) Reload(ctx context.Context) error {
	e.InvalidateCache()
	return e.LoadTemplates(ctx)
}

// BuildPrompt generates system and user prompts for the given request.
func (e *Engine) BuildPrompt(req BuildRequest) (*BuildResponse, error) {
	e.mu.RLock()
	tmpl, ok := e.templates[req.EventType]
	e.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("prompts: unknown event type %q (templates not loaded?)", req.EventType)
	}

	data := e.buildTemplateData(req)

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("prompts: template execution failed: %w", err)
	}

	parts := strings.SplitN(buf.String(), "---USER---", 2)
	systemPrompt := strings.TrimSpace(parts[0])
	userPrompt := ""
	if len(parts) > 1 {
		userPrompt = strings.TrimSpace(parts[1])
	}

	return &BuildResponse{
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		Context:      data,
	}, nil
}

// SetTemplate updates a template in the store and reloads it.
func (e *Engine) SetTemplate(ctx context.Context, eventType EventType, content string) error {
	// Validate template syntax first
	_, err := template.New(string(eventType)).Funcs(templateFuncs).Parse(content)
	if err != nil {
		return fmt.Errorf("prompts: invalid template syntax: %w", err)
	}

	// Save to store
	if err := e.store.SetPromptTemplate(ctx, string(eventType), content); err != nil {
		return fmt.Errorf("prompts: failed to save template: %w", err)
	}

	// Reload templates
	return e.Reload(ctx)
}

// GetTemplate returns the current template content for an event type.
func (e *Engine) GetTemplate(ctx context.Context, eventType EventType) (string, bool, error) {
	pt, err := e.store.GetPromptTemplate(ctx, string(eventType))
	if err != nil {
		if store.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return pt.Content, pt.IsCustom, nil
}

// ResetTemplate resets a template to its default.
func (e *Engine) ResetTemplate(ctx context.Context, eventType EventType) error {
	defaults := store.DefaultPromptTemplates()
	defaultContent, ok := defaults[string(eventType)]
	if !ok {
		return fmt.Errorf("prompts: unknown event type %q", eventType)
	}

	if err := e.store.ResetPromptTemplate(ctx, string(eventType), defaultContent); err != nil {
		return fmt.Errorf("prompts: failed to reset template: %w", err)
	}

	return e.Reload(ctx)
}

// ResetAllTemplates resets all templates to defaults.
func (e *Engine) ResetAllTemplates(ctx context.Context) error {
	if err := e.store.ResetAllPromptTemplates(ctx, store.DefaultPromptTemplates()); err != nil {
		return fmt.Errorf("prompts: failed to reset all templates: %w", err)
	}

	return e.Reload(ctx)
}

func (e *Engine) buildTemplateData(req BuildRequest) map[string]any {
	data := map[string]any{
		"event_type":      string(req.EventType),
		"session_id":      req.SessionID,
		"transcript":      req.Transcript,
		"current_tool":    req.CurrentTool,
		"session_output":  req.SessionOutput,
		"clarification_q": req.ClarificationQuestion,
		"has_session":     req.SessionID != "",
		"has_tool":        req.CurrentTool != "",
		"metadata":        req.Metadata,
	}

	// Format history
	var historyLines []string
	for _, turn := range req.History {
		historyLines = append(historyLines, fmt.Sprintf("[%s] %s", turn.Origin.Label(), turn.Text))
	}
	data["history"] = strings.Join(historyLines, "\n")
	data["history_count"] = len(req.History)

	// Format available sessions
	var sessionLines []string
	for _, sess := range req.AvailableSessions {
		tool := sess.Tool
		if tool == "" {
			tool = "unknown"
		}
		sessionLines = append(sessionLines, fmt.Sprintf("- %s: %s (tool: %s, status: %s)",
			sess.ID, sess.Command, tool, sess.Status))
	}
	data["sessions"] = strings.Join(sessionLines, "\n")
	data["sessions_count"] = len(req.AvailableSessions)

	return data
}

var templateFuncs = template.FuncMap{
	"truncate": func(maxLen int, s string) string {
		if len(s) <= maxLen {
			return s
		}
		return s[:maxLen] + "..."
	},
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
}
