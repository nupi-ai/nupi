package barge

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
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

	vadSub      *eventbus.TypedSubscription[eventbus.SpeechVADEvent]
	clientSub   *eventbus.TypedSubscription[eventbus.AudioInterruptEvent]
	playbackSub *eventbus.TypedSubscription[eventbus.AudioEgressPlaybackEvent]
	lifecycle   eventbus.ServiceLifecycle

	mu             sync.Mutex
	lastEvent      map[string]time.Time
	playback       map[string]playbackState
	sessionStreams map[string]string
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
	s.lifecycle.Start(ctx)
	s.vadSub = eventbus.Subscribe[eventbus.SpeechVADEvent](s.bus, eventbus.TopicSpeechVADDetected, eventbus.WithSubscriptionName("barge_vad"))
	s.clientSub = eventbus.Subscribe[eventbus.AudioInterruptEvent](s.bus, eventbus.TopicAudioInterrupt, eventbus.WithSubscriptionName("barge_client"))
	s.playbackSub = eventbus.Subscribe[eventbus.AudioEgressPlaybackEvent](s.bus, eventbus.TopicAudioEgressPlayback, eventbus.WithSubscriptionName("barge_playback"))
	s.lifecycle.AddSubscriptions(s.vadSub, s.clientSub, s.playbackSub)
	s.lifecycle.Go(s.consumeVAD)
	s.lifecycle.Go(s.consumeClient)
	s.lifecycle.Go(s.consumePlayback)
	return nil
}

// Shutdown stops background processing.
func (s *Service) Shutdown(ctx context.Context) error {
	return s.lifecycle.Shutdown(ctx)
}

func (s *Service) consumeVAD(ctx context.Context) {
	eventbus.Consume(ctx, s.vadSub, nil, s.handleVADEvent)
}

func (s *Service) consumeClient(ctx context.Context) {
	eventbus.Consume(ctx, s.clientSub, nil, s.handleClientEvent)
}

func (s *Service) consumePlayback(ctx context.Context) {
	eventbus.ConsumeEnvelope(ctx, s.playbackSub, nil, func(env eventbus.TypedEnvelope[eventbus.AudioEgressPlaybackEvent]) {
		ts := env.Timestamp
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		s.handlePlaybackEvent(env.Payload, ts)
	})
}

func (s *Service) handleVADEvent(event eventbus.SpeechVADEvent) {
	if !event.Active {
		if s.logger != nil {
			s.logger.Printf("[Barge] ignoring VAD event session=%s stream=%s: inactive", event.SessionID, event.StreamID)
		}
		return
	}
	if event.Confidence < s.minConfidence {
		if s.logger != nil {
			s.logger.Printf("[Barge] ignoring VAD event session=%s stream=%s: low confidence %.2f < %.2f", event.SessionID, event.StreamID, event.Confidence, s.minConfidence)
		}
		return
	}

	ts := event.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	streamID, ok := s.targetStreamForSession(event.SessionID, ts)
	if !ok {
		if s.logger != nil {
			s.logger.Printf("[Barge] no active playback for session=%s; VAD event ignored", event.SessionID)
		}
		return
	}

	key := streamKey(event.SessionID, streamID)
	if !s.registerTrigger(key, ts) {
		return
	}

	meta := maputil.Clone(event.Metadata)
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
		if s.logger != nil {
			s.logger.Printf("[Barge] no playback stream resolved for client interrupt session=%s", event.SessionID)
		}
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
		if s.logger != nil {
			sessionID, streamID := splitStreamKey(key)
			s.logger.Printf("[Barge] suppressing trigger session=%s stream=%s during cooldown (%s elapsed)", sessionID, streamID, ts.Sub(last))
		}
		return false
	}
	s.lastEvent[key] = ts
	return true
}

func (s *Service) publishBargeIn(sessionID, streamID string, ts time.Time, reason string, confidence float32, meta map[string]string) {
	if s.bus == nil {
		return
	}



	if s.logger != nil {
		s.logger.Printf("[Barge] publishing barge-in session=%s stream=%s reason=%s confidence=%.2f", sessionID, streamID, reason, confidence)
	}

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

	eventbus.Publish(context.Background(), s.bus, eventbus.Speech.BargeIn, eventbus.SourceSpeechBarge, payload)
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

