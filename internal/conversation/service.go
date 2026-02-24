package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/sanitize"
)

type summaryRequest struct {
	promptID string
	count    int // number of turns being summarized
	firstAt  time.Time
	timer    *time.Timer
}

type flushRequest struct {
	oldest    []eventbus.ConversationTurn
	batchSize int
	timer     *time.Timer
}

type Option func(*Service)

// WithHistoryLimit overrides the maximum number of stored turns per session.
func WithHistoryLimit(limit int) Option {
	return func(s *Service) {
		if limit > 0 {
			s.maxHistory = limit
		}
	}
}

// WithDetachTTL overrides the timeout after which detached sessions are purged.
func WithDetachTTL(ttl time.Duration) Option {
	return func(s *Service) {
		if ttl >= 0 {
			s.detachTTL = ttl
		}
	}
}

// WithSummaryThreshold overrides when history summarization triggers.
func WithSummaryThreshold(threshold int) Option {
	return func(s *Service) {
		if threshold > 0 {
			s.summaryThreshold = threshold
		}
	}
}

// WithSummaryBatchSize overrides how many oldest turns are summarized.
func WithSummaryBatchSize(size int) Option {
	return func(s *Service) {
		if size > 0 {
			s.summaryBatchSize = size
		}
	}
}

// WithFlushTimeout overrides the timeout for memory flush before compaction.
func WithFlushTimeout(timeout time.Duration) Option {
	return func(s *Service) {
		if timeout > 0 {
			s.flushTimeout = timeout
		}
	}
}

// WithGlobalStore enables global conversation history for sessionless messages.
func WithGlobalStore(store *GlobalStore) Option {
	return func(s *Service) {
		s.globalStore = store
	}
}

// Service maintains conversation context and emits prompts for AI adapters.
type Service struct {
	bus *eventbus.Bus

	maxHistory  int
	detachTTL   time.Duration
	globalStore *GlobalStore

	mu        sync.RWMutex
	sessions  map[string][]eventbus.ConversationTurn
	sessionAI map[string]bool // tracks whether session ever had an AI turn (survives FIFO trim)
	detach    map[string]*time.Timer
	toolCache map[string]string // sessionID -> current tool name

	// Rate limiting for SESSION_OUTPUT events
	lastSessionOutput sync.Map // sessionID -> time.Time

	// History summarization
	pendingSummary   sync.Map // sessionID -> *summaryRequest
	summaryThreshold int      // trigger summarization when len(history) >= this
	summaryBatchSize int      // number of oldest turns to summarize

	// Memory flush before compaction
	pendingFlush     sync.Map // sessionID -> *flushRequest
	flushTimeout     time.Duration
	flushResponseSub *eventbus.TypedSubscription[eventbus.MemoryFlushResponseEvent]
	shuttingDown     atomic.Bool

	lifecycle     eventbus.ServiceLifecycle
	cleanedSub    *eventbus.TypedSubscription[eventbus.PipelineMessageEvent]
	lifecycleSub  *eventbus.TypedSubscription[eventbus.SessionLifecycleEvent]
	replySub      *eventbus.TypedSubscription[eventbus.ConversationReplyEvent]
	bargeSub      *eventbus.TypedSubscription[eventbus.SpeechBargeInEvent]
	toolSub       *eventbus.TypedSubscription[eventbus.SessionToolEvent]
	toolChangeSub *eventbus.TypedSubscription[eventbus.SessionToolChangedEvent]
}

const (
	defaultHistory   = 50
	defaultDetachTTL = constants.Duration5Minutes

	defaultSummaryThreshold = 35
	defaultSummaryBatchSize = 20
	summaryTimeout          = constants.Duration60Seconds

	maxMetadataEntries    = sanitize.DefaultMetadataMaxEntries
	maxMetadataKeyRunes   = sanitize.DefaultMetadataMaxKeyRunes
	maxMetadataValueRunes = sanitize.DefaultMetadataMaxValueRunes
	maxMetadataTotalBytes = sanitize.DefaultMetadataMaxTotalBytes

	// Rate limiting for SESSION_OUTPUT events
	sessionOutputMinInterval = constants.Duration2Seconds
)

