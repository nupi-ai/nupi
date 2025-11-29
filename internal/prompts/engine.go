package prompts

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

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
	mu          sync.RWMutex
	templates   map[EventType]*template.Template
	templateDir string
}

// EngineOption configures the prompt engine.
type EngineOption func(*Engine)

// WithTemplateDir sets the directory to load custom templates from.
// Template files should be named: user_intent.tmpl, session_output.tmpl, etc.
func WithTemplateDir(dir string) EngineOption {
	return func(e *Engine) {
		e.templateDir = dir
	}
}

// New creates a new prompt engine with default templates.
// Custom templates can be loaded from a directory using WithTemplateDir option.
func New(opts ...EngineOption) *Engine {
	e := &Engine{
		templates: make(map[EventType]*template.Template),
	}

	for _, opt := range opts {
		opt(e)
	}

	e.loadDefaultTemplates()

	// Load custom templates from directory if configured (overrides defaults)
	if e.templateDir != "" {
		if err := e.LoadTemplatesFromDir(e.templateDir); err != nil {
			// Log but don't fail - defaults are already loaded
			fmt.Fprintf(os.Stderr, "[prompts] Warning: failed to load templates from %s: %v\n", e.templateDir, err)
		}
	}

	return e
}

// BuildPrompt generates system and user prompts for the given request.
func (e *Engine) BuildPrompt(req BuildRequest) (*BuildResponse, error) {
	e.mu.RLock()
	tmpl, ok := e.templates[req.EventType]
	e.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("prompts: unknown event type %q", req.EventType)
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

// SetTemplate allows overriding a template at runtime.
func (e *Engine) SetTemplate(eventType EventType, tmplContent string) error {
	tmpl, err := template.New(string(eventType)).Funcs(templateFuncs).Parse(tmplContent)
	if err != nil {
		return fmt.Errorf("prompts: invalid template: %w", err)
	}

	e.mu.Lock()
	e.templates[eventType] = tmpl
	e.mu.Unlock()

	return nil
}

// LoadTemplatesFromDir loads templates from the specified directory.
// Template files should be named: user_intent.tmpl, session_output.tmpl,
// history_summary.tmpl, clarification.tmpl. Only existing files are loaded;
// missing files keep the default templates.
func (e *Engine) LoadTemplatesFromDir(dir string) error {
	if dir == "" {
		return nil
	}

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Directory doesn't exist, use defaults
		}
		return fmt.Errorf("prompts: cannot access template dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("prompts: %s is not a directory", dir)
	}

	// Map event types to expected filenames
	eventTypeFiles := map[EventType]string{
		EventTypeUserIntent:     "user_intent.tmpl",
		EventTypeSessionOutput:  "session_output.tmpl",
		EventTypeHistorySummary: "history_summary.tmpl",
		EventTypeClarification:  "clarification.tmpl",
	}

	var loaded int
	for eventType, filename := range eventTypeFiles {
		path := filepath.Join(dir, filename)
		content, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // Skip missing files, keep default
			}
			return fmt.Errorf("prompts: failed to read %s: %w", path, err)
		}

		if err := e.SetTemplate(eventType, string(content)); err != nil {
			return fmt.Errorf("prompts: failed to parse %s: %w", path, err)
		}
		loaded++
	}

	if loaded > 0 {
		fmt.Fprintf(os.Stderr, "[prompts] Loaded %d custom templates from %s\n", loaded, dir)
	}

	return nil
}

// ReloadTemplates reloads templates from the configured directory.
// Returns an error if no directory was configured.
func (e *Engine) ReloadTemplates() error {
	e.mu.RLock()
	dir := e.templateDir
	e.mu.RUnlock()

	if dir == "" {
		return fmt.Errorf("prompts: no template directory configured")
	}

	// Reload defaults first, then overlay custom templates
	e.loadDefaultTemplates()
	return e.LoadTemplatesFromDir(dir)
}

func (e *Engine) loadDefaultTemplates() {
	for eventType, content := range defaultTemplates {
		tmpl, err := template.New(string(eventType)).Funcs(templateFuncs).Parse(content)
		if err != nil {
			// This should never happen with valid default templates
			panic(fmt.Sprintf("prompts: failed to parse default template %s: %v", eventType, err))
		}
		e.templates[eventType] = tmpl
	}
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
		origin := "user"
		if turn.Origin == eventbus.OriginTool {
			origin = "tool"
		} else if turn.Origin == eventbus.OriginAI {
			origin = "assistant"
		}
		historyLines = append(historyLines, fmt.Sprintf("[%s] %s", origin, turn.Text))
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
