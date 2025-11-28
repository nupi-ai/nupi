package intentrouter

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

var (
	// ErrNoAdapter is returned when no AI adapter is configured.
	ErrNoAdapter = errors.New("intentrouter: no AI adapter configured")

	// ErrSessionNotFound is returned when the target session doesn't exist.
	ErrSessionNotFound = errors.New("intentrouter: session not found")

	// ErrAdapterNotReady is returned when the adapter isn't ready.
	ErrAdapterNotReady = errors.New("intentrouter: adapter not ready")

	// ErrNoCommandExecutor is returned when no command executor is configured.
	ErrNoCommandExecutor = errors.New("intentrouter: no command executor configured")
)

// Service routes user intents to appropriate sessions via AI adapter.
type Service struct {
	bus             *eventbus.Bus
	adapter         IntentAdapter
	sessionProvider SessionProvider
	commandExecutor CommandExecutor

	mu     sync.RWMutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
	subs   []*eventbus.Subscription

	// Metrics - using atomic for thread-safe access
	requestsTotal   uint64
	requestsFailed  uint64
	commandsQueued  uint64
	clarifications  uint64
	speakEvents     uint64
}

// NewService creates an intent router service.
func NewService(bus *eventbus.Bus, opts ...Option) *Service {
	svc := &Service{
		bus: bus,
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// Start subscribes to conversation prompts and begins processing.
func (s *Service) Start(ctx context.Context) error {
	if s.bus == nil {
		log.Printf("[IntentRouter] No event bus configured, running in passive mode")
		return nil
	}

	derivedCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	// Subscribe to conversation prompts (user messages ready for AI processing)
	promptSub := s.bus.Subscribe(eventbus.TopicConversationPrompt, eventbus.WithSubscriptionName("intent_router_prompt"))
	s.subs = append(s.subs, promptSub)

	s.wg.Add(1)
	go s.consumePrompts(derivedCtx, promptSub)

	s.mu.RLock()
	adapterConfigured := s.adapter != nil
	s.mu.RUnlock()

	if adapterConfigured {
		log.Printf("[IntentRouter] Service started with AI adapter")
	} else {
		log.Printf("[IntentRouter] Service started (no AI adapter configured - will respond with errors)")
	}
	return nil
}

// Shutdown stops the service gracefully.
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
		log.Printf("[IntentRouter] Service shutdown complete")
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}

// SetAdapter sets or replaces the AI adapter at runtime.
func (s *Service) SetAdapter(adapter IntentAdapter) {
	s.mu.Lock()
	s.adapter = adapter
	s.mu.Unlock()
	if adapter != nil {
		log.Printf("[IntentRouter] AI adapter set: %s", adapter.Name())
	}
}

// SetSessionProvider sets the session provider at runtime.
func (s *Service) SetSessionProvider(provider SessionProvider) {
	s.mu.Lock()
	s.sessionProvider = provider
	s.mu.Unlock()
}

// SetCommandExecutor sets the command executor at runtime.
func (s *Service) SetCommandExecutor(executor CommandExecutor) {
	s.mu.Lock()
	s.commandExecutor = executor
	s.mu.Unlock()
}

func (s *Service) consumePrompts(ctx context.Context, sub *eventbus.Subscription) {
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

			prompt, ok := env.Payload.(eventbus.ConversationPromptEvent)
			if !ok {
				continue
			}

			s.handlePrompt(ctx, prompt)
		}
	}
}

func (s *Service) handlePrompt(ctx context.Context, prompt eventbus.ConversationPromptEvent) {
	s.mu.RLock()
	adapter := s.adapter
	sessionProvider := s.sessionProvider
	commandExecutor := s.commandExecutor
	s.mu.RUnlock()

	atomic.AddUint64(&s.requestsTotal, 1)

	// Check if we have an adapter - publish error to bus so UI gets feedback
	if adapter == nil {
		log.Printf("[IntentRouter] No adapter configured for prompt %s", prompt.PromptID)
		atomic.AddUint64(&s.requestsFailed, 1)
		s.publishError(prompt.SessionID, prompt.PromptID, ErrNoAdapter)
		return
	}

	if !adapter.Ready() {
		log.Printf("[IntentRouter] Adapter %s not ready for prompt %s", adapter.Name(), prompt.PromptID)
		atomic.AddUint64(&s.requestsFailed, 1)
		s.publishError(prompt.SessionID, prompt.PromptID, ErrAdapterNotReady)
		return
	}

	// Build the intent request
	req := s.buildIntentRequest(prompt, sessionProvider)

	// Call the AI adapter
	resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	response, err := adapter.ResolveIntent(resolveCtx, req)
	if err != nil {
		log.Printf("[IntentRouter] Adapter %s failed to resolve intent for prompt %s: %v",
			adapter.Name(), prompt.PromptID, err)
		atomic.AddUint64(&s.requestsFailed, 1)
		s.publishError(prompt.SessionID, prompt.PromptID, err)
		return
	}

	// Execute the actions
	s.executeActions(ctx, prompt, response, sessionProvider, commandExecutor)
}

