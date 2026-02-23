package notification

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

type mockTokenStore struct {
	mu     sync.Mutex
	tokens []configstore.PushToken
}

func (m *mockTokenStore) ListPushTokensForEvent(_ context.Context, eventType string) ([]configstore.PushToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var result []configstore.PushToken
	for _, t := range m.tokens {
		for _, ev := range t.EnabledEvents {
			if ev == eventType {
				result = append(result, t)
				break
			}
		}
	}
	if result == nil {
		result = []configstore.PushToken{}
	}
	return result, nil
}

func (m *mockTokenStore) addTokens(tokens ...configstore.PushToken) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens = append(m.tokens, tokens...)
}

func (m *mockTokenStore) tokenCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tokens)
}

func (m *mockTokenStore) DeletePushToken(_ context.Context, deviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, t := range m.tokens {
		if t.DeviceID == deviceID {
			m.tokens = append(m.tokens[:i], m.tokens[i+1:]...)
			return nil
		}
	}
	return nil
}

func newExpoTestServer(t *testing.T, received *[]ExpoMessage, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msgs []ExpoMessage
		json.NewDecoder(r.Body).Decode(&msgs)
		mu.Lock()
		*received = append(*received, msgs...)
		mu.Unlock()

		tickets := make([]ExpoTicket, len(msgs))
		for i := range msgs {
			tickets[i] = ExpoTicket{Status: "ok", ID: "t"}
		}
		resp := ExpoResponse{Data: tickets}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestService_LifecycleEvent_TaskCompleted(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{"TASK_COMPLETED"}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// Publish session stopped with exit code 0.
	exitCode := 0
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "sess-1",
		Label:     "claude",
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
	})

	// Wait for notification to be sent.
	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(received))
	}
	if received[0].Title != "Nupi: Task completed" {
		t.Errorf("title = %q, want 'Nupi: Task completed'", received[0].Title)
	}
	if want := "Session 'claude' has finished"; received[0].Body != want {
		t.Errorf("body = %q, want %q", received[0].Body, want)
	}
	if received[0].Data["sessionId"] != "sess-1" {
		t.Errorf("sessionId = %q, want sess-1", received[0].Data["sessionId"])
	}
	if received[0].Data["eventType"] != "TASK_COMPLETED" {
		t.Errorf("eventType = %q, want TASK_COMPLETED", received[0].Data["eventType"])
	}
}

func TestService_LifecycleEvent_Error(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{"ERROR"}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// Publish session stopped with non-zero exit code.
	exitCode := 1
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "sess-err",
		Label:     "npm",
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
	})

	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(received))
	}
	if received[0].Title != "Nupi: Session error" {
		t.Errorf("title = %q, want 'Nupi: Session error'", received[0].Title)
	}
	if want := "Session 'npm' exited with error (code 1)"; received[0].Body != want {
		t.Errorf("body = %q, want %q", received[0].Body, want)
	}
	if received[0].Data["eventType"] != "ERROR" {
		t.Errorf("eventType = %q, want ERROR", received[0].Data["eventType"])
	}
}

func TestService_SpeakEvent_InputNeeded(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{"INPUT_NEEDED"}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	eventbus.Publish(ctx, bus, eventbus.Conversation.Speak, eventbus.SourceIntentRouter, eventbus.ConversationSpeakEvent{
		SessionID: "sess-input",
		Text:      "Session needs your input: please confirm the deployment",
		Metadata:  map[string]string{"type": "clarification"},
	})

	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(received))
	}
	if received[0].Data["eventType"] != "INPUT_NEEDED" {
		t.Errorf("eventType = %q, want INPUT_NEEDED", received[0].Data["eventType"])
	}
}

func TestService_Deduplication(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{"TASK_COMPLETED"}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	exitCode := 0

	// Send the same event twice rapidly.
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "sess-dup",
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
	})
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "sess-dup",
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
	})

	// Wait for at least the first notification to arrive.
	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	// Give some extra time for any unexpected second message.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	count := len(received)
	mu.Unlock()

	if count != 1 {
		t.Errorf("expected 1 notification (dedup), got %d", count)
	}
}

func TestService_NoTokens_NoSend(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{tokens: nil}

	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(ExpoResponse{})
	}))
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	exitCode := 0
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "sess-no-tokens",
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
	})

	time.Sleep(500 * time.Millisecond)

	if rc := requestCount.Load(); rc != 0 {
		t.Errorf("expected 0 HTTP requests when no tokens, got %d", rc)
	}
}

