package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/conversation"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/intentrouter"
)

// sessionAwareMockAdapter is a mock AI adapter that understands session management
// voice commands: "start/create session", "list sessions", "kill/stop session X",
// "restart session X". It returns appropriate IntentActions based on the transcript.
type sessionAwareMockAdapter struct {
	mu       sync.RWMutex
	name     string
	ready    bool
	requests []intentrouter.IntentRequest // captured requests for assertions
}

func newSessionAwareMockAdapter() *sessionAwareMockAdapter {
	return &sessionAwareMockAdapter{
		name:  "session-aware-mock",
		ready: true,
	}
}

func (a *sessionAwareMockAdapter) Name() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.name
}

func (a *sessionAwareMockAdapter) Ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ready
}

func (a *sessionAwareMockAdapter) SetReady(ready bool) {
	a.mu.Lock()
	a.ready = ready
	a.mu.Unlock()
}

func (a *sessionAwareMockAdapter) GetRequests() []intentrouter.IntentRequest {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]intentrouter.IntentRequest{}, a.requests...)
}

func (a *sessionAwareMockAdapter) LastRequest() intentrouter.IntentRequest {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.requests) == 0 {
		return intentrouter.IntentRequest{}
	}
	return a.requests[len(a.requests)-1]
}

func (a *sessionAwareMockAdapter) ResolveIntent(_ context.Context, req intentrouter.IntentRequest) (*intentrouter.IntentResponse, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()

	transcript := strings.TrimSpace(strings.ToLower(req.Transcript))

	// Session restart: "restart session X" — must check BEFORE "start session".
	// Returns ActionSpeak only (intent recognition). Actual restart requires
	// ActionCreateSession which will be added in Story 3-2.
	if strings.Contains(transcript, "restart") && strings.Contains(transcript, "session") {
		sessionRef := extractSessionRef(transcript, req.AvailableSessions)
		// Find the session's original command
		var command string
		for _, s := range req.AvailableSessions {
			if s.ID == sessionRef {
				command = s.Command
				break
			}
		}
		return &intentrouter.IntentResponse{
			PromptID:   req.PromptID,
			Confidence: 0.9,
			Actions: []intentrouter.IntentAction{
				{
					Type: intentrouter.ActionSpeak,
					Text: fmt.Sprintf("Restarting session %s with command %s.", sessionRef, command),
				},
			},
			Reasoning: "User requested session restart",
		}, nil
	}

	// Session creation: "start a new claude session" / "create session".
	// Returns ActionSpeak only (intent recognition). Actual session creation
	// requires ActionCreateSession which will be added in Story 3-2.
	if strings.Contains(transcript, "start") && strings.Contains(transcript, "session") ||
		strings.Contains(transcript, "create") && strings.Contains(transcript, "session") {
		return &intentrouter.IntentResponse{
			PromptID:   req.PromptID,
			Confidence: 0.95,
			Actions: []intentrouter.IntentAction{
				{
					Type: intentrouter.ActionSpeak,
					Text: "Creating a new session for you.",
				},
			},
			Reasoning: "User requested session creation",
		}, nil
	}

	// Session listing: "list sessions" / "what's running"
	if strings.Contains(transcript, "list") && strings.Contains(transcript, "session") ||
		strings.Contains(transcript, "what") && strings.Contains(transcript, "running") {
		// Build summary from available sessions
		summary := fmt.Sprintf("You have %d sessions", len(req.AvailableSessions))
		if len(req.AvailableSessions) > 0 {
			parts := make([]string, 0, len(req.AvailableSessions))
			for _, s := range req.AvailableSessions {
				parts = append(parts, fmt.Sprintf("%s running %s (%s)", s.ID, s.Command, s.Status))
			}
			summary += ": " + strings.Join(parts, ", ")
		}

		return &intentrouter.IntentResponse{
			PromptID:   req.PromptID,
			Confidence: 0.9,
			Actions: []intentrouter.IntentAction{
				{
					Type: intentrouter.ActionSpeak,
					Text: summary,
				},
			},
			Reasoning: "User requested session listing",
		}, nil
	}

	// Session kill: "kill session X" / "stop session X"
	if (strings.Contains(transcript, "kill") || strings.Contains(transcript, "stop")) &&
		strings.Contains(transcript, "session") {
		// Extract session reference — look for a session ID in the transcript
		sessionRef := extractSessionRef(transcript, req.AvailableSessions)
		return &intentrouter.IntentResponse{
			PromptID:   req.PromptID,
			Confidence: 0.9,
			Actions: []intentrouter.IntentAction{
				{
					Type:       intentrouter.ActionCommand,
					SessionRef: sessionRef,
					Command:    "exit",
				},
				{
					Type: intentrouter.ActionSpeak,
					Text: fmt.Sprintf("Terminating session %s.", sessionRef),
				},
			},
			Reasoning: "User requested session termination",
		}, nil
	}

	// Run command: "run X" → execute in target session
	if strings.HasPrefix(transcript, "run ") {
		command := strings.TrimPrefix(transcript, "run ")
		sessionRef := ""
		if req.SessionID != "" {
			sessionRef = req.SessionID
		} else if len(req.AvailableSessions) == 1 {
			sessionRef = req.AvailableSessions[0].ID
		}
		return &intentrouter.IntentResponse{
			PromptID:   req.PromptID,
			Confidence: 0.85,
			Actions: []intentrouter.IntentAction{
				{
					Type:       intentrouter.ActionCommand,
					SessionRef: sessionRef,
					Command:    command,
				},
			},
			Reasoning: "User requested command execution",
		}, nil
	}

	// Default: echo back as speak
	return &intentrouter.IntentResponse{
		PromptID:   req.PromptID,
		Confidence: 0.5,
		Actions: []intentrouter.IntentAction{
			{
				Type: intentrouter.ActionSpeak,
				Text: "I heard: " + req.Transcript,
			},
		},
	}, nil
}

