package contentpipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// Service transforms session output through optional pipeline plugins and
// republishes cleaned messages on the event bus.
type Service struct {
	bus         *eventbus.Bus
	pipelineDir string

	toolBySession sync.Map // sessionID -> tool name

	cancel context.CancelFunc
	wg     sync.WaitGroup

	subs []*eventbus.Subscription
}

// NewService creates a content pipeline service bound to the provided bus and
// plugin directory (future use for JS cleaners).
func NewService(bus *eventbus.Bus, pipelineDir string) *Service {
	return &Service{
		bus:         bus,
		pipelineDir: pipelineDir,
	}
}

// Start subscribes to session output/tool events and begins streaming cleaned
// messages. For now, the cleaner is a pass-through that emits UTF-8 text.
func (s *Service) Start(ctx context.Context) error {
	if s.bus == nil {
		return errors.New("content pipeline: event bus not configured")
	}

	if s.pipelineDir != "" {
		if err := ensureDir(s.pipelineDir); err != nil {
			return fmt.Errorf("content pipeline: ensure pipeline dir: %w", err)
		}
	}

	derivedCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	toolSub := s.bus.Subscribe(eventbus.TopicSessionsTool, eventbus.WithSubscriptionName("pipeline_tool"))
	outputSub := s.bus.Subscribe(eventbus.TopicSessionsOutput, eventbus.WithSubscriptionName("pipeline_output"))
	lifecycleSub := s.bus.Subscribe(eventbus.TopicSessionsLifecycle, eventbus.WithSubscriptionName("pipeline_lifecycle"))
	s.subs = []*eventbus.Subscription{toolSub, outputSub, lifecycleSub}

	s.wg.Add(3)
	go s.consumeToolEvents(derivedCtx, toolSub)
	go s.consumeOutputEvents(derivedCtx, outputSub)
	go s.consumeLifecycleEvents(derivedCtx, lifecycleSub)

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
			s.toolBySession.Store(payload.SessionID, payload.ToolName)
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

func (s *Service) handleSessionOutput(evt eventbus.SessionOutputEvent) {
	text := string(bytes.ReplaceAll(evt.Data, []byte("\r\n"), []byte("\n")))

	annotations := map[string]string{}
	if tool, ok := s.toolName(evt.SessionID); ok && tool != "" {
		annotations["tool"] = tool
	}
	if evt.Mode != "" {
		annotations["mode"] = evt.Mode
	}

	cleaned := eventbus.PipelineMessageEvent{
		SessionID:   evt.SessionID,
		Origin:      evt.Origin,
		Text:        text,
		Annotations: annotations,
		Sequence:    evt.Sequence,
	}

	s.bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: cleaned,
	})
}

func (s *Service) toolName(sessionID string) (string, bool) {
	val, ok := s.toolBySession.Load(sessionID)
	if !ok {
		return "", false
	}
	tool, _ := val.(string)
	return tool, true
}

func ensureDir(path string) error {
	if path == "" {
		return nil
	}
	return os.MkdirAll(filepath.Clean(path), 0o755)
}
