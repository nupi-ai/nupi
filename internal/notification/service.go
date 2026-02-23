package notification

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/sanitize"
)

const (
	dedupWindow           = constants.Duration30Seconds
	notificationQueue     = 64
	maxConcurrentSend     = 4
	maxNotifBodyBytes     = 2048
	dedupCleanupThreshold = 100
)

// TokenStore abstracts push token storage used by the notification service.
type TokenStore interface {
	ListPushTokensForEvent(ctx context.Context, eventType string) ([]configstore.PushToken, error)
	DeletePushToken(ctx context.Context, deviceID string) error
	DeletePushTokenIfMatch(ctx context.Context, deviceID, token string) (bool, error)
}

// Service subscribes to session lifecycle and conversation speak events,
// formats push notifications, and sends them via the Expo Push API.
type Service struct {
	store TokenStore
	bus   *eventbus.Bus
	expo  *ExpoClient

	lifecycle    eventbus.ServiceLifecycle
	asyncWG      sync.WaitGroup
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

	// Counters for observability (logged on shutdown).
	metricSent    atomic.Int64
	metricFailed  atomic.Int64
	metricDeduped atomic.Int64
	metricCleaned atomic.Int64
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
	s.lifecycle.Start(ctx)

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

	// M3/R5: Pipeline events are high-volume; a larger buffer reduces the risk
	// of dropping notable idle-state events under heavy terminal output.
	// Longer-term fix: introduce a dedicated topic so tool handlers publish
	// idle notifications directly instead of filtering the full pipeline stream.
	s.pipelineSub = eventbus.Subscribe[eventbus.PipelineMessageEvent](
		s.bus,
		eventbus.TopicPipelineCleaned,
		eventbus.WithSubscriptionName("notification_pipeline"),
		eventbus.WithSubscriptionBuffer(256),
	)

	s.lifecycle.AddSubscriptions(s.lifecycleSub, s.speakSub, s.pipelineSub)
	s.lifecycle.Go(s.consumeLifecycleEvents)
	s.lifecycle.Go(s.consumeSpeakEvents)
	s.lifecycle.Go(s.consumePipelineEvents)

	return nil
}

// Shutdown cancels event consumers and waits for completion.
// Sets shuttingDown flag to suppress notifications triggered by daemon
// killing all sessions during graceful shutdown.
func (s *Service) Shutdown(ctx context.Context) error {
	s.shuttingDown.Store(true)
	s.lifecycle.Stop()
	if err := s.lifecycle.Wait(ctx); err != nil {
		return err
	}
	err := eventbus.WaitForWorkers(ctx, &s.asyncWG)
	log.Printf("[Notification] shutdown: sent=%d failed=%d deduped=%d cleaned=%d",
		s.metricSent.Load(), s.metricFailed.Load(), s.metricDeduped.Load(), s.metricCleaned.Load())

	// Release dedup map memory now that all workers are done.
	s.dedupMu.Lock()
	clear(s.dedup)
	s.dedupMu.Unlock()

	return err
}

func (s *Service) consumeLifecycleEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.lifecycleSub, nil, func(evt eventbus.SessionLifecycleEvent) {
		receivedAt := time.Now()
		s.dispatchAsync(ctx, func() { s.handleLifecycleEvent(ctx, evt, receivedAt) })
	})
}

func (s *Service) consumeSpeakEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.speakSub, nil, func(evt eventbus.ConversationSpeakEvent) {
		receivedAt := time.Now()
		s.dispatchAsync(ctx, func() { s.handleSpeakEvent(ctx, evt, receivedAt) })
	})
}

func (s *Service) consumePipelineEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.pipelineSub, nil, func(evt eventbus.PipelineMessageEvent) {
		receivedAt := time.Now()
		s.dispatchAsync(ctx, func() { s.handlePipelineEvent(ctx, evt, receivedAt) })
	})
}

// dispatchAsync runs fn in a goroutine, bounded by sendSem to limit
// concurrent push notification IO. Panics in fn are recovered to prevent
// the notification service from crashing the daemon.
func (s *Service) dispatchAsync(ctx context.Context, fn func()) {
	select {
	case s.sendSem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	// Add to WaitGroup BEFORE launching goroutine so Shutdown's WaitForWorkers
	// cannot return zero while a goroutine is about to start (H1 race fix).
	s.asyncWG.Add(1)
	go func() {
		defer s.asyncWG.Done()
		defer func() { <-s.sendSem }()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[Notification] recovered panic in dispatch: %v", r)
			}
		}()
		fn()
	}()
}