// extractSessionRef finds a session ID from the transcript by matching against
// available sessions. Matches longest ID first to avoid partial collisions
// (e.g., "sess-a" matching before "sess-aa"). For equal-length IDs, the first
// match in iteration order wins (strict > means equal lengths don't replace).
// Test IDs are always distinct lengths or non-overlapping to avoid ambiguity.
func extractSessionRef(transcript string, sessions []intentrouter.SessionInfo) string {
	var best string
	for _, s := range sessions {
		if strings.Contains(transcript, strings.ToLower(s.ID)) && len(s.ID) > len(best) {
			best = s.ID
		}
	}
	return best
}

// mutableSessionProvider is a thread-safe SessionProvider that supports adding/removing sessions at runtime.
type mutableSessionProvider struct {
	mu               sync.RWMutex
	sessions         []intentrouter.SessionInfo
	rejectedSessions map[string]bool // sessions that ValidateSession should reject
}

func newMutableSessionProvider(sessions ...intentrouter.SessionInfo) *mutableSessionProvider {
	return &mutableSessionProvider{
		sessions: append([]intentrouter.SessionInfo{}, sessions...),
	}
}

func (p *mutableSessionProvider) ListSessionInfos() []intentrouter.SessionInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]intentrouter.SessionInfo{}, p.sessions...)
}

func (p *mutableSessionProvider) GetSessionInfo(sessionID string) (intentrouter.SessionInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, s := range p.sessions {
		if s.ID == sessionID {
			return s, true
		}
	}
	return intentrouter.SessionInfo{}, false
}

func (p *mutableSessionProvider) ValidateSession(sessionID string) error {
	p.mu.RLock()
	rejected := p.rejectedSessions[sessionID]
	p.mu.RUnlock()
	if rejected {
		return fmt.Errorf("session %s: validation rejected", sessionID)
	}
	_, found := p.GetSessionInfo(sessionID)
	if !found {
		return fmt.Errorf("session %s not found", sessionID)
	}
	return nil
}

// RejectValidation makes ValidateSession reject the given session even if it
// exists in the session list. This simulates scenarios where a session is visible
// (e.g., in AvailableSessions) but not commandable.
func (p *mutableSessionProvider) RejectValidation(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rejectedSessions == nil {
		p.rejectedSessions = make(map[string]bool)
	}
	p.rejectedSessions[sessionID] = true
}

func (p *mutableSessionProvider) UpdateStatus(sessionID, status string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.sessions {
		if p.sessions[i].ID == sessionID {
			p.sessions[i].Status = status
			return
		}
	}
}

// recordingCommandExecutor records all commands for later assertion.
type recordingCommandExecutor struct {
	mu       sync.RWMutex
	commands []executedCommand
}

type executedCommand struct {
	SessionID string
	Command   string
	Origin    eventbus.ContentOrigin
}

func newRecordingCommandExecutor() *recordingCommandExecutor {
	return &recordingCommandExecutor{}
}

func (e *recordingCommandExecutor) QueueCommand(sessionID, command string, origin eventbus.ContentOrigin) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commands = append(e.commands, executedCommand{
		SessionID: sessionID,
		Command:   command,
		Origin:    origin,
	})
	return nil
}

func (e *recordingCommandExecutor) Stop() {}

func (e *recordingCommandExecutor) GetCommands() []executedCommand {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]executedCommand{}, e.commands...)
}

// waitForReply waits for a ConversationReplyEvent on the subscription.
func waitForReply(t *testing.T, sub *eventbus.Subscription, timeout time.Duration) eventbus.ConversationReplyEvent {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case env, ok := <-sub.C():
			if !ok {
				t.Fatal("reply subscription closed")
			}
			reply, ok := env.Payload.(eventbus.ConversationReplyEvent)
			if !ok {
				continue
			}
			return reply
		case <-timer.C:
			t.Fatalf("timeout waiting for reply (%s)", timeout)
			panic("unreachable")
		}
	}
}