func (s *Service) buildIntentRequest(prompt eventbus.ConversationPromptEvent, provider SessionProvider) IntentRequest {
	req := IntentRequest{
		PromptID:            prompt.PromptID,
		SessionID:           prompt.SessionID,
		Transcript:          prompt.NewMessage.Text,
		ConversationHistory: prompt.Context,
		Metadata:            prompt.Metadata,
	}

	// Add available sessions if provider is configured
	if provider != nil {
		req.AvailableSessions = provider.ListSessionInfos()
	}

	return req
}

func (s *Service) executeActions(ctx context.Context, prompt eventbus.ConversationPromptEvent, response *IntentResponse, sessionProvider SessionProvider, commandExecutor CommandExecutor) {
	if response == nil || len(response.Actions) == 0 {
		log.Printf("[IntentRouter] No actions from adapter for prompt %s", prompt.PromptID)
		// Publish acknowledgment so UI knows the prompt was processed
		s.publishReply(prompt.SessionID, prompt.PromptID, "", nil, map[string]string{
			"status": "no_action",
		})
		return
	}

	for i, action := range response.Actions {
		switch action.Type {
		case ActionCommand:
			s.executeCommand(ctx, prompt, action, response, sessionProvider, commandExecutor)

		case ActionSpeak:
			s.executeSpeak(ctx, prompt, action)

		case ActionClarify:
			s.executeClarify(ctx, prompt, action)

		case ActionNoop:
			log.Printf("[IntentRouter] Noop action for prompt %s", prompt.PromptID)
			s.publishReply(prompt.SessionID, prompt.PromptID, "", nil, map[string]string{
				"status": "noop",
			})

		default:
			log.Printf("[IntentRouter] Unknown action type %q at index %d for prompt %s",
				action.Type, i, prompt.PromptID)
		}
	}
}

func (s *Service) executeCommand(ctx context.Context, prompt eventbus.ConversationPromptEvent, action IntentAction, response *IntentResponse, sessionProvider SessionProvider, commandExecutor CommandExecutor) {
	sessionRef := action.SessionRef
	if sessionRef == "" {
		sessionRef = prompt.SessionID
	}

	// Validate session exists
	if sessionProvider != nil {
		if err := sessionProvider.ValidateSession(sessionRef); err != nil {
			log.Printf("[IntentRouter] Invalid session %s for command: %v", sessionRef, err)
			s.publishError(prompt.SessionID, prompt.PromptID, fmt.Errorf("session %s not found", sessionRef))
			return
		}
	}

	// Check command executor - publish error so UI gets feedback
	if commandExecutor == nil {
		log.Printf("[IntentRouter] No command executor configured for session %s", sessionRef)
		s.publishError(prompt.SessionID, prompt.PromptID, ErrNoCommandExecutor)
		return
	}

	// Queue the command with origin and metadata
	if err := commandExecutor.QueueCommand(sessionRef, action.Command, eventbus.OriginAI); err != nil {
		log.Printf("[IntentRouter] Failed to queue command for session %s: %v", sessionRef, err)
		s.publishError(prompt.SessionID, prompt.PromptID, err)
		return
	}

	atomic.AddUint64(&s.commandsQueued, 1)

	// Publish confirmation with command metadata
	s.publishReply(prompt.SessionID, prompt.PromptID, "", []eventbus.ConversationAction{
		{
			Type:   "command",
			Target: sessionRef,
			Args: map[string]string{
				"command":    action.Command,
				"origin":     string(eventbus.OriginAI),
				"confidence": fmt.Sprintf("%.2f", response.Confidence),
			},
		},
	}, map[string]string{
		"status":     "command_queued",
		"session_id": sessionRef,
	})

	log.Printf("[IntentRouter] Queued command for session %s: %s", sessionRef, truncate(action.Command, 50))
}

func (s *Service) executeSpeak(ctx context.Context, prompt eventbus.ConversationPromptEvent, action IntentAction) {
	if s.bus == nil || action.Text == "" {
		return
	}

	atomic.AddUint64(&s.speakEvents, 1)

	// Publish speak event for TTS
	s.bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationSpeak,
		Source: eventbus.SourceIntentRouter,
		Payload: eventbus.ConversationSpeakEvent{
			SessionID: prompt.SessionID,
			PromptID:  prompt.PromptID,
			Text:      action.Text,
			Metadata:  action.Metadata,
		},
	})

	// Also publish as conversation reply for history tracking
	s.publishReply(prompt.SessionID, prompt.PromptID, action.Text, nil, map[string]string{
		"status": "speak",
	})

	log.Printf("[IntentRouter] Speak action for session %s: %s", prompt.SessionID, truncate(action.Text, 50))
}

