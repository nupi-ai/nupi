package contentpipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/jsruntime"
	pipelinecleaners "github.com/nupi-ai/nupi/internal/plugins/pipeline_cleaners"
	toolhandlers "github.com/nupi-ai/nupi/internal/plugins/tool_handlers"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
)

// Service transforms session output through optional pipeline plugins and
// republishes cleaned messages on the event bus.
type Service struct {
	bus        *eventbus.Bus
	pluginsSvc PipelineProvider

	toolBySession sync.Map // sessionID -> toolMetadata

	// Output buffering per session for idle detection
	buffers    sync.Map    // sessionID -> *OutputBuffer
	idleTimers sync.Map    // sessionID -> *time.Timer
	stopped    atomic.Bool // prevents timer callbacks after shutdown

	// Last-seen metadata per session for tool-change flush
	// When a tool changes, we need to preserve the Sequence/Mode from the last
	// output event to ensure proper ordering in the conversation
	lastSeenBySession sync.Map // sessionID -> lastSeenMeta

	lifecycle eventbus.ServiceLifecycle

	toolSub       *eventbus.TypedSubscription[eventbus.SessionToolEvent]
	outputSub     *eventbus.TypedSubscription[eventbus.SessionOutputEvent]
	lifecycleSub  *eventbus.TypedSubscription[eventbus.SessionLifecycleEvent]
	transcriptSub *eventbus.TypedSubscription[eventbus.SpeechTranscriptEvent]

	logger *log.Logger
}

// lastSeenMeta stores metadata from the most recent output event for a session.
// Used to preserve Sequence/Mode when flushing on tool change.
type lastSeenMeta struct {
	Sequence uint64
	Mode     string
}

// PipelineProvider exposes access to pipeline plugins and JS runtime.
type PipelineProvider interface {
	PipelinePluginFor(name string) (*pipelinecleaners.PipelinePlugin, bool)
	ToolHandlerPluginFor(name string) (*toolhandlers.JSPlugin, bool)
	JSRuntime() *jsruntime.Runtime
}

// Option configures optional behaviour on the Service.
type Option func(*Service)

// WithLogger overrides the logger used for service output.
func WithLogger(logger *log.Logger) Option {
	return func(s *Service) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// NewService creates a content pipeline service bound to the provided bus and
// plugins provider.
func NewService(bus *eventbus.Bus, provider PipelineProvider, opts ...Option) *Service {
	svc := &Service{
		bus:        bus,
		pluginsSvc: provider,
		logger:     log.Default(),
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// Start subscribes to session output/tool events and begins streaming cleaned
// messages. For now, the cleaner is a pass-through that emits UTF-8 text.
func (s *Service) Start(ctx context.Context) error {
	if s.bus == nil {
		return errors.New("content pipeline: event bus not configured")
	}

	s.lifecycle.Start(ctx)
	s.stopped.Store(false)

	s.toolSub = eventbus.Subscribe[eventbus.SessionToolEvent](s.bus, eventbus.TopicSessionsTool, eventbus.WithSubscriptionName("pipeline_tool"))
	s.outputSub = eventbus.Subscribe[eventbus.SessionOutputEvent](s.bus, eventbus.TopicSessionsOutput, eventbus.WithSubscriptionName("pipeline_output"))
	s.lifecycleSub = eventbus.Subscribe[eventbus.SessionLifecycleEvent](s.bus, eventbus.TopicSessionsLifecycle, eventbus.WithSubscriptionName("pipeline_lifecycle"))
	s.transcriptSub = eventbus.Subscribe[eventbus.SpeechTranscriptEvent](s.bus, eventbus.TopicSpeechTranscriptFinal, eventbus.WithSubscriptionName("pipeline_transcripts"))
	s.lifecycle.AddSubscriptions(s.toolSub, s.outputSub, s.lifecycleSub, s.transcriptSub)

	s.lifecycle.Go(s.consumeToolEvents)
	s.lifecycle.Go(s.consumeOutputEvents)
	s.lifecycle.Go(s.consumeLifecycleEvents)
	s.lifecycle.Go(s.consumeTranscriptEvents)

	return nil
}

// Shutdown stops subscriptions and waits for workers.
func (s *Service) Shutdown(ctx context.Context) error {
	// Set stopped flag first to prevent any new timer callbacks from executing
	s.stopped.Store(true)

	s.lifecycle.Stop()

	// Stop all idle timers to prevent events after shutdown
	s.idleTimers.Range(func(key, value any) bool {
		if timer, ok := value.(*time.Timer); ok {
			timer.Stop()
		}
		s.idleTimers.Delete(key)
		return true
	})

	// Clear all buffers and metadata
	s.buffers.Range(func(key, _ any) bool {
		s.buffers.Delete(key)
		return true
	})
	s.lastSeenBySession.Range(func(key, _ any) bool {
		s.lastSeenBySession.Delete(key)
		return true
	})

	return s.lifecycle.Wait(ctx)
}

func (s *Service) consumeToolEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.toolSub, nil, func(payload eventbus.SessionToolEvent) {
		if payload.SessionID == "" {
			return
		}
		// Check if tool changed - if so, flush current buffer to avoid mixing output
		// from different tools in the same buffer
		if oldVal, exists := s.toolBySession.Load(payload.SessionID); exists {
			oldMeta := oldVal.(toolMetadata)
			newToolKey := payload.ToolID
			if newToolKey == "" {
				newToolKey = payload.ToolName
			}
			oldToolKey := oldMeta.ID
			if oldToolKey == "" {
				oldToolKey = oldMeta.Name
			}
			// If tool actually changed, flush the buffer
			if newToolKey != oldToolKey && oldToolKey != "" {
				s.flushOnToolChange(ctx, payload.SessionID, oldMeta)
			}
		}
		s.toolBySession.Store(payload.SessionID, toolMetadata{ID: payload.ToolID, Name: payload.ToolName})
	})
}

func (s *Service) consumeOutputEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.outputSub, nil, func(event eventbus.SessionOutputEvent) {
		s.handleSessionOutput(ctx, event)
	})
}

