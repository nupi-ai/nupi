package contentpipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/plugins"
)

// Service transforms session output through optional pipeline plugins and
// republishes cleaned messages on the event bus.
type Service struct {
	bus        *eventbus.Bus
	pluginsSvc PipelineProvider

	toolBySession sync.Map // sessionID -> toolMetadata

	cancel context.CancelFunc
	wg     sync.WaitGroup

	subs []*eventbus.Subscription

	logger          *log.Logger
	metricsInterval time.Duration
	processedTotal  atomic.Uint64
	errorTotal      atomic.Uint64
}

// PipelineProvider exposes access to pipeline plugins.
type PipelineProvider interface {
	PipelinePluginFor(name string) (*plugins.PipelinePlugin, bool)
}

// Option configures optional behaviour on the Service.
type Option func(*Service)

// WithLogger overrides the logger used for observability output.
func WithLogger(logger *log.Logger) Option {
	return func(s *Service) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// WithMetricsInterval enables periodic metrics logging with the specified interval.
// A non-positive value disables the reporter.
func WithMetricsInterval(interval time.Duration) Option {
	return func(s *Service) {
		s.metricsInterval = interval
	}
}

// NewService creates a content pipeline service bound to the provided bus and
// plugins provider.
func NewService(bus *eventbus.Bus, provider PipelineProvider, opts ...Option) *Service {
	svc := &Service{
		bus:             bus,
		pluginsSvc:      provider,
		logger:          log.Default(),
		metricsInterval: 0,
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

	derivedCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	toolSub := s.bus.Subscribe(eventbus.TopicSessionsTool, eventbus.WithSubscriptionName("pipeline_tool"))
	outputSub := s.bus.Subscribe(eventbus.TopicSessionsOutput, eventbus.WithSubscriptionName("pipeline_output"))
	lifecycleSub := s.bus.Subscribe(eventbus.TopicSessionsLifecycle, eventbus.WithSubscriptionName("pipeline_lifecycle"))
	transcriptSub := s.bus.Subscribe(eventbus.TopicSpeechTranscriptFinal, eventbus.WithSubscriptionName("pipeline_transcripts"))
	s.subs = []*eventbus.Subscription{toolSub, outputSub, lifecycleSub, transcriptSub}

	s.wg.Add(4)
	go s.consumeToolEvents(derivedCtx, toolSub)
	go s.consumeOutputEvents(derivedCtx, outputSub)
	go s.consumeLifecycleEvents(derivedCtx, lifecycleSub)
	go s.consumeTranscriptEvents(derivedCtx, transcriptSub)
	s.startMetricsReporter(derivedCtx)

	return nil
}

// Shutdown stops subscriptions and waits for workers.
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
			payload, ok := env.Payload.(eventbus.SessionToolEvent)
			if !ok {
				continue
			}
			if payload.SessionID == "" {
				continue
			}
			s.toolBySession.Store(payload.SessionID, toolMetadata{ID: payload.ToolID, Name: payload.ToolName})
		}
	}
}

func (s *Service) consumeOutputEvents(ctx context.Context, sub *eventbus.Subscription) {
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
			payload, ok := env.Payload.(eventbus.SessionOutputEvent)
			if !ok {
				continue
			}
			s.handleSessionOutput(payload)
		}
	}
}

func (s *Service) consumeLifecycleEvents(ctx context.Context, sub *eventbus.Subscription) {
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
			payload, ok := env.Payload.(eventbus.SessionLifecycleEvent)
			if !ok {
				continue
			}
			if payload.SessionID == "" {
				continue
			}
			switch payload.State {
			case eventbus.SessionStateStopped:
				s.toolBySession.Delete(payload.SessionID)
			case eventbus.SessionStateCreated:
				// ensure a clean slate when a session is recreated
				s.toolBySession.Delete(payload.SessionID)
			}
		}
	}
}

func (s *Service) consumeTranscriptEvents(ctx context.Context, sub *eventbus.Subscription) {
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
			payload, ok := env.Payload.(eventbus.SpeechTranscriptEvent)
			if !ok {
				continue
			}
			s.handleTranscript(payload)
		}
	}
}