// waitForSpeak waits for a ConversationSpeakEvent on the subscription.
func waitForSpeak(t *testing.T, sub *eventbus.Subscription, timeout time.Duration) eventbus.ConversationSpeakEvent {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case env, ok := <-sub.C():
			if !ok {
				t.Fatal("speak subscription closed")
			}
			speak, ok := env.Payload.(eventbus.ConversationSpeakEvent)
			if !ok {
				continue
			}
			return speak
		case <-timer.C:
			t.Fatalf("timeout waiting for speak event (%s)", timeout)
			panic("unreachable")
		}
	}
}

// publishPrompt publishes a ConversationPromptEvent simulating a voice transcript.
func publishPrompt(ctx context.Context, bus *eventbus.Bus, sessionID, promptID, transcript string) {
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			SessionID: sessionID,
			PromptID:  promptID,
			NewMessage: eventbus.ConversationMessage{
				Text:   transcript,
				Origin: eventbus.OriginUser,
			},
			Metadata: map[string]string{
				"event_type":   "user_intent",
				"input_source": "voice",
			},
		},
	})
}

// publishSessionlessPrompt publishes a prompt with no session context (e.g., "list sessions").
func publishSessionlessPrompt(ctx context.Context, bus *eventbus.Bus, promptID, transcript string) {
	bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationPrompt,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationPromptEvent{
			PromptID: promptID,
			NewMessage: eventbus.ConversationMessage{
				Text:   transcript,
				Origin: eventbus.OriginUser,
			},
			Metadata: map[string]string{
				"event_type":   "user_intent",
				"input_source": "voice",
				"sessionless":  "true",
			},
		},
	})
}

// setupIntentRouterTest creates a standard test setup with intent router, mocks, and subscriptions.
type intentRouterTestSetup struct {
	Bus             *eventbus.Bus
	Router          *intentrouter.Service
	Adapter         *sessionAwareMockAdapter
	SessionProvider *mutableSessionProvider
	CommandExecutor *recordingCommandExecutor
	ReplySub        *eventbus.Subscription
	SpeakSub        *eventbus.Subscription
	Ctx             context.Context
	Cancel          context.CancelFunc
}

func setupIntentRouterTest(t *testing.T, sessions ...intentrouter.SessionInfo) *intentRouterTestSetup {
	t.Helper()

	bus := eventbus.New()
	adapter := newSessionAwareMockAdapter()
	provider := newMutableSessionProvider(sessions...)
	executor := newRecordingCommandExecutor()

	router := intentrouter.NewService(bus,
		intentrouter.WithAdapter(adapter),
		intentrouter.WithSessionProvider(provider),
		intentrouter.WithCommandExecutor(executor),
	)

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	speakSub := bus.Subscribe(eventbus.TopicConversationSpeak, eventbus.WithSubscriptionName("test_speak"))

	ctx, cancel := context.WithCancel(context.Background())

	if err := router.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start intent router: %v", err)
	}

	// No sleep needed: bus.Subscribe creates buffered channels synchronously
	// before Start(). Events published after Start() are buffered in the
	// subscription channel and consumed once the goroutine reaches its select loop.

	t.Cleanup(func() {
		cancel()
		replySub.Close()
		speakSub.Close()
		router.Shutdown(context.Background())
		bus.Shutdown()
	})

	return &intentRouterTestSetup{
		Bus:             bus,
		Router:          router,
		Adapter:         adapter,
		SessionProvider: provider,
		CommandExecutor: executor,
		ReplySub:        replySub,
		SpeakSub:        speakSub,
		Ctx:             ctx,
		Cancel:          cancel,
	}
}

// --- Task 1: Verify intent router handles session creation commands (AC: #1) ---

func TestVoiceSessionCreation(t *testing.T) {
	// NOTE: Story 3-1 validates intent recognition and routing only.
	// The mock returns ActionSpeak (confirmation) because actual session creation
	// requires a new ActionCreateSession action type that doesn't exist yet.
	// Story 3-2 will add the real create/restart side-effects.

	// Subtask 1.1: Wire intent router with session-aware mock adapter,
	// verify session creation voice command produces ActionSpeak confirmation.
	setup := setupIntentRouterTest(t, intentrouter.SessionInfo{
		ID:      "sess-001",
		Command: "claude",
		Status:  "running",
	})

	// User says "start a new Claude session"
	publishPrompt(setup.Ctx, setup.Bus, "sess-001", "prompt-create-1", "start a new Claude session")

	// Verify AI speaks confirmation
	speak := waitForSpeak(t, setup.SpeakSub, 2*time.Second)
	if !strings.Contains(speak.Text, "Creating") {
		t.Errorf("expected speak text to contain 'Creating', got %q", speak.Text)
	}

	// Verify reply is published
	reply := waitForReply(t, setup.ReplySub, 2*time.Second)
	if reply.Metadata["status"] != "speak" {
		t.Errorf("expected reply status=speak, got %q", reply.Metadata["status"])
	}

	// Subtask 1.2: Verify IntentRequest sent to AI adapter includes transcript,
	// AvailableSessions, EventType, and session context.
	req := setup.Adapter.LastRequest()
	if req.Transcript != "start a new Claude session" {
		t.Errorf("expected transcript 'start a new Claude session', got %q", req.Transcript)
	}
	if len(req.AvailableSessions) != 1 {
		t.Errorf("expected 1 available session, got %d", len(req.AvailableSessions))
	} else {
		if req.AvailableSessions[0].ID != "sess-001" {
			t.Errorf("expected session ID 'sess-001', got %q", req.AvailableSessions[0].ID)
		}
		if req.AvailableSessions[0].Command != "claude" {
			t.Errorf("expected session command 'claude', got %q", req.AvailableSessions[0].Command)
		}
	}
	if req.EventType != intentrouter.EventTypeUserIntent {
		t.Errorf("expected event_type=user_intent, got %q", req.EventType)
	}

	// Subtask 1.3: Verify ActionSpeak response produces ConversationSpeakEvent on bus
	// (already verified above via waitForSpeak)

	// Verify metrics
	metrics := setup.Router.Metrics()
	if metrics.RequestsTotal != 1 {
		t.Errorf("expected 1 request, got %d", metrics.RequestsTotal)
	}
	if metrics.SpeakEvents != 1 {
		t.Errorf("expected 1 speak event, got %d", metrics.SpeakEvents)
	}
}