var conversationMetadataLimits = sanitize.MetadataLimits{
	MaxEntries:    maxMetadataEntries,
	MaxKeyRunes:   maxMetadataKeyRunes,
	MaxValueRunes: maxMetadataValueRunes,
	MaxTotalBytes: maxMetadataTotalBytes,
}

// NewService creates a conversation service bound to the provided bus.
func NewService(bus *eventbus.Bus, opts ...Option) *Service {
	svc := &Service{
		bus:              bus,
		maxHistory:       defaultHistory,
		detachTTL:        defaultDetachTTL,
		summaryThreshold: defaultSummaryThreshold,
		summaryBatchSize: defaultSummaryBatchSize,
		flushTimeout:     eventbus.DefaultFlushTimeout,
		sessions:         make(map[string][]eventbus.ConversationTurn),
		sessionAI:        make(map[string]bool),
		detach:           make(map[string]*time.Timer),
		toolCache:        make(map[string]string),
	}
	for _, opt := range opts {
		opt(svc)
	}
	// Ensure summaryThreshold <= maxHistory so FIFO truncation doesn't prevent triggering
	if svc.summaryThreshold > svc.maxHistory {
		svc.summaryThreshold = svc.maxHistory
	}
	// Ensure summaryBatchSize < summaryThreshold to prevent infinite summarization loops
	if svc.summaryBatchSize >= svc.summaryThreshold {
		svc.summaryBatchSize = svc.summaryThreshold / 2
		if svc.summaryBatchSize < 1 {
			svc.summaryBatchSize = 1
		}
	}
	return svc
}

// Start subscribes to pipeline cleaned and lifecycle events.
func (s *Service) Start(ctx context.Context) error {
	if s.bus == nil {
		return nil
	}

	// Reset shutdown flag so timer callbacks work correctly after a
	// Shutdown → Start restart cycle.
	s.shuttingDown.Store(false)

	s.lifecycle.Start(ctx)

	s.cleanedSub = eventbus.Subscribe[eventbus.PipelineMessageEvent](s.bus, eventbus.TopicPipelineCleaned, eventbus.WithSubscriptionName("conversation_pipeline"))
	s.lifecycleSub = eventbus.Subscribe[eventbus.SessionLifecycleEvent](s.bus, eventbus.TopicSessionsLifecycle, eventbus.WithSubscriptionName("conversation_lifecycle"))
	s.replySub = eventbus.Subscribe[eventbus.ConversationReplyEvent](s.bus, eventbus.TopicConversationReply, eventbus.WithSubscriptionName("conversation_reply"))
	s.bargeSub = eventbus.Subscribe[eventbus.SpeechBargeInEvent](s.bus, eventbus.TopicSpeechBargeIn, eventbus.WithSubscriptionName("conversation_barge"))
	s.toolSub = eventbus.Subscribe[eventbus.SessionToolEvent](s.bus, eventbus.TopicSessionsTool, eventbus.WithSubscriptionName("conversation_tool"))
	s.toolChangeSub = eventbus.Subscribe[eventbus.SessionToolChangedEvent](s.bus, eventbus.TopicSessionsToolChanged, eventbus.WithSubscriptionName("conversation_tool_changed"))
	s.flushResponseSub = eventbus.Subscribe[eventbus.MemoryFlushResponseEvent](s.bus, eventbus.TopicMemoryFlushResponse, eventbus.WithSubscriptionName("conversation_flush_response"))

	s.lifecycle.AddSubscriptions(s.cleanedSub, s.lifecycleSub, s.replySub, s.bargeSub, s.toolSub, s.toolChangeSub, s.flushResponseSub)
	s.lifecycle.Go(s.consumePipeline)
	s.lifecycle.Go(s.consumeLifecycle)
	s.lifecycle.Go(s.consumeReplies)
	s.lifecycle.Go(s.consumeBarge)
	s.lifecycle.Go(s.consumeToolEvents)
	s.lifecycle.Go(s.consumeToolChangeEvents)
	s.lifecycle.Go(s.consumeFlushResponses)
	return nil
}

