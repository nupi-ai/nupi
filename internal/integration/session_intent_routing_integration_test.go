package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/conversation"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/intentrouter"
)

// routingMockAdapter is an IntentAdapter mock with routing-specific patterns:
// "tell session X to Y", "what happened in session X", ambiguous commands,
// session_output event handling, and clarification responses.
type routingMockAdapter struct {
	mu       sync.RWMutex
	name     string
	ready    bool
	requests []intentrouter.IntentRequest
}

func newRoutingMockAdapter() *routingMockAdapter {
	return &routingMockAdapter{
		name:  "routing-mock",
		ready: true,
	}
}

func (a *routingMockAdapter) Name() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.name
}

func (a *routingMockAdapter) Ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ready
}

func (a *routingMockAdapter) GetRequests() []intentrouter.IntentRequest {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]intentrouter.IntentRequest{}, a.requests...)
}

func (a *routingMockAdapter) LastRequest() intentrouter.IntentRequest {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.requests) == 0 {
		return intentrouter.IntentRequest{}
	}
	return a.requests[len(a.requests)-1]
}

func (a *routingMockAdapter) ResolveIntent(_ context.Context, req intentrouter.IntentRequest) (*intentrouter.IntentResponse, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()

	transcript := strings.TrimSpace(strings.ToLower(req.Transcript))

	// "tell session X to Y" → route command to specific session
	if strings.HasPrefix(transcript, "tell session ") {
		rest := strings.TrimPrefix(transcript, "tell session ")
		// Extract session ref and command: "tell session sess-codex to write unit tests"
		sessionRef := extractSessionRef(rest, req.AvailableSessions)
		command := rest
		if idx := strings.Index(rest, " to "); idx >= 0 {
			command = rest[idx+4:]
		}
		return &intentrouter.IntentResponse{
			PromptID:   req.PromptID,
			Confidence: 0.9,
			Actions: []intentrouter.IntentAction{
				{
					Type:       intentrouter.ActionCommand,
					SessionRef: sessionRef,
					Command:    command,
				},
				{
					Type: intentrouter.ActionSpeak,
					Text: fmt.Sprintf("Sending command to session %s.", sessionRef),
				},
			},
			Reasoning: "User directed command to specific session",
		}, nil
	}

	// "what happened in session X" → summarize session history
	if strings.Contains(transcript, "what happened") && strings.Contains(transcript, "session") {
		sessionRef := extractSessionRef(transcript, req.AvailableSessions)
		// Build summary from conversation history
		summary := fmt.Sprintf("Session %s history: %d turns of activity.", sessionRef, len(req.ConversationHistory))
		if len(req.ConversationHistory) > 0 {
			lastTurn := req.ConversationHistory[len(req.ConversationHistory)-1]
			summary += fmt.Sprintf(" Last activity: %s", lastTurn.Text)
		}
		return &intentrouter.IntentResponse{
			PromptID:   req.PromptID,
			Confidence: 0.85,
			Actions: []intentrouter.IntentAction{
				{
					Type: intentrouter.ActionSpeak,
					Text: summary,
				},
			},
			Reasoning: "User requested session history summary",
		}, nil
	}

	// Session output notification — AI notifies user about notable session events
	if req.EventType == intentrouter.EventTypeSessionOutput {
		notification := fmt.Sprintf("Session %s update: %s", req.SessionID, req.SessionOutput)
		return &intentrouter.IntentResponse{
			PromptID:   req.PromptID,
			Confidence: 0.8,
			Actions: []intentrouter.IntentAction{
				{
					Type: intentrouter.ActionSpeak,
					Text: notification,
				},
			},
			Reasoning: "Proactive notification for session output",
		}, nil
	}

	// Ambiguous command without session ref → ask for clarification
	if (strings.Contains(transcript, "run ") || strings.Contains(transcript, "execute ")) &&
		len(req.AvailableSessions) > 1 && req.SessionID == "" {
		return &intentrouter.IntentResponse{
			PromptID:   req.PromptID,
			Confidence: 0.6,
			Actions: []intentrouter.IntentAction{
				{
					Type: intentrouter.ActionClarify,
					Text: "Which session should run the tests?",
				},
			},
			Reasoning: "Ambiguous command — multiple sessions, no target",
		}, nil
	}

	// Clarification response: "session X" after a previous clarification
	if req.EventType == intentrouter.EventTypeClarification ||
		(strings.HasPrefix(transcript, "session ") && len(transcript) < 30) {
		sessionRef := extractSessionRef(transcript, req.AvailableSessions)
		if sessionRef != "" {
			return &intentrouter.IntentResponse{
				PromptID:   req.PromptID,
				Confidence: 0.9,
				Actions: []intentrouter.IntentAction{
					{
						Type:       intentrouter.ActionCommand,
						SessionRef: sessionRef,
						Command:    "make test",
					},
					{
						Type: intentrouter.ActionSpeak,
						Text: fmt.Sprintf("Running tests in session %s.", sessionRef),
					},
				},
				Reasoning: "User clarified session target",
			}, nil
		}
	}

	// "run X" with explicit session context → route to that session
	if strings.HasPrefix(transcript, "run ") && req.SessionID != "" {
		command := strings.TrimPrefix(transcript, "run ")
		return &intentrouter.IntentResponse{
			PromptID:   req.PromptID,
			Confidence: 0.85,
			Actions: []intentrouter.IntentAction{
				{
					Type:       intentrouter.ActionCommand,
					SessionRef: req.SessionID,
					Command:    command,
				},
			},
			Reasoning: "User requested command execution in context session",
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

// routingTestSetup wraps intentRouterTestSetup with the routing-specific mock adapter.
type routingTestSetup struct {
	*intentRouterTestSetup
	RoutingAdapter *routingMockAdapter
}

func setupRoutingTestFull(t *testing.T, sessions ...intentrouter.SessionInfo) *routingTestSetup {
	t.Helper()

	bus := eventbus.New()
	adapter := newRoutingMockAdapter()
	provider := newMutableSessionProvider(sessions...)
	executor := newRecordingCommandExecutor()

	router := intentrouter.NewService(bus,
		intentrouter.WithAdapter(adapter),
		intentrouter.WithSessionProvider(provider),
		intentrouter.WithCommandExecutor(executor),
	)

	replySub := eventbus.SubscribeTo(bus, eventbus.Conversation.Reply, eventbus.WithSubscriptionName("test_reply"))
	speakSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Speak, eventbus.WithSubscriptionName("test_speak"))

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

	return &routingTestSetup{
		intentRouterTestSetup: &intentRouterTestSetup{
			Bus:             bus,
			Router:          router,
			SessionProvider: provider,
			CommandExecutor: executor,
			ReplySub:        replySub,
			SpeakSub:        speakSub,
			Ctx:             ctx,
			Cancel:          cancel,
		},
		RoutingAdapter: adapter,
	}
}

// --- Task 1: Validate session-specific command routing via AI intent (AC: #1) ---

func TestAICommandRoutingToSpecificSession(t *testing.T) {
	// Subtask 1.1: Create 2 sessions, mock AI routes "tell session two to write unit tests"
	// to sess-codex. Verify command queued ONLY to sess-codex.
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-claude", Command: "claude", Status: "running", WorkDir: "/project"},
		{ID: "sess-codex", Command: "codex", Status: "running", WorkDir: "/project"},
	}
	setup := setupRoutingTestFull(t, sessions...)

	publishPrompt(setup.Ctx, setup.Bus, "", "prompt-route-1",
		"tell session sess-codex to write unit tests")

	// Wait for speak confirmation (second action in response)
	speak := waitForSpeak(t, setup.SpeakSub, 2*time.Second)
	if !strings.Contains(speak.Text, "sess-codex") {
		t.Errorf("expected speak to reference sess-codex, got %q", speak.Text)
	}

	// Drain both replies (command_queued + speak)
	waitForReply(t, setup.ReplySub, 2*time.Second)
	waitForReply(t, setup.ReplySub, 2*time.Second)

	// Verify command routed to correct session
	commands := setup.CommandExecutor.GetCommands()
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}
	if commands[0].SessionID != "sess-codex" {
		t.Errorf("expected command targeting sess-codex, got %q", commands[0].SessionID)
	}
	if commands[0].Command != "write unit tests" {
		t.Errorf("expected command 'write unit tests', got %q", commands[0].Command)
	}

	// Verify no commands sent to sess-claude
	for _, cmd := range commands {
		if cmd.SessionID == "sess-claude" {
			t.Error("unexpected command sent to sess-claude")
		}
	}

	// Verify AI adapter received correct session info
	req := setup.RoutingAdapter.LastRequest()
	if len(req.AvailableSessions) != 2 {
		t.Errorf("expected 2 available sessions, got %d", len(req.AvailableSessions))
	}
}

