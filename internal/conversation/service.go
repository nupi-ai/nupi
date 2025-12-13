package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

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
	detach    map[string]*time.Timer
	toolCache map[string]string // sessionID -> current tool name

	// Rate limiting for SESSION_OUTPUT events
	lastSessionOutput sync.Map // sessionID -> time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
	subs   []*eventbus.Subscription
}

const (
	defaultHistory   = 50
	defaultDetachTTL = 5 * time.Minute

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
		bus:        bus,
		maxHistory: defaultHistory,
		detachTTL:  defaultDetachTTL,
		sessions:   make(map[string][]eventbus.ConversationTurn),
		detach:     make(map[string]*time.Timer),
		toolCache:  make(map[string]string),
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

	derivedCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	cleanedSub := s.bus.Subscribe(eventbus.TopicPipelineCleaned, eventbus.WithSubscriptionName("conversation_pipeline"))
	lifecycleSub := s.bus.Subscribe(eventbus.TopicSessionsLifecycle, eventbus.WithSubscriptionName("conversation_lifecycle"))
	replySub := s.bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("conversation_reply"))
	bargeSub := s.bus.Subscribe(eventbus.TopicSpeechBargeIn, eventbus.WithSubscriptionName("conversation_barge"))
	toolSub := s.bus.Subscribe(eventbus.TopicSessionsTool, eventbus.WithSubscriptionName("conversation_tool"))
	toolChangeSub := s.bus.Subscribe(eventbus.TopicSessionsToolChanged, eventbus.WithSubscriptionName("conversation_tool_changed"))
	s.subs = []*eventbus.Subscription{cleanedSub, lifecycleSub, replySub, bargeSub, toolSub, toolChangeSub}

	s.wg.Add(6)
	go s.consumePipeline(derivedCtx, cleanedSub)
	go s.consumeLifecycle(derivedCtx, lifecycleSub)
	go s.consumeReplies(derivedCtx, replySub)
	go s.consumeBarge(derivedCtx, bargeSub)
	go s.consumeToolEvents(derivedCtx, toolSub)
	go s.consumeToolChangeEvents(derivedCtx, toolChangeSub)
	return nil
}

// Shutdown stops background goroutines.
func (s *Service) Shutdown(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	for _, sub := range s.subs {
		if sub != nil {
			sub.Close()
		}
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

func (s *Service) consumePipeline(ctx context.Context, sub *eventbus.Subscription) {
	defer s.wg.Done()
	if sub == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			msg, ok := env.Payload.(eventbus.PipelineMessageEvent)
			if !ok {
				continue
			}
			s.handlePipelineMessage(env.Timestamp, msg)
		}
	}
}

func (s *Service) consumeLifecycle(ctx context.Context, sub *eventbus.Subscription) {
	defer s.wg.Done()
	if sub == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			msg, ok := env.Payload.(eventbus.SessionLifecycleEvent)
			if !ok {
				continue
			}
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
		}
	}
}

func (s *Service) consumeReplies(ctx context.Context, sub *eventbus.Subscription) {
	defer s.wg.Done()
	if sub == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			msg, ok := env.Payload.(eventbus.ConversationReplyEvent)
			if !ok {
				continue
			}
			s.handleReplyMessage(env.Timestamp, msg)
		}
	}
}

func (s *Service) consumeBarge(ctx context.Context, sub *eventbus.Subscription) {
	defer s.wg.Done()
	if sub == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			event, ok := env.Payload.(eventbus.SpeechBargeInEvent)
			if !ok {
				continue
			}
			s.handleBargeEvent(event)
		}
	}
}

func (s *Service) consumeToolEvents(ctx context.Context, sub *eventbus.Subscription) {
	defer s.wg.Done()
	if sub == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			event, ok := env.Payload.(eventbus.SessionToolEvent)
			if !ok {
				continue
			}
			s.updateToolCache(event.SessionID, event.ToolName)
		}
	}
}

func (s *Service) consumeToolChangeEvents(ctx context.Context, sub *eventbus.Subscription) {
	defer s.wg.Done()
	if sub == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			event, ok := env.Payload.(eventbus.SessionToolChangedEvent)
			if !ok {
				continue
			}
			s.updateToolCache(event.SessionID, event.NewTool)
		}
	}
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
	s.mu.Unlock()

	if shouldTriggerAI {
		s.publishPrompt(msg.SessionID, contextSnapshot, turn)
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

	s.bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicConversationPrompt,
		Source:  eventbus.SourceConversation,
		Payload: prompt,
	})
}

func (s *Service) clearSession(sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, sessionID)
	delete(s.toolCache, sessionID)
	if timer, ok := s.detach[sessionID]; ok {
		timer.Stop()
		delete(s.detach, sessionID)
	}
	s.mu.Unlock()

	// Clean up rate limiting entry to prevent memory leaks
	s.lastSessionOutput.Delete(sessionID)
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
	s.mu.RUnlock()

	out := make([]eventbus.ConversationTurn, len(window))
	copy(out, window)
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
	out := make(map[string]string, len(m.entries))
	for k, v := range m.entries {
		out[k] = v
	}
	return out
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