// --- Task 2: Verify intent router handles session listing queries (AC: #2) ---

func TestVoiceSessionListing(t *testing.T) {
	// Subtask 2.1: Create 3 sessions with distinct commands, inject "list sessions" transcript.
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-aaa", Command: "claude", Args: []string{"--model", "opus"}, Status: "running", WorkDir: "/project1"},
		{ID: "sess-bbb", Command: "codex", Status: "running", WorkDir: "/project2"},
		{ID: "sess-ccc", Command: "bash", Status: "detached", WorkDir: "/home/user"},
	}
	setup := setupIntentRouterTest(t, sessions...)

	// User says "list sessions" without targeting a specific session
	publishSessionlessPrompt(setup.Ctx, setup.Bus, "prompt-list-1", "list sessions")

	// Subtask 2.2: Verify AI responds with ActionSpeak containing session summary
	speak := waitForSpeak(t, setup.SpeakSub, 2*time.Second)
	if !strings.Contains(speak.Text, "3 sessions") {
		t.Errorf("expected speak text to mention '3 sessions', got %q", speak.Text)
	}

	// Subtask 2.3: Verify SessionInfo includes ID, Command, Status for each session
	req := setup.Adapter.LastRequest()
	if len(req.AvailableSessions) != 3 {
		t.Fatalf("expected 3 available sessions, got %d", len(req.AvailableSessions))
	}

	// Verify each session has correct info
	sessionMap := make(map[string]intentrouter.SessionInfo)
	for _, s := range req.AvailableSessions {
		sessionMap[s.ID] = s
	}

	if s, ok := sessionMap["sess-aaa"]; !ok {
		t.Error("missing session sess-aaa")
	} else {
		if s.Command != "claude" {
			t.Errorf("sess-aaa: expected command=claude, got %q", s.Command)
		}
		if s.Status != "running" {
			t.Errorf("sess-aaa: expected status=running, got %q", s.Status)
		}
		if s.WorkDir != "/project1" {
			t.Errorf("sess-aaa: expected workdir=/project1, got %q", s.WorkDir)
		}
	}

	if s, ok := sessionMap["sess-ccc"]; !ok {
		t.Error("missing session sess-ccc")
	} else {
		if s.Status != "detached" {
			t.Errorf("sess-ccc: expected status=detached, got %q", s.Status)
		}
	}
}

func TestVoiceSessionListingWhatsRunning(t *testing.T) {
	// Alternative phrasing: "what's running"
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-111", Command: "claude", Status: "running"},
		{ID: "sess-222", Command: "bash", Status: "running"},
	}
	setup := setupIntentRouterTest(t, sessions...)

	publishSessionlessPrompt(setup.Ctx, setup.Bus, "prompt-list-2", "what's running")

	speak := waitForSpeak(t, setup.SpeakSub, 2*time.Second)
	if !strings.Contains(speak.Text, "2 sessions") {
		t.Errorf("expected speak text to mention '2 sessions', got %q", speak.Text)
	}
}

// --- Task 3: Verify session termination via voice (AC: #5) ---