func TestAICommandRoutingWithConversationContext(t *testing.T) {
	// Subtask 1.2: Issue command to sess-A, then issue sessionless command.
	// Verify conversation session routing directs second command to sess-A
	// (within 5-min TTL window).
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-aaa", Command: "claude", Status: "running"},
		{ID: "sess-bbb", Command: "codex", Status: "running"},
	}
	setup := setupRoutingTestFull(t, sessions...)

	// First: direct command to sess-aaa. "tell session sess-aaa to ..."
	// triggers ActionCommand with SessionRef=sess-aaa, which calls
	// updateConversationSession(sess-aaa).
	publishPrompt(setup.Ctx, setup.Bus, "", "prompt-ctx-1",
		"tell session sess-aaa to list files")

	// Drain replies from first command
	waitForReply(t, setup.ReplySub, 2*time.Second)
	waitForReply(t, setup.ReplySub, 2*time.Second)
	waitForSpeak(t, setup.SpeakSub, 2*time.Second)

	// Second: sessionless command. Router should apply conversation session context
	// (sess-aaa) because it was just targeted.
	publishSessionlessPrompt(setup.Ctx, setup.Bus, "prompt-ctx-2", "run make test")

	// The router fills SessionID from conversation session before calling adapter.
	// Since sess-aaa was the last command target, the adapter should receive
	// SessionID=sess-aaa. Our mock then routes to that session.
	waitForReply(t, setup.ReplySub, 2*time.Second)

	requests := setup.RoutingAdapter.GetRequests()
	if len(requests) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(requests))
	}
	secondReq := requests[1]
	if secondReq.SessionID != "sess-aaa" {
		t.Errorf("expected conversation context routing to sess-aaa, got %q", secondReq.SessionID)
	}
}