func (s *Service) executeClarify(ctx context.Context, prompt eventbus.ConversationPromptEvent, action IntentAction) {
	if s.bus == nil || action.Text == "" {
		return
	}

	atomic.AddUint64(&s.clarifications, 1)

	// Publish speak event with clarification
	s.bus.Publish(ctx, eventbus.Envelope{
		Topic:  eventbus.TopicConversationSpeak,
		Source: eventbus.SourceIntentRouter,
		Payload: eventbus.ConversationSpeakEvent{
			SessionID: prompt.SessionID,
			PromptID:  prompt.PromptID,
			Text:      action.Text,
			Metadata: map[string]string{
				"type": "clarification",
			},
		},
	})

	// Publish reply with clarify action marker
	actions := []eventbus.ConversationAction{
		{
			Type: "clarify",
			Args: map[string]string{
				"question": action.Text,
			},
		},
	}
	s.publishReply(prompt.SessionID, prompt.PromptID, action.Text, actions, map[string]string{
		"status": "clarification",
	})

	log.Printf("[IntentRouter] Clarification requested for session %s: %s", prompt.SessionID, truncate(action.Text, 50))
}

func (s *Service) publishReply(sessionID, promptID, text string, actions []eventbus.ConversationAction, metadata map[string]string) {
	if s.bus == nil {
		return
	}

	s.bus.Publish(context.Background(), eventbus.Envelope{
		Topic:  eventbus.TopicConversationReply,
		Source: eventbus.SourceIntentRouter,
		Payload: eventbus.ConversationReplyEvent{
			SessionID: sessionID,
			PromptID:  promptID,
			Text:      text,
			Actions:   actions,
			Metadata:  metadata,
		},
	})
}

func (s *Service) publishError(sessionID, promptID string, err error) {
	if s.bus == nil {
		return
	}

	s.bus.Publish(context.Background(), eventbus.Envelope{
		Topic:  eventbus.TopicConversationReply,
		Source: eventbus.SourceIntentRouter,
		Payload: eventbus.ConversationReplyEvent{
			SessionID: sessionID,
			PromptID:  promptID,
			Text:      fmt.Sprintf("Error: %v", err),
			Metadata: map[string]string{
				"error":       "true",
				"error_type":  errorType(err),
				"recoverable": recoverableError(err),
			},
		},
	})

	// Also publish speak for TTS feedback
	s.bus.Publish(context.Background(), eventbus.Envelope{
		Topic:  eventbus.TopicConversationSpeak,
		Source: eventbus.SourceIntentRouter,
		Payload: eventbus.ConversationSpeakEvent{
			SessionID: sessionID,
			PromptID:  promptID,
			Text:      userFriendlyError(err),
			Metadata: map[string]string{
				"type":  "error",
				"error": err.Error(),
			},
		},
	})
}

func errorType(err error) string {
	switch {
	case errors.Is(err, ErrNoAdapter):
		return "no_adapter"
	case errors.Is(err, ErrAdapterNotReady):
		return "adapter_not_ready"
	case errors.Is(err, ErrNoCommandExecutor):
		return "no_executor"
	case errors.Is(err, ErrSessionNotFound):
		return "session_not_found"
	default:
		return "unknown"
	}
}

func recoverableError(err error) string {
	switch {
	case errors.Is(err, ErrNoAdapter), errors.Is(err, ErrNoCommandExecutor), errors.Is(err, ErrAdapterNotReady):
		// Configuration issues that can be fixed by user action
		return "true"
	case errors.Is(err, ErrSessionNotFound):
		return "false"
	default:
		return "unknown"
	}
}

func userFriendlyError(err error) string {
	switch {
	case errors.Is(err, ErrNoAdapter):
		return "No AI adapter is configured. Please configure an AI adapter to enable voice commands."
	case errors.Is(err, ErrAdapterNotReady):
		return "The AI adapter is not ready yet. Please wait a moment and try again."
	case errors.Is(err, ErrNoCommandExecutor):
		return "Cannot execute commands at this time."
	case errors.Is(err, ErrSessionNotFound):
		return "The requested session was not found."
	default:
		return "An error occurred while processing your request."
	}
}

// Metrics returns current service metrics.
func (s *Service) Metrics() ServiceMetrics {
	s.mu.RLock()
	adapterName := ""
	adapterReady := false
	if s.adapter != nil {
		adapterName = s.adapter.Name()
		adapterReady = s.adapter.Ready()
	}
	s.mu.RUnlock()

	return ServiceMetrics{
		RequestsTotal:  atomic.LoadUint64(&s.requestsTotal),
		RequestsFailed: atomic.LoadUint64(&s.requestsFailed),
		CommandsQueued: atomic.LoadUint64(&s.commandsQueued),
		Clarifications: atomic.LoadUint64(&s.clarifications),
		SpeakEvents:    atomic.LoadUint64(&s.speakEvents),
		AdapterName:    adapterName,
		AdapterReady:   adapterReady,
	}
}

// ServiceMetrics contains runtime statistics.
type ServiceMetrics struct {
	RequestsTotal  uint64
	RequestsFailed uint64
	CommandsQueued uint64
	Clarifications uint64
	SpeakEvents    uint64
	AdapterName    string
	AdapterReady   bool
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