func TestVoiceSessionKill(t *testing.T) {
	// Subtask 3.1: Create 2 sessions, inject "kill session sess-bbb" transcript.
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-aaa", Command: "claude", Status: "running"},
		{ID: "sess-bbb", Command: "codex", Status: "running"},
	}
	setup := setupIntentRouterTest(t, sessions...)

	// User says "kill session sess-bbb"
	publishPrompt(setup.Ctx, setup.Bus, "", "prompt-kill-1", "kill session sess-bbb")

	// Verify speak confirmation
	speak := waitForSpeak(t, setup.SpeakSub, 2*time.Second)
	if !strings.Contains(speak.Text, "sess-bbb") {
		t.Errorf("expected speak text to reference 'sess-bbb', got %q", speak.Text)
	}

	// Verify command was sent to correct session (ActionCommand with "exit").
	// executeActions processes actions sequentially, so command_queued is
	// always the first reply, followed by the speak reply.
	reply := waitForReply(t, setup.ReplySub, 2*time.Second)
	if reply.Metadata["status"] != "command_queued" {
		t.Errorf("expected reply with status=command_queued, got %q", reply.Metadata["status"])
	}
	// Drain the second reply (speak) so it doesn't leak into other assertions.
	waitForReply(t, setup.ReplySub, 2*time.Second)

	commands := setup.CommandExecutor.GetCommands()
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}
	if commands[0].SessionID != "sess-bbb" {
		t.Errorf("expected command targeting sess-bbb, got %q", commands[0].SessionID)
	}

	// Subtask 3.2: Verify remaining session is unaffected
	// (sess-aaa should still be in the provider, no commands sent to it)
	for _, cmd := range commands {
		if cmd.SessionID == "sess-aaa" {
			t.Error("command should not have been sent to sess-aaa")
		}
	}

	// Subtask 3.3: Verify adapter received correct session info
	req := setup.Adapter.LastRequest()
	if len(req.AvailableSessions) != 2 {
		t.Errorf("expected 2 available sessions in request, got %d", len(req.AvailableSessions))
	}
}

// --- Task 4: Verify session persistence across client disconnect (AC: #4) ---

func TestSessionPersistenceOnDisconnect(t *testing.T) {
	// Subtask 4.1: Create sessions, simulate client disconnect (change status to detached),
	// verify sessions remain accessible.
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-aaa", Command: "claude", Status: "running"},
		{ID: "sess-bbb", Command: "codex", Status: "running"},
	}
	setup := setupIntentRouterTest(t, sessions...)

	// Simulate client disconnect — update all sessions to detached
	setup.SessionProvider.UpdateStatus("sess-aaa", "detached")
	setup.SessionProvider.UpdateStatus("sess-bbb", "detached")

	// Publish lifecycle events for detach
	setup.Bus.Publish(setup.Ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsLifecycle,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionLifecycleEvent{
			SessionID: "sess-aaa",
			State:     eventbus.SessionStateDetached,
		},
	})
	setup.Bus.Publish(setup.Ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsLifecycle,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionLifecycleEvent{
			SessionID: "sess-bbb",
			State:     eventbus.SessionStateDetached,
		},
	})

	// Subtask 4.2: Verify ListSessionInfos returns all sessions with detached status
	// No sleep needed: the provider was updated synchronously via UpdateStatus().
	// Lifecycle events to the router are fire-and-forget (no side-effect for detach).
	infos := setup.SessionProvider.ListSessionInfos()
	if len(infos) != 2 {
		t.Fatalf("expected 2 sessions after disconnect, got %d", len(infos))
	}
	for _, info := range infos {
		if info.Status != "detached" {
			t.Errorf("session %s: expected status=detached, got %q", info.ID, info.Status)
		}
	}

	// Voice commands should still work — list sessions on detached sessions
	publishSessionlessPrompt(setup.Ctx, setup.Bus, "prompt-list-detached", "list sessions")
	speak := waitForSpeak(t, setup.SpeakSub, 2*time.Second)
	if !strings.Contains(speak.Text, "2 sessions") {
		t.Errorf("expected speak text to mention '2 sessions' while detached, got %q", speak.Text)
	}

	// Verify adapter sees detached status
	req := setup.Adapter.LastRequest()
	for _, s := range req.AvailableSessions {
		if s.Status != "detached" {
			t.Errorf("session %s: expected detached in AI request, got %q", s.ID, s.Status)
		}
	}

	// Subtask 4.3: Re-attach — update status back to running
	setup.SessionProvider.UpdateStatus("sess-aaa", "running")
	setup.SessionProvider.UpdateStatus("sess-bbb", "running")

	setup.Bus.Publish(setup.Ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsLifecycle,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionLifecycleEvent{
			SessionID: "sess-aaa",
			State:     eventbus.SessionStateRunning,
		},
	})
	setup.Bus.Publish(setup.Ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsLifecycle,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionLifecycleEvent{
			SessionID: "sess-bbb",
			State:     eventbus.SessionStateRunning,
		},
	})

	// Provider updated synchronously; lifecycle events are fire-and-forget.
	infos = setup.SessionProvider.ListSessionInfos()
	for _, info := range infos {
		if info.Status != "running" {
			t.Errorf("session %s: expected status=running after reattach, got %q", info.ID, info.Status)
		}
	}
}

// --- Task 5: Verify session restart via voice (AC: #3) ---