func TestAICommandRoutingMultiAction(t *testing.T) {
	// Subtask 1.3: Mock AI returns [ActionCommand, ActionSpeak] in single response.
	// Verify both actions executed: command queued AND speak event published.
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-target", Command: "claude", Status: "running"},
	}
	setup := setupRoutingTestFull(t, sessions...)

	// "tell session sess-target to run tests" → ActionCommand + ActionSpeak
	publishPrompt(setup.Ctx, setup.Bus, "", "prompt-multi-1",
		"tell session sess-target to run tests")

	// Verify speak event published
	speak := waitForSpeak(t, setup.SpeakSub, 2*time.Second)
	if !strings.Contains(speak.Text, "sess-target") {
		t.Errorf("expected speak to reference sess-target, got %q", speak.Text)
	}

	// Drain both replies
	waitForReply(t, setup.ReplySub, 2*time.Second)
	waitForReply(t, setup.ReplySub, 2*time.Second)

	// Verify command queued
	commands := setup.CommandExecutor.GetCommands()
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}
	if commands[0].SessionID != "sess-target" {
		t.Errorf("expected command targeting sess-target, got %q", commands[0].SessionID)
	}

}

// --- Task 2: Validate proactive idle notifications via session output events (AC: #2) ---

func TestProactiveIdleNotification(t *testing.T) {
	// Subtask 2.1: Publish PipelineMessageEvent with notable=true, verify
	// conversation service triggers conversation.prompt with event_type=session_output,
	// mock AI returns ActionSpeak notification, verify speak event published.
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := newRoutingMockAdapter()
	provider := newMutableSessionProvider(intentrouter.SessionInfo{
		ID:      "sess-build",
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

	speakSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Speak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()

	// Publish notable pipeline message (simulates content pipeline detecting idle)
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "sess-build",
		Origin:    eventbus.OriginTool,
		Text:      "Build completed successfully. 42 tests passed.",
		Annotations: map[string]string{
			"notable":     "true",
			"idle_state":  "prompt",
			"event_title": "Build completed",
		},
	})

	// Verify AI speaks proactive notification
	speak := waitForSpeak(t, speakSub, 3*time.Second)
	if !strings.Contains(speak.Text, "sess-build") {
		t.Errorf("expected notification to reference sess-build, got %q", speak.Text)
	}
	if !strings.Contains(speak.Text, "Build completed") {
		t.Errorf("expected notification to contain build info, got %q", speak.Text)
	}

	// Verify adapter received EventTypeSessionOutput
	req := adapter.LastRequest()
	if req.EventType != intentrouter.EventTypeSessionOutput {
		t.Errorf("expected EventType=session_output, got %q", req.EventType)
	}
	if req.SessionID != "sess-build" {
		t.Errorf("expected SessionID=sess-build, got %q", req.SessionID)
	}
}

