package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/sanitize"
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

func (m *mockTokenStore) DeletePushTokenIfMatch(_ context.Context, deviceID, token string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, t := range m.tokens {
		if t.DeviceID == deviceID && t.Token == token {
			m.tokens = append(m.tokens[:i], m.tokens[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// L3 fix (Review 14): replace fragile time.Sleep(time.Second) negative
// assertions with a polling helper that checks the counter at short intervals
// for a bounded duration. Fails the test only if the counter becomes non-zero.
func assertNoRequestsWithin(t *testing.T, counter *atomic.Int32, duration time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if counter.Load() != 0 {
			t.Errorf("%s: expected 0 HTTP requests, got %d", msg, counter.Load())
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if counter.Load() != 0 {
		t.Errorf("%s: expected 0 HTTP requests, got %d", msg, counter.Load())
	}
}

func newExpoTestServer(t *testing.T, received *[]ExpoMessage, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msgs []ExpoMessage
		if err := json.NewDecoder(r.Body).Decode(&msgs); err != nil {
			t.Errorf("newExpoTestServer: failed to decode request body: %v", err)
			http.Error(w, "decode error", http.StatusBadRequest)
			return
		}
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
	t.Cleanup(srv.Close)
	return srv
}

func TestService_LifecycleEvent_TaskCompleted(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventTaskCompleted}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)

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
	if received[0].Data["eventType"] != constants.NotificationEventTaskCompleted {
		t.Errorf("eventType = %q, want TASK_COMPLETED", received[0].Data["eventType"])
	}
}

func TestService_LifecycleEvent_Error(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventError}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)

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
	if received[0].Data["eventType"] != constants.NotificationEventError {
		t.Errorf("eventType = %q, want ERROR", received[0].Data["eventType"])
	}
}

// M4/R5: verify that nil ExitCode produces a generic error body without "(code ...)".
func TestService_LifecycleEvent_NilExitCode(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventError}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// Session stopped with nil exit code (e.g., killed by signal without exit status).
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "sess-nil-exit",
		Label:     "sigkill-victim",
		State:     eventbus.SessionStateStopped,
		ExitCode:  nil,
	})

	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(received))
	}
	if received[0].Data["eventType"] != constants.NotificationEventError {
		t.Errorf("eventType = %q, want ERROR", received[0].Data["eventType"])
	}
	if want := "Session 'sigkill-victim' exited with error"; received[0].Body != want {
		t.Errorf("body = %q, want %q", received[0].Body, want)
	}
	// Verify no "(code ...)" in body since exit code is nil.
	if strings.Contains(received[0].Body, "code") {
		t.Errorf("body should not contain 'code' for nil exit: %q", received[0].Body)
	}
}

func TestService_LifecycleEvent_UserKill_Suppressed(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventError, constants.NotificationEventTaskCompleted}},
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

	// User-initiated kill (KillSession RPC sets Reason=SessionReasonKilled).
	// Should NOT trigger an ERROR notification — the user already knows.
	exitCode := 137
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "sess-killed",
		Label:     "claude",
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
		Reason:    eventbus.SessionReasonKilled,
	})

	// L3 fix (Review 14): polling-based negative assertion instead of fixed sleep.
	assertNoRequestsWithin(t, &requestCount, 500*time.Millisecond, "user-initiated kill")
}

func TestService_SpeakEvent_InputNeeded(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventInputNeeded}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)

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
		Metadata:  map[string]string{constants.SpeakMetadataTypeKey: constants.SpeakTypeClarification},
	})

	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(received))
	}
	if received[0].Data["eventType"] != constants.NotificationEventInputNeeded {
		t.Errorf("eventType = %q, want INPUT_NEEDED", received[0].Data["eventType"])
	}
}

func TestService_Deduplication(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventTaskCompleted}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)

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
	t.Parallel()
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

	// L3 fix (Review 14): polling-based negative assertion instead of fixed sleep.
	assertNoRequestsWithin(t, &requestCount, 500*time.Millisecond, "no tokens")
}

func TestService_IgnoresRunningState(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventTaskCompleted, constants.NotificationEventError}},
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

	// L3 fix (Review 14): polling-based negative assertion instead of fixed sleep.
	assertNoRequestsWithin(t, &requestCount, 500*time.Millisecond, "running state")
}

