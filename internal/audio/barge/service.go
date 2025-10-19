package barge

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// Option configures the coordinator behaviour.
type Option func(*Service)

// WithLogger overrides the logger used for diagnostics.
func WithLogger(logger *log.Logger) Option {
	return func(s *Service) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// WithConfidenceThreshold sets the minimum VAD confidence required to trigger a barge-in.
func WithConfidenceThreshold(threshold float32) Option {
	return func(s *Service) {
		if threshold >= 0 && threshold <= 1 {
			s.minConfidence = threshold
		}
	}
}

// WithCooldown configures the minimum duration between consecutive barge-in events per stream.
func WithCooldown(cooldown time.Duration) Option {
	return func(s *Service) {
		if cooldown >= 0 {
			s.cooldown = cooldown
		}
	}
}

// WithQuietPeriod configures the window after playback stops during which VAD events are ignored.
func WithQuietPeriod(period time.Duration) Option {
	return func(s *Service) {
		if period >= 0 {
			s.quietPeriod = period
		}
	}
}

// Service coordinates barge-in signals from VAD and client interrupts.
type Service struct {
	bus *eventbus.Bus

	logger        *log.Logger
	minConfidence float32
	cooldown      time.Duration
	quietPeriod   time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	vadSub      *eventbus.Subscription
	clientSub   *eventbus.Subscription
	playbackSub *eventbus.Subscription
	wg          sync.WaitGroup

	mu             sync.Mutex
	lastEvent      map[string]time.Time
	playback       map[string]playbackState
	sessionStreams map[string]string

	bargeInTotal atomic.Uint64
}

const (
	defaultConfidence  = 0.35
	defaultCooldown    = 750 * time.Millisecond
	defaultQuietPeriod = 500 * time.Millisecond

	maxMetadataEntries    = 32
	maxMetadataKeyRunes   = 64
	maxMetadataValueRunes = 512
)

// New constructs a barge-in coordinator bound to the provided event bus.
func New(bus *eventbus.Bus, opts ...Option) *Service {
	svc := &Service{
		bus:            bus,
		logger:         log.Default(),
		minConfidence:  defaultConfidence,
		cooldown:       defaultCooldown,
		quietPeriod:    defaultQuietPeriod,
		lastEvent:      make(map[string]time.Time),
		playback:       make(map[string]playbackState),
		sessionStreams: make(map[string]string),
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// Start subscribes to speech detection events.
func (s *Service) Start(ctx context.Context) error {
	if s.bus == nil {
		return nil
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.vadSub = s.bus.Subscribe(eventbus.TopicSpeechVADDetected, eventbus.WithSubscriptionName("barge_vad"))
	s.clientSub = s.bus.Subscribe(eventbus.TopicAudioInterrupt, eventbus.WithSubscriptionName("barge_client"))
	s.playbackSub = s.bus.Subscribe(eventbus.TopicAudioEgressPlayback, eventbus.WithSubscriptionName("barge_playback"))
	s.wg.Add(3)
	go s.consumeVAD()
	go s.consumeClient()
	go s.consumePlayback()
	return nil
}

// Shutdown stops background processing.
func (s *Service) Shutdown(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.vadSub != nil {
		s.vadSub.Close()
	}
	if s.clientSub != nil {
		s.clientSub.Close()
	}
	if s.playbackSub != nil {
		s.playbackSub.Close()
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

func (s *Service) consumeVAD() {
	defer s.wg.Done()
	if s.vadSub == nil {
		return
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case env, ok := <-s.vadSub.C():
			if !ok {
				return
			}
			event, ok := env.Payload.(eventbus.SpeechVADEvent)
			if !ok {
				continue
			}
			s.handleVADEvent(event)
		}
	}
}

func (s *Service) consumeClient() {
	defer s.wg.Done()
	if s.clientSub == nil {
		return
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case env, ok := <-s.clientSub.C():
			if !ok {
				return
			}
			event, ok := env.Payload.(eventbus.AudioInterruptEvent)
			if !ok {
				continue
			}
			s.handleClientEvent(event)
		}
	}
}

func (s *Service) consumePlayback() {
	defer s.wg.Done()
	if s.playbackSub == nil {
		return
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case env, ok := <-s.playbackSub.C():
			if !ok {
				return
			}
			event, ok := env.Payload.(eventbus.AudioEgressPlaybackEvent)
			if !ok {
				continue
			}
			ts := env.Timestamp
			if ts.IsZero() {
				ts = time.Now().UTC()
			}
			s.handlePlaybackEvent(event, ts)
		}
	}
}

func (s *Service) handleVADEvent(event eventbus.SpeechVADEvent) {
	if !event.Active {
		return
	}
	if event.Confidence < s.minConfidence {
		return
	}

	ts := event.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	streamID, ok := s.targetStreamForSession(event.SessionID, ts)
	if !ok {
		return
	}

	key := streamKey(event.SessionID, streamID)
	if !s.registerTrigger(key, ts) {
		return
	}

	meta := cloneMetadata(event.Metadata)
	if event.StreamID != "" {
		if meta == nil {
			meta = map[string]string{"vad_stream_id": event.StreamID}
		} else {
			meta["vad_stream_id"] = event.StreamID
		}
	}

	s.publishBargeIn(event.SessionID, streamID, ts, "vad_detected", event.Confidence, meta)
}

func (s *Service) handleClientEvent(event eventbus.AudioInterruptEvent) {
	if event.SessionID == "" {
		return
	}

	ts := event.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	streamID := strings.TrimSpace(event.StreamID)
	if streamID == "" {
		if resolved, ok := s.targetStreamForSession(event.SessionID, ts); ok {
			streamID = resolved
		}
	}
	if streamID == "" {
		return
	}

	key := streamKey(event.SessionID, streamID)
	if !s.registerTrigger(key, ts) {
		return
	}

	conf := float32(1.0)
	s.publishBargeIn(event.SessionID, streamID, ts, reasonOrDefault(event.Reason, "client_interrupt"), conf, event.Metadata)
}

func (s *Service) handlePlaybackEvent(event eventbus.AudioEgressPlaybackEvent, ts time.Time) {
	key := streamKey(event.SessionID, event.StreamID)

	s.mu.Lock()
	state := s.playback[key]
	if event.Final {
		state.playing = false
		state.quietUntil = ts.Add(s.quietPeriod)
		if current, ok := s.sessionStreams[event.SessionID]; ok && current == event.StreamID {
			delete(s.sessionStreams, event.SessionID)
		}
	} else {
		state.playing = true
		state.quietUntil = time.Time{}
		s.sessionStreams[event.SessionID] = event.StreamID
	}
	s.playback[key] = state
	s.mu.Unlock()
}

func (s *Service) targetStreamForSession(sessionID string, ts time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if streamID, ok := s.sessionStreams[sessionID]; ok && streamID != "" {
		state := s.playback[streamKey(sessionID, streamID)]
		if state.playing {
			return streamID, true
		}
		if !state.quietUntil.IsZero() && ts.Before(state.quietUntil) {
			return "", false
		}
	}

	for key, state := range s.playback {
		sid, stream := splitStreamKey(key)
		if sid != sessionID {
			continue
		}
		if state.playing {
			s.sessionStreams[sessionID] = stream
			return stream, true
		}
		if !state.quietUntil.IsZero() && ts.Before(state.quietUntil) {
			return "", false
		}
	}

	return "", false
}

func (s *Service) registerTrigger(key string, ts time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.lastEvent[key]
	if !last.IsZero() && ts.Sub(last) < s.cooldown {
		return false
	}
	s.lastEvent[key] = ts
	return true
}

func (s *Service) publishBargeIn(sessionID, streamID string, ts time.Time, reason string, confidence float32, meta map[string]string) {
	if s.bus == nil {
		return
	}

	s.bargeInTotal.Add(1)

	metadata := sanitizeMetadata(meta)
	metadata["trigger"] = reason
	metadata["confidence"] = formatFloat(confidence)

	payload := eventbus.SpeechBargeInEvent{
		SessionID: sessionID,
		StreamID:  streamID,
		Reason:    reason,
		Timestamp: ts,
		Metadata:  metadata,
	}

	s.bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicSpeechBargeIn,
		Source:  eventbus.SourceSpeechBarge,
		Payload: payload,
	})
}

type playbackState struct {
	playing    bool
	quietUntil time.Time
}

func streamKey(sessionID, streamID string) string {
	return sessionID + "::" + streamID
}

func formatFloat(v float32) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".")
}

func reasonOrDefault(reason, fallback string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fallback
	}
	return reason
}

func sanitizeMetadata(meta map[string]string) map[string]string {
	if len(meta) == 0 {
		return make(map[string]string)
	}
	out := make(map[string]string, len(meta))
	count := 0
	for k, v := range meta {
		if count >= maxMetadataEntries {
			break
		}
		key := trimToRuneLimit(k, maxMetadataKeyRunes)
		if key == "" {
			continue
		}
		value := trimToRuneLimit(v, maxMetadataValueRunes)
		out[key] = value
		count++
	}
	return out
}

func trimToRuneLimit(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

func splitStreamKey(key string) (string, string) {
	const sep = "::"
	if idx := strings.Index(key, sep); idx >= 0 {
		return key[:idx], key[idx+len(sep):]
	}
	return key, ""
}

func cloneMetadata(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// Metrics reports aggregated statistics for the barge-in service.
type Metrics struct {
	BargeInTotal uint64
}

// Metrics returns the current barge-in metrics snapshot.
func (s *Service) Metrics() Metrics {
	return Metrics{BargeInTotal: s.bargeInTotal.Load()}
}