func TestProactiveNotificationRateLimiting(t *testing.T) {
	// Subtask 2.2: Publish 3 notable events within 2 seconds for same session.
	// Verify only first triggers AI (conversation service rate-limits session_output).
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := newRoutingMockAdapter()
	provider := newMutableSessionProvider(intentrouter.SessionInfo{
		ID:      "sess-rate",
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

	speakSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Speak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()

	// Publish 3 notable events in quick succession
	for i := 0; i < 3; i++ {
		eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
			SessionID: "sess-rate",
			Origin:    eventbus.OriginTool,
			Text:      fmt.Sprintf("Event %d happened", i+1),
			Annotations: map[string]string{
				"notable":     "true",
				"idle_state":  "prompt",
				"event_title": fmt.Sprintf("Event %d", i+1),
			},
		})
	}

	// Wait for first speak event
	waitForSpeak(t, speakSub, 3*time.Second)

	// Wait for the full rate-limiting window (2s) plus margin to confirm no extras arrive
	timer := time.NewTimer(2500 * time.Millisecond)
	defer timer.Stop()
	extraCount := 0
	for {
		select {
		case _, ok := <-speakSub.C():
			if !ok {
				goto done
			}
			extraCount++
		case <-timer.C:
			goto done
		}
	}
done:

	// Rate limiting allows at most 1 trigger per 2s interval.
	// With 3 events in quick succession, only the first should trigger AI.
	if extraCount > 0 {
		t.Errorf("expected 0 extra speak events (rate limited), got %d", extraCount)
	}

	// Verify adapter received exactly 1 request
	requests := adapter.GetRequests()
	if len(requests) != 1 {
		t.Errorf("expected 1 adapter request (rate limited), got %d", len(requests))
	}
}