func TestService_IgnoresRunningState(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{"TASK_COMPLETED", "ERROR"}},
		},
	}

	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(ExpoResponse{Data: []ExpoTicket{{Status: "ok"}}})
	}))
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// running state should not trigger notification.
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "sess-running",
		State:     eventbus.SessionStateRunning,
	})

	time.Sleep(500 * time.Millisecond)

	if rc := requestCount.Load(); rc != 0 {
		t.Errorf("expected 0 HTTP requests for running state, got %d", rc)
	}
}

func TestService_DeviceNotRegisteredCleanup(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[stale]", EnabledEvents: []string{"TASK_COMPLETED"}},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ExpoResponse{
			Data: []ExpoTicket{
				{Status: "error", Details: json.RawMessage(`{"error":"DeviceNotRegistered"}`)},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	exitCode := 0
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "sess-cleanup",
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
	})

	// Wait for processing.
	time.Sleep(500 * time.Millisecond)

	store.mu.Lock()
	remaining := len(store.tokens)
	store.mu.Unlock()

	if remaining != 0 {
		t.Errorf("expected 0 tokens after DeviceNotRegistered cleanup, got %d", remaining)
	}
}

func TestService_SpeakEvent_IgnoresRegularSpeak(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{"TASK_COMPLETED", "INPUT_NEEDED", "ERROR"}},
		},
	}

	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(ExpoResponse{})
	}))
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// Regular speak event (no "type" metadata) should NOT trigger a notification.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Speak, eventbus.SourceIntentRouter, eventbus.ConversationSpeakEvent{
		SessionID: "sess-tts",
		Text:      "Here is the TTS response for the user",
	})

	time.Sleep(500 * time.Millisecond)

	if rc := requestCount.Load(); rc != 0 {
		t.Errorf("expected 0 HTTP requests for regular speak, got %d", rc)
	}
}

func TestService_SpeakEvent_Completion(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{"TASK_COMPLETED"}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	eventbus.Publish(ctx, bus, eventbus.Conversation.Speak, eventbus.SourceIntentRouter, eventbus.ConversationSpeakEvent{
		SessionID: "sess-completion",
		Text:      "Task has been completed successfully",
		Metadata:  map[string]string{"type": "completion"},
	})

	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(received))
	}
	if received[0].Data["eventType"] != "TASK_COMPLETED" {
		t.Errorf("eventType = %q, want TASK_COMPLETED", received[0].Data["eventType"])
	}
	if received[0].Title != "Nupi: Task completed" {
		t.Errorf("title = %q, want 'Nupi: Task completed'", received[0].Title)
	}
}

func TestService_ShuttingDown_SuppressesNotifications(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{"TASK_COMPLETED", "ERROR"}},
		},
	}

	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(ExpoResponse{Data: []ExpoTicket{{Status: "ok"}}})
	}))
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Shut down the service — sets shuttingDown flag.
	svc.Shutdown(context.Background())

	// Publish a lifecycle event after shutdown (simulates daemon killing sessions).
	exitCode := 1
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "sess-shutdown",
		Label:     "claude",
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
	})

	// Wait briefly for any unexpected processing.
	time.Sleep(500 * time.Millisecond)

	if rc := requestCount.Load(); rc != 0 {
		t.Errorf("expected 0 HTTP requests after shutdown, got %d", rc)
	}
}

func TestService_SpeakEvent_ErrorMetadata(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{"ERROR"}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// Error speak event with "type": "error" metadata should trigger ERROR notification.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Speak, eventbus.SourceIntentRouter, eventbus.ConversationSpeakEvent{
		SessionID: "sess-speak-err",
		Text:      "Something went wrong with the adapter",
		Metadata:  map[string]string{"type": "error", "error": "adapter not ready"},
	})

	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(received))
	}
	if received[0].Data["eventType"] != "ERROR" {
		t.Errorf("eventType = %q, want ERROR", received[0].Data["eventType"])
	}
}