func TestService_DeviceNotRegisteredCleanup(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[stale]", EnabledEvents: []string{constants.NotificationEventTaskCompleted}},
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

	// Poll until token store is empty or timeout.
	waitForTokenCount(t, store, 0, 2*time.Second)

	remaining := store.tokenCount()
	if remaining != 0 {
		t.Errorf("expected 0 tokens after DeviceNotRegistered cleanup, got %d", remaining)
	}
}

func TestService_SpeakEvent_IgnoresRegularSpeak(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventTaskCompleted, constants.NotificationEventInputNeeded, constants.NotificationEventError}},
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

	// L3 fix (Review 14): polling-based negative assertion instead of fixed sleep.
	assertNoRequestsWithin(t, &requestCount, 500*time.Millisecond, "regular speak")
}

func TestService_SpeakEvent_Completion(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventTaskCompleted}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)

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
	if received[0].Data["eventType"] != constants.NotificationEventTaskCompleted {
		t.Errorf("eventType = %q, want TASK_COMPLETED", received[0].Data["eventType"])
	}
	if received[0].Title != "Nupi: Task completed" {
		t.Errorf("title = %q, want 'Nupi: Task completed'", received[0].Title)
	}
}

func TestService_ShuttingDown_SuppressesNotifications(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventTaskCompleted, constants.NotificationEventError}},
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

	// L3 fix (Review 14): polling-based negative assertion instead of fixed sleep.
	assertNoRequestsWithin(t, &requestCount, 500*time.Millisecond, "after shutdown")
}

func TestService_SpeakEvent_ErrorMetadata(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventError}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)

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
	if received[0].Data["eventType"] != constants.NotificationEventError {
		t.Errorf("eventType = %q, want ERROR", received[0].Data["eventType"])
	}
}

func TestService_DedupClearedWhenNoTokens(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{tokens: nil} // Start with no tokens.

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)

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
		DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventTaskCompleted},
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
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[same]", EnabledEvents: []string{constants.NotificationEventTaskCompleted}},
			{DeviceID: "d2", Token: "ExponentPushToken[same]", EnabledEvents: []string{constants.NotificationEventTaskCompleted}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)

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
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[stale]", EnabledEvents: []string{constants.NotificationEventTaskCompleted}},
			{DeviceID: "d2", Token: "ExponentPushToken[stale]", EnabledEvents: []string{constants.NotificationEventTaskCompleted}},
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

	// Poll until token store is empty or timeout.
	waitForTokenCount(t, store, 0, 2*time.Second)

	remaining := store.tokenCount()
	if remaining != 0 {
		t.Errorf("expected 0 tokens after stale cleanup of all matching devices, got %d", remaining)
	}
}

func TestService_PipelineEvent_IdleInputNeeded(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventInputNeeded}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)

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
			constants.MetadataKeyNotable:    constants.MetadataValueTrue,
			constants.MetadataKeyIdleState:  "prompt",
			constants.MetadataKeyWaitingFor: constants.PipelineWaitingForUserInput,
			constants.MetadataKeyPromptText: "Waiting for user input at shell prompt",
		},
	})

	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(received))
	}
	if received[0].Data["eventType"] != constants.NotificationEventInputNeeded {
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
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventInputNeeded}},
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
		Annotations: map[string]string{constants.MetadataKeyIdleState: "timeout"},
	})

	// L3 fix (Review 14): polling-based negative assertion instead of fixed sleep.
	assertNoRequestsWithin(t, &requestCount, 500*time.Millisecond, "non-notable pipeline event")
}

// M5/R8: verify that unknown waiting_for values are ignored by the pipeline handler.
func TestService_PipelineEvent_UnknownWaitingFor_Ignored(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventInputNeeded}},
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

	// Pipeline event with notable=true and idle_state but an unknown waiting_for
	// value — should be ignored by the allowlist filter.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "sess-unknown-wait",
		Text:      "$ ",
		Annotations: map[string]string{
			constants.MetadataKeyNotable:    constants.MetadataValueTrue,
			constants.MetadataKeyIdleState:  "prompt",
			constants.MetadataKeyWaitingFor: "unknown_type",
		},
	})

	// L3 fix (Review 14): polling-based negative assertion instead of fixed sleep.
	assertNoRequestsWithin(t, &requestCount, 500*time.Millisecond, "unknown waiting_for")
}