func (s *Service) handleSessionOutput(evt eventbus.SessionOutputEvent) {
	text := string(bytes.ReplaceAll(evt.Data, []byte("\r\n"), []byte("\n")))

	annotations := map[string]string{}
	if tool, ok := s.toolName(evt.SessionID); ok && tool != "" {
		annotations["tool"] = tool
	}
	if evt.Mode != "" {
		annotations["mode"] = evt.Mode
	}

	toolKey := annotations["tool"]
	if id, ok := s.toolID(evt.SessionID); ok && id != "" {
		annotations["tool_id"] = id
		toolKey = id
	}

	if plugin, ok := s.selectPlugin(toolKey); ok {
		newText, extraAnn, err := s.runPlugin(plugin, text, annotations)
		if err != nil {
			s.errorTotal.Add(1)
			log.Printf("[ContentPipeline] transform error for session %s: %v", evt.SessionID, err)
			s.bus.Publish(context.Background(), eventbus.Envelope{
				Topic:  eventbus.TopicPipelineError,
				Source: eventbus.SourceContentPipeline,
				Payload: eventbus.PipelineErrorEvent{
					SessionID:   evt.SessionID,
					Stage:       plugin.Name,
					Message:     err.Error(),
					Recoverable: true,
				},
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

	s.processedTotal.Add(1)
	cleaned := eventbus.PipelineMessageEvent{
		SessionID:   evt.SessionID,
		Origin:      evt.Origin,
		Text:        text,
		Annotations: annotations,
		Sequence:    evt.Sequence,
	}

	s.publishPipelineMessage(cleaned)
}

func (s *Service) handleTranscript(evt eventbus.SpeechTranscriptEvent) {
	if evt.SessionID == "" {
		return
	}
	if strings.TrimSpace(evt.Text) == "" {
		return
	}

	annotations := map[string]string{
		"input_source": "voice",
	}
	if evt.StreamID != "" {
		annotations["stream_id"] = evt.StreamID
	}
	if evt.Confidence > 0 {
		annotations["confidence"] = fmt.Sprintf("%.3f", evt.Confidence)
	}
	for k, v := range evt.Metadata {
		if v == "" {
			continue
		}
		annotations[k] = v
	}

	s.processedTotal.Add(1)
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
	s.bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: evt,
	})
}

// Metrics aggregates processed/error counters.
type Metrics struct {
	Processed uint64
	Errors    uint64
}

// Metrics returns the current metrics snapshot.
func (s *Service) Metrics() Metrics {
	return Metrics{
		Processed: s.processedTotal.Load(),
		Errors:    s.errorTotal.Load(),
	}
}

func (s *Service) startMetricsReporter(ctx context.Context) {
	if s.metricsInterval <= 0 {
		return
	}
	logger := s.logger
	if logger == nil {
		logger = log.Default()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.metricsInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				metrics := s.Metrics()
				logger.Printf("[ContentPipeline] metrics processed_total=%d error_total=%d", metrics.Processed, metrics.Errors)
			}
		}
	}()
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

func (s *Service) selectPlugin(tool string) (*plugins.PipelinePlugin, bool) {
	if s.pluginsSvc == nil {
		return nil, false
	}
	if plugin, ok := s.pluginsSvc.PipelinePluginFor(tool); ok {
		return plugin, true
	}
	return s.pluginsSvc.PipelinePluginFor("default")
}

func (s *Service) runPlugin(plugin *plugins.PipelinePlugin, text string, annotations map[string]string) (string, map[string]string, error) {
	if plugin == nil {
		return text, nil, nil
	}

	vm := goja.New()
	exports := vm.NewObject()
	vm.Set("module", vm.NewObject())
	vm.Set("exports", exports)

	if _, err := vm.RunString(plugin.Source); err != nil {
		return text, nil, fmt.Errorf("run plugin %s: %w", plugin.Name, err)
	}

	moduleObj := vm.Get("module")
	if moduleObj == nil {
		moduleObj = vm.Get("exports")
	} else {
		moduleExports := moduleObj.ToObject(vm).Get("exports")
		if moduleExports != nil {
			exports = moduleExports.ToObject(vm)
		}
	}

	transform := exports.Get("transform")
	fn, ok := goja.AssertFunction(transform)
	if !ok {
		return text, nil, fmt.Errorf("plugin %s: transform is not function", plugin.Name)
	}

	input := map[string]interface{}{
		"text":        text,
		"annotations": copyStringMap(annotations),
	}

	result, err := fn(goja.Undefined(), vm.ToValue(input))
	if err != nil {
		return text, nil, fmt.Errorf("plugin %s transform: %w", plugin.Name, err)
	}

	switch exported := result.Export().(type) {
	case nil:
		return text, nil, nil
	case string:
		return exported, nil, nil
	case map[string]interface{}:
		newText := text
		if v, ok := exported["text"].(string); ok {
			newText = v
		}
		var extra map[string]string
		if annVal, ok := exported["annotations"]; ok {
			extra = toStringMap(annVal)
		}
		return newText, extra, nil
	default:
		return text, nil, fmt.Errorf("plugin %s returned unsupported type %T", plugin.Name, exported)
	}
}

func copyStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return map[string]string{}
	}
	dup := make(map[string]string, len(src))
	for k, v := range src {
		dup[k] = v
	}
	return dup
}

func toStringMap(val interface{}) map[string]string {
	if val == nil {
		return nil
	}
	result := make(map[string]string)
	switch typed := val.(type) {
	case map[string]interface{}:
		for k, v := range typed {
			if str, ok := v.(string); ok {
				result[k] = str
			}
		}
	case map[string]string:
		for k, v := range typed {
			result[k] = v
		}
	}
	return result
}

type toolMetadata struct {
	ID   string
	Name string
}
