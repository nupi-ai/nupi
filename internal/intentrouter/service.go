package intentrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
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

	// ErrMaxToolIterations is returned when the tool-use loop exceeds the safety cap.
	ErrMaxToolIterations = errors.New("intentrouter: max tool iterations reached")

	// ErrUnexpectedToolUse is returned when a tool_use action reaches executeActions
	// instead of being handled in the tool loop.
	ErrUnexpectedToolUse = errors.New("intentrouter: unexpected tool_use action in final response")

	// ErrNoToolRegistry is returned when the adapter requests tool_use but no tool registry is configured.
	ErrNoToolRegistry = errors.New("intentrouter: no tool registry configured")
)

// maxToolIterations is the safety cap for the multi-turn tool-use loop (NFR32).
// With per-iteration requestTimeout (30s), worst-case wall time is 10 × 30s = 5 min.
const maxToolIterations = 10

const (
	metadataKeyEventType             = constants.MetadataKeyEventType
	metadataKeyCurrentTool           = constants.MetadataKeyCurrentTool
	metadataKeySessionOutput         = constants.MetadataKeySessionOutput
	metadataKeyClarificationQuestion = constants.MetadataKeyClarificationQuestion
	metadataKeySuggestedSession      = constants.MetadataKeySuggestedSession
	metadataKeyRoutingHint           = constants.MetadataKeyRoutingHint
	metadataRoutingHintConversation  = "conversation_context"
)

// Service routes user intents to appropriate sessions via AI adapter.
type Service struct {
	bus                *eventbus.Bus
	adapter            IntentAdapter
	sessionProvider    SessionProvider
	commandExecutor    CommandExecutor
	promptEngine       PromptEngine
	toolRegistry       ToolRegistry
	coreMemoryProvider CoreMemoryProvider
	onboardingProvider OnboardingProvider

	mu        sync.RWMutex
	lifecycle eventbus.ServiceLifecycle

	promptSub     *eventbus.TypedSubscription[eventbus.ConversationPromptEvent]
	toolSub       *eventbus.TypedSubscription[eventbus.SessionToolEvent]
	toolChangeSub *eventbus.TypedSubscription[eventbus.SessionToolChangedEvent]
	lifecycleSub  *eventbus.TypedSubscription[eventbus.SessionLifecycleEvent]

	// Tool cache - tracks current tool per session for prompt context
	toolCache map[string]string

	// Smart session routing - tracks conversation context for session selection
	conversationSession     string    // Last session that was discussed/targeted
	conversationSessionTime time.Time // When it was last referenced
}

