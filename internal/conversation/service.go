package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/sanitize"
)

type Option func(*Service)

// WithDetachTTL overrides the timeout after which detached sessions are purged.
func WithDetachTTL(ttl time.Duration) Option {
	return func(s *Service) {
		if ttl >= 0 {
			s.detachTTL = ttl
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

	detachTTL   time.Duration
	globalStore *GlobalStore

	mu        sync.RWMutex
	detach    map[string]*time.Timer
	toolCache map[string]string // sessionID -> current tool name

	// Rate limiting for SESSION_OUTPUT events
	lastSessionOutput sync.Map // sessionID -> time.Time

	shuttingDown atomic.Bool

	lifecycle     eventbus.ServiceLifecycle
	cleanedSub    *eventbus.TypedSubscription[eventbus.PipelineMessageEvent]
	lifecycleSub  *eventbus.TypedSubscription[eventbus.SessionLifecycleEvent]
	replySub      *eventbus.TypedSubscription[eventbus.ConversationReplyEvent]
	toolSub       *eventbus.TypedSubscription[eventbus.SessionToolEvent]
	toolChangeSub *eventbus.TypedSubscription[eventbus.SessionToolChangedEvent]
}

const (
	defaultDetachTTL = constants.Duration5Minutes

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
		bus:       bus,
		detachTTL: defaultDetachTTL,
		detach:    make(map[string]*time.Timer),
		toolCache: make(map[string]string),
	}
	for _, opt := range opts {
		opt(svc)
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
	s.toolSub = eventbus.Subscribe[eventbus.SessionToolEvent](s.bus, eventbus.TopicSessionsTool, eventbus.WithSubscriptionName("conversation_tool"))
	s.toolChangeSub = eventbus.Subscribe[eventbus.SessionToolChangedEvent](s.bus, eventbus.TopicSessionsToolChanged, eventbus.WithSubscriptionName("conversation_tool_changed"))
	s.lifecycle.AddSubscriptions(s.cleanedSub, s.lifecycleSub, s.replySub, s.toolSub, s.toolChangeSub)
	s.lifecycle.Go(s.consumePipeline)
	s.lifecycle.Go(s.consumeLifecycle)
	s.lifecycle.Go(s.consumeReplies)
	s.lifecycle.Go(s.consumeToolEvents)
	s.lifecycle.Go(s.consumeToolChangeEvents)
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

	return s.lifecycle.Wait(ctx)
}

func (s *Service) consumePipeline(ctx context.Context) {
	eventbus.ConsumeEnvelope(ctx, s.cleanedSub, nil, func(env eventbus.TypedEnvelope[eventbus.PipelineMessageEvent]) {
		s.handlePipelineMessage(ctx, env.Timestamp, env.Payload)
	})
}

func (s *Service) consumeLifecycle(ctx context.Context) {
	eventbus.Consume(ctx, s.lifecycleSub, nil, func(msg eventbus.SessionLifecycleEvent) {
		switch msg.State {
		case eventbus.SessionStateStopped:
			s.clearSession(msg.SessionID)
		case eventbus.SessionStateDetached:
			// Clear tool cache immediately to prevent stale current_tool injection
			s.clearToolCache(msg.SessionID)
			s.scheduleDetachCleanup(msg.SessionID)
		case eventbus.SessionStateRunning, eventbus.SessionStateCreated:
			s.cancelDetachCleanup(msg.SessionID)
		}
	})
}

func (s *Service) consumeReplies(ctx context.Context) {
	eventbus.ConsumeEnvelope(ctx, s.replySub, nil, func(env eventbus.TypedEnvelope[eventbus.ConversationReplyEvent]) {
		s.handleReplyMessage(ctx, env.Timestamp, env.Payload)
	})
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

func (s *Service) handlePipelineMessage(ctx context.Context, ts time.Time, msg eventbus.PipelineMessageEvent) {
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	meta := sanitize.NewMetadataAccumulator(nil, conversationMetadataLimits)
	meta.Merge(msg.Annotations)

	// Determine event type BEFORE creating turn
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

	if shouldTriggerAI || msg.Annotations[constants.MetadataKeyNotable] == constants.MetadataValueTrue {
		meta.Merge(map[string]string{constants.MetadataKeyEventType: eventType})
	}

	turn := eventbus.ConversationTurn{
		Origin: msg.Origin,
		Text:   msg.Text,
		At:     ts,
		Meta:   meta.Result(),
	}

	// Handle sessionless messages via GlobalStore
	if msg.SessionID == "" {
		if s.globalStore != nil {
			contextSnapshot := s.globalStore.GetContext()
			s.globalStore.AddTurn(turn)
			s.publishTurn(ctx, "", turn)
			if msg.Origin == eventbus.OriginUser {
				s.publishPrompt(ctx, "", contextSnapshot, turn)
			}
		}
		return
	}

	s.cancelDetachCleanup(msg.SessionID)

	// Check rate limiting for session_output (needs sessionID, so done here)
	if eventType == constants.PromptEventSessionOutput {
		shouldTriggerAI = s.shouldTriggerSessionOutput(msg.SessionID)
	}

	// Publish turn event for downstream consumers (Conversation Log Service)
	s.publishTurn(ctx, msg.SessionID, turn)

	if shouldTriggerAI {
		s.publishPrompt(ctx, msg.SessionID, nil, turn)
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

// ResetRateLimitForTesting overrides the rate limit timestamp for the given
// session, allowing tests to verify rate limiting without real-time waits.
func (s *Service) ResetRateLimitForTesting(sessionID string, t time.Time) {
	s.lastSessionOutput.Store(sessionID, t)
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

// publishTurn publishes a ConversationTurnEvent on the event bus for downstream consumers.
func (s *Service) publishTurn(ctx context.Context, sessionID string, turn eventbus.ConversationTurn) {
	if s.bus == nil {
		return
	}
	eventbus.Publish(ctx, s.bus, eventbus.Conversation.Turn, eventbus.SourceConversation, eventbus.ConversationTurnEvent{
		SessionID: sessionID,
		Turn:      turn,
	})
}

// publishPrompt builds and publishes a ConversationPromptEvent.
// ctxTurns is only populated for sessionless messages (GlobalStore context snapshot);
// for session messages it is always nil (file-backed context deferred to Epic 21).
func (s *Service) publishPrompt(ctx context.Context, sessionID string, ctxTurns []eventbus.ConversationTurn, newTurn eventbus.ConversationTurn) {
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
		NewMessage: eventbus.ConversationMessage{
			Origin: newTurn.Origin,
			Text:   newTurn.Text,
			At:     newTurn.At,
			Meta:   newTurn.Meta,
		},
		Metadata: metadata,
	}

	eventbus.Publish(ctx, s.bus, eventbus.Conversation.Prompt, eventbus.SourceConversation, prompt)
}

func (s *Service) clearSession(sessionID string) {
	if sessionID == "" {
		return
	}

	s.mu.Lock()
	delete(s.toolCache, sessionID)
	if timer, ok := s.detach[sessionID]; ok {
		timer.Stop()
		delete(s.detach, sessionID)
	}
	s.mu.Unlock()

	// Clean up rate limiting entry to prevent memory leaks
	s.lastSessionOutput.Delete(sessionID)
}

// clearToolCache removes just the tool cache for a session.
// Called on detach to prevent stale current_tool injection.
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
		if s.shuttingDown.Load() {
			return
		}
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

// Context returns a stub — RAM history has been removed. Returns nil.
// The file-backed implementation will be provided by Conversation Log Service (Epic 21).
func (s *Service) Context(sessionID string) []eventbus.ConversationTurn {
	return nil
}

// Slice returns a stub — RAM history has been removed. Returns (0, nil).
// The file-backed implementation will be provided by Conversation Log Service (Epic 21).
func (s *Service) Slice(sessionID string, offset, limit int) (int, []eventbus.ConversationTurn) {
	return 0, nil
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

func (s *Service) handleReplyMessage(ctx context.Context, ts time.Time, msg eventbus.ConversationReplyEvent) {
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	// Intercept compaction replies — internal AI tasks, NOT dialog turns
	eventType := msg.Metadata[constants.MetadataKeyEventType]
	switch eventType {
	case constants.PromptEventConversationCompaction,
		constants.PromptEventJournalCompaction:
		return // Do not publish turn, do not add to dialog
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

	// Handle sessionless replies via GlobalStore
	if msg.SessionID == "" {
		if s.globalStore != nil {
			s.globalStore.AddTurn(turn)
			s.publishTurn(ctx, "", turn)
		}
		return
	}

	s.cancelDetachCleanup(msg.SessionID)

	// Publish turn event for downstream consumers (Conversation Log Service)
	s.publishTurn(ctx, msg.SessionID, turn)
}