// Shutdown stops background goroutines.
func (s *Service) Shutdown(ctx context.Context) error {
	// Signal timer callbacks to bail out early, preventing log noise
	// and publishes to a draining bus after Shutdown returns.
	s.shuttingDown.Store(true)

	// Close subscriptions first to unblock consumer goroutines.
	// This ensures in-flight message processing completes before
	// goroutines exit (matches awareness service shutdown pattern).
	s.lifecycle.Stop()

	// Stop all pending summary timers to prevent goroutine leaks
	s.pendingSummary.Range(func(key, val any) bool {
		if req, ok := val.(*summaryRequest); ok && req.timer != nil {
			req.timer.Stop()
		}
		s.pendingSummary.Delete(key)
		return true
	})

	// Stop all pending flush timers
	s.pendingFlush.Range(func(key, val any) bool {
		if flush, ok := val.(*flushRequest); ok && flush.timer != nil {
			flush.timer.Stop()
		}
		s.pendingFlush.Delete(key)
		return true
	})

	return s.lifecycle.Wait(ctx)
}

func (s *Service) consumePipeline(ctx context.Context) {
	eventbus.ConsumeEnvelope(ctx, s.cleanedSub, nil, func(env eventbus.TypedEnvelope[eventbus.PipelineMessageEvent]) {
		s.handlePipelineMessage(env.Timestamp, env.Payload)
	})
}

func (s *Service) consumeLifecycle(ctx context.Context) {
	eventbus.Consume(ctx, s.lifecycleSub, nil, func(msg eventbus.SessionLifecycleEvent) {
		switch msg.State {
		case eventbus.SessionStateStopped:
			s.clearSession(msg.SessionID)
		case eventbus.SessionStateDetached:
			// Clear tool cache immediately to prevent stale current_tool injection
			// (conversation history is still kept until detachTTL expires)
			s.clearToolCache(msg.SessionID)
			s.scheduleDetachCleanup(msg.SessionID)
		case eventbus.SessionStateRunning, eventbus.SessionStateCreated:
			s.cancelDetachCleanup(msg.SessionID)
		}
	})
}

func (s *Service) consumeReplies(ctx context.Context) {
	eventbus.ConsumeEnvelope(ctx, s.replySub, nil, func(env eventbus.TypedEnvelope[eventbus.ConversationReplyEvent]) {
		s.handleReplyMessage(env.Timestamp, env.Payload)
	})
}

func (s *Service) consumeBarge(ctx context.Context) {
	eventbus.Consume(ctx, s.bargeSub, nil, s.handleBargeEvent)
}

func (s *Service) consumeToolEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.toolSub, nil, func(event eventbus.SessionToolEvent) {
		s.updateToolCache(event.SessionID, event.ToolName)
	})
}

func (s *Service) consumeToolChangeEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.toolChangeSub, nil, func(event eventbus.SessionToolChangedEvent) {
		s.updateToolCache(event.SessionID, event.NewTool)
	})
}

func (s *Service) updateToolCache(sessionID, toolName string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	s.toolCache[sessionID] = toolName
	s.mu.Unlock()
}