func (s *Service) handleLifecycleEvent(ctx context.Context, evt eventbus.SessionLifecycleEvent, receivedAt time.Time) {
	if evt.State != eventbus.SessionStateStopped {
		return
	}

	// Skip events without a session ID — they lack context for a meaningful
	// push notification (no session URL, no session name).
	if evt.SessionID == "" {
		return
	}

	// Suppress notifications during daemon shutdown to avoid spamming ERROR
	// notifications for every session being killed as part of graceful stop.
	if s.shuttingDown.Load() {
		return
	}

	// Suppress notification for user-initiated kills (KillSession RPC).
	// The user already knows they killed the session.
	if evt.Reason == eventbus.SessionReasonKilled {
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
		eventType = constants.NotificationEventTaskCompleted
		title = "Nupi: Task completed"
		body = fmt.Sprintf("Session '%s' has finished", displayName)
	} else {
		eventType = constants.NotificationEventError
		title = "Nupi: Session error"
		if evt.ExitCode != nil {
			body = fmt.Sprintf("Session '%s' exited with error (code %d)", displayName, *evt.ExitCode)
		} else {
			body = fmt.Sprintf("Session '%s' exited with error", displayName)
		}
	}

	s.sendNotification(ctx, evt.SessionID, eventType, "lifecycle", title, body, receivedAt)
}

func (s *Service) handlePipelineEvent(ctx context.Context, evt eventbus.PipelineMessageEvent, receivedAt time.Time) {
	if s.shuttingDown.Load() {
		return
	}

	// Skip events without a session ID — they lack context for a meaningful
	// push notification (no session URL, no session name).
	if evt.SessionID == "" {
		return
	}

	// Only process events marked as notable with an idle state from tool handlers.
	// The content pipeline sets these annotations when a tool handler detects
	// the session is waiting for user input (AC2: "idle via tool handler").
	if evt.Annotations[constants.MetadataKeyNotable] != constants.MetadataValueTrue || evt.Annotations[constants.MetadataKeyIdleState] == "" {
		return
	}

	waitingFor := evt.Annotations[constants.MetadataKeyWaitingFor]
	switch waitingFor {
	case constants.PipelineWaitingForUserInput, constants.PipelineWaitingForConfirmation, constants.PipelineWaitingForChoice:
		// Tool handler detected the session needs user attention.
	default:
		return
	}

	title := "Nupi: Input needed"
	body := evt.Annotations[constants.MetadataKeyPromptText]
	if body == "" {
		body = fmt.Sprintf("Session is waiting for %s", waitingFor)
	}
	// L2 fix: strip terminal control sequences from tool handler prompt text
	// before sending as push notification (could contain raw terminal output).
	body = sanitize.StripControlChars(body)
	body = sanitize.TruncateUTF8(body, maxNotifBodyBytes)

	s.sendNotification(ctx, evt.SessionID, constants.NotificationEventInputNeeded, "pipeline", title, body, receivedAt)
}

func (s *Service) handleSpeakEvent(ctx context.Context, evt eventbus.ConversationSpeakEvent, receivedAt time.Time) {
	if evt.Text == "" {
		return
	}

	// Skip sessionless speak events — they lack the context needed for
	// a meaningful push notification (no session URL, no session name).
	if evt.SessionID == "" {
		return
	}

	// Suppress notifications during daemon shutdown.
	if s.shuttingDown.Load() {
		return
	}

	// Only process notification-worthy speak events based on metadata type tag:
	// - "clarification" → INPUT_NEEDED (AI asking user a question)
	// - "error" → ERROR (intent router error)
	// - "completion" → TASK_COMPLETED (AI reporting task completion mid-session)
	// Regular TTS speak events (no "type" metadata) are ignored.
	speakType := evt.Metadata[constants.SpeakMetadataTypeKey]
	var eventType string
	switch speakType {
	case constants.SpeakTypeClarification:
		eventType = constants.NotificationEventInputNeeded
	case constants.SpeakTypeError:
		eventType = constants.NotificationEventError
	case constants.SpeakTypeCompletion:
		eventType = constants.NotificationEventTaskCompleted
	default:
		return
	}

	title := "Nupi: " + eventLabel(eventType)
	body := sanitize.TruncateUTF8(evt.Text, maxNotifBodyBytes)
	s.sendNotification(ctx, evt.SessionID, eventType, "speak", title, body, receivedAt)
}

