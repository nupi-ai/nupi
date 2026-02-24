package notification

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/sanitize"
)

const (
	defaultDedupWindow           = constants.Duration30Seconds
	defaultNotificationQueue     = 64
	defaultMaxConcurrentSend     = 4
	maxNotifBodyBytes            = 2048
	dedupCleanupThreshold        = 100
	defaultDedupHardCap          = 500
	defaultExpoSendTimeout       = 8 * time.Second
)

// TokenStore abstracts push token storage used by the notification service.
type TokenStore interface {
	ListPushTokensForEvent(ctx context.Context, eventType string) ([]configstore.PushToken, error)
	DeletePushToken(ctx context.Context, deviceID string) error
	DeletePushTokenIfMatch(ctx context.Context, deviceID, token string) (bool, error)
}

// ServiceOption configures a Service.
type ServiceOption func(*Service)

// WithDedupWindow overrides the deduplication window (default 30s).
func WithDedupWindow(d time.Duration) ServiceOption {
	return func(s *Service) { s.dedupWindow = d }
}

// WithMaxConcurrentSend overrides the semaphore limit (default 4).
func WithMaxConcurrentSend(n int) ServiceOption {
	return func(s *Service) { s.maxConcurrentSend = n }
}

// WithDedupHardCap overrides the hard cap on dedup entries (default 500).
func WithDedupHardCap(n int) ServiceOption {
	return func(s *Service) { s.dedupHardCap = n }
}

// WithExpoSendTimeout overrides the per-batch send timeout (default 8s).
func WithExpoSendTimeout(d time.Duration) ServiceOption {
	return func(s *Service) { s.expoSendTimeout = d }
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

	// Configurable parameters (set via ServiceOption or defaults).
	dedupWindow      time.Duration
	maxConcurrentSend int
	dedupHardCap     int
	expoSendTimeout  time.Duration
}

// NewService creates a notification service.
func NewService(store TokenStore, bus *eventbus.Bus, expoOpts []ExpoClientOption, svcOpts ...ServiceOption) *Service {
	s := &Service{
		store:            store,
		bus:              bus,
		expo:             NewExpoClient(expoOpts...),
		dedup:            make(map[string]time.Time),
		dedupWindow:      defaultDedupWindow,
		maxConcurrentSend: defaultMaxConcurrentSend,
		dedupHardCap:     defaultDedupHardCap,
		expoSendTimeout:  defaultExpoSendTimeout,
	}
	for _, opt := range svcOpts {
		opt(s)
	}
	s.sendSem = make(chan struct{}, s.maxConcurrentSend)
	return s
}

// Start subscribes to event bus topics and begins consuming events.
func (s *Service) Start(ctx context.Context) error {
	s.lifecycle.Start(ctx)

	s.lifecycleSub = eventbus.Subscribe[eventbus.SessionLifecycleEvent](
		s.bus,
		eventbus.TopicSessionsLifecycle,
		eventbus.WithSubscriptionName("notification_lifecycle"),
		eventbus.WithSubscriptionBuffer(defaultNotificationQueue),
	)

	s.speakSub = eventbus.Subscribe[eventbus.ConversationSpeakEvent](
		s.bus,
		eventbus.TopicConversationSpeak,
		eventbus.WithSubscriptionName("notification_speak"),
		eventbus.WithSubscriptionBuffer(defaultNotificationQueue),
	)

	// Pipeline events are high-volume; a larger buffer reduces the risk
	// of dropping notable idle-state events under heavy terminal output.
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
	// Add to WaitGroup before launching the goroutine so Shutdown does not
	// finish while a worker is about to start.
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
	// Strip terminal control sequences from tool handler prompt text.
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
	// Strip control chars before truncating; speak messages may contain ANSI codes.
	body := sanitize.StripControlChars(evt.Text)
	body = sanitize.TruncateUTF8(body, maxNotifBodyBytes)
	s.sendNotification(ctx, evt.SessionID, eventType, "speak", title, body, receivedAt)
}