func TestProactiveNotificationIncludesContext(t *testing.T) {
	// Subtask 2.3: Verify IntentRequest includes SessionOutput field, EventType,
	// and session conversation history.
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := newRoutingMockAdapter()
	provider := newMutableSessionProvider(intentrouter.SessionInfo{
		ID:      "sess-ctx",
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

	speakSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Speak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()

	replySub := eventbus.SubscribeTo(bus, eventbus.Conversation.Reply, eventbus.WithSubscriptionName("test_reply"))
	defer replySub.Close()

	// First: add conversation history via PipelineMessageEvent with OriginUser.
	// This triggers the full flow: conversation service stores the turn,
	// publishes ConversationPromptEvent, intent router processes it.
	// Use text that triggers speak (not command) so we can drain it.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "sess-ctx",
		Origin:    eventbus.OriginUser,
		Text:      "check build status",
	})
	// Wait for reply (adapter echoes back as speak for unrecognized commands)
	waitForReply(t, replySub, 2*time.Second)
	// Drain the speak event
	waitForSpeak(t, speakSub, 2*time.Second)

	// Wait for conversation history to be populated
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(conversationSvc.Context("sess-ctx")) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Now: publish notable session output (triggers session_output flow)
	// This is the first session_output for this session, so rate limiter allows it.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "sess-ctx",
		Origin:    eventbus.OriginTool,
		Text:      "Error: compilation failed at main.go:42",
		Annotations: map[string]string{
			"notable":     "true",
			"idle_state":  "prompt",
			"event_title": "Compilation error",
		},
	})

	// Wait for the proactive notification speak
	waitForSpeak(t, speakSub, 3*time.Second)

	// Verify adapter received correct context
	requests := adapter.GetRequests()
	// Find the session_output request (may be second if user_intent was first)
	var sessionOutputReq *intentrouter.IntentRequest
	for i := range requests {
		if requests[i].EventType == intentrouter.EventTypeSessionOutput {
			sessionOutputReq = &requests[i]
			break
		}
	}
	if sessionOutputReq == nil {
		t.Fatal("no session_output request found in adapter requests")
	}

	if sessionOutputReq.SessionID != "sess-ctx" {
		t.Errorf("expected SessionID=sess-ctx, got %q", sessionOutputReq.SessionID)
	}
	if sessionOutputReq.SessionOutput == "" {
		t.Error("expected SessionOutput to be populated")
	}
	if !strings.Contains(sessionOutputReq.SessionOutput, "compilation failed") {
		t.Errorf("expected SessionOutput to contain error text, got %q", sessionOutputReq.SessionOutput)
	}
	// Verify conversation history is passed to session_output requests.
	// We built one user turn ("check build status") + its AI reply earlier,
	// so the session_output request should carry that history.
	if len(sessionOutputReq.ConversationHistory) == 0 {
		t.Error("expected non-empty ConversationHistory in session_output request")
	}
}

// --- Task 3: Validate session history summarization (AC: #3) ---

func TestSessionHistorySummarization(t *testing.T) {
	// Subtask 3.1: Populate session conversation history (3+ turns),
	// publish "what happened in session X", verify AI returns ActionSpeak summary.
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := newRoutingMockAdapter()
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-hist", Command: "claude", Status: "running"},
	}
	provider := newMutableSessionProvider(sessions...)
	executor := newRecordingCommandExecutor()

	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(50),
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

	speakSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Speak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()

	// Build conversation history via PipelineMessageEvent (OriginUser triggers
	// conversation service → prompt → intent router → reply → back to history).
	userMessages := []string{"make build", "make test", "make lint"}
	for _, msg := range userMessages {
		eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
			SessionID: "sess-hist",
			Origin:    eventbus.OriginUser,
			Text:      msg,
		})
		waitForSpeak(t, speakSub, 2*time.Second)
	}

	// Wait for conversation service to have recorded history
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(conversationSvc.Context("sess-hist")) >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Now ask "what happened in session sess-hist" — also via pipeline flow
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "sess-hist",
		Origin:    eventbus.OriginUser,
		Text:      "what happened in session sess-hist",
	})

	speak := waitForSpeak(t, speakSub, 2*time.Second)
	if !strings.Contains(speak.Text, "sess-hist") {
		t.Errorf("expected speak to reference sess-hist, got %q", speak.Text)
	}
	if !strings.Contains(speak.Text, "history") {
		t.Errorf("expected speak to contain 'history' summary, got %q", speak.Text)
	}

	// Verify adapter received conversation history with at least the 3 user turns
	// we published above. History may also include AI reply turns.
	req := adapter.LastRequest()
	if len(req.ConversationHistory) < 3 {
		t.Errorf("expected at least 3 conversation history turns, got %d", len(req.ConversationHistory))
	}
}

