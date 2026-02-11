package intentrouter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// testAdapter implements IntentAdapter for testing.
type testAdapter struct {
	name       string
	ready      bool
	response   *IntentResponse
	err        error
	lastReq    IntentRequest
	callCount  int
	mu         sync.Mutex
}

func (m *testAdapter) ResolveIntent(ctx context.Context, req IntentRequest) (*IntentResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastReq = req
	m.callCount++
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func (m *testAdapter) Name() string {
	return m.name
}

func (m *testAdapter) Ready() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready
}

func (m *testAdapter) SetReady(ready bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ready = ready
}

func (m *testAdapter) SetResponse(resp *IntentResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.response = resp
}

func (m *testAdapter) SetError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func (m *testAdapter) GetLastRequest() IntentRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastReq
}

func (m *testAdapter) GetCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callCount
}

// mockSessionProvider implements SessionProvider for testing.
type mockSessionProvider struct {
	sessions      []SessionInfo
	validateError error
}

func (m *mockSessionProvider) ListSessionInfos() []SessionInfo {
	return m.sessions
}

func (m *mockSessionProvider) GetSessionInfo(sessionID string) (SessionInfo, bool) {
	for _, s := range m.sessions {
		if s.ID == sessionID {
			return s, true
		}
	}
	return SessionInfo{}, false
}

func (m *mockSessionProvider) ValidateSession(sessionID string) error {
	if m.validateError != nil {
		return m.validateError
	}
	_, found := m.GetSessionInfo(sessionID)
	if !found {
		return errors.New("session not found")
	}
	return nil
}

// mockCommandExecutor implements CommandExecutor for testing.
type mockCommandExecutor struct {
	commands []queuedCommand
	err      error
	mu       sync.Mutex
}

type queuedCommand struct {
	SessionID string
	Command   string
	Origin    eventbus.ContentOrigin
}

func (m *mockCommandExecutor) QueueCommand(sessionID string, command string, origin eventbus.ContentOrigin) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.commands = append(m.commands, queuedCommand{
		SessionID: sessionID,
		Command:   command,
		Origin:    origin,
	})
	return nil
}

func (m *mockCommandExecutor) Stop() {}

func (m *mockCommandExecutor) GetCommands() []queuedCommand {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]queuedCommand{}, m.commands...)
}

func TestServiceStartShutdown(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := svc.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() failed: %v", err)
	}
}

func TestServiceWithoutBus(t *testing.T) {
	svc := NewService(nil)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() without bus should not fail: %v", err)
	}

	if err := svc.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() without bus should not fail: %v", err)
	}
}

