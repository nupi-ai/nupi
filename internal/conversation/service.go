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
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nupi-ai/nupi/internal/eventbus"
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

	cancel        context.CancelFunc
	wg            sync.WaitGroup
	cleanedSub    *eventbus.TypedSubscription[eventbus.PipelineMessageEvent]
	lifecycleSub  *eventbus.TypedSubscription[eventbus.SessionLifecycleEvent]
	replySub      *eventbus.TypedSubscription[eventbus.ConversationReplyEvent]
	bargeSub      *eventbus.TypedSubscription[eventbus.SpeechBargeInEvent]
	toolSub       *eventbus.TypedSubscription[eventbus.SessionToolEvent]
	toolChangeSub *eventbus.TypedSubscription[eventbus.SessionToolChangedEvent]
}

const (
	defaultHistory   = 50
	defaultDetachTTL = 5 * time.Minute

	defaultSummaryThreshold = 35
	defaultSummaryBatchSize = 20
	summaryTimeout          = 60 * time.Second

	maxMetadataEntries    = 32
	maxMetadataKeyRunes   = 64
	maxMetadataValueRunes = 512
	maxMetadataTotalBytes = 8192

	// Rate limiting for SESSION_OUTPUT events
	sessionOutputMinInterval = 2 * time.Second
)

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

	derivedCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	s.cleanedSub = eventbus.Subscribe[eventbus.PipelineMessageEvent](s.bus, eventbus.TopicPipelineCleaned, eventbus.WithSubscriptionName("conversation_pipeline"))
	s.lifecycleSub = eventbus.Subscribe[eventbus.SessionLifecycleEvent](s.bus, eventbus.TopicSessionsLifecycle, eventbus.WithSubscriptionName("conversation_lifecycle"))
	s.replySub = eventbus.Subscribe[eventbus.ConversationReplyEvent](s.bus, eventbus.TopicConversationReply, eventbus.WithSubscriptionName("conversation_reply"))
	s.bargeSub = eventbus.Subscribe[eventbus.SpeechBargeInEvent](s.bus, eventbus.TopicSpeechBargeIn, eventbus.WithSubscriptionName("conversation_barge"))
	s.toolSub = eventbus.Subscribe[eventbus.SessionToolEvent](s.bus, eventbus.TopicSessionsTool, eventbus.WithSubscriptionName("conversation_tool"))
	s.toolChangeSub = eventbus.Subscribe[eventbus.SessionToolChangedEvent](s.bus, eventbus.TopicSessionsToolChanged, eventbus.WithSubscriptionName("conversation_tool_changed"))
	s.flushResponseSub = eventbus.Subscribe[eventbus.MemoryFlushResponseEvent](s.bus, eventbus.TopicMemoryFlushResponse, eventbus.WithSubscriptionName("conversation_flush_response"))

	s.wg.Add(7)
	go s.consumePipeline(derivedCtx)
	go s.consumeLifecycle(derivedCtx)
	go s.consumeReplies(derivedCtx)
	go s.consumeBarge(derivedCtx)
	go s.consumeToolEvents(derivedCtx)
	go s.consumeToolChangeEvents(derivedCtx)
	go s.consumeFlushResponses(derivedCtx)
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
	if s.cleanedSub != nil {
		s.cleanedSub.Close()
	}
	if s.lifecycleSub != nil {
		s.lifecycleSub.Close()
	}
	if s.replySub != nil {
		s.replySub.Close()
	}
	if s.bargeSub != nil {
		s.bargeSub.Close()
	}
	if s.toolSub != nil {
		s.toolSub.Close()
	}
	if s.toolChangeSub != nil {
		s.toolChangeSub.Close()
	}
	if s.flushResponseSub != nil {
		s.flushResponseSub.Close()
	}

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

	if s.cancel != nil {
		s.cancel()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.wg.Wait()
	}()

	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (s *Service) consumePipeline(ctx context.Context) {
	eventbus.ConsumeEnvelope(ctx, s.cleanedSub, &s.wg, func(env eventbus.TypedEnvelope[eventbus.PipelineMessageEvent]) {
		s.handlePipelineMessage(env.Timestamp, env.Payload)
	})
}

