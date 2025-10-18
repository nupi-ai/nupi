package ingress

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// Service ingests raw audio streams, segments them and publishes events on the bus.
type Service struct {
	bus             *eventbus.Bus
	segmentDuration time.Duration

	mu      sync.RWMutex
	streams map[string]*Stream
}

// Option configures optional behaviour on the Service.
type Option func(*Service)

// WithSegmentDuration override the target duration for emitted segments (default 20ms).
func WithSegmentDuration(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.segmentDuration = d
		}
	}
}

// New constructs an audio ingress service backed by the provided bus.
func New(bus *eventbus.Bus, opts ...Option) *Service {
	svc := &Service{
		bus:             bus,
		segmentDuration: 20 * time.Millisecond,
		streams:         make(map[string]*Stream),
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// Start satisfies daemonruntime.Service; ingress has no background goroutines yet.
func (s *Service) Start(ctx context.Context) error { //nolint:revive // context unused currently
	return nil
}

// Shutdown closes all active streams and releases resources.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, stream := range s.streams {
		stream.closeLocked()
		delete(s.streams, key)
	}
	return nil
}

// OpenStream registers a new audio stream for the given session and stream identifiers.
func (s *Service) OpenStream(sessionID, streamID string, format eventbus.AudioFormat, metadata map[string]string) (*Stream, error) {
	if sessionID == "" || streamID == "" {
		return nil, errors.New("audio ingress: sessionID and streamID are required")
	}
	if err := validateFormat(format); err != nil {
		return nil, err
	}

	key := streamKey(sessionID, streamID)

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.streams[key]; exists {
		return nil, fmt.Errorf("audio ingress: stream %s already exists", key)
	}

	stream := &Stream{
		service:    s,
		sessionID:  sessionID,
		streamID:   streamID,
		format:     format,
		metadata:   copyMetadata(metadata),
		segmentDur: s.segmentDuration,
		first:      true,
	}
	s.streams[key] = stream
	return stream, nil
}

// Stream retrieves an existing stream by identifiers.
func (s *Service) Stream(sessionID, streamID string) (*Stream, bool) {
	key := streamKey(sessionID, streamID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	stream, ok := s.streams[key]
	return stream, ok
}

// CloseStream terminates the stream if it exists.
func (s *Service) CloseStream(sessionID, streamID string) error {
	stream, ok := s.Stream(sessionID, streamID)
	if !ok {
		return fmt.Errorf("audio ingress: stream %s/%s not found", sessionID, streamID)
	}
	return stream.Close()
}

func (s *Service) removeStream(stream *Stream) {
	key := streamKey(stream.sessionID, stream.streamID)
	s.mu.Lock()
	delete(s.streams, key)
	s.mu.Unlock()
}

func streamKey(sessionID, streamID string) string {
	return sessionID + "::" + streamID
}

// Stream represents an active audio input.
type Stream struct {
	service   *Service
	sessionID string
	streamID  string
	format    eventbus.AudioFormat
	metadata  map[string]string

	mu           sync.Mutex
	closed       bool
	first        bool
	rawSeq       uint64
	segmentSeq   uint64
	buffer       []byte
	segmentStart time.Time
	segmentDur   time.Duration
}

// Write appends PCM data to the stream, emitting raw and segmented events.
func (st *Stream) Write(p []byte) error {
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.closed {
		return errors.New("audio ingress: stream closed")
	}
	if len(p) == 0 {
		return nil
	}

	now := time.Now().UTC()
	st.rawSeq++
	rawCopy := make([]byte, len(p))
	copy(rawCopy, p)
	st.publishRaw(rawCopy, now)

	if len(st.buffer) == 0 {
		st.segmentStart = now
	}
	st.buffer = append(st.buffer, rawCopy...)
	st.flushSegmentsLocked(false)
	return nil
}

// Close finalises the stream, flushing remaining data.
func (st *Stream) Close() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.closeLocked()
}

func (st *Stream) closeLocked() error {
	if st.closed {
		return nil
	}
	if len(st.buffer) > 0 {
		st.flushSegmentsLocked(true)
	}
	st.closed = true
	st.service.removeStream(st)
	return nil
}

func (st *Stream) publishRaw(data []byte, received time.Time) {
	evt := eventbus.AudioIngressRawEvent{
		SessionID: st.sessionID,
		StreamID:  st.streamID,
		Sequence:  st.rawSeq,
		Format:    st.format,
		Data:      data,
		Received:  received,
		Metadata:  copyMetadata(st.metadata),
	}
	st.service.bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicAudioIngressRaw,
		Source:  eventbus.SourceAudioIngress,
		Payload: evt,
	})
}

func (st *Stream) flushSegmentsLocked(closing bool) {
	segSize := segmentSizeBytes(st.format, st.segmentDur)
	for segSize > 0 && len(st.buffer) >= segSize {
		segment := st.buffer[:segSize]
		st.emitSegment(segment, st.segmentDur, false)
		st.buffer = st.buffer[segSize:]
	}
	if closing && len(st.buffer) > 0 {
		duration := durationForBytes(len(st.buffer), st.format)
		segment := st.buffer
		st.emitSegment(segment, duration, true)
		st.buffer = nil
	} else if closing {
		// no leftover -> nothing to emit
		st.buffer = nil
	}
}

func (st *Stream) emitSegment(data []byte, duration time.Duration, final bool) {
	st.segmentSeq++
	segmentCopy := make([]byte, len(data))
	copy(segmentCopy, data)

	startedAt := st.segmentStart
	endedAt := startedAt.Add(duration)

	evt := eventbus.AudioIngressSegmentEvent{
		SessionID: st.sessionID,
		StreamID:  st.streamID,
		Sequence:  st.segmentSeq,
		Format:    st.format,
		Data:      segmentCopy,
		Duration:  duration,
		First:     st.first,
		Last:      final && len(segmentCopy) > 0,
		StartedAt: startedAt,
		EndedAt:   endedAt,
		Metadata:  copyMetadata(st.metadata),
	}
	st.first = false
	st.segmentStart = endedAt
	st.service.bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicAudioIngressSegment,
		Source:  eventbus.SourceAudioIngress,
		Payload: evt,
	})
}

func validateFormat(format eventbus.AudioFormat) error {
	if format.SampleRate <= 0 {
		return errors.New("audio ingress: sample rate must be positive")
	}
	if format.Channels <= 0 {
		return errors.New("audio ingress: channels must be positive")
	}
	if format.BitDepth <= 0 || format.BitDepth%8 != 0 {
		return errors.New("audio ingress: bit depth must be positive and divisible by 8")
	}
	return nil
}

func segmentSizeBytes(format eventbus.AudioFormat, segmentDuration time.Duration) int {
	if segmentDuration <= 0 {
		return 0
	}
	bytesPerSample := format.BitDepth / 8
	if bytesPerSample == 0 {
		return 0
	}
	samples := float64(format.SampleRate) * segmentDuration.Seconds()
	return int(math.Round(samples)) * format.Channels * bytesPerSample
}

func durationForBytes(b int, format eventbus.AudioFormat) time.Duration {
	bytesPerSample := format.BitDepth / 8
	if bytesPerSample == 0 {
		return 0
	}
	bytesPerSecond := format.SampleRate * format.Channels * bytesPerSample
	if bytesPerSecond == 0 {
		return 0
	}
	seconds := float64(b) / float64(bytesPerSecond)
	return time.Duration(seconds * float64(time.Second))
}

func copyMetadata(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