func TestService_DedupClearedWhenNoTokens(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{tokens: nil} // Start with no tokens.

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	exitCode := 0

	// First event — no tokens registered, should not send but should clear dedup.
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "sess-dedup-clear",
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
	})

	// Wait for the event to be processed.
	time.Sleep(300 * time.Millisecond)

	// Now register a token.
	store.addTokens(configstore.PushToken{
		DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{"TASK_COMPLETED"},
	})

	// Second event within 30s — should still send because dedup was cleared.
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "sess-dedup-clear",
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
	})

	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	mu.Lock()
	count := len(received)
	mu.Unlock()

	if count != 1 {
		t.Fatalf("expected 1 notification after token registration, got %d", count)
	}
}

func TestService_DuplicateExpoTokens_SinglePush(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[same]", EnabledEvents: []string{"TASK_COMPLETED"}},
			{DeviceID: "d2", Token: "ExponentPushToken[same]", EnabledEvents: []string{"TASK_COMPLETED"}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	exitCode := 0
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "sess-dup-token",
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
	})

	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	// Give extra time for any unexpected second message.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	count := len(received)
	mu.Unlock()

	if count != 1 {
		t.Errorf("expected 1 push message (deduped), got %d", count)
	}
}

func TestService_StaleCleanup_RemovesAllMatchingDevices(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[stale]", EnabledEvents: []string{"TASK_COMPLETED"}},
			{DeviceID: "d2", Token: "ExponentPushToken[stale]", EnabledEvents: []string{"TASK_COMPLETED"}},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ExpoResponse{
			Data: []ExpoTicket{
				{Status: "error", Details: json.RawMessage(`{"error":"DeviceNotRegistered"}`)},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	exitCode := 0
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "sess-stale-all",
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
	})

	// Wait for processing.
	time.Sleep(500 * time.Millisecond)

	remaining := store.tokenCount()
	if remaining != 0 {
		t.Errorf("expected 0 tokens after stale cleanup of all matching devices, got %d", remaining)
	}
}

func TestService_PipelineEvent_IdleInputNeeded(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{"INPUT_NEEDED"}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// Publish a pipeline event with idle state annotations from tool handler.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "sess-idle",
		Text:      "$ ",
		Annotations: map[string]string{
			"notable":     "true",
			"idle_state":  "prompt",
			"waiting_for": "user_input",
			"prompt_text": "Waiting for user input at shell prompt",
		},
	})

	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(received))
	}
	if received[0].Data["eventType"] != "INPUT_NEEDED" {
		t.Errorf("eventType = %q, want INPUT_NEEDED", received[0].Data["eventType"])
	}
	if received[0].Title != "Nupi: Input needed" {
		t.Errorf("title = %q, want 'Nupi: Input needed'", received[0].Title)
	}
	if want := "Waiting for user input at shell prompt"; received[0].Body != want {
		t.Errorf("body = %q, want %q", received[0].Body, want)
	}
}

func TestService_PipelineEvent_IgnoresNonNotable(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{"INPUT_NEEDED"}},
		},
	}

	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(ExpoResponse{})
	}))
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// Pipeline event without "notable" annotation — should be ignored.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID:   "sess-normal",
		Text:        "some output",
		Annotations: map[string]string{"idle_state": "timeout"},
	})

	time.Sleep(500 * time.Millisecond)

	if rc := requestCount.Load(); rc != 0 {
		t.Errorf("expected 0 HTTP requests for non-notable pipeline event, got %d", rc)
	}
}

func TestService_PipelineEvent_FallbackBody(t *testing.T) {
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{"INPUT_NEEDED"}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)
	defer srv.Close()

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// Pipeline event with idle state but no prompt_text — should use fallback body.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "sess-confirm",
		Text:      "y/n?",
		Annotations: map[string]string{
			"notable":     "true",
			"idle_state":  "prompt",
			"waiting_for": "confirmation",
		},
	})

	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(received))
	}
	if want := "Session is waiting for confirmation"; received[0].Body != want {
		t.Errorf("body = %q, want %q", received[0].Body, want)
	}
}

func waitForMessages(t *testing.T, received *[]ExpoMessage, mu *sync.Mutex, minCount int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			mu.Lock()
			actual := len(*received)
			mu.Unlock()
			if actual < minCount {
				t.Fatalf("timed out waiting for %d messages, got %d", minCount, actual)
			}
			return
		case <-time.After(50 * time.Millisecond):
			mu.Lock()
			actual := len(*received)
			mu.Unlock()
			if actual >= minCount {
				return
			}
		}
	}
}