func (s *Service) consumeLifecycle(ctx context.Context) {
	eventbus.Consume(ctx, s.lifecycleSub, &s.wg, func(msg eventbus.SessionLifecycleEvent) {
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
	eventbus.ConsumeEnvelope(ctx, s.replySub, &s.wg, func(env eventbus.TypedEnvelope[eventbus.ConversationReplyEvent]) {
		s.handleReplyMessage(env.Timestamp, env.Payload)
	})
}

func (s *Service) consumeBarge(ctx context.Context) {
	eventbus.Consume(ctx, s.bargeSub, &s.wg, s.handleBargeEvent)
}

func (s *Service) consumeToolEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.toolSub, &s.wg, func(event eventbus.SessionToolEvent) {
		s.updateToolCache(event.SessionID, event.ToolName)
	})
}

func (s *Service) consumeToolChangeEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.toolChangeSub, &s.wg, func(event eventbus.SessionToolChangedEvent) {
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

	meta := newMetadataAccumulator(nil)
	meta.merge(msg.Annotations)

	// Determine event type BEFORE creating turn (to avoid mutating Meta after it's in history)
	shouldTriggerAI := false
	eventType := "user_intent"

	if msg.Origin == eventbus.OriginUser {
		shouldTriggerAI = true
		eventType = "user_intent"
	} else if msg.Annotations["notable"] == "true" {
		// Tool output marked as notable by content pipeline
		// Note: rate limiting check is deferred until after we have sessionID context
		eventType = "session_output"
	}

	// Set event_type in meta BEFORE turn is added to history (prevents race condition)
	if shouldTriggerAI || msg.Annotations["notable"] == "true" {
		meta.merge(map[string]string{"event_type": eventType})
	}

	turn := eventbus.ConversationTurn{
		Origin: msg.Origin,
		Text:   msg.Text,
		At:     ts,
		Meta:   meta.result(),
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
	if eventType == "session_output" {
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
var validEventTypes = map[string]bool{
	"user_intent":     true,
	"session_output":  true,
	"history_summary": true,
	"clarification":   true,
	"memory_flush":    true,
	"session_slug":    true,
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
		case "tool", "tool_id", "confidence", "stream_id", "event_type",
			"session_output", "clarification_question", "severity":
			metadata[key] = value
		// Extended pipeline metadata (input source, mode, annotations)
		case "input_source", "mode", "sessionless", "adapter", "language",
			"model", "provider", "priority", "context_window":
			metadata[key] = value
		// SESSION_OUTPUT specific metadata (idle detection, events, buffer status)
		case "notable", "idle_state", "waiting_for", "prompt_text",
			"event_count", "event_title", "event_details", "event_action",
			"summarized", "original_length",
			"buffer_truncated", "buffer_max_size",
			"tool_changed":
			metadata[key] = value
		default:
			// Language metadata (nupi.lang.*) is propagated unconditionally
			// so that every adapter receives the client's language preference.
			if strings.HasPrefix(key, "nupi.lang.") {
				metadata[key] = value
			}
		}
	}

	// Add current_tool from our tool cache if not already in metadata
	if sessionID != "" {
		s.mu.RLock()
		if tool, ok := s.toolCache[sessionID]; ok && tool != "" {
			if _, exists := metadata["current_tool"]; !exists {
				metadata["current_tool"] = tool
			}
			if _, exists := metadata["tool"]; !exists {
				metadata["tool"] = tool
			}
		}
		s.mu.RUnlock()
	}

	// Validate and set event_type
	if eventType, exists := metadata["event_type"]; exists {
		if !validEventTypes[eventType] {
			// Invalid event_type, reset to default
			metadata["event_type"] = "user_intent"
		}
		// For session_output events, populate session_output field with the turn text
		// This is what the prompts engine template expects via {{.session_output}}
		if eventType == "session_output" {
			metadata["session_output"] = newTurn.Text
		}
	} else {
		metadata["event_type"] = "user_intent"
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
				"event_type": "history_summary",
			},
		},
		Metadata: map[string]string{
			"event_type": "history_summary",
		},
	}

	eventbus.Publish(context.Background(), s.bus, eventbus.Conversation.Prompt, eventbus.SourceConversation, prompt)
}