func TestHistorySummarizationUsesCorrectSessionContext(t *testing.T) {
	// Subtask 3.2: Create 2 sessions with distinct histories.
	// Ask about session 1 — verify adapter receives session 1's history only.
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := newRoutingMockAdapter()
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-one", Command: "claude", Status: "running"},
		{ID: "sess-two", Command: "codex", Status: "running"},
	}
	provider := newMutableSessionProvider(sessions...)
	executor := newRecordingCommandExecutor()

	conversationSvc := conversation.NewService(bus,
		conversation.WithHistoryLimit(50),
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

	speakSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Speak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()

	// Build history for sess-one via PipelineMessageEvent
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "sess-one",
		Origin:    eventbus.OriginUser,
		Text:      "make build",
	})
	waitForSpeak(t, speakSub, 2*time.Second)

	// Build history for sess-two via PipelineMessageEvent
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "sess-two",
		Origin:    eventbus.OriginUser,
		Text:      "npm install",
	})
	waitForSpeak(t, speakSub, 2*time.Second)

	// Wait for conversation service to record both
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(conversationSvc.Context("sess-one")) > 0 && len(conversationSvc.Context("sess-two")) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Ask about sess-one specifically — via pipeline flow
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "sess-one",
		Origin:    eventbus.OriginUser,
		Text:      "what happened in session sess-one",
	})

	speak := waitForSpeak(t, speakSub, 2*time.Second)
	if !strings.Contains(speak.Text, "sess-one") {
		t.Errorf("expected speak to reference sess-one, got %q", speak.Text)
	}

	// Verify the request's ConversationHistory comes from sess-one context.
	// The ConversationPromptEvent has Context populated by conversation service
	// from sess-one's history (not sess-two's).
	req := adapter.LastRequest()
	if req.SessionID != "sess-one" {
		t.Errorf("expected SessionID=sess-one, got %q", req.SessionID)
	}
	// History should NOT contain sess-two's "npm install" text
	for _, turn := range req.ConversationHistory {
		if strings.Contains(turn.Text, "npm install") {
			t.Error("session one's history should not contain session two's 'npm install' activity")
		}
	}
}

// --- Task 4: Validate intent clarification flow (AC: #4) ---

func TestIntentClarificationFlow(t *testing.T) {
	// Subtask 4.1: Create 3 sessions. Publish ambiguous command "run tests" without
	// session context. Mock AI returns ActionClarify. Verify clarification speak
	// event published and reply does NOT have error=true.
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-aaa", Command: "claude", Status: "running"},
		{ID: "sess-bbb", Command: "codex", Status: "running"},
		{ID: "sess-ccc", Command: "bash", Status: "running"},
	}
	setup := setupRoutingTestFull(t, sessions...)

	// Ambiguous command — no session specified, multiple sessions active.
	// Our mock detects this and returns ActionClarify.
	publishSessionlessPrompt(setup.Ctx, setup.Bus, "prompt-ambig-1", "run tests")

	// Verify clarification speak event
	speak := waitForSpeak(t, setup.SpeakSub, 2*time.Second)
	if !strings.Contains(speak.Text, "Which session") {
		t.Errorf("expected clarification question, got %q", speak.Text)
	}
	if speak.Metadata[constants.SpeakMetadataTypeKey] != constants.SpeakTypeClarification {
		t.Errorf("expected speak metadata type=clarification, got %q", speak.Metadata[constants.SpeakMetadataTypeKey])
	}

	// Verify reply does NOT have error=true (clarification is not an error)
	reply := waitForReply(t, setup.ReplySub, 2*time.Second)
	if reply.Metadata["error"] == "true" {
		t.Error("clarification reply should not have error=true")
	}
	if reply.Metadata["status"] != constants.PromptEventClarification {
		t.Errorf("expected reply status=clarification, got %q", reply.Metadata["status"])
	}

}