func (s *Service) handlePipelineMessage(ts time.Time, msg eventbus.PipelineMessageEvent) {
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	meta := sanitize.NewMetadataAccumulator(nil, conversationMetadataLimits)
	meta.Merge(msg.Annotations)

	// Determine event type BEFORE creating turn (to avoid mutating Meta after it's in history)
	shouldTriggerAI := false
	eventType := constants.PromptEventUserIntent

	if msg.Origin == eventbus.OriginUser {
		shouldTriggerAI = true
		eventType = constants.PromptEventUserIntent
	} else if msg.Annotations[constants.MetadataKeyNotable] == constants.MetadataValueTrue {
		// Tool output marked as notable by content pipeline
		// Note: rate limiting check is deferred until after we have sessionID context
		eventType = constants.PromptEventSessionOutput
	}

	// Set event_type in meta BEFORE turn is added to history (prevents race condition)
	if shouldTriggerAI || msg.Annotations[constants.MetadataKeyNotable] == constants.MetadataValueTrue {
		meta.Merge(map[string]string{constants.MetadataKeyEventType: eventType})
	}

	turn := eventbus.ConversationTurn{
		Origin: msg.Origin,
		Text:   msg.Text,
		At:     ts,
		Meta:   meta.Result(),
	}

	// Handle sessionless ("bezpańskie") messages via GlobalStore
	if msg.SessionID == "" {
		if s.globalStore != nil {
			contextSnapshot := s.globalStore.GetContext()
			s.globalStore.AddTurn(turn)
			if msg.Origin == eventbus.OriginUser {
				s.publishPrompt("", contextSnapshot, turn)
			}
		}
		return
	}

	s.cancelDetachCleanup(msg.SessionID)

	// Check rate limiting for session_output (needs sessionID, so done here)
	if eventType == constants.PromptEventSessionOutput {
		shouldTriggerAI = s.shouldTriggerSessionOutput(msg.SessionID)
	}

	var contextSnapshot []eventbus.ConversationTurn

	s.mu.Lock()
	history := s.sessions[msg.SessionID]
	contextSnapshot = append(contextSnapshot, history...)

	history = append(history, turn)
	sort.SliceStable(history, func(i, j int) bool { return history[i].At.Before(history[j].At) })
	if len(history) > s.maxHistory {
		history = history[len(history)-s.maxHistory:]
	}
	s.sessions[msg.SessionID] = history
	historyLen := len(history)
	s.mu.Unlock()

	if shouldTriggerAI {
		s.publishPrompt(msg.SessionID, contextSnapshot, turn)
	}

	// Trigger history summarization if threshold reached
	if historyLen >= s.summaryThreshold {
		s.requestSummary(msg.SessionID)
	}
}

// shouldTriggerSessionOutput checks rate limiting for SESSION_OUTPUT events.
func (s *Service) shouldTriggerSessionOutput(sessionID string) bool {
	now := time.Now()

	if last, ok := s.lastSessionOutput.Load(sessionID); ok {
		if lastTime, ok := last.(time.Time); ok {
			if now.Sub(lastTime) < sessionOutputMinInterval {
				return false // Too soon
			}
		}
	}

	s.lastSessionOutput.Store(sessionID, now)
	return true
}

// validEventTypes are the event types supported by the prompts engine.
var validEventTypes = boolSet(constants.PromptEventTypes)

func boolSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func (s *Service) publishPrompt(sessionID string, ctxTurns []eventbus.ConversationTurn, newTurn eventbus.ConversationTurn) {
	if s.bus == nil {
		return
	}

	// Build Metadata from turn annotations (propagate pipeline annotations)
	// Extended whitelist to include pipeline metadata needed by AI adapters
	metadata := make(map[string]string)
	for key, value := range newTurn.Meta {
		// Propagate relevant annotations to prompt metadata
		switch key {
		// Core event metadata
		case constants.MetadataKeyTool, constants.MetadataKeyToolID, constants.MetadataKeyConfidence, constants.MetadataKeyStreamID, constants.MetadataKeyEventType,
			constants.MetadataKeySessionOutput, constants.MetadataKeyClarificationQuestion, constants.MetadataKeySeverity:
			metadata[key] = value
		// Extended pipeline metadata (input source, mode, annotations)
		case constants.MetadataKeyInputSource, constants.MetadataKeyMode, constants.MetadataKeySessionless, constants.MetadataKeyAdapter, constants.MetadataKeyLanguage,
			constants.MetadataKeyModel, constants.MetadataKeyProvider, constants.MetadataKeyPriority, constants.MetadataKeyContextWindow:
			metadata[key] = value
		// SESSION_OUTPUT specific metadata (idle detection, events, buffer status)
		case constants.MetadataKeyNotable, constants.MetadataKeyIdleState, constants.MetadataKeyWaitingFor, constants.MetadataKeyPromptText,
			constants.MetadataKeyEventCount, constants.MetadataKeyEventTitle, constants.MetadataKeyEventDetails, constants.MetadataKeyEventAction,
			constants.MetadataKeySummarized, constants.MetadataKeyOriginalLength,
			constants.MetadataKeyBufferTruncated, constants.MetadataKeyBufferMaxSize,
			constants.MetadataKeyToolChanged:
			metadata[key] = value
		default:
			// Language metadata (nupi.lang.*) is propagated unconditionally
			// so that every adapter receives the client's language preference.
			if strings.HasPrefix(key, constants.MetadataKeyLanguagePrefix) {
				metadata[key] = value
			}
		}
	}

	// Add current_tool from our tool cache if not already in metadata
	if sessionID != "" {
		s.mu.RLock()
		if tool, ok := s.toolCache[sessionID]; ok && tool != "" {
			if _, exists := metadata[constants.MetadataKeyCurrentTool]; !exists {
				metadata[constants.MetadataKeyCurrentTool] = tool
			}
			if _, exists := metadata[constants.MetadataKeyTool]; !exists {
				metadata[constants.MetadataKeyTool] = tool
			}
		}
		s.mu.RUnlock()
	}

	// Validate and set event_type
	if eventType, exists := metadata[constants.MetadataKeyEventType]; exists {
		if !validEventTypes[eventType] {
			// Invalid event_type, reset to default
			metadata[constants.MetadataKeyEventType] = constants.PromptEventUserIntent
		}
		// For session_output events, populate session_output field with the turn text
		// This is what the prompts engine template expects via {{.session_output}}
		if eventType == constants.PromptEventSessionOutput {
			metadata[constants.MetadataKeySessionOutput] = newTurn.Text
		}
	} else {
		metadata[constants.MetadataKeyEventType] = constants.PromptEventUserIntent
	}

	prompt := eventbus.ConversationPromptEvent{
		SessionID: sessionID,
		PromptID:  uuid.NewString(),
		Context:   ctxTurns,
		NewMessage: eventbus.ConversationMessage{
			Origin: newTurn.Origin,
			Text:   newTurn.Text,
			At:     newTurn.At,
			Meta:   newTurn.Meta,
		},
		Metadata: metadata,
	}

	eventbus.Publish(context.Background(), s.bus, eventbus.Conversation.Prompt, eventbus.SourceConversation, prompt)
}

