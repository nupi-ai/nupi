package intentrouter

import (
	"context"
	"strings"
	"sync"
)

// MockAdapter is a simple AI adapter for testing and development.
// It provides basic command parsing and pass-through functionality.
type MockAdapter struct {
	mu           sync.RWMutex
	name         string
	ready        bool
	echoMode     bool                                              // If true, echoes back the transcript as speak
	parseCommands bool                                             // If true, attempts to parse simple commands
	customHandler func(context.Context, IntentRequest) (*IntentResponse, error)
}

// MockAdapterOption configures a MockAdapter.
type MockAdapterOption func(*MockAdapter)

// WithMockName sets the adapter name.
func WithMockName(name string) MockAdapterOption {
	return func(m *MockAdapter) {
		m.name = name
	}
}

// WithMockEchoMode enables echo mode (speaks back the transcript).
func WithMockEchoMode(enabled bool) MockAdapterOption {
	return func(m *MockAdapter) {
		m.echoMode = enabled
	}
}

// WithMockParseCommands enables simple command parsing.
func WithMockParseCommands(enabled bool) MockAdapterOption {
	return func(m *MockAdapter) {
		m.parseCommands = enabled
	}
}

// WithMockCustomHandler sets a custom handler for intent resolution.
func WithMockCustomHandler(handler func(context.Context, IntentRequest) (*IntentResponse, error)) MockAdapterOption {
	return func(m *MockAdapter) {
		m.customHandler = handler
	}
}

// NewMockAdapter creates a mock AI adapter for testing.
func NewMockAdapter(opts ...MockAdapterOption) *MockAdapter {
	m := &MockAdapter{
		name:          "mock-adapter",
		ready:         true,
		echoMode:      false,
		parseCommands: true,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name returns the adapter's identifier.
func (m *MockAdapter) Name() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.name
}

// Ready returns true if the adapter is ready to process requests.
func (m *MockAdapter) Ready() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ready
}

// SetReady sets the ready state.
func (m *MockAdapter) SetReady(ready bool) {
	m.mu.Lock()
	m.ready = ready
	m.mu.Unlock()
}

// ResolveIntent processes the user's input and returns the intended action(s).
func (m *MockAdapter) ResolveIntent(ctx context.Context, req IntentRequest) (*IntentResponse, error) {
	m.mu.RLock()
	customHandler := m.customHandler
	echoMode := m.echoMode
	parseCommands := m.parseCommands
	m.mu.RUnlock()

	// Use custom handler if provided
	if customHandler != nil {
		return customHandler(ctx, req)
	}

	transcript := strings.TrimSpace(req.Transcript)
	if transcript == "" {
		return &IntentResponse{
			PromptID:   req.PromptID,
			Actions:    []IntentAction{{Type: ActionNoop}},
			Reasoning:  "Empty transcript",
			Confidence: 1.0,
		}, nil
	}

	// Parse commands if enabled
	if parseCommands {
		if action := m.parseCommand(req); action != nil {
			return &IntentResponse{
				PromptID:   req.PromptID,
				Actions:    []IntentAction{*action},
				Reasoning:  "Parsed command from transcript",
				Confidence: 0.8,
			}, nil
		}
	}

	// Echo mode - speak back the transcript
	if echoMode {
		return &IntentResponse{
			PromptID: req.PromptID,
			Actions: []IntentAction{
				{
					Type: ActionSpeak,
					Text: "You said: " + transcript,
				},
			},
			Reasoning:  "Echo mode",
			Confidence: 1.0,
		}, nil
	}

	// Default: clarify what the user wants
	return &IntentResponse{
		PromptID: req.PromptID,
		Actions: []IntentAction{
			{
				Type: ActionClarify,
				Text: "I understood: \"" + transcript + "\". What would you like me to do with this?",
			},
		},
		Reasoning:  "No clear action detected, requesting clarification",
		Confidence: 0.3,
	}, nil
}

// parseCommand attempts to parse simple command patterns from the transcript.
func (m *MockAdapter) parseCommand(req IntentRequest) *IntentAction {
	transcript := strings.ToLower(strings.TrimSpace(req.Transcript))

	// Simple command patterns
	patterns := []struct {
		prefix  string
		extract func(string) string
	}{
		{"run ", func(s string) string { return strings.TrimPrefix(s, "run ") }},
		{"execute ", func(s string) string { return strings.TrimPrefix(s, "execute ") }},
	}

	for _, p := range patterns {
		if strings.HasPrefix(transcript, p.prefix) {
			command := p.extract(transcript)
			if command != "" {
				sessionRef := ""
				if len(req.AvailableSessions) == 1 {
					sessionRef = req.AvailableSessions[0].ID
				} else if req.SessionID != "" {
					sessionRef = req.SessionID
				}

				return &IntentAction{
					Type:       ActionCommand,
					SessionRef: sessionRef,
					Command:    command,
				}
			}
		}
	}

	return nil
}

// EchoAdapter creates a simple echo adapter that speaks back the transcript.
func EchoAdapter() *MockAdapter {
	return NewMockAdapter(
		WithMockName("echo-adapter"),
		WithMockEchoMode(true),
		WithMockParseCommands(false),
	)
}

// PassthroughAdapter creates an adapter that attempts to parse and execute commands.
func PassthroughAdapter() *MockAdapter {
	return NewMockAdapter(
		WithMockName("passthrough-adapter"),
		WithMockEchoMode(false),
		WithMockParseCommands(true),
	)
}