func TestVoiceSessionRestart(t *testing.T) {
	// NOTE: Story 3-1 validates intent recognition only. The mock returns
	// ActionSpeak (confirmation) because actual session restart requires
	// ActionCreateSession which doesn't exist yet. Story 3-2 will add the
	// real restart side-effects. This test verifies the AI receives stopped
	// session info and the original command for restart context.

	// Subtask 5.1: Create session with command "echo test", kill it, restart via voice.
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-aaa", Command: "echo", Args: []string{"test"}, Status: "running"},
	}
	setup := setupIntentRouterTest(t, sessions...)

	// Kill the session (transition to stopped)
	setup.SessionProvider.UpdateStatus("sess-aaa", "stopped")
	setup.Bus.Publish(setup.Ctx, eventbus.Envelope{
		Topic:  eventbus.TopicSessionsLifecycle,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionLifecycleEvent{
			SessionID: "sess-aaa",
			State:     eventbus.SessionStateStopped,
		},
	})

	// Provider updated synchronously. Router's lifecycle handler only clears
	// tool cache on stopped — doesn't affect prompt routing or test assertions.

	// Subtask 5.2: Verify AI receives stopped session info
	publishSessionlessPrompt(setup.Ctx, setup.Bus, "prompt-restart-1", "restart session sess-aaa")

	speak := waitForSpeak(t, setup.SpeakSub, 2*time.Second)
	if !strings.Contains(speak.Text, "sess-aaa") {
		t.Errorf("expected speak text to reference 'sess-aaa', got %q", speak.Text)
	}

	// Subtask 5.3: Verify AI speaks confirmation of restart
	if !strings.Contains(speak.Text, "Restarting") {
		t.Errorf("expected speak text to contain 'Restarting', got %q", speak.Text)
	}

	// Verify the AI received stopped session info
	req := setup.Adapter.LastRequest()
	found := false
	for _, s := range req.AvailableSessions {
		if s.ID == "sess-aaa" {
			found = true
			if s.Status != "stopped" {
				t.Errorf("expected stopped status for sess-aaa in AI request, got %q", s.Status)
			}
			if s.Command != "echo" {
				t.Errorf("expected command=echo for sess-aaa, got %q", s.Command)
			}
		}
	}
	if !found {
		t.Error("sess-aaa not found in AvailableSessions")
	}
}

// --- Task 6: Verify multi-session voice isolation (AC: #1, #2, #5) ---

func TestMultiSessionVoiceIsolation(t *testing.T) {
	// Subtask 6.1: Create 3 sessions, issue voice commands targeting specific sessions.
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-aaa", Command: "claude", Status: "running"},
		{ID: "sess-bbb", Command: "codex", Status: "running"},
		{ID: "sess-ccc", Command: "bash", Status: "running"},
	}
	setup := setupIntentRouterTest(t, sessions...)

	// Kill sess-bbb specifically
	publishPrompt(setup.Ctx, setup.Bus, "", "prompt-iso-1", "kill session sess-bbb")

	// Wait for both replies to be processed (command_queued + speak)
	waitForReply(t, setup.ReplySub, 2*time.Second)
	waitForReply(t, setup.ReplySub, 2*time.Second)
	// Drain speak event (the adapter returns two actions: command + speak)
	waitForSpeak(t, setup.SpeakSub, 2*time.Second)

	commands := setup.CommandExecutor.GetCommands()
	if len(commands) != 1 {
		t.Fatalf("expected exactly 1 command, got %d", len(commands))
	}
	if commands[0].SessionID != "sess-bbb" {
		t.Errorf("expected command targeting sess-bbb, got %q", commands[0].SessionID)
	}

	// Verify no commands were sent to sess-aaa or sess-ccc
	for _, cmd := range commands {
		if cmd.SessionID == "sess-aaa" {
			t.Error("unexpected command to sess-aaa")
		}
		if cmd.SessionID == "sess-ccc" {
			t.Error("unexpected command to sess-ccc")
		}
	}

	// Subtask 6.2: Verify selectTargetSession correctly resolves session references
	req := setup.Adapter.LastRequest()
	if len(req.AvailableSessions) != 3 {
		t.Errorf("expected 3 available sessions, got %d", len(req.AvailableSessions))
	}
}

// --- Task 7: Verify intent router smart session routing (AC: #1, #2) ---