func (s *Service) clearSession(sessionID string) {
	if sessionID == "" {
		return
	}

	// Collect export data under lock, then publish outside the lock
	// to avoid holding s.mu during event bus dispatch.
	var exportEvent *eventbus.SessionExportRequestEvent

	s.mu.Lock()

	// Capture session export request before clearing turns so the
	// awareness service can generate a session summary markdown file.
	// Guard with shuttingDown to avoid publishing to a draining bus
	// when detach cleanup timers fire after Shutdown.
	// Only export sessions with actual AI interaction (AC#4): sessionAI
	// flag survives FIFO history trimming (unlike scanning the buffer).
	if turns := s.sessions[sessionID]; s.bus != nil && !s.shuttingDown.Load() && s.sessionAI[sessionID] && len(turns) > 0 {
		exportTurns := turns
		if len(exportTurns) > 30 {
			exportTurns = exportTurns[len(exportTurns)-30:]
		}
		// Deep copy turns including Meta maps to prevent shared references
		// after the session entry is deleted from s.sessions.
		copied := make([]eventbus.ConversationTurn, len(exportTurns))
		for i, turn := range exportTurns {
			copied[i] = turn
			if turn.Meta != nil {
				copied[i].Meta = maps.Clone(turn.Meta)
			}
		}
		exportEvent = &eventbus.SessionExportRequestEvent{
			SessionID: sessionID,
			Turns:     copied,
		}
	}

	delete(s.sessions, sessionID)
	delete(s.sessionAI, sessionID)
	delete(s.toolCache, sessionID)
	if timer, ok := s.detach[sessionID]; ok {
		timer.Stop()
		delete(s.detach, sessionID)
	}
	s.mu.Unlock()

	// Publish synchronously outside the lock (no untracked goroutine).
	if exportEvent != nil {
		eventbus.Publish(context.Background(), s.bus, eventbus.Memory.ExportRequest, eventbus.SourceConversation, *exportEvent)
	}

	// Clean up rate limiting entry to prevent memory leaks
	s.lastSessionOutput.Delete(sessionID)

	// Clean up pending summary state
	if val, ok := s.pendingSummary.LoadAndDelete(sessionID); ok {
		if req, ok := val.(*summaryRequest); ok && req.timer != nil {
			req.timer.Stop()
		}
	}

	// Clean up pending flush state
	if val, ok := s.pendingFlush.LoadAndDelete(sessionID); ok {
		if flush, ok := val.(*flushRequest); ok && flush.timer != nil {
			flush.timer.Stop()
		}
	}
}