// H1/R10: verify that pipeline events with empty SessionID are ignored.
func TestService_PipelineEvent_EmptySessionID_Ignored(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventInputNeeded}},
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

	// Pipeline event with empty SessionID — should be ignored.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "",
		Text:      "$ ",
		Annotations: map[string]string{
			constants.MetadataKeyNotable:    constants.MetadataValueTrue,
			constants.MetadataKeyIdleState:  "prompt",
			constants.MetadataKeyWaitingFor: constants.PipelineWaitingForUserInput,
			constants.MetadataKeyPromptText: "Waiting for input",
		},
	})

	// L3 fix (Review 14): polling-based negative assertion instead of fixed sleep.
	assertNoRequestsWithin(t, &requestCount, 500*time.Millisecond, "empty sessionID pipeline event")
}

func TestService_PipelineEvent_FallbackBody(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventInputNeeded}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)

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
			constants.MetadataKeyNotable:    constants.MetadataValueTrue,
			constants.MetadataKeyIdleState:  "prompt",
			constants.MetadataKeyWaitingFor: constants.PipelineWaitingForConfirmation,
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

func TestService_PanicRecovery_DoesNotCrash(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventTaskCompleted}},
		},
	}

	// Use a server that will cause a panic inside the service's send path.
	// We achieve this by creating a service with a nil expo client internally,
	// but the simpler approach is to test that the dispatchAsync recover works.
	svc := NewService(store, bus, WithExpoURL("http://127.0.0.1:1")) // unreachable
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Directly test that dispatchAsync recovers panics.
	done := make(chan struct{})
	svc.dispatchAsync(ctx, func() {
		defer func() { close(done) }()
		panic("test panic in notification handler")
	})

	select {
	case <-done:
		// Panic was recovered — goroutine completed.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for panic recovery")
	}

	// Service should still be operational — shutdown should not hang.
	svc.Shutdown(context.Background())
}

// TestService_ConcurrentSemaphoreBound verifies that dispatchAsync limits
// concurrent notification goroutines to maxConcurrentSend (4).
func TestService_ConcurrentSemaphoreBound(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventError}},
		},
	}

	var maxConcurrent atomic.Int32
	var current atomic.Int32
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := current.Add(1)
		// Track peak concurrency.
		for {
			old := maxConcurrent.Load()
			if n <= old || maxConcurrent.CompareAndSwap(old, n) {
				break
			}
		}
		<-gate // Block until test releases.
		current.Add(-1)

		var msgs []ExpoMessage
		json.NewDecoder(r.Body).Decode(&msgs)
		tickets := make([]ExpoTicket, len(msgs))
		for i := range msgs {
			tickets[i] = ExpoTicket{Status: "ok", ID: "t"}
		}
		resp := ExpoResponse{Data: tickets}
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

	// Publish 8 events (2x maxConcurrentSend) with unique session IDs to
	// bypass dedup. Use different sessions so dedup doesn't collapse them.
	for i := 0; i < 8; i++ {
		exitCode := 1
		eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
			SessionID: fmt.Sprintf("sess-%d", i),
			State:     eventbus.SessionStateStopped,
			ExitCode:  &exitCode,
		})
	}

	// Wait for semaphore to fill (all 4 slots busy).
	deadline := time.After(2 * time.Second)
	for current.Load() < int32(maxConcurrentSend) {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d concurrent handlers, got %d", maxConcurrentSend, current.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Give a brief moment for any extra goroutines to sneak past.
	time.Sleep(100 * time.Millisecond)

	peak := maxConcurrent.Load()
	if peak > int32(maxConcurrentSend) {
		t.Errorf("peak concurrent handlers = %d, want <= %d", peak, maxConcurrentSend)
	}

	// Release all blocked handlers.
	close(gate)
	svc.Shutdown(context.Background())
}