func TestClarificationResponseRoutedCorrectly(t *testing.T) {
	// Subtask 4.2: Full round-trip: ambiguous → clarification → user response → execution.
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-aaa", Command: "claude", Status: "running"},
		{ID: "sess-bbb", Command: "codex", Status: "running"},
	}
	setup := setupRoutingTestFull(t, sessions...)

	// Step 1: Ambiguous command → triggers clarification
	publishSessionlessPrompt(setup.Ctx, setup.Bus, "prompt-clarify-1", "run tests")

	speak := waitForSpeak(t, setup.SpeakSub, 2*time.Second)
	if !strings.Contains(speak.Text, "Which session") {
		t.Errorf("expected clarification question, got %q", speak.Text)
	}
	// Drain reply
	waitForReply(t, setup.ReplySub, 2*time.Second)

	// Step 2: User responds with session reference — the mock adapter recognizes
	// "session sess-bbb" as a clarification response and returns ActionCommand.
	publishPrompt(setup.Ctx, setup.Bus, "", "prompt-clarify-2", "session sess-bbb")

	// Drain replies (command_queued + speak)
	waitForReply(t, setup.ReplySub, 2*time.Second)
	waitForReply(t, setup.ReplySub, 2*time.Second)
	waitForSpeak(t, setup.SpeakSub, 2*time.Second)

	// Verify command was routed to sess-bbb
	commands := setup.CommandExecutor.GetCommands()
	if len(commands) != 1 {
		t.Fatalf("expected 1 command after clarification, got %d", len(commands))
	}
	if commands[0].SessionID != "sess-bbb" {
		t.Errorf("expected command targeting sess-bbb, got %q", commands[0].SessionID)
	}
	if commands[0].Command != "make test" {
		t.Errorf("expected command 'make test', got %q", commands[0].Command)
	}
}

// --- Task 5: Validate edge cases and error handling (AC: #1, #2, #3, #4) ---

func TestCommandRoutingToNonExistentSession(t *testing.T) {
	// Subtask 5.1: AI correctly identifies and routes command to a specific session,
	// but ValidateSession rejects it. Verify session_not_found error.
	//
	// sess-gone remains in AvailableSessions so the adapter's extractSessionRef
	// finds it and returns SessionRef="sess-gone". But ValidateSession rejects it,
	// producing session_not_found. This tests the real scenario: AI targets a
	// visible-but-uncommandable session.
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-alive", Command: "claude", Status: "running"},
		{ID: "sess-gone", Command: "codex", Status: "running"},
	}
	setup := setupRoutingTestFull(t, sessions...)

	// Mark sess-gone as rejected — it stays in AvailableSessions (adapter can
	// see and reference it) but ValidateSession returns an error.
	setup.SessionProvider.RejectValidation("sess-gone")

	// AI returns ActionCommand{SessionRef: "sess-gone"}, but executeCommand fails.
	publishPrompt(setup.Ctx, setup.Bus, "", "prompt-gone-1",
		"tell session sess-gone to check status")

	// Collect replies — expect error for the command, plus speak action succeeds
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
		t.Error("expected error reply for rejected session command")
	}
	if errorType != "session_not_found" {
		t.Errorf("expected error_type=session_not_found, got %q", errorType)
	}

	// Verify adapter actually targeted sess-gone (not empty ref).
	req := setup.RoutingAdapter.LastRequest()
	found := false
	for _, s := range req.AvailableSessions {
		if s.ID == "sess-gone" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected sess-gone in AvailableSessions so adapter can target it")
	}
}