func (s *Service) sendNotification(ctx context.Context, sessionID, eventType, source, title, body string, receivedAt time.Time) {
	dk := dedupKey(sessionID, eventType, source, body)
	if s.isDuplicate(dk, receivedAt) {
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

	// Bound the Expo API send to prevent slow responses from blocking semaphore
	// slots for the full service-level context lifetime. With maxConcurrentSend=4
	// and a 10s HTTP timeout, all slots could be blocked for 10s without this.
	sendCtx, sendCancel := context.WithTimeout(ctx, s.expoSendTimeout)
	defer sendCancel()
	result, err := s.expo.Send(sendCtx, messages)
	if err != nil {
		// On partial batch failure, keep successful tickets from earlier batches.
		var partialOK int64
		for _, t := range result.Tickets {
			if t.Status == "ok" {
				partialOK++
			}
		}
		log.Printf("[Notification] send push: %v (partial ok=%d)", err, partialOK)
		// Process stale token cleanup from partial results.
		s.cleanupStaleTokens(ctx, result.DeviceNotRegistered, tokens)
		// Don't mark as dedup — allow retry on next event.
		s.clearDedup(dk)
		return
	}

	// Count only successful tickets; a successful batch can still contain
	// individual ticket errors (for example DeviceNotRegistered).
	var okCount int64
	for _, t := range result.Tickets {
		if t.Status == "ok" {
			okCount++
		}
	}

	// If all tickets failed, clear dedup so the notification can be retried
	// on the next event instead of waiting for the full dedup window.
	if okCount == 0 && len(messages) > 0 {
		s.clearDedup(dk)
	}

	// Auto-cleanup stale tokens.
	s.cleanupStaleTokens(ctx, result.DeviceNotRegistered, tokens)
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
//
// Check-and-set is atomic under dedupMu. Two concurrent events with the same
// key cannot both pass. The window between isDuplicate returning false and
// clearDedup on send failure is intentional while send is in progress.
func (s *Service) isDuplicate(key string, receivedAt time.Time) bool {
	s.dedupMu.Lock()
	defer s.dedupMu.Unlock()

	if last, ok := s.dedup[key]; ok && receivedAt.Sub(last) < s.dedupWindow {
		return true
	}
	s.dedup[key] = receivedAt

	// Periodic cleanup of expired entries. Runs every dedupCleanupThreshold
	// inserts to amortize the cost of a full map scan under mutex.
	// Hard cap prevents unbounded growth even if all entries are within the window.
	if len(s.dedup) > dedupCleanupThreshold {
		now := time.Now()
		for k, v := range s.dedup {
			if now.Sub(v) >= s.dedupWindow {
				delete(s.dedup, k)
			}
		}
		// If TTL cleanup didn't bring us below the hard cap, evict the oldest
		// half of entries. This preserves recent dedup timestamps (avoiding a
		// window where all dedup protection is lost) while bounding memory.
		if len(s.dedup) > s.dedupHardCap {
			type entry struct {
				key string
				ts  time.Time
			}
			entries := make([]entry, 0, len(s.dedup))
			for k, v := range s.dedup {
				entries = append(entries, entry{k, v})
			}
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].ts.Before(entries[j].ts)
			})
			evictCount := len(entries) / 2
			for i := 0; i < evictCount; i++ {
				delete(s.dedup, entries[i].key)
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

// cleanupStaleTokens removes all stale tokens from the store in a single pass.
func (s *Service) cleanupStaleTokens(ctx context.Context, staleTokens []string, tokens []configstore.PushToken) {
	if len(staleTokens) == 0 {
		return
	}
	// Bound the store deletes to prevent a busy SQLite lock from blocking
	// a semaphore slot for the full service-level context lifetime.
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, constants.Duration3Seconds)
	defer cleanupCancel()

	staleSet := make(map[string]bool, len(staleTokens))
	for _, t := range staleTokens {
		staleSet[t] = true
	}

	for _, pt := range tokens {
		if !staleSet[pt.Token] {
			continue
		}
		// Delete only when the stored token still matches the stale value.
		deleted, err := s.store.DeletePushTokenIfMatch(cleanupCtx, pt.DeviceID, pt.Token)
		if err != nil {
			log.Printf("[Notification] cleanup stale token for device %q: %v", pt.DeviceID, err)
		} else if deleted {
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