// requestSummary triggers an async history summarization if the threshold is met
// and no summary or flush is already pending for the session.
// Step 1: publishes a MemoryFlushRequestEvent so the awareness service can
// extract important context before the turns are compacted.
// Step 2: when the flush response arrives (or times out), proceedWithSummary
// sends the actual history_summary prompt.
func (s *Service) requestSummary(sessionID string) {
	if sessionID == "" || s.bus == nil {
		return
	}

	// Idempotency: check if a summary or flush is already pending for this session
	if _, loaded := s.pendingSummary.Load(sessionID); loaded {
		return
	}
	if _, loaded := s.pendingFlush.Load(sessionID); loaded {
		return
	}

	s.mu.RLock()
	history := s.sessions[sessionID]
	histLen := len(history)
	if histLen < s.summaryThreshold {
		s.mu.RUnlock()
		return
	}

	batchSize := s.summaryBatchSize
	if batchSize > histLen {
		batchSize = histLen
	}

	// Copy the oldest N turns for serialization
	oldest := make([]eventbus.ConversationTurn, batchSize)
	copy(oldest, history[:batchSize])
	s.mu.RUnlock()

	// Request memory flush before compaction.
	flush := &flushRequest{
		oldest:    oldest,
		batchSize: batchSize,
	}

	// Set timer before storing so the struct is fully initialized before
	// being exposed to other goroutines via the map. The theoretical risk
	// of the timer firing before LoadOrStore is negligible with 30s timeouts
	// and handled gracefully: LoadAndDelete returns false (no-op), and the
	// awareness service's own timeout serves as the safety net.
	flush.timer = time.AfterFunc(s.flushTimeout, func() {
		// Guard: skip if the service is shutting down to prevent
		// publishing to a draining bus after Shutdown returns.
		if s.shuttingDown.Load() {
			s.pendingFlush.Delete(sessionID)
			return
		}
		// Timeout: proceed with summary without waiting for flush.
		if _, loaded := s.pendingFlush.LoadAndDelete(sessionID); loaded {
			s.proceedWithSummary(sessionID, oldest, batchSize)
		}
	})

	// Atomically store only if no other goroutine stored first.
	if _, loaded := s.pendingFlush.LoadOrStore(sessionID, flush); loaded {
		flush.timer.Stop()
		return
	}

	eventbus.Publish(context.Background(), s.bus, eventbus.Memory.FlushRequest, eventbus.SourceConversation, eventbus.MemoryFlushRequestEvent{
		SessionID: sessionID,
		Turns:     oldest,
	})
}

// proceedWithSummary sends the history_summary prompt to the AI adapter.
// Called after the memory flush completes or times out.
func (s *Service) proceedWithSummary(sessionID string, oldest []eventbus.ConversationTurn, batchSize int) {
	if len(oldest) == 0 {
		return
	}

	promptText := eventbus.SerializeTurns(oldest)
	promptID := uuid.NewString()

	req := &summaryRequest{
		promptID: promptID,
		count:    batchSize,
		firstAt:  oldest[0].At,
	}

	// Set timer before storing so the struct is fully initialized before
	// being exposed to other goroutines via the map (prevents data race
	// on req.timer field detected by Go race detector).
	req.timer = time.AfterFunc(summaryTimeout, func() {
		s.pendingSummary.Delete(sessionID)
	})

	// Atomically store only if no other goroutine stored first.
	if _, loaded := s.pendingSummary.LoadOrStore(sessionID, req); loaded {
		req.timer.Stop()
		return
	}

	now := time.Now().UTC()
	prompt := eventbus.ConversationPromptEvent{
		SessionID: sessionID,
		PromptID:  promptID,
		Context:   oldest, // Structured turns for prompt engine (populates {{.history}})
		NewMessage: eventbus.ConversationMessage{
			Origin: eventbus.OriginSystem,
			Text:   promptText, // Serialized text as fallback for adapters without prompt engine
			At:     now,
			Meta: map[string]string{
				constants.MetadataKeyEventType: constants.PromptEventHistorySummary,
			},
		},
		Metadata: map[string]string{
			constants.MetadataKeyEventType: constants.PromptEventHistorySummary,
		},
	}

	eventbus.Publish(context.Background(), s.bus, eventbus.Conversation.Prompt, eventbus.SourceConversation, prompt)
}

