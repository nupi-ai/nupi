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

// Service maintains conversation context and emits prompts for AI modules.
type Service struct {
	bus *eventbus.Bus

	maxHistory int
	detachTTL  time.Duration

	mu       sync.RWMutex
	sessions map[string][]eventbus.ConversationTurn
	detach   map[string]*time.Timer

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
)

// NewService creates a conversation service bound to the provided bus.
func NewService(bus *eventbus.Bus, opts ...Option) *Service {
	svc := &Service{
		bus:        bus,
		maxHistory: defaultHistory,
		detachTTL:  defaultDetachTTL,
		sessions:   make(map[string][]eventbus.ConversationTurn),
		detach:     make(map[string]*time.Timer),
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
	s.subs = []*eventbus.Subscription{cleanedSub, lifecycleSub, replySub}

	s.wg.Add(3)
	go s.consumePipeline(derivedCtx, cleanedSub)
	go s.consumeLifecycle(derivedCtx, lifecycleSub)
	go s.consumeReplies(derivedCtx, replySub)
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

func (s *Service) handlePipelineMessage(ts time.Time, msg eventbus.PipelineMessageEvent) {
	if msg.SessionID == "" {
		return
	}

	s.cancelDetachCleanup(msg.SessionID)

	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	meta := newMetadataAccumulator(nil)
	meta.merge(msg.Annotations)

	turn := eventbus.ConversationTurn{
		Origin: msg.Origin,
		Text:   msg.Text,
		At:     ts,
		Meta:   meta.result(),
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

	if msg.Origin == eventbus.OriginUser {
		s.publishPrompt(msg.SessionID, contextSnapshot, turn)
	}
}

func (s *Service) publishPrompt(sessionID string, ctxTurns []eventbus.ConversationTurn, newTurn eventbus.ConversationTurn) {
	if s.bus == nil {
		return
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
		Metadata: map[string]string{},
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
	if timer, ok := s.detach[sessionID]; ok {
		timer.Stop()
		delete(s.detach, sessionID)
	}
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

func (s *Service) handleReplyMessage(ts time.Time, msg eventbus.ConversationReplyEvent) {
	if msg.SessionID == "" {
		return
	}

	s.cancelDetachCleanup(msg.SessionID)

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