func (s *Service) consumeLifecycleEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.lifecycleSub, nil, func(payload eventbus.SessionLifecycleEvent) {
		if payload.SessionID == "" {
			return
		}
		switch payload.State {
		case eventbus.SessionStateStopped:
			s.toolBySession.Delete(payload.SessionID)
			s.cleanupSessionBuffer(payload.SessionID)
		case eventbus.SessionStateCreated:
			// ensure a clean slate when a session is recreated
			s.toolBySession.Delete(payload.SessionID)
			s.cleanupSessionBuffer(payload.SessionID)
		}
	})
}

func (s *Service) consumeTranscriptEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.transcriptSub, nil, s.handleTranscript)
}

func (s *Service) handleSessionOutput(ctx context.Context, evt eventbus.SessionOutputEvent) {
	// Skip empty session ID - would create orphan buffer/timer
	if evt.SessionID == "" {
		return
	}

	// Store last-seen metadata for tool-change flush
	// This preserves Sequence/Mode even when flushOnToolChange creates synthetic event
	s.lastSeenBySession.Store(evt.SessionID, lastSeenMeta{
		Sequence: evt.Sequence,
		Mode:     evt.Mode,
	})

	// Get or create buffer for this session
	buf := s.getOrCreateBuffer(evt.SessionID)

	// Append chunk to buffer
	buf.Write(evt.Data)

	// Capture tool info NOW to freeze it for this buffer's lifetime
	// This prevents race where tool changes between buffering and flush
	toolName, _ := s.toolName(evt.SessionID)
	toolID, _ := s.toolID(evt.SessionID)
	toolKey := toolID
	if toolKey == "" {
		toolKey = toolName
	}

	// Get tool handler plugin for idle detection
	var handler *toolhandlers.JSPlugin
	if s.pluginsSvc != nil && toolKey != "" {
		handler, _ = s.pluginsSvc.ToolHandlerPluginFor(toolKey)
	}

	// Check if tool is in idle state (waiting for input)
	if handler != nil && handler.HasDetectIdleState {
		rt := s.pluginsSvc.JSRuntime()
		if rt != nil {
			idleState, err := handler.DetectIdleState(ctx, rt, buf.Peek())
			if err == nil && idleState != nil && idleState.IsIdle {
				// Tool is waiting for input - flush and process
				s.flushAndProcess(ctx, evt.SessionID, buf, handler, idleState, evt, toolName, toolID)
				return
			}
		}
	}

	// Reset idle timer - will fire if no more output arrives
	// Capture toolName/toolID in closure to freeze them for when timer fires
	// Pass generation to detect race where new chunk arrives just as timer fires
	currentGen := buf.Generation()
	s.resetIdleTimer(evt.SessionID, buf, currentGen, func() {
		idleState := &toolhandlers.IdleState{
			IsIdle: true,
			Reason: "timeout",
		}
		s.flushAndProcess(ctx, evt.SessionID, buf, handler, idleState, evt, toolName, toolID)
	})
}