func TestServiceNoAdapterPublishesError(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	svc := NewService(bus)

	// Subscribe to replies to verify error is published
	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	speakSub := bus.Subscribe(eventbus.TopicConversationSpeak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	// Publish a prompt without adapter configured
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID:  "test-session",
			PromptID:   "test-prompt",
			NewMessage: eventbus.ConversationMessage{Text: "hello"},
		},
	})

	// Should receive error reply
	select {
	case env := <-replySub.C():
		reply, ok := env.Payload.(eventbus.ConversationReplyEvent)
		if !ok {
			t.Fatal("Expected ConversationReplyEvent")
		}
		if reply.Metadata["error"] != "true" {
			t.Errorf("Expected error metadata")
		}
		if reply.Metadata["error_type"] != "no_adapter" {
			t.Errorf("Expected error_type=no_adapter, got %s", reply.Metadata["error_type"])
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for error reply")
	}

	// Should also receive speak event for TTS feedback
	select {
	case env := <-speakSub.C():
		speak, ok := env.Payload.(eventbus.ConversationSpeakEvent)
		if !ok {
			t.Fatal("Expected ConversationSpeakEvent")
		}
		if speak.Metadata["type"] != "error" {
			t.Errorf("Expected type=error metadata")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for error speak event")
	}

	metrics := svc.Metrics()
	if metrics.RequestsTotal != 1 {
		t.Errorf("Expected 1 request, got %d", metrics.RequestsTotal)
	}
	if metrics.RequestsFailed != 1 {
		t.Errorf("Expected 1 failed request, got %d", metrics.RequestsFailed)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestServiceAdapterNotReady(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := &testAdapter{name: "test-adapter", ready: false}
	svc := NewService(bus, WithAdapter(adapter))

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID:  "test-session",
			PromptID:   "test-prompt",
			NewMessage: eventbus.ConversationMessage{Text: "hello"},
		},
	})

	// Should receive error with recoverable=true
	select {
	case env := <-replySub.C():
		reply, ok := env.Payload.(eventbus.ConversationReplyEvent)
		if !ok {
			t.Fatal("Expected ConversationReplyEvent")
		}
		if reply.Metadata["recoverable"] != "true" {
			t.Errorf("Expected recoverable=true for adapter not ready")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for error reply")
	}

	if adapter.GetCallCount() != 0 {
		t.Errorf("Adapter should not be called when not ready, got %d calls", adapter.GetCallCount())
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestServiceCommandAction(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := &testAdapter{
		name:  "test-adapter",
		ready: true,
		response: &IntentResponse{
			PromptID:   "test-prompt",
			Confidence: 0.95,
			Actions: []IntentAction{
				{
					Type:       ActionCommand,
					SessionRef: "session-1",
					Command:    "go test ./...",
				},
			},
		},
	}

	sessionProvider := &mockSessionProvider{
		sessions: []SessionInfo{
			{ID: "session-1", Command: "bash", Status: "running"},
		},
	}

	commandExecutor := &mockCommandExecutor{}

	svc := NewService(bus,
		WithAdapter(adapter),
		WithSessionProvider(sessionProvider),
		WithCommandExecutor(commandExecutor),
	)

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "test-prompt",
			NewMessage: eventbus.ConversationMessage{
				Text: "run tests",
			},
		},
	})

	// Wait for reply with command confirmation
	select {
	case env := <-replySub.C():
		reply, ok := env.Payload.(eventbus.ConversationReplyEvent)
		if !ok {
			t.Fatal("Expected ConversationReplyEvent")
		}
		if reply.Metadata["status"] != "command_queued" {
			t.Errorf("Expected status=command_queued, got %s", reply.Metadata["status"])
		}
		if len(reply.Actions) != 1 || reply.Actions[0].Type != "command" {
			t.Errorf("Expected command action in reply")
		}
		if reply.Actions[0].Args["origin"] != string(eventbus.OriginAI) {
			t.Errorf("Expected origin=ai in action args")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for reply")
	}

	commands := commandExecutor.GetCommands()
	if len(commands) != 1 {
		t.Fatalf("Expected 1 command, got %d", len(commands))
	}

	if commands[0].SessionID != "session-1" {
		t.Errorf("Expected session-1, got %s", commands[0].SessionID)
	}
	if commands[0].Command != "go test ./..." {
		t.Errorf("Expected 'go test ./...', got %s", commands[0].Command)
	}
	if commands[0].Origin != eventbus.OriginAI {
		t.Errorf("Expected OriginAI, got %s", commands[0].Origin)
	}

	metrics := svc.Metrics()
	if metrics.CommandsQueued != 1 {
		t.Errorf("Expected 1 command queued, got %d", metrics.CommandsQueued)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestServiceSpeakAction(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := &testAdapter{
		name:  "test-adapter",
		ready: true,
		response: &IntentResponse{
			PromptID: "test-prompt",
			Actions: []IntentAction{
				{
					Type: ActionSpeak,
					Text: "Hello, how can I help you?",
				},
			},
		},
	}

	svc := NewService(bus, WithAdapter(adapter))

	speakSub := bus.Subscribe(eventbus.TopicConversationSpeak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "test-prompt",
			NewMessage: eventbus.ConversationMessage{
				Text: "hello",
			},
		},
	})

	// Wait for speak event
	select {
	case env := <-speakSub.C():
		speak, ok := env.Payload.(eventbus.ConversationSpeakEvent)
		if !ok {
			t.Fatal("Expected ConversationSpeakEvent")
		}
		if speak.Text != "Hello, how can I help you?" {
			t.Errorf("Unexpected speak text: %s", speak.Text)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for speak event")
	}

	// Wait for reply event
	select {
	case env := <-replySub.C():
		reply, ok := env.Payload.(eventbus.ConversationReplyEvent)
		if !ok {
			t.Fatal("Expected ConversationReplyEvent")
		}
		if reply.Metadata["status"] != "speak" {
			t.Errorf("Expected status=speak, got %s", reply.Metadata["status"])
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for reply event")
	}

	metrics := svc.Metrics()
	if metrics.SpeakEvents != 1 {
		t.Errorf("Expected 1 speak event, got %d", metrics.SpeakEvents)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestServiceClarifyAction(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := &testAdapter{
		name:  "test-adapter",
		ready: true,
		response: &IntentResponse{
			PromptID: "test-prompt",
			Actions: []IntentAction{
				{
					Type: ActionClarify,
					Text: "Which session do you want to use?",
				},
			},
		},
	}

	svc := NewService(bus, WithAdapter(adapter))

	speakSub := bus.Subscribe(eventbus.TopicConversationSpeak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "test-prompt",
			NewMessage: eventbus.ConversationMessage{
				Text: "run tests",
			},
		},
	})

	select {
	case env := <-speakSub.C():
		speak, ok := env.Payload.(eventbus.ConversationSpeakEvent)
		if !ok {
			t.Fatal("Expected ConversationSpeakEvent")
		}
		if speak.Metadata["type"] != "clarification" {
			t.Errorf("Expected clarification type metadata")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for clarification event")
	}

	metrics := svc.Metrics()
	if metrics.Clarifications != 1 {
		t.Errorf("Expected 1 clarification, got %d", metrics.Clarifications)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestServiceAdapterError(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := &testAdapter{
		name:  "test-adapter",
		ready: true,
		err:   errors.New("adapter error"),
	}

	svc := NewService(bus, WithAdapter(adapter))

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "test-prompt",
			NewMessage: eventbus.ConversationMessage{
				Text: "hello",
			},
		},
	})

	select {
	case env := <-replySub.C():
		reply, ok := env.Payload.(eventbus.ConversationReplyEvent)
		if !ok {
			t.Fatal("Expected ConversationReplyEvent")
		}
		if reply.Metadata["error"] != "true" {
			t.Errorf("Expected error metadata")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for error reply")
	}

	metrics := svc.Metrics()
	if metrics.RequestsFailed != 1 {
		t.Errorf("Expected 1 failed request, got %d", metrics.RequestsFailed)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestServiceNoCommandExecutorPublishesError(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := &testAdapter{
		name:  "test-adapter",
		ready: true,
		response: &IntentResponse{
			PromptID: "test-prompt",
			Actions: []IntentAction{
				{
					Type:       ActionCommand,
					SessionRef: "session-1",
					Command:    "test",
				},
			},
		},
	}

	sessionProvider := &mockSessionProvider{
		sessions: []SessionInfo{
			{ID: "session-1", Command: "bash", Status: "running"},
		},
	}

	// No command executor configured
	svc := NewService(bus,
		WithAdapter(adapter),
		WithSessionProvider(sessionProvider),
	)

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "test-prompt",
			NewMessage: eventbus.ConversationMessage{
				Text: "run test",
			},
		},
	})

	select {
	case env := <-replySub.C():
		reply, ok := env.Payload.(eventbus.ConversationReplyEvent)
		if !ok {
			t.Fatal("Expected ConversationReplyEvent")
		}
		if reply.Metadata["error"] != "true" {
			t.Errorf("Expected error metadata")
		}
		if reply.Metadata["error_type"] != "no_executor" {
			t.Errorf("Expected error_type=no_executor, got %s", reply.Metadata["error_type"])
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for error reply")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestServiceInvalidSession(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := &testAdapter{
		name:  "test-adapter",
		ready: true,
		response: &IntentResponse{
			PromptID: "test-prompt",
			Actions: []IntentAction{
				{
					Type:       ActionCommand,
					SessionRef: "nonexistent-session",
					Command:    "test",
				},
			},
		},
	}

	sessionProvider := &mockSessionProvider{
		sessions: []SessionInfo{
			{ID: "session-1", Command: "bash", Status: "running"},
		},
	}

	commandExecutor := &mockCommandExecutor{}

	svc := NewService(bus,
		WithAdapter(adapter),
		WithSessionProvider(sessionProvider),
		WithCommandExecutor(commandExecutor),
	)

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "test-prompt",
			NewMessage: eventbus.ConversationMessage{
				Text: "run test",
			},
		},
	})

	select {
	case env := <-replySub.C():
		reply, ok := env.Payload.(eventbus.ConversationReplyEvent)
		if !ok {
			t.Fatal("Expected ConversationReplyEvent")
		}
		if reply.Metadata["error"] != "true" {
			t.Errorf("Expected error metadata for invalid session")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for error reply")
	}

	commands := commandExecutor.GetCommands()
	if len(commands) != 0 {
		t.Errorf("Expected no commands for invalid session, got %d", len(commands))
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestServiceContextPassedToAdapter(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := &testAdapter{
		name:  "test-adapter",
		ready: true,
		response: &IntentResponse{
			PromptID: "test-prompt",
			Actions:  []IntentAction{{Type: ActionNoop}},
		},
	}

	sessionProvider := &mockSessionProvider{
		sessions: []SessionInfo{
			{ID: "session-1", Command: "bash", WorkDir: "/home/user", Status: "running"},
			{ID: "session-2", Command: "claude", WorkDir: "/project", Status: "running"},
		},
	}

	svc := NewService(bus,
		WithAdapter(adapter),
		WithSessionProvider(sessionProvider),
	)

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "test-prompt",
			Context: []eventbus.ConversationTurn{
				{Origin: eventbus.OriginUser, Text: "previous message"},
			},
			NewMessage: eventbus.ConversationMessage{
				Text: "new message",
			},
		},
	})

	// Wait for no_action reply
	select {
	case <-replySub.C():
		// OK
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for reply")
	}

	req := adapter.GetLastRequest()
	if req.PromptID != "test-prompt" {
		t.Errorf("Expected prompt ID test-prompt, got %s", req.PromptID)
	}
	if req.SessionID != "session-1" {
		t.Errorf("Expected session ID session-1, got %s", req.SessionID)
	}
	if req.Transcript != "new message" {
		t.Errorf("Expected transcript 'new message', got %s", req.Transcript)
	}
	if len(req.ConversationHistory) != 1 {
		t.Errorf("Expected 1 history entry, got %d", len(req.ConversationHistory))
	}
	if len(req.AvailableSessions) != 2 {
		t.Errorf("Expected 2 available sessions, got %d", len(req.AvailableSessions))
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestServiceSetAdapterRuntime(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	// Initially no adapter
	metrics := svc.Metrics()
	if metrics.AdapterName != "" {
		t.Errorf("Expected empty adapter name initially")
	}

	// Set adapter at runtime
	adapter := &testAdapter{name: "runtime-adapter", ready: true}
	svc.SetAdapter(adapter)

	metrics = svc.Metrics()
	if metrics.AdapterName != "runtime-adapter" {
		t.Errorf("Expected runtime-adapter, got %s", metrics.AdapterName)
	}
	if !metrics.AdapterReady {
		t.Errorf("Expected adapter to be ready")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input    string
		maxLen   int
		expected string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc..."},
	}

	for _, tt := range tests {
		result := truncate(tt.input, tt.maxLen)
		if result != tt.expected {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, result, tt.expected)
		}
	}
}

// Mock adapter tests

func TestMockAdapterEchoMode(t *testing.T) {
	adapter := EchoAdapter()

	req := IntentRequest{
		PromptID:   "test-prompt",
		Transcript: "hello world",
	}

	resp, err := adapter.ResolveIntent(context.Background(), req)
	if err != nil {
		t.Fatalf("ResolveIntent failed: %v", err)
	}

	if len(resp.Actions) != 1 {
		t.Fatalf("Expected 1 action, got %d", len(resp.Actions))
	}

	if resp.Actions[0].Type != ActionSpeak {
		t.Errorf("Expected ActionSpeak, got %s", resp.Actions[0].Type)
	}

	if resp.Actions[0].Text != "You said: hello world" {
		t.Errorf("Unexpected text: %s", resp.Actions[0].Text)
	}
}

func TestMockAdapterParseCommands(t *testing.T) {
	adapter := PassthroughAdapter()

	testCases := []struct {
		transcript string
		expectCmd  bool
		command    string
	}{
		{"run go test", true, "go test"},
		{"execute ls -la", true, "ls -la"},
		{"hello world", false, ""},
	}

	for _, tc := range testCases {
		req := IntentRequest{
			PromptID:   "test-prompt",
			SessionID:  "session-1",
			Transcript: tc.transcript,
		}

		resp, err := adapter.ResolveIntent(context.Background(), req)
		if err != nil {
			t.Fatalf("ResolveIntent failed for %q: %v", tc.transcript, err)
		}

		if len(resp.Actions) != 1 {
			t.Fatalf("Expected 1 action for %q, got %d", tc.transcript, len(resp.Actions))
		}

		if tc.expectCmd {
			if resp.Actions[0].Type != ActionCommand {
				t.Errorf("Expected ActionCommand for %q, got %s", tc.transcript, resp.Actions[0].Type)
			}
			if resp.Actions[0].Command != tc.command {
				t.Errorf("Expected command %q for %q, got %s", tc.command, tc.transcript, resp.Actions[0].Command)
			}
		} else {
			if resp.Actions[0].Type == ActionCommand {
				t.Errorf("Did not expect ActionCommand for %q", tc.transcript)
			}
		}
	}
}

func TestMockAdapterEmptyTranscript(t *testing.T) {
	adapter := NewMockAdapter()

	req := IntentRequest{
		PromptID:   "test-prompt",
		Transcript: "",
	}

	resp, err := adapter.ResolveIntent(context.Background(), req)
	if err != nil {
		t.Fatalf("ResolveIntent failed: %v", err)
	}

	if len(resp.Actions) != 1 || resp.Actions[0].Type != ActionNoop {
		t.Errorf("Expected noop for empty transcript")
	}
}

func TestMockAdapterCustomHandler(t *testing.T) {
	called := false
	adapter := NewMockAdapter(
		WithMockCustomHandler(func(ctx context.Context, req IntentRequest) (*IntentResponse, error) {
			called = true
			return &IntentResponse{
				PromptID: req.PromptID,
				Actions: []IntentAction{
					{Type: ActionSpeak, Text: "Custom response"},
				},
			}, nil
		}),
	)

	req := IntentRequest{
		PromptID:   "test-prompt",
		Transcript: "anything",
	}

	resp, err := adapter.ResolveIntent(context.Background(), req)
	if err != nil {
		t.Fatalf("ResolveIntent failed: %v", err)
	}

	if !called {
		t.Error("Custom handler was not called")
	}

	if resp.Actions[0].Text != "Custom response" {
		t.Errorf("Unexpected response: %s", resp.Actions[0].Text)
	}
}

// E2E Integration Tests

func TestE2ECommandFlowWithMockAdapter(t *testing.T) {
	// This test verifies the full flow from user prompt to command execution
	// using the real MockAdapter (PassthroughAdapter) instead of test stubs.
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := PassthroughAdapter()
	sessionProvider := &mockSessionProvider{
		sessions: []SessionInfo{
			{ID: "session-1", Command: "bash", Status: "running"},
		},
	}
	commandExecutor := &mockCommandExecutor{}

	svc := NewService(bus,
		WithAdapter(adapter),
		WithSessionProvider(sessionProvider),
		WithCommandExecutor(commandExecutor),
	)

	// Subscribe to replies
	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	// Simulate user saying "run go test ./..."
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "prompt-1",
			NewMessage: eventbus.ConversationMessage{
				Text: "run go test ./...",
			},
		},
	})

	// Wait for reply confirming command was queued
	select {
	case env := <-replySub.C():
		reply, ok := env.Payload.(eventbus.ConversationReplyEvent)
		if !ok {
			t.Fatal("Expected ConversationReplyEvent")
		}
		if reply.Metadata["status"] != "command_queued" {
			t.Errorf("Expected status=command_queued, got %s", reply.Metadata["status"])
		}
		if len(reply.Actions) != 1 || reply.Actions[0].Type != "command" {
			t.Errorf("Expected command action in reply")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for reply")
	}

	// Verify command was actually queued
	commands := commandExecutor.GetCommands()
	if len(commands) != 1 {
		t.Fatalf("Expected 1 command queued, got %d", len(commands))
	}
	if commands[0].Command != "go test ./..." {
		t.Errorf("Expected 'go test ./...', got %s", commands[0].Command)
	}
	if commands[0].SessionID != "session-1" {
		t.Errorf("Expected session-1, got %s", commands[0].SessionID)
	}
	if commands[0].Origin != eventbus.OriginAI {
		t.Errorf("Expected OriginAI, got %s", commands[0].Origin)
	}

	// Verify metrics
	metrics := svc.Metrics()
	if metrics.RequestsTotal != 1 {
		t.Errorf("Expected 1 request, got %d", metrics.RequestsTotal)
	}
	if metrics.RequestsFailed != 0 {
		t.Errorf("Expected 0 failed, got %d", metrics.RequestsFailed)
	}
	if metrics.CommandsQueued != 1 {
		t.Errorf("Expected 1 command queued, got %d", metrics.CommandsQueued)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestE2ESpeakFlowWithEchoAdapter(t *testing.T) {
	// This test verifies the TTS flow using the EchoAdapter
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := EchoAdapter()

	svc := NewService(bus,
		WithAdapter(adapter),
	)

	// Subscribe to speak events
	speakSub := bus.Subscribe(eventbus.TopicConversationSpeak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	// Simulate user input
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "prompt-1",
			NewMessage: eventbus.ConversationMessage{
				Text: "hello world",
			},
		},
	})

	// Wait for speak event
	select {
	case env := <-speakSub.C():
		speak, ok := env.Payload.(eventbus.ConversationSpeakEvent)
		if !ok {
			t.Fatal("Expected ConversationSpeakEvent")
		}
		if speak.Text != "You said: hello world" {
			t.Errorf("Unexpected speak text: %s", speak.Text)
		}
		if speak.SessionID != "session-1" {
			t.Errorf("Expected session-1, got %s", speak.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for speak event")
	}

	// Wait for reply
	select {
	case env := <-replySub.C():
		reply, ok := env.Payload.(eventbus.ConversationReplyEvent)
		if !ok {
			t.Fatal("Expected ConversationReplyEvent")
		}
		if reply.Metadata["status"] != "speak" {
			t.Errorf("Expected status=speak, got %s", reply.Metadata["status"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for reply")
	}

	// Verify metrics
	metrics := svc.Metrics()
	if metrics.SpeakEvents != 1 {
		t.Errorf("Expected 1 speak event, got %d", metrics.SpeakEvents)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestE2EClarifyFlowWithMockAdapter(t *testing.T) {
	// Test clarification flow when MockAdapter doesn't recognize the command
	bus := eventbus.New()
	defer bus.Shutdown()

	// Default MockAdapter (parseCommands=true, echoMode=false) will clarify unknown inputs
	adapter := NewMockAdapter()

	svc := NewService(bus,
		WithAdapter(adapter),
	)

	// Subscribe to speak events (clarifications go through speak)
	speakSub := bus.Subscribe(eventbus.TopicConversationSpeak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	// Simulate ambiguous user input (not a command)
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "prompt-1",
			NewMessage: eventbus.ConversationMessage{
				Text: "something unclear",
			},
		},
	})

	// Wait for speak event with clarification
	select {
	case env := <-speakSub.C():
		speak, ok := env.Payload.(eventbus.ConversationSpeakEvent)
		if !ok {
			t.Fatal("Expected ConversationSpeakEvent")
		}
		if speak.Metadata["type"] != "clarification" {
			t.Errorf("Expected clarification type, got %s", speak.Metadata["type"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for speak event")
	}

	// Wait for reply
	select {
	case env := <-replySub.C():
		reply, ok := env.Payload.(eventbus.ConversationReplyEvent)
		if !ok {
			t.Fatal("Expected ConversationReplyEvent")
		}
		if reply.Metadata["status"] != "clarification" {
			t.Errorf("Expected status=clarification, got %s", reply.Metadata["status"])
		}
		if len(reply.Actions) != 1 || reply.Actions[0].Type != "clarify" {
			t.Errorf("Expected clarify action in reply")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for reply")
	}

	// Verify metrics
	metrics := svc.Metrics()
	if metrics.Clarifications != 1 {
		t.Errorf("Expected 1 clarification, got %d", metrics.Clarifications)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestE2EMultipleSessionsCommandRouting(t *testing.T) {
	// Test that commands are routed to the correct session
	bus := eventbus.New()
	defer bus.Shutdown()

	// Create adapter that returns a command for specific session
	adapter := NewMockAdapter(
		WithMockCustomHandler(func(ctx context.Context, req IntentRequest) (*IntentResponse, error) {
			// Route command to session-2 based on user request
			if req.Transcript == "run ls on session two" {
				return &IntentResponse{
					PromptID: req.PromptID,
					Actions: []IntentAction{
						{
							Type:       ActionCommand,
							SessionRef: "session-2",
							Command:    "ls",
						},
					},
					Confidence: 0.9,
				}, nil
			}
			return &IntentResponse{
				PromptID: req.PromptID,
				Actions:  []IntentAction{{Type: ActionNoop}},
			}, nil
		}),
	)

	sessionProvider := &mockSessionProvider{
		sessions: []SessionInfo{
			{ID: "session-1", Command: "bash", Status: "running"},
			{ID: "session-2", Command: "zsh", Status: "running"},
		},
	}
	commandExecutor := &mockCommandExecutor{}

	svc := NewService(bus,
		WithAdapter(adapter),
		WithSessionProvider(sessionProvider),
		WithCommandExecutor(commandExecutor),
	)

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	// User is on session-1 but asks to run command on session-2
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "prompt-1",
			NewMessage: eventbus.ConversationMessage{
				Text: "run ls on session two",
			},
		},
	})

	// Wait for reply
	select {
	case env := <-replySub.C():
		reply, ok := env.Payload.(eventbus.ConversationReplyEvent)
		if !ok {
			t.Fatal("Expected ConversationReplyEvent")
		}
		if reply.Metadata["session_id"] != "session-2" {
			t.Errorf("Expected session-2, got %s", reply.Metadata["session_id"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for reply")
	}

	// Verify command was routed to session-2
	commands := commandExecutor.GetCommands()
	if len(commands) != 1 {
		t.Fatalf("Expected 1 command, got %d", len(commands))
	}
	if commands[0].SessionID != "session-2" {
		t.Errorf("Expected session-2, got %s", commands[0].SessionID)
	}
	if commands[0].Command != "ls" {
		t.Errorf("Expected 'ls', got %s", commands[0].Command)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestServiceToolChangeEventsUpdateCache(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	// Adapter that captures the CurrentTool from request
	var capturedTool string
	adapter := NewMockAdapter(
		WithMockCustomHandler(func(ctx context.Context, req IntentRequest) (*IntentResponse, error) {
			capturedTool = req.CurrentTool
			return &IntentResponse{
				PromptID: req.PromptID,
				Actions:  []IntentAction{{Type: ActionNoop}},
			}, nil
		}),
	)

	svc := NewService(bus, WithAdapter(adapter))

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	// Give subscriptions time to be active
	time.Sleep(50 * time.Millisecond)

	// Publish initial tool detection event
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsTool,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionToolEvent{
			SessionID: "session-1",
			ToolName:  "vim",
		},
	})

	// Wait for event to be processed
	time.Sleep(50 * time.Millisecond)

	// Publish a prompt - should include the cached tool
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "prompt-1",
			NewMessage: eventbus.ConversationMessage{
				Text: "hello",
			},
		},
	})

	select {
	case <-replySub.C():
		// OK
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for reply")
	}

	if capturedTool != "vim" {
		t.Errorf("Expected CurrentTool=vim, got %q", capturedTool)
	}

	// Now publish a tool change event
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsToolChanged,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionToolChangedEvent{
			SessionID:    "session-1",
			PreviousTool: "vim",
			NewTool:      "python",
		},
	})

	// Wait for event to be processed
	time.Sleep(50 * time.Millisecond)

	// Publish another prompt - should have updated tool
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "prompt-2",
			NewMessage: eventbus.ConversationMessage{
				Text: "hello again",
			},
		},
	})

	select {
	case <-replySub.C():
		// OK
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for second reply")
	}

	if capturedTool != "python" {
		t.Errorf("Expected CurrentTool=python after tool change, got %q", capturedTool)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestServiceToolCacheCleanupOnSessionLifecycle(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := NewMockAdapter(
		WithMockCustomHandler(func(ctx context.Context, req IntentRequest) (*IntentResponse, error) {
			return &IntentResponse{
				PromptID: req.PromptID,
				Actions:  []IntentAction{{Type: ActionNoop}},
			}, nil
		}),
	)

	svc := NewService(bus, WithAdapter(adapter))

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Set tool for session
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsTool,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionToolEvent{
			SessionID: "session-1",
			ToolName:  "vim",
		},
	})

	time.Sleep(50 * time.Millisecond)

	// Verify tool is cached
	if tool := svc.getToolFromCache("session-1"); tool != "vim" {
		t.Errorf("Expected cached tool=vim, got %q", tool)
	}

	// Session stops
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsLifecycle,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionLifecycleEvent{
			SessionID: "session-1",
			State:     eventbus.SessionStateStopped,
		},
	})

	time.Sleep(50 * time.Millisecond)

	// Cache should be cleared
	if tool := svc.getToolFromCache("session-1"); tool != "" {
		t.Errorf("Expected empty tool after session stopped, got %q", tool)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

// MockPromptEngine is a mock implementation of PromptEngine for testing.
type MockPromptEngine struct {
	lastRequest   PromptBuildRequest
	systemPrompt  string
	userPrompt    string
	shouldError   bool
	buildCount    int
	mu            sync.Mutex
}

func (m *MockPromptEngine) Build(req PromptBuildRequest) (*PromptBuildResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buildCount++
	m.lastRequest = req
	if m.shouldError {
		return nil, errors.New("mock prompt engine error")
	}
	return &PromptBuildResponse{
		SystemPrompt: m.systemPrompt,
		UserPrompt:   m.userPrompt,
	}, nil
}

func (m *MockPromptEngine) GetLastRequest() PromptBuildRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastRequest
}

func (m *MockPromptEngine) GetBuildCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buildCount
}

func TestServiceEventTypeMetadataPropagation(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	var capturedEventType EventType
	adapter := NewMockAdapter(
		WithMockCustomHandler(func(ctx context.Context, req IntentRequest) (*IntentResponse, error) {
			capturedEventType = req.EventType
			return &IntentResponse{
				PromptID: req.PromptID,
				Actions:  []IntentAction{{Type: ActionNoop}},
			}, nil
		}),
	)

	svc := NewService(bus, WithAdapter(adapter))

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Test user_intent (default)
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "prompt-1",
			NewMessage: eventbus.ConversationMessage{
				Text: "hello",
			},
			Metadata: map[string]string{
				"event_type": "user_intent",
			},
		},
	})

	select {
	case <-replySub.C():
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for reply")
	}

	if capturedEventType != EventTypeUserIntent {
		t.Errorf("Expected EventType=user_intent, got %q", capturedEventType)
	}

	// Test session_output
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "prompt-2",
			NewMessage: eventbus.ConversationMessage{
				Text: "output text",
			},
			Metadata: map[string]string{
				"event_type":     "session_output",
				"session_output": "Error: something failed",
			},
		},
	})

	select {
	case <-replySub.C():
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for reply")
	}

	if capturedEventType != EventTypeSessionOutput {
		t.Errorf("Expected EventType=session_output, got %q", capturedEventType)
	}

	// Test clarification
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "prompt-3",
			NewMessage: eventbus.ConversationMessage{
				Text: "yes, run it",
			},
			Metadata: map[string]string{
				"event_type":             "clarification",
				"clarification_question": "Do you want me to run the tests?",
			},
		},
	})

	select {
	case <-replySub.C():
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for reply")
	}

	if capturedEventType != EventTypeClarification {
		t.Errorf("Expected EventType=clarification, got %q", capturedEventType)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestServicePromptEnginePopulatesSystemAndUserPrompts(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	var capturedSystemPrompt, capturedUserPrompt string
	adapter := NewMockAdapter(
		WithMockCustomHandler(func(ctx context.Context, req IntentRequest) (*IntentResponse, error) {
			capturedSystemPrompt = req.SystemPrompt
			capturedUserPrompt = req.UserPrompt
			return &IntentResponse{
				PromptID: req.PromptID,
				Actions:  []IntentAction{{Type: ActionNoop}},
			}, nil
		}),
	)

	mockEngine := &MockPromptEngine{
		systemPrompt: "You are a helpful assistant",
		userPrompt:   "User says: hello",
	}

	svc := NewService(bus, WithAdapter(adapter), WithPromptEngine(mockEngine))

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "prompt-1",
			NewMessage: eventbus.ConversationMessage{
				Text: "hello",
			},
			Metadata: map[string]string{
				"event_type": "user_intent",
			},
		},
	})

	select {
	case <-replySub.C():
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for reply")
	}

	if capturedSystemPrompt != "You are a helpful assistant" {
		t.Errorf("Expected SystemPrompt='You are a helpful assistant', got %q", capturedSystemPrompt)
	}
	if capturedUserPrompt != "User says: hello" {
		t.Errorf("Expected UserPrompt='User says: hello', got %q", capturedUserPrompt)
	}

	// Verify prompt engine was called with correct event type
	lastReq := mockEngine.GetLastRequest()
	if lastReq.EventType != EventTypeUserIntent {
		t.Errorf("Expected PromptEngine called with EventType=user_intent, got %q", lastReq.EventType)
	}
	if lastReq.Transcript != "hello" {
		t.Errorf("Expected PromptEngine called with Transcript='hello', got %q", lastReq.Transcript)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}