func TestIntentRouterSessionRouting(t *testing.T) {
	t.Run("ConversationSessionTracking", func(t *testing.T) {
		// Subtask 7.1: After talking about session A, next command without explicit session
		// reference routes to A (within 5-minute TTL).
		sessions := []intentrouter.SessionInfo{
			{ID: "sess-aaa", Command: "claude", Status: "running"},
			{ID: "sess-bbb", Command: "codex", Status: "running"},
		}
		setup := setupIntentRouterTest(t, sessions...)

		// First command targets sess-aaa explicitly — "run ls" produces ActionCommand,
		// which causes intent router to call updateConversationSession(sess-aaa).
		publishPrompt(setup.Ctx, setup.Bus, "sess-aaa", "prompt-rt-1", "run ls")

		// Wait for command_queued reply
		waitForReply(t, setup.ReplySub, 2*time.Second)

		// Now publish a sessionless prompt — should route to sess-aaa because of conversation context
		publishSessionlessPrompt(setup.Ctx, setup.Bus, "prompt-rt-2", "what's running")

		waitForSpeak(t, setup.SpeakSub, 2*time.Second)

		// The second request should have been routed with sess-aaa context
		requests := setup.Adapter.GetRequests()
		if len(requests) < 2 {
			t.Fatalf("expected at least 2 requests, got %d", len(requests))
		}
		// Intent router should have used sess-aaa as conversation session for the 2nd request
		// This is reflected in the SessionID field of the IntentRequest
		secondReq := requests[1]
		if secondReq.SessionID != "sess-aaa" {
			t.Errorf("expected session routing to sess-aaa, got %q (conversation context should apply)", secondReq.SessionID)
		}
	})

	t.Run("SingleSessionFallback", func(t *testing.T) {
		// Subtask 7.2: When only 1 session exists, commands implicitly target it.
		setup := setupIntentRouterTest(t, intentrouter.SessionInfo{
			ID:      "only-session",
			Command: "claude",
			Status:  "running",
		})

		// Publish sessionless prompt
		publishSessionlessPrompt(setup.Ctx, setup.Bus, "prompt-single-1", "run ls")

		waitForReply(t, setup.ReplySub, 2*time.Second)

		// Should have routed to the only available session
		req := setup.Adapter.LastRequest()
		if req.SessionID != "only-session" {
			t.Errorf("expected single-session fallback to 'only-session', got %q", req.SessionID)
		}
	})

	t.Run("SessionlessQueriesWorkWithoutTarget", func(t *testing.T) {
		// Subtask 7.3: "list sessions" works without a target session.
		sessions := []intentrouter.SessionInfo{
			{ID: "sess-aaa", Command: "claude", Status: "running"},
			{ID: "sess-bbb", Command: "codex", Status: "running"},
			{ID: "sess-ccc", Command: "bash", Status: "running"},
		}
		setup := setupIntentRouterTest(t, sessions...)

		// "list sessions" should work even without an explicit session target
		publishSessionlessPrompt(setup.Ctx, setup.Bus, "prompt-sessionless-1", "list sessions")

		speak := waitForSpeak(t, setup.SpeakSub, 2*time.Second)
		if !strings.Contains(speak.Text, "3 sessions") {
			t.Errorf("expected speak text to mention '3 sessions', got %q", speak.Text)
		}

		// AI should receive all 3 sessions in AvailableSessions
		req := setup.Adapter.LastRequest()
		if len(req.AvailableSessions) != 3 {
			t.Errorf("expected 3 available sessions, got %d", len(req.AvailableSessions))
		}
	})
}

// --- Additional test: Conversation service integration ---

