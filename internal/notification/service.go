package notification

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

const (
	dedupWindow       = constants.Duration30Seconds
	notificationQueue = 64
	maxConcurrentSend = 4
)

// TokenStore abstracts push token storage used by the notification service.
type TokenStore interface {
	ListPushTokensForEvent(ctx context.Context, eventType string) ([]configstore.PushToken, error)
	DeletePushToken(ctx context.Context, deviceID string) error
}

// Service subscribes to session lifecycle and conversation speak events,
// formats push notifications, and sends them via the Expo Push API.
type Service struct {
	store TokenStore
	bus   *eventbus.Bus
	expo  *ExpoClient

	cancel       context.CancelFunc
	wg           sync.WaitGroup
	lifecycleSub *eventbus.TypedSubscription[eventbus.SessionLifecycleEvent]
	speakSub     *eventbus.TypedSubscription[eventbus.ConversationSpeakEvent]
	pipelineSub  *eventbus.TypedSubscription[eventbus.PipelineMessageEvent]

	// shuttingDown is set during Shutdown to suppress notifications for
	// sessions being killed as part of daemon graceful shutdown.
	shuttingDown atomic.Bool

	// dedup: key = "{sessionID}:{eventType}", value = last notification time
	dedupMu sync.Mutex
	dedup   map[string]time.Time

	// sendSem bounds concurrent sendNotification goroutines.
	sendSem chan struct{}
}

// NewService creates a notification service.
func NewService(store TokenStore, bus *eventbus.Bus, opts ...ExpoClientOption) *Service {
	return &Service{
		store:   store,
		bus:     bus,
		expo:    NewExpoClient(opts...),
		dedup:   make(map[string]time.Time),
		sendSem: make(chan struct{}, maxConcurrentSend),
	}
}

// Start subscribes to event bus topics and begins consuming events.
func (s *Service) Start(ctx context.Context) error {
	derived, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	s.lifecycleSub = eventbus.Subscribe[eventbus.SessionLifecycleEvent](
		s.bus,
		eventbus.TopicSessionsLifecycle,
		eventbus.WithSubscriptionName("notification_lifecycle"),
		eventbus.WithSubscriptionBuffer(notificationQueue),
	)

	s.speakSub = eventbus.Subscribe[eventbus.ConversationSpeakEvent](
		s.bus,
		eventbus.TopicConversationSpeak,
		eventbus.WithSubscriptionName("notification_speak"),
		eventbus.WithSubscriptionBuffer(notificationQueue),
	)

	s.pipelineSub = eventbus.Subscribe[eventbus.PipelineMessageEvent](
		s.bus,
		eventbus.TopicPipelineCleaned,
		eventbus.WithSubscriptionName("notification_pipeline"),
		eventbus.WithSubscriptionBuffer(notificationQueue),
	)

	s.wg.Add(3)
	go s.consumeLifecycleEvents(derived)
	go s.consumeSpeakEvents(derived)
	go s.consumePipelineEvents(derived)

	return nil
}

// Shutdown cancels event consumers and waits for completion.
// Sets shuttingDown flag to suppress notifications triggered by daemon
// killing all sessions during graceful shutdown.
func (s *Service) Shutdown(ctx context.Context) error {
	s.shuttingDown.Store(true)
	if s.cancel != nil {
		s.cancel()
	}
	if s.lifecycleSub != nil {
		s.lifecycleSub.Close()
	}
	if s.speakSub != nil {
		s.speakSub.Close()
	}
	if s.pipelineSub != nil {
		s.pipelineSub.Close()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.wg.Wait()
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) consumeLifecycleEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.lifecycleSub, &s.wg, func(evt eventbus.SessionLifecycleEvent) {
		s.dispatchAsync(ctx, func() { s.handleLifecycleEvent(ctx, evt) })
	})
}

func (s *Service) consumeSpeakEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.speakSub, &s.wg, func(evt eventbus.ConversationSpeakEvent) {
		s.dispatchAsync(ctx, func() { s.handleSpeakEvent(ctx, evt) })
	})
}

func (s *Service) consumePipelineEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.pipelineSub, &s.wg, func(evt eventbus.PipelineMessageEvent) {
		s.dispatchAsync(ctx, func() { s.handlePipelineEvent(ctx, evt) })
	})
}

// dispatchAsync runs fn in a goroutine, bounded by sendSem to limit
// concurrent push notification IO.
func (s *Service) dispatchAsync(ctx context.Context, fn func()) {
	select {
	case s.sendSem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.sendSem }()
		fn()
	}()
}

func (s *Service) handleLifecycleEvent(ctx context.Context, evt eventbus.SessionLifecycleEvent) {
	if evt.State != eventbus.SessionStateStopped {
		return
	}

	// Suppress notifications during daemon shutdown to avoid spamming ERROR
	// notifications for every session being killed as part of graceful stop.
	if s.shuttingDown.Load() {
		return
	}

	// Use Label (command name) for user-facing text, fall back to session ID.
	displayName := evt.Label
	if displayName == "" {
		displayName = evt.SessionID
	}

	var eventType string
	var title string
	var body string

	if evt.ExitCode != nil && *evt.ExitCode == 0 {
		eventType = "TASK_COMPLETED"
		title = "Nupi: Task completed"
		body = fmt.Sprintf("Session '%s' has finished", displayName)
	} else {
		eventType = "ERROR"
		title = "Nupi: Session error"
		if evt.ExitCode != nil {
			body = fmt.Sprintf("Session '%s' exited with error (code %d)", displayName, *evt.ExitCode)
		} else {
			body = fmt.Sprintf("Session '%s' exited with error", displayName)
		}
	}

	s.sendNotification(ctx, evt.SessionID, eventType, title, body)
}