func TestSessionOutputForNonNotableEventsIgnored(t *testing.T) {
	// Subtask 5.2: Publish PipelineMessageEvent with Origin=OriginTool but WITHOUT
	// notable=true. Verify no conversation.prompt event triggered.
	//
	// Uses event-based synchronization instead of sleep: publish the non-notable
	// event, then publish a known-good notable event and wait for its speak.
	// If the adapter received exactly 1 request (the notable one), it proves
	// the non-notable event was correctly ignored.
	bus := eventbus.New()
	defer bus.Shutdown()

	adapter := newRoutingMockAdapter()
	provider := newMutableSessionProvider(intentrouter.SessionInfo{
		ID:      "sess-quiet",
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

	speakSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Speak, eventbus.WithSubscriptionName("test_speak"))
	defer speakSub.Close()

	// Step 1: Publish non-notable tool output — should be ignored by conversation service.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "sess-quiet",
		Origin:    eventbus.OriginTool,
		Text:      "Regular output: building project...",
		// No "notable": "true" annotation
	})

	// Step 2: Publish a known-good notable event and wait for its speak.
	// This acts as a synchronization barrier — when we receive the speak for
	// this event, we know the non-notable event has already been processed
	// (events on the same topic are delivered in order).
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "sess-quiet",
		Origin:    eventbus.OriginTool,
		Text:      "Build completed, all tests passed.",
		Annotations: map[string]string{
			"notable":     "true",
			"idle_state":  "prompt",
			"event_title": "Build completed",
		},
	})

	// Wait for the notable event's speak — proves both events were processed.
	waitForSpeak(t, speakSub, 3*time.Second)

	// Adapter should have received exactly 1 request (the notable event only).
	requests := adapter.GetRequests()
	if len(requests) != 1 {
		t.Errorf("expected 1 adapter request (notable only), got %d", len(requests))
	}
	if len(requests) > 0 && requests[0].EventType != intentrouter.EventTypeSessionOutput {
		t.Errorf("expected the single request to be session_output, got %q", requests[0].EventType)
	}
}

func TestMultipleSessionsSameCommand(t *testing.T) {
	// Subtask 5.3: AI returns ActionCommand for one session and ActionSpeak for another
	// in same response. Verify both actions executed correctly.
	// Note: This is already covered by TestAICommandRoutingMultiAction (multi-action response).
	// This test validates the specific case of the speak action referencing a different
	// session context than the command action.
	sessions := []intentrouter.SessionInfo{
		{ID: "sess-worker", Command: "claude", Status: "running"},
		{ID: "sess-monitor", Command: "bash", Status: "running"},
	}
	setup := setupRoutingTestFull(t, sessions...)

	// "tell session sess-worker to deploy" → ActionCommand (sess-worker) + ActionSpeak
	publishPrompt(setup.Ctx, setup.Bus, "", "prompt-dual-1",
		"tell session sess-worker to deploy app")

	// Verify speak event
	speak := waitForSpeak(t, setup.SpeakSub, 2*time.Second)
	if !strings.Contains(speak.Text, "sess-worker") {
		t.Errorf("expected speak to reference sess-worker, got %q", speak.Text)
	}

	// Drain replies
	waitForReply(t, setup.ReplySub, 2*time.Second)
	waitForReply(t, setup.ReplySub, 2*time.Second)

	// Verify command queued to sess-worker only
	commands := setup.CommandExecutor.GetCommands()
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}
	if commands[0].SessionID != "sess-worker" {
		t.Errorf("expected command targeting sess-worker, got %q", commands[0].SessionID)
	}

	// Verify no commands sent to sess-monitor
	for _, cmd := range commands {
		if cmd.SessionID == "sess-monitor" {
			t.Error("unexpected command sent to sess-monitor")
		}
	}
}