// NewService creates an intent router service.
func NewService(bus *eventbus.Bus, opts ...Option) *Service {
	svc := &Service{
		bus:       bus,
		toolCache: make(map[string]string),
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

	s.lifecycle.Start(ctx)

	s.promptSub = eventbus.Subscribe[eventbus.ConversationPromptEvent](s.bus, eventbus.TopicConversationPrompt, eventbus.WithSubscriptionName("intent_router_prompt"))
	s.toolSub = eventbus.Subscribe[eventbus.SessionToolEvent](s.bus, eventbus.TopicSessionsTool, eventbus.WithSubscriptionName("intent_router_tool"))
	s.toolChangeSub = eventbus.Subscribe[eventbus.SessionToolChangedEvent](s.bus, eventbus.TopicSessionsToolChanged, eventbus.WithSubscriptionName("intent_router_tool_changed"))
	s.lifecycleSub = eventbus.Subscribe[eventbus.SessionLifecycleEvent](s.bus, eventbus.TopicSessionsLifecycle, eventbus.WithSubscriptionName("intent_router_lifecycle"))

	s.lifecycle.AddSubscriptions(s.promptSub, s.toolSub, s.toolChangeSub, s.lifecycleSub)
	s.lifecycle.Go(s.consumePrompts)
	s.lifecycle.Go(s.consumeToolEvents)
	s.lifecycle.Go(s.consumeToolChangeEvents)
	s.lifecycle.Go(s.consumeLifecycleEvents)

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
	if err := s.lifecycle.Shutdown(ctx); err != nil {
		return err
	}
	log.Printf("[IntentRouter] Service shutdown complete")
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

// SetPromptEngine sets the prompt engine at runtime.
func (s *Service) SetPromptEngine(engine PromptEngine) {
	s.mu.Lock()
	s.promptEngine = engine
	s.mu.Unlock()
	if engine != nil {
		log.Printf("[IntentRouter] Prompt engine configured")
	}
}

// SetToolRegistry sets the tool registry at runtime.
func (s *Service) SetToolRegistry(registry ToolRegistry) {
	s.mu.Lock()
	s.toolRegistry = registry
	s.mu.Unlock()
	if registry != nil {
		log.Printf("[IntentRouter] Tool registry configured")
	}
}

// SetCoreMemoryProvider sets the core memory provider at runtime.
func (s *Service) SetCoreMemoryProvider(provider CoreMemoryProvider) {
	s.mu.Lock()
	s.coreMemoryProvider = provider
	s.mu.Unlock()
	if provider != nil {
		log.Printf("[IntentRouter] Core memory provider configured")
	}
}

func (s *Service) consumePrompts(ctx context.Context) {
	eventbus.Consume(ctx, s.promptSub, nil, func(prompt eventbus.ConversationPromptEvent) {
		s.handlePrompt(ctx, prompt)
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
		log.Printf("[IntentRouter] Tool changed for session %s: %s -> %s",
			event.SessionID, event.PreviousTool, event.NewTool)
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

func (s *Service) getToolFromCache(sessionID string) string {
	s.mu.RLock()
	tool := s.toolCache[sessionID]
	s.mu.RUnlock()
	return tool
}

func (s *Service) clearToolCache(sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	delete(s.toolCache, sessionID)
	// Also clear conversation session if this was it
	if s.conversationSession == sessionID {
		s.conversationSession = ""
	}
	s.mu.Unlock()
}

// updateConversationSession tracks the session that was most recently referenced.
// This helps with smart routing when the user speaks without specifying a session.
func (s *Service) updateConversationSession(sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	s.conversationSession = sessionID
	s.conversationSessionTime = time.Now()
	s.mu.Unlock()
}

// getConversationSession returns the most recently referenced session.
// Returns empty if no session was referenced recently (within 5 minutes).
func (s *Service) getConversationSession() string {
	s.mu.RLock()
	session := s.conversationSession
	sessionTime := s.conversationSessionTime
	s.mu.RUnlock()

	// Consider session stale after 5 minutes of inactivity
	if time.Since(sessionTime) > constants.Duration5Minutes {
		return ""
	}
	return session
}

// selectTargetSession determines which session to target for a prompt.
// Uses smart routing based on:
// 1. Explicit session ID from prompt
// 2. Last conversation session (if recent)
// 3. Single available session (if only one exists)
// 4. Empty (AI will decide or clarify)
func (s *Service) selectTargetSession(prompt eventbus.ConversationPromptEvent, provider SessionProvider) string {
	// 1. If prompt already has sessionID, use it
	if prompt.SessionID != "" {
		return prompt.SessionID
	}

	// 2. Use last conversation session if recent
	if lastSession := s.getConversationSession(); lastSession != "" {
		// Verify session still exists
		if provider != nil {
			if err := provider.ValidateSession(lastSession); err == nil {
				return lastSession
			}
		}
	}

	// 3. If only one session exists, use it
	if provider != nil {
		sessions := provider.ListSessionInfos()
		if len(sessions) == 1 {
			return sessions[0].ID
		}
	}

	// 4. Return empty - AI will decide or ask for clarification
	return ""
}

func (s *Service) consumeLifecycleEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.lifecycleSub, nil, func(event eventbus.SessionLifecycleEvent) {
		// Clean up tool cache when session stops or detaches
		switch event.State {
		case eventbus.SessionStateStopped, eventbus.SessionStateDetached:
			s.clearToolCache(event.SessionID)
		}
	})
}

func (s *Service) handlePrompt(ctx context.Context, prompt eventbus.ConversationPromptEvent) {
	s.mu.RLock()
	adapter := s.adapter
	sessionProvider := s.sessionProvider
	commandExecutor := s.commandExecutor
	promptEngine := s.promptEngine
	toolRegistry := s.toolRegistry
	coreMemoryProvider := s.coreMemoryProvider
	onboardingProvider := s.onboardingProvider
	s.mu.RUnlock()

	// Check if we have an adapter - publish error to bus so UI gets feedback
	if adapter == nil {
		log.Printf("[IntentRouter] No adapter configured for prompt %s", prompt.PromptID)
		s.publishError(prompt.SessionID, prompt.PromptID, ErrNoAdapter)
		return
	}

	if !adapter.Ready() {
		log.Printf("[IntentRouter] Adapter %s not ready for prompt %s", adapter.Name(), prompt.PromptID)
		s.publishError(prompt.SessionID, prompt.PromptID, ErrAdapterNotReady)
		return
	}

	// Build the intent request
	req := s.buildIntentRequest(prompt, sessionProvider, promptEngine, onboardingProvider)

	// Prepend core memory to system prompt (awareness injection).
	// Wrapped in <core-memory> tag so headings inside don't conflict with
	// the surrounding prompt template or other injected sections.
	if coreMemoryProvider != nil {
		if cm := coreMemoryProvider.CoreMemory(); cm != "" {
			req.SystemPrompt = "<core-memory>\n" + cm + "\n</core-memory>\n\n" + req.SystemPrompt
		}
	}

	// Inject BOOTSTRAP.md content for onboarding events.
	// Placed AFTER the prompt template (which references the ONBOARDING section).
	if req.EventType == EventTypeOnboarding {
		req.SystemPrompt = injectBootstrapContent(req.SystemPrompt, onboardingProvider)
	}

	// Inject available tools filtered by event type
	if toolRegistry != nil {
		req.AvailableTools = toolRegistry.GetToolsForEventType(req.EventType)
	}

	// Multi-turn tool-use loop (AD-13, NFR32)
	var toolHistory []ToolInteraction

	for iteration := 0; iteration < maxToolIterations; iteration++ {
		req.ToolHistory = toolHistory

		// Per-iteration timeout for adapter call
		resolveCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		response, err := adapter.ResolveIntent(resolveCtx, req)
		cancel()

		if err != nil {
			log.Printf("[IntentRouter] Adapter %s failed to resolve intent for prompt %s (iteration %d): %v",
				adapter.Name(), prompt.PromptID, iteration, err)
			s.publishError(prompt.SessionID, prompt.PromptID, err)
			return
		}

		// Check if AI wants to use tools
		if !hasToolUseAction(response) {
			// Warn about malformed tool-use response (one condition present but not both)
			if response != nil {
				hasToolAction := false
				for _, a := range response.Actions {
					if a.Type == ActionToolUse {
						hasToolAction = true
						break
					}
				}
				hasToolCalls := len(response.ToolCalls) > 0
				if hasToolAction != hasToolCalls {
					log.Printf("[IntentRouter] Malformed tool-use response for prompt %s: ActionToolUse=%v, ToolCalls=%d (both required for tool loop)",
						prompt.PromptID, hasToolAction, len(response.ToolCalls))
				}
			}
			// Final response — execute actions normally
			s.executeActions(ctx, prompt, response, sessionProvider, commandExecutor, req.SessionID)
			return
		}

		// Tool-use requires a registry
		if toolRegistry == nil {
			log.Printf("[IntentRouter] Adapter returned tool_use but no tool registry configured for prompt %s", prompt.PromptID)
			s.publishError(prompt.SessionID, prompt.PromptID, ErrNoToolRegistry)
			return
		}

		log.Printf("[IntentRouter] Tool iteration %d: %d calls for prompt %s", iteration, len(response.ToolCalls), prompt.PromptID)

		// Execute each tool call via the registry
		for _, tc := range response.ToolCalls {
			toolCtx, toolCancel := context.WithTimeout(ctx, requestTimeout)
			resultJSON, execErr := toolRegistry.Execute(toolCtx, tc.ToolName, json.RawMessage(tc.ArgumentsJSON))
			toolCancel()

			tr := ToolResult{
				CallID: tc.CallID,
			}
			if execErr != nil {
				errBytes, _ := json.Marshal(execErr.Error())
				tr.ResultJSON = fmt.Sprintf(`{"error":%s}`, errBytes)
				tr.IsError = true
				log.Printf("[IntentRouter] Tool %s failed for prompt %s: %v", tc.ToolName, prompt.PromptID, execErr)
			} else {
				tr.ResultJSON = string(resultJSON)
			}

			toolHistory = append(toolHistory, ToolInteraction{
				Call:   tc,
				Result: tr,
			})
		}
	}

	// Safety cap reached — publish error
	log.Printf("[IntentRouter] Max tool iterations (%d) reached for prompt %s", maxToolIterations, prompt.PromptID)
	s.publishError(prompt.SessionID, prompt.PromptID, ErrMaxToolIterations)
}

func (s *Service) buildIntentRequest(prompt eventbus.ConversationPromptEvent, provider SessionProvider, engine PromptEngine, onboarding OnboardingProvider) IntentRequest {
	// Determine event type from metadata
	eventType := EventTypeUserIntent
	if et, ok := prompt.Metadata[metadataKeyEventType]; ok {
		switch et {
		case constants.PromptEventSessionOutput:
			eventType = EventTypeSessionOutput
		case constants.PromptEventHistorySummary:
			eventType = EventTypeHistorySummary
		case constants.PromptEventClarification:
			eventType = EventTypeClarification
		case constants.PromptEventMemoryFlush:
			eventType = EventTypeMemoryFlush
		case constants.PromptEventHeartbeat:
			eventType = EventTypeHeartbeat
		case constants.PromptEventSessionSlug:
			eventType = EventTypeSessionSlug
		case constants.PromptEventOnboarding:
			eventType = EventTypeOnboarding
		}
	}

	// Override user_intent → onboarding when BOOTSTRAP.md exists AND this session
	// holds (or can claim) the onboarding lock. Other sessions stay as user_intent.
	if eventType == EventTypeUserIntent && onboarding != nil && onboarding.IsOnboardingSession(prompt.SessionID) {
		eventType = EventTypeOnboarding
	}

	// Use smart session routing to determine target session
	targetSession := s.selectTargetSession(prompt, provider)

	// Extract current tool: check metadata first, then our cache, then session provider
	var currentTool string
	if tool, ok := prompt.Metadata[metadataKeyCurrentTool]; ok {
		currentTool = tool
	} else if targetSession != "" {
		// Check our local tool cache (updated by tool change events)
		currentTool = s.getToolFromCache(targetSession)
	}
	// Fallback to session provider if still empty
	if currentTool == "" && provider != nil && targetSession != "" {
		if info, found := provider.GetSessionInfo(targetSession); found {
			currentTool = info.Tool
		}
	}

	req := IntentRequest{
		PromptID:            prompt.PromptID,
		SessionID:           targetSession,
		EventType:           eventType,
		Transcript:          prompt.NewMessage.Text,
		ConversationHistory: prompt.Context,
		CurrentTool:         currentTool,
		Metadata:            prompt.Metadata,
	}

	// Add smart routing hint to metadata
	if prompt.SessionID == "" && targetSession != "" {
		if req.Metadata == nil {
			req.Metadata = make(map[string]string)
		}
		req.Metadata[metadataKeySuggestedSession] = targetSession
		req.Metadata[metadataKeyRoutingHint] = metadataRoutingHintConversation
	}

	// Extract optional fields from metadata
	if sessionOutput, ok := prompt.Metadata[metadataKeySessionOutput]; ok {
		req.SessionOutput = sessionOutput
	}
	if clarificationQ, ok := prompt.Metadata[metadataKeyClarificationQuestion]; ok {
		req.ClarificationQuestion = clarificationQ
	}

	// Add available sessions if provider is configured
	if provider != nil {
		req.AvailableSessions = provider.ListSessionInfos()
	}

	// Build prompts using the engine if available
	if engine != nil {
		sessions := make([]SessionInfo, len(req.AvailableSessions))
		copy(sessions, req.AvailableSessions)

		buildReq := PromptBuildRequest{
			EventType:             eventType,
			SessionID:             targetSession, // Use smart-routed session, not original prompt.SessionID
			Transcript:            prompt.NewMessage.Text,
			History:               prompt.Context,
			AvailableSessions:     sessions,
			CurrentTool:           currentTool,
			SessionOutput:         req.SessionOutput,
			ClarificationQuestion: req.ClarificationQuestion,
			Metadata:              prompt.Metadata,
		}

		if resp, err := engine.Build(buildReq); err == nil && resp != nil {
			req.SystemPrompt = resp.SystemPrompt
			req.UserPrompt = resp.UserPrompt
		} else if err != nil {
			log.Printf("[IntentRouter] Failed to build prompts: %v", err)
		}
	}

	return req
}

func (s *Service) executeActions(ctx context.Context, prompt eventbus.ConversationPromptEvent, response *IntentResponse, sessionProvider SessionProvider, commandExecutor CommandExecutor, targetSession string) {
	if response == nil || len(response.Actions) == 0 {
		log.Printf("[IntentRouter] No actions from adapter for prompt %s", prompt.PromptID)
		// Publish acknowledgment so UI knows the prompt was processed
		replyMeta := s.buildReplyMetadata(prompt, map[string]string{
			constants.MetadataKeyStatus: "no_action",
		})
		s.publishReply(prompt.SessionID, prompt.PromptID, "", nil, replyMeta)
		return
	}

	for i, action := range response.Actions {
		switch action.Type {
		case ActionCommand:
			s.executeCommand(ctx, prompt, action, response, sessionProvider, commandExecutor)

		case ActionSpeak:
			s.executeSpeak(ctx, prompt, action, targetSession)

		case ActionClarify:
			s.executeClarify(ctx, prompt, action, targetSession)

		case ActionNoop:
			log.Printf("[IntentRouter] Noop action for prompt %s", prompt.PromptID)
			replyMeta := s.buildReplyMetadata(prompt, map[string]string{
				constants.MetadataKeyStatus: "noop",
			})
			s.publishReply(prompt.SessionID, prompt.PromptID, "", nil, replyMeta)

		case ActionToolUse:
			// Tool-use actions should be handled in the tool loop, not here.
			// If we reach this point, the adapter returned tool_use as a final action.
			log.Printf("[IntentRouter] Unexpected tool_use action in executeActions at index %d for prompt %s (should be handled in tool loop)",
				i, prompt.PromptID)
			s.publishError(prompt.SessionID, prompt.PromptID, ErrUnexpectedToolUse)

		default:
			log.Printf("[IntentRouter] Unknown action type %q at index %d for prompt %s",
				action.Type, i, prompt.PromptID)
		}
	}
}

// buildReplyMetadata creates reply metadata that preserves the event_type from the
// original prompt, allowing downstream consumers (e.g., conversation service) to
// identify summary replies and other non-standard event types.
func (s *Service) buildReplyMetadata(prompt eventbus.ConversationPromptEvent, extra map[string]string) map[string]string {
	meta := make(map[string]string, len(extra)+1)
	for k, v := range extra {
		meta[k] = v
	}
	if et, ok := prompt.Metadata[metadataKeyEventType]; ok && et != "" {
		meta[metadataKeyEventType] = et
	}
	return meta
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
			s.publishError(prompt.SessionID, prompt.PromptID, fmt.Errorf("session %s: %w", sessionRef, ErrSessionNotFound))
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

	// Update conversation session tracking - this session is now the context
	s.updateConversationSession(sessionRef)

	// Publish confirmation with command metadata
	replyMeta := s.buildReplyMetadata(prompt, map[string]string{
		constants.MetadataKeyStatus:    "command_queued",
		constants.MetadataKeySessionID: sessionRef,
	})
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
	}, replyMeta)

	log.Printf("[IntentRouter] Queued command for session %s: %s", sessionRef, truncate(action.Command, 50))
}

func (s *Service) executeSpeak(ctx context.Context, prompt eventbus.ConversationPromptEvent, action IntentAction, targetSession string) {
	if s.bus == nil || action.Text == "" {
		return
	}

	// Use targetSession from smart routing (may differ from prompt.SessionID for sessionless prompts)
	sessionID := targetSession
	if sessionID == "" {
		sessionID = prompt.SessionID // fallback to original if no target determined
	}

	// Publish speak event for TTS — merge language metadata from the prompt
	// so that TTS adapters receive the client's language preference.
	// Prompt language (from client header) intentionally overrides any
	// language keys that may already exist in action.Metadata.
	speakMeta := maputil.Clone(action.Metadata)
	for k, v := range prompt.Metadata {
		if strings.HasPrefix(k, constants.MetadataKeyLanguagePrefix) {
			if speakMeta == nil {
				speakMeta = make(map[string]string)
			}
			speakMeta[k] = v
		}
	}
	eventbus.Publish(ctx, s.bus, eventbus.Conversation.Speak, eventbus.SourceIntentRouter, eventbus.ConversationSpeakEvent{
		SessionID: sessionID,
		PromptID:  prompt.PromptID,
		Text:      action.Text,
		Metadata:  speakMeta,
	})

	// Also publish as conversation reply for history tracking
	replyMeta := s.buildReplyMetadata(prompt, map[string]string{
		constants.MetadataKeyStatus: "speak",
	})
	s.publishReply(sessionID, prompt.PromptID, action.Text, nil, replyMeta)

	log.Printf("[IntentRouter] Speak action for session %s: %s", sessionID, truncate(action.Text, 50))
}

func (s *Service) executeClarify(ctx context.Context, prompt eventbus.ConversationPromptEvent, action IntentAction, targetSession string) {
	if s.bus == nil || action.Text == "" {
		return
	}

	// Use targetSession from smart routing (may differ from prompt.SessionID for sessionless prompts)
	sessionID := targetSession
	if sessionID == "" {
		sessionID = prompt.SessionID // fallback to original if no target determined
	}

	// Publish speak event with clarification — include language for TTS
	clarifyMeta := map[string]string{constants.SpeakMetadataTypeKey: constants.SpeakTypeClarification}
	for k, v := range prompt.Metadata {
		if strings.HasPrefix(k, constants.MetadataKeyLanguagePrefix) {
			clarifyMeta[k] = v
		}
	}
	eventbus.Publish(ctx, s.bus, eventbus.Conversation.Speak, eventbus.SourceIntentRouter, eventbus.ConversationSpeakEvent{
		SessionID: sessionID,
		PromptID:  prompt.PromptID,
		Text:      action.Text,
		Metadata:  clarifyMeta,
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
	replyMeta := s.buildReplyMetadata(prompt, map[string]string{
		constants.MetadataKeyStatus: "clarification",
	})
	s.publishReply(sessionID, prompt.PromptID, action.Text, actions, replyMeta)

	log.Printf("[IntentRouter] Clarification requested for session %s: %s", sessionID, truncate(action.Text, 50))
}

func (s *Service) publishReply(sessionID, promptID, text string, actions []eventbus.ConversationAction, metadata map[string]string) {
	eventbus.Publish(context.Background(), s.bus, eventbus.Conversation.Reply, eventbus.SourceIntentRouter, eventbus.ConversationReplyEvent{
		SessionID: sessionID,
		PromptID:  promptID,
		Text:      text,
		Actions:   actions,
		Metadata:  metadata,
	})
}

func (s *Service) publishError(sessionID, promptID string, err error) {
	eventbus.Publish(context.Background(), s.bus, eventbus.Conversation.Reply, eventbus.SourceIntentRouter, eventbus.ConversationReplyEvent{
		SessionID: sessionID,
		PromptID:  promptID,
		Text:      fmt.Sprintf("Error: %v", err),
		Metadata: map[string]string{
			constants.MetadataKeyError:       constants.MetadataValueTrue,
			constants.MetadataKeyErrorType:   errorType(err),
			constants.MetadataKeyRecoverable: recoverableError(err),
		},
	})

	// Also publish speak for TTS feedback
	eventbus.Publish(context.Background(), s.bus, eventbus.Conversation.Speak, eventbus.SourceIntentRouter, eventbus.ConversationSpeakEvent{
		SessionID: sessionID,
		PromptID:  promptID,
		Text:      userFriendlyError(err),
		Metadata: map[string]string{
			constants.SpeakMetadataTypeKey: constants.SpeakTypeError,
			constants.MetadataKeyError:     err.Error(),
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
	case errors.Is(err, ErrMaxToolIterations):
		return "max_tool_iterations"
	case errors.Is(err, ErrUnexpectedToolUse):
		return "unexpected_tool_use"
	case errors.Is(err, ErrNoToolRegistry):
		return "no_tool_registry"
	default:
		return "unknown"
	}
}

func recoverableError(err error) string {
	switch {
	case errors.Is(err, ErrNoAdapter), errors.Is(err, ErrNoCommandExecutor), errors.Is(err, ErrAdapterNotReady):
		// Configuration issues that can be fixed by user action
		return "true"
	case errors.Is(err, ErrSessionNotFound), errors.Is(err, ErrMaxToolIterations), errors.Is(err, ErrUnexpectedToolUse):
		return "false"
	case errors.Is(err, ErrNoToolRegistry):
		return "true"
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
	case errors.Is(err, ErrMaxToolIterations):
		return "The AI tool-use loop exceeded the maximum number of iterations. Please try again."
	case errors.Is(err, ErrUnexpectedToolUse):
		return "An unexpected tool request was received. Please try again."
	case errors.Is(err, ErrNoToolRegistry):
		return "Tool use was requested but no tool registry is configured."
	default:
		return "An error occurred while processing your request."
	}
}

// AdapterStatus reports the current AI adapter binding.
type AdapterStatus struct {
	AdapterName  string
	AdapterReady bool
}

// AdapterStatus returns the current adapter binding status.
func (s *Service) AdapterStatus() AdapterStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var name string
	var ready bool
	if s.adapter != nil {
		name = s.adapter.Name()
		ready = s.adapter.Ready()
	}
	return AdapterStatus{
		AdapterName:  name,
		AdapterReady: ready,
	}
}

// hasToolUseAction returns true if the response indicates the AI wants to use tools.
// Both conditions must be true: at least one action with type ActionToolUse AND non-empty ToolCalls.
func hasToolUseAction(response *IntentResponse) bool {
	if response == nil || len(response.ToolCalls) == 0 {
		return false
	}
	for _, action := range response.Actions {
		if action.Type == ActionToolUse {
			return true
		}
	}
	return false
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// injectBootstrapContent appends BOOTSTRAP.md content wrapped in an
// <onboarding> XML tag. The tag boundary eliminates markdown heading
// hierarchy conflicts — any headings inside the tag are scoped to that
// section and don't interfere with the surrounding system prompt.
func injectBootstrapContent(systemPrompt string, provider OnboardingProvider) string {
	if provider == nil {
		return systemPrompt
	}
	bc := strings.TrimSpace(provider.BootstrapContent())
	if bc == "" {
		return systemPrompt
	}
	return systemPrompt + "\n\n<onboarding>\n" + bc + "\n</onboarding>"
}