// getOrCreateBuffer returns the output buffer for a session, creating one if needed.
func (s *Service) getOrCreateBuffer(sessionID string) *OutputBuffer {
	if val, ok := s.buffers.Load(sessionID); ok {
		return val.(*OutputBuffer)
	}

	buf := NewOutputBuffer()
	actual, loaded := s.buffers.LoadOrStore(sessionID, buf)
	if loaded {
		return actual.(*OutputBuffer)
	}
	return buf
}

// resetIdleTimer resets or creates the idle timer for a session.
// The buf and expectedGen parameters are used to detect if new data arrived
// between timer scheduling and firing (generation race).
// Uses the buffer's configured idleTimeout instead of the global default.
func (s *Service) resetIdleTimer(sessionID string, buf *OutputBuffer, expectedGen uint64, onIdle func()) {
	// Cancel existing timer
	if val, ok := s.idleTimers.Load(sessionID); ok {
		if timer, ok := val.(*time.Timer); ok {
			timer.Stop()
		}
	}

	// Use buffer's configured idle timeout
	timeout := buf.IdleTimeout()

	// Create new timer with guard against post-shutdown execution and generation race
	timer := time.AfterFunc(timeout, func() {
		// Check stopped flag to prevent events after shutdown
		// This handles the race where timer fires just as Shutdown() runs
		if s.stopped.Load() {
			return
		}
		// Check generation to detect if new data arrived since timer was set.
		// If generation changed, a new timer was/will be set, so skip this callback.
		if buf.Generation() != expectedGen {
			return
		}
		onIdle()
	})
	s.idleTimers.Store(sessionID, timer)
}

// cancelIdleTimer cancels the idle timer for a session.
func (s *Service) cancelIdleTimer(sessionID string) {
	if val, ok := s.idleTimers.LoadAndDelete(sessionID); ok {
		if timer, ok := val.(*time.Timer); ok {
			timer.Stop()
		}
	}
}

// flushOnToolChange flushes the buffer when a tool changes to avoid mixing
// output from different tools in the same buffer. Uses the normal processing
// pipeline (clean/extractEvents/summarize) for consistency.
func (s *Service) flushOnToolChange(ctx context.Context, sessionID string, oldTool toolMetadata) {
	val, ok := s.buffers.Load(sessionID)
	if !ok {
		return
	}
	buf := val.(*OutputBuffer)
	if buf.IsEmpty() {
		return
	}

	// Get handler for the OLD tool (we're processing its output)
	toolKey := oldTool.ID
	if toolKey == "" {
		toolKey = oldTool.Name
	}
	var handler *toolhandlers.JSPlugin
	if s.pluginsSvc != nil && toolKey != "" {
		handler, _ = s.pluginsSvc.ToolHandlerPluginFor(toolKey)
	}

	// Create idle state for tool_change
	idleState := &toolhandlers.IdleState{
		IsIdle: true,
		Reason: "tool_change",
	}

	// Retrieve last-seen metadata for Sequence/Mode
	var seq uint64
	var mode string
	if meta, ok := s.lastSeenBySession.Load(sessionID); ok {
		m := meta.(lastSeenMeta)
		seq, mode = m.Sequence, m.Mode
	}

	// Create event with proper Origin and preserved Sequence/Mode
	evt := eventbus.SessionOutputEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Sequence:  seq,
		Mode:      mode,
	}

	// Use normal processing pipeline with tool_changed flag
	s.flushAndProcess(ctx, sessionID, buf, handler, idleState, evt, oldTool.Name, oldTool.ID)
}

// cleanupSessionBuffer removes buffer, timer, and metadata for a session.
func (s *Service) cleanupSessionBuffer(sessionID string) {
	s.cancelIdleTimer(sessionID)
	s.buffers.Delete(sessionID)
	s.lastSeenBySession.Delete(sessionID)
}