// consumeFlushResponses listens for memory flush responses from the awareness service.
func (s *Service) consumeFlushResponses(ctx context.Context) {
	eventbus.Consume(ctx, s.flushResponseSub, nil, s.handleFlushResponse)
}

// handleFlushResponse processes a memory flush response, then proceeds with summary.
func (s *Service) handleFlushResponse(event eventbus.MemoryFlushResponseEvent) {
	val, ok := s.pendingFlush.LoadAndDelete(event.SessionID)
	if !ok {
		return
	}

	flush, ok := val.(*flushRequest)
	if !ok {
		return
	}

	if flush.timer != nil {
		flush.timer.Stop()
	}

	s.proceedWithSummary(event.SessionID, flush.oldest, flush.batchSize)
}

// handleSummaryReply processes an AI reply to a history_summary prompt,
// replacing the oldest N turns with a single summary turn.
func (s *Service) handleSummaryReply(sessionID string, msg eventbus.ConversationReplyEvent) {
	s.cancelDetachCleanup(sessionID)

	val, ok := s.pendingSummary.LoadAndDelete(sessionID)
	if !ok {
		return // No pending summary for this session
	}
	req, ok := val.(*summaryRequest)
	if !ok {
		return
	}

	// Cancel the timeout timer
	if req.timer != nil {
		req.timer.Stop()
	}

	// Verify PromptID matches to reject stale replies
	if msg.PromptID != req.promptID {
		return
	}

	s.mu.Lock()
	history := s.sessions[sessionID]
	if len(history) >= req.count {
		// Verify the history hasn't shifted during the flush wait. If FIFO
		// truncation moved the oldest turns, the first turn's timestamp won't
		// match what was captured at requestSummary time. Skip replacement to
		// avoid replacing turns that differ from what the AI actually summarized.
		if !history[0].At.Equal(req.firstAt) {
			s.mu.Unlock()
			return
		}

		// Use the timestamp of the first summarized turn to preserve chronological
		// ordering. Using time.Now() would cause the summary to sort after the
		// remaining turns on the next sort.SliceStable call.
		summaryAt := history[0].At

		summaryTurn := eventbus.ConversationTurn{
			Origin: eventbus.OriginSystem,
			Text:   msg.Text,
			At:     summaryAt,
			Meta: map[string]string{
				constants.MetadataKeySummarized:     constants.MetadataValueTrue,
				constants.MetadataKeyOriginalLength: strconv.Itoa(req.count),
			},
		}

		// Replace the oldest N turns with the summary turn
		remaining := history[req.count:]
		newHistory := make([]eventbus.ConversationTurn, 0, 1+len(remaining))
		newHistory = append(newHistory, summaryTurn)
		newHistory = append(newHistory, remaining...)
		s.sessions[sessionID] = newHistory
	}
	s.mu.Unlock()
}

// clearToolCache removes just the tool cache for a session without affecting
// conversation history. Called on detach to prevent stale current_tool injection.
func (s *Service) clearToolCache(sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	delete(s.toolCache, sessionID)
	s.mu.Unlock()
}

func (s *Service) scheduleDetachCleanup(sessionID string) {
	if sessionID == "" || s.detachTTL <= 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if timer, ok := s.detach[sessionID]; ok {
		timer.Stop()
	}

	timer := time.AfterFunc(s.detachTTL, func() {
		s.clearSession(sessionID)
	})
	s.detach[sessionID] = timer
}

func (s *Service) cancelDetachCleanup(sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	if timer, ok := s.detach[sessionID]; ok {
		timer.Stop()
		delete(s.detach, sessionID)
	}
	s.mu.Unlock()
}

// Context returns a snapshot of the stored conversation turns for the session.
func (s *Service) Context(sessionID string) []eventbus.ConversationTurn {
	_, turns := s.Slice(sessionID, 0, 0)
	return turns
}