func (s *Service) handlePipelineEvent(ctx context.Context, evt eventbus.PipelineMessageEvent) {
	if s.shuttingDown.Load() {
		return
	}

	// Only process events marked as notable with an idle state from tool handlers.
	// The content pipeline sets these annotations when a tool handler detects
	// the session is waiting for user input (AC2: "idle via tool handler").
	if evt.Annotations["notable"] != "true" || evt.Annotations["idle_state"] == "" {
		return
	}

	waitingFor := evt.Annotations["waiting_for"]
	switch waitingFor {
	case "user_input", "confirmation", "choice":
		// Tool handler detected the session needs user attention.
	default:
		return
	}

	title := "Nupi: Input needed"
	body := evt.Annotations["prompt_text"]
	if body == "" {
		body = fmt.Sprintf("Session is waiting for %s", waitingFor)
	}

	s.sendNotification(ctx, evt.SessionID, "INPUT_NEEDED", title, body)
}

func (s *Service) handleSpeakEvent(ctx context.Context, evt eventbus.ConversationSpeakEvent) {
	if evt.Text == "" {
		return
	}

	// Only process notification-worthy speak events based on metadata type tag:
	// - "clarification" → INPUT_NEEDED (AI asking user a question)
	// - "error" → ERROR (intent router error)
	// - "completion" → TASK_COMPLETED (AI reporting task completion mid-session)
	// Regular TTS speak events (no "type" metadata) are ignored.
	speakType := evt.Metadata["type"]
	var eventType string
	switch speakType {
	case "clarification":
		eventType = "INPUT_NEEDED"
	case "error":
		eventType = "ERROR"
	case "completion":
		eventType = "TASK_COMPLETED"
	default:
		return
	}

	title := "Nupi: " + eventLabel(eventType)
	s.sendNotification(ctx, evt.SessionID, eventType, title, evt.Text)
}

func (s *Service) sendNotification(ctx context.Context, sessionID, eventType, title, body string) {
	dk := dedupKey(sessionID, eventType, body)
	if s.isDuplicate(dk) {
		return
	}

	tokens, err := s.store.ListPushTokensForEvent(ctx, eventType)
	if err != nil {
		log.Printf("[Notification] list tokens for event %q: %v", eventType, err)
		s.clearDedup(dk)
		return
	}
	if len(tokens) == 0 {
		s.clearDedup(dk)
		return
	}

	data := map[string]string{
		"sessionId": sessionID,
		"eventType": eventType,
	}
	if sessionID != "" {
		data["url"] = "/session/" + sessionID
	}

	messages := make([]ExpoMessage, 0, len(tokens))
	seenTokens := make(map[string]bool, len(tokens))
	for _, tok := range tokens {
		if seenTokens[tok.Token] {
			continue
		}
		seenTokens[tok.Token] = true
		messages = append(messages, ExpoMessage{
			To:        tok.Token,
			Title:     title,
			Body:      body,
			Sound:     "default",
			Priority:  "high",
			ChannelID: "nupi-session-events",
			Data:      data,
		})
	}

	result, err := s.expo.Send(ctx, messages)
	if err != nil {
		log.Printf("[Notification] send push: %v", err)
		// Don't mark as dedup — allow retry on next event.
		s.clearDedup(dk)
		return
	}

	// Auto-cleanup stale tokens.
	for _, staleToken := range result.DeviceNotRegistered {
		s.cleanupStaleToken(ctx, staleToken, tokens)
	}
}

// dedupKey builds a dedup map key. For session-scoped events the key is
// "sessionID:eventType". For sessionless events (empty sessionID) the
// truncated body is appended so that unrelated errors don't collide.
func dedupKey(sessionID, eventType, body string) string {
	if sessionID != "" {
		return sessionID + ":" + eventType
	}
	hint := body
	if len(hint) > 64 {
		hint = hint[:64]
	}
	return ":" + eventType + ":" + hint
}

func (s *Service) isDuplicate(key string) bool {
	s.dedupMu.Lock()
	defer s.dedupMu.Unlock()

	now := time.Now()
	if last, ok := s.dedup[key]; ok && now.Sub(last) < dedupWindow {
		return true
	}
	s.dedup[key] = now

	// Lazy cleanup of expired entries.
	if len(s.dedup) > 100 {
		for k, v := range s.dedup {
			if now.Sub(v) >= dedupWindow {
				delete(s.dedup, k)
			}
		}
	}

	return false
}

func (s *Service) clearDedup(key string) {
	s.dedupMu.Lock()
	delete(s.dedup, key)
	s.dedupMu.Unlock()
}

func (s *Service) cleanupStaleToken(ctx context.Context, token string, tokens []configstore.PushToken) {
	for _, pt := range tokens {
		if pt.Token == token {
			if err := s.store.DeletePushToken(ctx, pt.DeviceID); err != nil {
				log.Printf("[Notification] cleanup stale token for device %q: %v", pt.DeviceID, err)
			} else {
				log.Printf("[Notification] removed stale push token for device %q (DeviceNotRegistered)", pt.DeviceID)
			}
		}
	}
}

func eventLabel(eventType string) string {
	switch eventType {
	case "TASK_COMPLETED":
		return "Task completed"
	case "INPUT_NEEDED":
		return "Input needed"
	case "ERROR":
		return "Session error"
	default:
		return eventType
	}
}