func (s *Service) sendNotification(ctx context.Context, sessionID, eventType, source, title, body string, receivedAt time.Time) {
	dk := dedupKey(sessionID, eventType, source, body)
	if s.isDuplicate(dk, receivedAt) {
		s.metricDeduped.Add(1)
		return
	}

	// Bound store queries to prevent a busy SQLite lock from blocking a
	// semaphore slot for the full service-level context lifetime.
	storeCtx, storeCancel := context.WithTimeout(ctx, constants.Duration3Seconds)
	defer storeCancel()

	tokens, err := s.store.ListPushTokensForEvent(storeCtx, eventType)
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
		"url":       "/session/" + url.PathEscape(sessionID),
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
		s.metricFailed.Add(int64(len(messages)))
		log.Printf("[Notification] send push: %v", err)
		// Don't mark as dedup — allow retry on next event.
		s.clearDedup(dk)
		return
	}

	// H3/R5 fix: count only successful tickets, not total messages sent.
	// Individual tickets can report errors (e.g., DeviceNotRegistered)
	// even when the batch request succeeds.
	var okCount int64
	for _, t := range result.Tickets {
		if t.Status == "ok" {
			okCount++
		}
	}
	s.metricSent.Add(okCount)
	if errCount := int64(len(messages)) - okCount; errCount > 0 {
		s.metricFailed.Add(errCount)
	}

	// M1 fix: if all tickets failed (e.g., rate-limited), clear dedup so the
	// notification can be retried on the next event instead of being blocked
	// for the full 30s dedup window.
	if okCount == 0 && len(messages) > 0 {
		s.clearDedup(dk)
	}

	// Auto-cleanup stale tokens.
	for _, staleToken := range result.DeviceNotRegistered {
		s.cleanupStaleToken(ctx, staleToken, tokens)
	}
}

// dedupKey builds a dedup map key. The source tag (e.g., "lifecycle",
// "speak", "pipeline") prevents collisions between different event sources
// that emit the same event type for the same session.
// For sessionless events (empty sessionID) the truncated body is also
// appended so that unrelated errors don't collide.
func dedupKey(sessionID, eventType, source, body string) string {
	if sessionID != "" {
		return sessionID + ":" + eventType + ":" + source
	}
	hint := sanitize.TruncateUTF8(body, 64)
	return ":" + eventType + ":" + source + ":" + hint
}

// isDuplicate checks whether a notification with the given key was already
// sent within the dedup window. receivedAt is the event receipt time (captured
// synchronously in the consume callback) so that dedup timing is not skewed
// by async dispatch delays.
func (s *Service) isDuplicate(key string, receivedAt time.Time) bool {
	s.dedupMu.Lock()
	defer s.dedupMu.Unlock()

	if last, ok := s.dedup[key]; ok && receivedAt.Sub(last) < dedupWindow {
		return true
	}
	s.dedup[key] = receivedAt

	// Lazy cleanup of expired entries.
	now := time.Now()
	if len(s.dedup) > dedupCleanupThreshold {
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

func (s *Service) cleanupStaleToken(ctx context.Context, staleToken string, tokens []configstore.PushToken) {
	// Bound the store delete to prevent a busy SQLite lock from blocking
	// a semaphore slot for the full service-level context lifetime.
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, constants.Duration3Seconds)
	defer cleanupCancel()

	for _, pt := range tokens {
		if pt.Token != staleToken {
			continue
		}
		// M6 fix: use DeletePushTokenIfMatch to only delete when the stored
		// token still matches the stale value. This prevents removing a freshly
		// re-registered valid token if a concurrent RegisterPushToken RPC ran.
		deleted, err := s.store.DeletePushTokenIfMatch(cleanupCtx, pt.DeviceID, staleToken)
		if err != nil {
			log.Printf("[Notification] cleanup stale token for device %q: %v", pt.DeviceID, err)
		} else if deleted {
			s.metricCleaned.Add(1)
			log.Printf("[Notification] removed stale push token for device %q (DeviceNotRegistered)", pt.DeviceID)
		}
	}
}

func eventLabel(eventType string) string {
	switch eventType {
	case constants.NotificationEventTaskCompleted:
		return "Task completed"
	case constants.NotificationEventInputNeeded:
		return "Input needed"
	case constants.NotificationEventError:
		return "Session error"
	default:
		return eventType
	}
}