func TestServiceExtendedMetadataPropagation(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	var capturedMetadata map[string]string
	adapter := NewMockAdapter(
		WithMockCustomHandler(func(ctx context.Context, req IntentRequest) (*IntentResponse, error) {
			capturedMetadata = req.Metadata
			return &IntentResponse{
				PromptID: req.PromptID,
				Actions:  []IntentAction{{Type: ActionNoop}},
			}, nil
		}),
	)

	svc := NewService(bus, WithAdapter(adapter))

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Publish prompt with extended metadata
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: "session-1",
			PromptID:  "prompt-1",
			NewMessage: eventbus.ConversationMessage{
				Text: "hello",
			},
			Metadata: map[string]string{
				"event_type":   "user_intent",
				"input_source": "voice",
				"sessionless":  "true",
				"confidence":   "0.95",
			},
		},
	})

	select {
	case <-replySub.C():
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for reply")
	}

	// Verify extended metadata was propagated
	if capturedMetadata == nil {
		t.Fatal("Expected metadata to be captured")
	}
	if capturedMetadata["event_type"] != "user_intent" {
		t.Errorf("Expected event_type=user_intent, got %q", capturedMetadata["event_type"])
	}
	if capturedMetadata["input_source"] != "voice" {
		t.Errorf("Expected input_source=voice, got %q", capturedMetadata["input_source"])
	}
	if capturedMetadata["sessionless"] != "true" {
		t.Errorf("Expected sessionless=true, got %q", capturedMetadata["sessionless"])
	}
	if capturedMetadata["confidence"] != "0.95" {
		t.Errorf("Expected confidence=0.95, got %q", capturedMetadata["confidence"])
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
}