func TestVoiceSessionCreationWithConversationService(t *testing.T) {
	// This test wires the conversation service together with the intent router
	// to verify the full prompt → AI → reply → conversation history flow.
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := newSessionAwareMockAdapter()
	provider := newMutableSessionProvider(intentrouter.SessionInfo{
		ID:      "sess-001",
		Command: "claude",
		Status:  "running",
	})
	executor := newRecordingCommandExecutor()

	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(8),
		conversation.WithDetachTTL(5*time.Second),
	)

	router := intentrouter.NewService(bus,
		intentrouter.WithAdapter(adapter),
		intentrouter.WithSessionProvider(provider),
		intentrouter.WithCommandExecutor(executor),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conversationSvc.Start(ctx); err != nil {
		t.Fatalf("start conversation service: %v", err)
	}
	defer conversationSvc.Shutdown(context.Background())

	if err := router.Start(ctx); err != nil {
		t.Fatalf("start intent router: %v", err)
	}
	defer router.Shutdown(context.Background())

	speakSub := bus.Subscribe(eventbus.TopicConversationSpeak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()

	// Simulate voice transcript flowing through the pipeline.
	// In the real flow, this would come from STT → content pipeline → conversation service.
	// Here we publish directly to the conversation prompt topic.
	publishPrompt(ctx, bus, "sess-001", "prompt-conv-1", "start a new Claude session")

	// Verify speak event
	speak := waitForSpeak(t, speakSub, 2*time.Second)
	if !strings.Contains(speak.Text, "Creating") {
		t.Errorf("expected speak text to contain 'Creating', got %q", speak.Text)
	}

	// Verify conversation service recorded the AI reply.
	// Poll instead of sleeping — the conversation service processes the reply
	// event asynchronously via its own subscription goroutine.
	deadline := time.Now().Add(2 * time.Second)
	var foundAIReply bool
	for time.Now().Before(deadline) {
		for _, turn := range conversationSvc.Context("sess-001") {
			if turn.Origin == eventbus.OriginAI && strings.Contains(turn.Text, "Creating") {
				foundAIReply = true
				break
			}
		}
		if foundAIReply {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !foundAIReply {
		t.Fatal("timeout waiting for AI reply in conversation history")
	}
}

// --- Negative tests: Error handling paths ---

func TestIntentRouterAdapterNotReady(t *testing.T) {
	// Verify: when adapter.Ready() returns false, the router publishes an error
	// reply with error_type=adapter_not_ready and increments RequestsFailed.
	setup := setupIntentRouterTest(t, intentrouter.SessionInfo{
		ID:      "sess-001",
		Command: "claude",
		Status:  "running",
	})

	// Set adapter to not ready
	setup.Adapter.SetReady(false)

	publishPrompt(setup.Ctx, setup.Bus, "sess-001", "prompt-err-1", "list sessions")

	// Router should publish error reply
	reply := waitForReply(t, setup.ReplySub, 2*time.Second)
	if reply.Metadata["error"] != "true" {
		t.Errorf("expected error=true in reply metadata, got %q", reply.Metadata["error"])
	}
	if reply.Metadata["error_type"] != "adapter_not_ready" {
		t.Errorf("expected error_type=adapter_not_ready, got %q", reply.Metadata["error_type"])
	}

	// Router should also publish a user-friendly speak event for TTS
	speak := waitForSpeak(t, setup.SpeakSub, 2*time.Second)
	if speak.Metadata["type"] != "error" {
		t.Errorf("expected speak metadata type=error, got %q", speak.Metadata["type"])
	}

	// Verify metrics
	metrics := setup.Router.Metrics()
	if metrics.RequestsFailed != 1 {
		t.Errorf("expected 1 failed request, got %d", metrics.RequestsFailed)
	}
}

func TestIntentRouterAdapterError(t *testing.T) {
	// Verify: when adapter.ResolveIntent() returns an error, the router
	// publishes an error reply and increments RequestsFailed.
	bus := eventbus.New()

	// Use an adapter that always returns an error
	adapter := &errorMockAdapter{err: fmt.Errorf("AI service unavailable")}
	provider := newMutableSessionProvider(intentrouter.SessionInfo{
		ID:      "sess-001",
		Command: "claude",
		Status:  "running",
	})
	executor := newRecordingCommandExecutor()

	router := intentrouter.NewService(bus,
		intentrouter.WithAdapter(adapter),
		intentrouter.WithSessionProvider(provider),
		intentrouter.WithCommandExecutor(executor),
	)

	replySub := bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("test_reply"))
	speakSub := bus.Subscribe(eventbus.TopicConversationSpeak, eventbus.WithSubscriptionName("test_speak"))

	ctx, cancel := context.WithCancel(context.Background())

	if err := router.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start intent router: %v", err)
	}

	t.Cleanup(func() {
		cancel()
		replySub.Close()
		speakSub.Close()
		router.Shutdown(context.Background())
		bus.Shutdown()
	})

	publishPrompt(ctx, bus, "sess-001", "prompt-err-2", "start a new session")

	reply := waitForReply(t, replySub, 2*time.Second)
	if reply.Metadata["error"] != "true" {
		t.Errorf("expected error=true in reply metadata, got %q", reply.Metadata["error"])
	}

	// Router's publishError also sends a user-friendly TTS speak event
	speak := waitForSpeak(t, speakSub, 2*time.Second)
	if speak.Metadata["type"] != "error" {
		t.Errorf("expected speak metadata type=error, got %q", speak.Metadata["type"])
	}

	// Verify metrics
	metrics := router.Metrics()
	if metrics.RequestsFailed != 1 {
		t.Errorf("expected 1 failed request, got %d", metrics.RequestsFailed)
	}
	if metrics.SpeakEvents != 0 {
		t.Errorf("expected 0 speak events (error path doesn't count), got %d", metrics.SpeakEvents)
	}
}

func TestIntentRouterKillNonExistentSession(t *testing.T) {
	// Verify: when AI returns ActionCommand targeting a non-existent session,
	// the router publishes an error reply (session not found) instead of
	// queuing the command.
	setup := setupIntentRouterTest(t,
		intentrouter.SessionInfo{ID: "sess-aaa", Command: "claude", Status: "running"},
		intentrouter.SessionInfo{ID: "sess-bbb", Command: "codex", Status: "running"},
	)

	// Kill a session that doesn't exist — "sess-ghost" doesn't match any
	// available session ID, so extractSessionRef returns "".
	// The router receives ActionCommand with SessionRef="" and no prompt
	// session, so ValidateSession("") fails → error reply.
	publishPrompt(setup.Ctx, setup.Bus, "", "prompt-err-3", "kill session sess-ghost")

	// The router sends two replies: one error for the failed command
	// (session not found) and one for the speak action.
	// Collect replies and check that at least one has error=true.
	var gotError bool
	var errorType string
	for i := 0; i < 2; i++ {
		reply := waitForReply(t, setup.ReplySub, 2*time.Second)
		if reply.Metadata["error"] == "true" {
			gotError = true
			errorType = reply.Metadata["error_type"]
		}
	}
	if !gotError {
		t.Error("expected at least one error reply for non-existent session")
	}
	if errorType != "session_not_found" {
		t.Errorf("expected error_type=session_not_found, got %q", errorType)
	}

	// No commands should have been queued
	commands := setup.CommandExecutor.GetCommands()
	if len(commands) != 0 {
		t.Errorf("expected 0 commands (session doesn't exist), got %d", len(commands))
	}
}

// errorMockAdapter always returns an error from ResolveIntent.
type errorMockAdapter struct {
	err error
}

func (a *errorMockAdapter) Name() string          { return "error-mock" }
func (a *errorMockAdapter) Ready() bool            { return true }
func (a *errorMockAdapter) ResolveIntent(_ context.Context, _ intentrouter.IntentRequest) (*intentrouter.IntentResponse, error) {
	return nil, a.err
}