// TestService_SpeakEvent_BodyTruncation verifies that notification body is
// truncated to maxNotifBodyBytes (2048) for speak events.
func TestService_SpeakEvent_BodyTruncation(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventInputNeeded}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// Publish speak event with body larger than maxNotifBodyBytes.
	longBody := strings.Repeat("A", 4096)
	eventbus.Publish(ctx, bus, eventbus.Conversation.Speak, eventbus.SourceIntentRouter, eventbus.ConversationSpeakEvent{
		SessionID: "sess-trunc",
		Text:      longBody,
		Metadata:  map[string]string{constants.SpeakMetadataTypeKey: constants.SpeakTypeClarification},
	})

	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(received))
	}
	if len(received[0].Body) > maxNotifBodyBytes {
		t.Errorf("body length = %d, want <= %d", len(received[0].Body), maxNotifBodyBytes)
	}
	if len(received[0].Body) == 0 {
		t.Error("body should not be empty after truncation")
	}
}

func TestTruncateUTF8(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		maxBytes int
		want     string
	}{
		{"ascii short", "hello", 10, "hello"},
		{"ascii exact", "hello", 5, "hello"},
		{"ascii truncate", "hello world", 5, "hello"},
		{"utf8 no split", "héllo", 6, "héllo"}, // é is 2 bytes, total 6
		{"utf8 mid-char", "héllo", 2, "h"},     // byte 2 is continuation of é
		{"emoji no split", "hi🎉x", 4, "hi"},    // 🎉 is 4 bytes at offset 2
		{"empty", "", 10, ""},
		{"zero max", "hello", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitize.TruncateUTF8(tt.input, tt.maxBytes)
			if got != tt.want {
				t.Errorf("sanitize.TruncateUTF8(%q, %d) = %q, want %q", tt.input, tt.maxBytes, got, tt.want)
			}
		})
	}
}

func waitForTokenCount(t *testing.T, store *mockTokenStore, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			actual := store.tokenCount()
			if actual != want {
				t.Fatalf("timed out waiting for token count %d, got %d", want, actual)
			}
			return
		case <-time.After(50 * time.Millisecond):
			if store.tokenCount() == want {
				return
			}
		}
	}
}

// M4/R9: verify that empty Label falls back to SessionID for display name.
func TestService_LifecycleEvent_EmptyLabel_FallbackToSessionID(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventTaskCompleted}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	exitCode := 0
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "abc-123-def",
		Label:     "", // empty — should fall back to SessionID
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
	})

	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(received))
	}
	// Body should use SessionID as display name.
	if want := "Session 'abc-123-def' has finished"; received[0].Body != want {
		t.Errorf("body = %q, want %q", received[0].Body, want)
	}
}

// H1/R9: verify that lifecycle events with empty SessionID are ignored.
func TestService_LifecycleEvent_EmptySessionID_Ignored(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventTaskCompleted, constants.NotificationEventError}},
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

	exitCode := 0
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "", // empty — should be ignored
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
	})

	// L3 fix (Review 14): polling-based negative assertion instead of fixed sleep.
	assertNoRequestsWithin(t, &requestCount, 500*time.Millisecond, "empty sessionID")
}

// M5/R9: verify pipeline event body truncation for oversized prompt_text.
func TestService_PipelineEvent_BodyTruncation(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	store := &mockTokenStore{
		tokens: []configstore.PushToken{
			{DeviceID: "d1", Token: "ExponentPushToken[a]", EnabledEvents: []string{constants.NotificationEventInputNeeded}},
		},
	}

	var received []ExpoMessage
	var mu sync.Mutex
	srv := newExpoTestServer(t, &received, &mu)

	svc := NewService(store, bus, WithExpoURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(context.Background())

	longPrompt := strings.Repeat("X", 4096)
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "sess-trunc-pipe",
		Text:      "$ ",
		Annotations: map[string]string{
			constants.MetadataKeyNotable:    constants.MetadataValueTrue,
			constants.MetadataKeyIdleState:  "prompt",
			constants.MetadataKeyWaitingFor: constants.PipelineWaitingForUserInput,
			constants.MetadataKeyPromptText: longPrompt,
		},
	})

	waitForMessages(t, &received, &mu, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(received))
	}
	if len(received[0].Body) > maxNotifBodyBytes {
		t.Errorf("body length = %d, want <= %d", len(received[0].Body), maxNotifBodyBytes)
	}
	if len(received[0].Body) == 0 {
		t.Error("body should not be empty after truncation")
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