// flushAndProcess flushes the buffer and processes the output.
// toolName and toolID are passed in frozen from handleSessionOutput to avoid race conditions.
func (s *Service) flushAndProcess(ctx context.Context, sessionID string, buf *OutputBuffer, handler *toolhandlers.JSPlugin, idleState *toolhandlers.IdleState, evt eventbus.SessionOutputEvent, toolName, toolID string) {
	// Cancel idle timer
	s.cancelIdleTimer(sessionID)

	// Flush buffer (returns text and overflow status)
	text, wasOverflowed := buf.Flush()
	if text == "" {
		return
	}

	annotations := map[string]string{}

	// Mark if buffer was truncated due to overflow (AI sees incomplete context)
	if wasOverflowed {
		annotations[constants.MetadataKeyBufferTruncated] = constants.MetadataValueTrue
		annotations[constants.MetadataKeyBufferMaxSize] = fmt.Sprintf("%d", buf.MaxSize())
	}

	// Use frozen tool info passed from handleSessionOutput (avoids race condition)
	if toolName != "" {
		annotations[constants.MetadataKeyTool] = toolName
	}
	if evt.Mode != "" {
		annotations[constants.MetadataKeyMode] = evt.Mode
	}

	toolKey := toolName
	if toolID != "" {
		annotations[constants.MetadataKeyToolID] = toolID
		toolKey = toolID
	}

	// Get JS runtime (guard against nil pluginsSvc in tests)
	var rt *jsruntime.Runtime
	if s.pluginsSvc != nil {
		rt = s.pluginsSvc.JSRuntime()
	}

	// Apply tool-specific cleaning via handler plugin
	if handler != nil && handler.HasClean && rt != nil {
		if cleaned, err := handler.Clean(ctx, rt, text); err == nil {
			text = cleaned
		} else {
			log.Printf("[ContentPipeline] Clean error for session %s: %v", sessionID, err)
		}
	}

	// Extract notable events via handler plugin
	// Use flat fields to avoid JSON truncation in meta (512 rune limit per value)
	if handler != nil && handler.HasExtractEvents && rt != nil {
		events, err := handler.ExtractEvents(ctx, rt, text)
		if err != nil {
			log.Printf("[ContentPipeline] ExtractEvents error for session %s: %v", sessionID, err)
		} else if len(events) > 0 {
			annotations[constants.MetadataKeyNotable] = constants.MetadataValueTrue
			annotations[constants.MetadataKeyEventType] = constants.PromptEventSessionOutput
			annotations[constants.MetadataKeyEventCount] = fmt.Sprintf("%d", len(events))

			// Store first event details in flat fields (most important event)
			first := events[0]
			if first.Severity != "" {
				annotations[constants.MetadataKeySeverity] = first.Severity
			}
			if first.Title != "" {
				annotations[constants.MetadataKeyEventTitle] = first.Title
			}
			if first.Details != "" {
				// Truncate details to avoid exceeding meta limits (use runes for UTF-8 safety)
				details := first.Details
				runes := []rune(details)
				if len(runes) > 200 {
					details = string(runes[:200]) + "..."
				}
				annotations[constants.MetadataKeyEventDetails] = details
			}
			if first.ActionSuggestion != "" {
				annotations[constants.MetadataKeyEventAction] = first.ActionSuggestion
			}
		}
	}

	// Add idle state info and mark as notable when waiting for user input
	if idleState != nil {
		annotations[constants.MetadataKeyIdleState] = idleState.Reason
		// Mark tool_changed for flush triggered by tool change
		if idleState.Reason == "tool_change" {
			annotations[constants.MetadataKeyToolChanged] = constants.MetadataValueTrue
		}
		if idleState.WaitingFor != "" {
			annotations[constants.MetadataKeyWaitingFor] = idleState.WaitingFor
			// Tool is waiting for user interaction - mark as notable to trigger AI
			// This ensures proactive AI response when tool prompts for input
			if annotations[constants.MetadataKeyNotable] != constants.MetadataValueTrue {
				annotations[constants.MetadataKeyNotable] = constants.MetadataValueTrue
				annotations[constants.MetadataKeyEventType] = constants.PromptEventSessionOutput
			}
		}
		if idleState.PromptText != "" {
			annotations[constants.MetadataKeyPromptText] = idleState.PromptText
		}
	}

	// Run pipeline cleaner plugin (existing functionality)
	if plugin, ok := s.selectPlugin(toolKey); ok {
		newText, extraAnn, err := s.runPlugin(ctx, plugin, text, annotations)
		if err != nil {
			log.Printf("[ContentPipeline] transform error for session %s: %v", sessionID, err)
			eventbus.Publish(context.Background(), s.bus, eventbus.Pipeline.Error, eventbus.SourceContentPipeline, eventbus.PipelineErrorEvent{
				SessionID:   sessionID,
				Stage:       plugin.Name,
				Message:     err.Error(),
				Recoverable: true,
			})
		} else {
			text = newText
			if extraAnn != nil {
				for k, v := range extraAnn {
					if v == "" {
						delete(annotations, k)
						continue
					}
					annotations[k] = v
				}
			}
		}
	}

	cleaned := eventbus.PipelineMessageEvent{
		SessionID:   sessionID,
		Origin:      evt.Origin,
		Text:        text,
		Annotations: annotations,
		Sequence:    evt.Sequence,
	}

	s.publishPipelineMessage(cleaned)
}