// consumeFlushResponses listens for memory flush responses from the awareness service.
func (s *Service) consumeFlushResponses(ctx context.Context) {
	eventbus.Consume(ctx, s.flushResponseSub, &s.wg, s.handleFlushResponse)
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
				"summarized":      "true",
				"original_length": strconv.Itoa(req.count),
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
	if msg.Metadata["event_type"] == "history_summary" && msg.SessionID != "" {
		s.handleSummaryReply(msg.SessionID, msg)
		return
	}

	// Intercept memory_flush replies — awareness service handles these,
	// do not store in conversation history.
	if msg.Metadata["event_type"] == "memory_flush" {
		return
	}

	// Intercept session_slug replies — awareness service handles these.
	if msg.Metadata["event_type"] == "session_slug" {
		return
	}

	meta := newMetadataAccumulator(msg.Metadata)

	turn := eventbus.ConversationTurn{
		Origin: eventbus.OriginAI,
		Text:   msg.Text,
		At:     ts,
		Meta:   nil,
	}
	for idx, action := range msg.Actions {
		base := fmt.Sprintf("action_%d", idx)
		meta.add(base+".type", action.Type)
		if action.Target != "" {
			meta.add(base+".target", action.Target)
		}
		if len(action.Args) > 0 {
			data, err := json.Marshal(action.Args)
			if err == nil {
				meta.add(base+".args", string(data))
			}
		}
	}
	if msg.PromptID != "" {
		meta.add("prompt_id", msg.PromptID)
	}
	turn.Meta = meta.result()

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
		meta["barge_in"] = "true"
		meta["barge_in_reason"] = event.Reason
		if !event.Timestamp.IsZero() {
			meta["barge_in_timestamp"] = event.Timestamp.Format(time.RFC3339Nano)
		}
		for k, v := range event.Metadata {
			meta["barge_"+k] = v
		}
		turn.Meta = meta
		history[i] = turn
		s.sessions[event.SessionID] = history
		break
	}
	s.mu.Unlock()
}

type metadataAccumulator struct {
	entries map[string]string
	total   int
}

func newMetadataAccumulator(base map[string]string) *metadataAccumulator {
	acc := &metadataAccumulator{
		entries: nil,
		total:   0,
	}
	acc.merge(base)
	return acc
}

func (m *metadataAccumulator) merge(src map[string]string) {
	if len(src) == 0 {
		return
	}
	keys := make([]string, 0, len(src))
	for k := range src {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		m.add(key, src[key])
	}
}

func (m *metadataAccumulator) add(key, value string) {
	key = sanitizeKey(key)
	if key == "" {
		return
	}
	value = sanitizeValue(value)

	if m.entries == nil {
		m.entries = make(map[string]string)
	}

	if len(m.entries) >= maxMetadataEntries {
		if _, exists := m.entries[key]; !exists {
			return
		}
	}

	addition := len(key) + len(value)
	if existing, ok := m.entries[key]; ok {
		m.total -= len(key) + len(existing)
	}

	if maxMetadataTotalBytes > 0 && m.total+addition > maxMetadataTotalBytes {
		if existing, ok := m.entries[key]; ok {
			m.total += len(key) + len(existing)
		}
		return
	}

	m.entries[key] = value
	m.total += addition

	if len(m.entries) > maxMetadataEntries {
		delete(m.entries, key)
		m.total -= addition
	}
}

func (m *metadataAccumulator) result() map[string]string {
	if len(m.entries) == 0 {
		return nil
	}
	return maps.Clone(m.entries)
}

func sanitizeKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if utf8.RuneCountInString(key) <= maxMetadataKeyRunes {
		return key
	}
	runes := []rune(key)
	if len(runes) > maxMetadataKeyRunes {
		runes = runes[:maxMetadataKeyRunes]
	}
	return string(runes)
}

func sanitizeValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if utf8.RuneCountInString(value) <= maxMetadataValueRunes {
		return value
	}
	runes := []rune(value)
	if len(runes) > maxMetadataValueRunes {
		runes = runes[:maxMetadataValueRunes]
	}
	return string(runes)
}