// Slice returns a paginated view over the stored conversation turns.
func (s *Service) Slice(sessionID string, offset, limit int) (int, []eventbus.ConversationTurn) {
	s.mu.RLock()
	history := s.sessions[sessionID]
	total := len(history)

	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}

	end := total
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}

	window := history[offset:end]
	out := make([]eventbus.ConversationTurn, len(window))
	copy(out, window)
	s.mu.RUnlock()

	return total, out
}

// GlobalContext returns a snapshot of the global (sessionless) conversation turns.
func (s *Service) GlobalContext() []eventbus.ConversationTurn {
	if s.globalStore == nil {
		return nil
	}
	return s.globalStore.GetContext()
}

// GlobalSlice returns a paginated view over the global conversation turns.
func (s *Service) GlobalSlice(offset, limit int) (int, []eventbus.ConversationTurn) {
	if s.globalStore == nil {
		return 0, nil
	}
	return s.globalStore.Slice(offset, limit)
}

func (s *Service) handleReplyMessage(ts time.Time, msg eventbus.ConversationReplyEvent) {
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	// Intercept history_summary replies before normal processing
	if msg.Metadata[constants.MetadataKeyEventType] == constants.PromptEventHistorySummary && msg.SessionID != "" {
		s.handleSummaryReply(msg.SessionID, msg)
		return
	}

	// Intercept memory_flush replies — awareness service handles these,
	// do not store in conversation history.
	if msg.Metadata[constants.MetadataKeyEventType] == constants.PromptEventMemoryFlush {
		return
	}

	// Intercept session_slug replies — awareness service handles these.
	if msg.Metadata[constants.MetadataKeyEventType] == constants.PromptEventSessionSlug {
		return
	}

	meta := sanitize.NewMetadataAccumulator(msg.Metadata, conversationMetadataLimits)

	turn := eventbus.ConversationTurn{
		Origin: eventbus.OriginAI,
		Text:   msg.Text,
		At:     ts,
		Meta:   nil,
	}
	for idx, action := range msg.Actions {
		base := fmt.Sprintf("action_%d", idx)
		meta.Add(base+".type", action.Type)
		if action.Target != "" {
			meta.Add(base+".target", action.Target)
		}
		if len(action.Args) > 0 {
			data, err := json.Marshal(action.Args)
			if err == nil {
				meta.Add(base+".args", string(data))
			}
		}
	}
	if msg.PromptID != "" {
		meta.Add(constants.MetadataKeyPromptID, msg.PromptID)
	}
	turn.Meta = meta.Result()

	// Handle sessionless ("bezpańskie") replies via GlobalStore
	if msg.SessionID == "" {
		if s.globalStore != nil {
			s.globalStore.AddTurn(turn)
		}
		return
	}

	s.cancelDetachCleanup(msg.SessionID)

	s.mu.Lock()
	history := s.sessions[msg.SessionID]
	history = append(history, turn)
	sort.SliceStable(history, func(i, j int) bool { return history[i].At.Before(history[j].At) })
	if len(history) > s.maxHistory {
		history = history[len(history)-s.maxHistory:]
	}
	s.sessions[msg.SessionID] = history
	s.sessionAI[msg.SessionID] = true
	s.mu.Unlock()
}

func (s *Service) handleBargeEvent(event eventbus.SpeechBargeInEvent) {
	if event.SessionID == "" {
		return
	}

	s.mu.Lock()
	history := s.sessions[event.SessionID]
	for i := len(history) - 1; i >= 0; i-- {
		turn := history[i]
		if turn.Origin != eventbus.OriginAI {
			continue
		}
		meta := make(map[string]string, len(turn.Meta)+3)
		for k, v := range turn.Meta {
			meta[k] = v
		}
		meta[constants.MetadataKeyBargeIn] = constants.MetadataValueTrue
		meta[constants.MetadataKeyBargeInReason] = event.Reason
		if !event.Timestamp.IsZero() {
			meta[constants.MetadataKeyBargeInTimestamp] = event.Timestamp.Format(time.RFC3339Nano)
		}
		for k, v := range event.Metadata {
			meta[constants.MetadataKeyBargePrefix+k] = v
		}
		turn.Meta = meta
		history[i] = turn
		s.sessions[event.SessionID] = history
		break
	}
	s.mu.Unlock()
}