func (s *Service) handleTranscript(evt eventbus.SpeechTranscriptEvent) {
	if strings.TrimSpace(evt.Text) == "" {
		return
	}

	// Per architecture 4.4.2: sessionless transcripts (SessionID="") are valid
	// and should be passed to conversation.Service for GlobalStore handling.
	// This enables "sessionless" voice commands like asking about available
	// sessions, general questions, or system commands.

	annotations := map[string]string{
		constants.MetadataKeyInputSource: "voice",
	}
	if evt.SessionID == "" {
		annotations[constants.MetadataKeySessionless] = constants.MetadataValueTrue
	}
	if evt.StreamID != "" {
		annotations[constants.MetadataKeyStreamID] = evt.StreamID
	}
	if evt.Confidence > 0 {
		annotations[constants.MetadataKeyConfidence] = fmt.Sprintf("%.3f", evt.Confidence)
	}
	for k, v := range evt.Metadata {
		if v == "" {
			continue
		}
		annotations[k] = v
	}

	cleaned := eventbus.PipelineMessageEvent{
		SessionID:   evt.SessionID,
		Origin:      eventbus.OriginUser,
		Text:        evt.Text,
		Annotations: annotations,
		Sequence:    evt.Sequence,
	}

	s.publishPipelineMessage(cleaned)
}

func (s *Service) publishPipelineMessage(evt eventbus.PipelineMessageEvent) {
	eventbus.Publish(context.Background(), s.bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, evt)
}

func (s *Service) toolName(sessionID string) (string, bool) {
	val, ok := s.toolBySession.Load(sessionID)
	if !ok {
		return "", false
	}
	meta, _ := val.(toolMetadata)
	if meta.Name != "" {
		return meta.Name, true
	}
	if meta.ID != "" {
		return meta.ID, true
	}
	return "", false
}

func (s *Service) toolID(sessionID string) (string, bool) {
	val, ok := s.toolBySession.Load(sessionID)
	if !ok {
		return "", false
	}
	meta, _ := val.(toolMetadata)
	if meta.ID != "" {
		return meta.ID, true
	}
	return "", false
}

func (s *Service) selectPlugin(tool string) (*pipelinecleaners.PipelinePlugin, bool) {
	if s.pluginsSvc == nil {
		return nil, false
	}
	if plugin, ok := s.pluginsSvc.PipelinePluginFor(tool); ok {
		return plugin, true
	}
	return s.pluginsSvc.PipelinePluginFor("default")
}

func (s *Service) runPlugin(ctx context.Context, plugin *pipelinecleaners.PipelinePlugin, text string, annotations map[string]string) (string, map[string]string, error) {
	if plugin == nil {
		return text, nil, nil
	}

	rt := s.pluginsSvc.JSRuntime()
	if rt == nil {
		return text, nil, fmt.Errorf("plugin %s: jsruntime not available", plugin.Name)
	}

	input := pipelinecleaners.TransformInput{
		Text:        text,
		Annotations: maputil.Clone(annotations),
	}
	if input.Annotations == nil {
		input.Annotations = map[string]string{}
	}

	output, err := plugin.Transform(ctx, rt, input)
	if err != nil {
		return text, nil, err
	}

	return output.Text, output.Annotations, nil
}

type toolMetadata struct {
	ID   string
	Name string
}
