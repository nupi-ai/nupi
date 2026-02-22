package ingress

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// Service ingests raw audio streams, segments them and publishes events on the bus.
type Service struct {
	bus             *eventbus.Bus
	segmentDuration time.Duration

	mu      sync.RWMutex
	streams map[string]*Stream

	bytesTotal atomic.Uint64
	logger     *log.Logger
}

var ErrStreamExists = errors.New("audio ingress: stream already exists")

// Option configures optional behaviour on the Service.
type Option func(*Service)

// WithLogger overrides the logger used for diagnostics.
func WithLogger(logger *log.Logger) Option {
	return func(s *Service) {
		if logger != nil {
			s.logger = logger
		}
	}
}

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
		logger:          log.Default(),
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
	streams := make([]*Stream, 0, len(s.streams))
	for _, stream := range s.streams {
		streams = append(streams, stream)
	}
	s.mu.Unlock()

	var err error
	for _, stream := range streams {
		err = errors.Join(err, stream.Close())
	}
	return err
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
		return nil, fmt.Errorf("%w: %s", ErrStreamExists, key)
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
	if s.logger != nil {
		s.logger.Printf("[Ingress] opened stream %s/%s (rate=%dHz, channels=%d, bit_depth=%d)", sessionID, streamID, format.SampleRate, format.Channels, format.BitDepth)
	}
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
	lastSent     bool
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
	st.service.bytesTotal.Add(uint64(len(rawCopy)))
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
	st.flushSegmentsLocked(true)
	st.closed = true
	if st.service.logger != nil {
		st.service.logger.Printf("[Ingress] closed stream %s/%s (raw=%d, segments=%d)", st.sessionID, st.streamID, st.rawSeq, st.segmentSeq)
	}
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
	eventbus.Publish(context.Background(), st.service.bus, eventbus.Audio.IngressRaw, eventbus.SourceAudioIngress, evt)
}

func (st *Stream) flushSegmentsLocked(closing bool) {
	segSize := segmentSizeBytes(st.format, st.segmentDur)
	for segSize > 0 && len(st.buffer) >= segSize {
		finalSegment := closing && len(st.buffer) == segSize && !st.lastSent
		segment := st.buffer[:segSize]
		st.emitSegment(segment, st.segmentDur, finalSegment)
		st.buffer = st.buffer[segSize:]
		if finalSegment {
			st.buffer = nil
			return
		}
	}
	if closing && len(st.buffer) > 0 {
		duration := durationForBytes(len(st.buffer), st.format)
		segment := st.buffer
		st.emitSegment(segment, duration, true)
		st.buffer = nil
		return
	}

	if closing {
		// ensure buffer is cleared after terminal flush
		st.buffer = nil
		if !st.lastSent {
			st.emitTerminalLocked()
		}
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
	eventbus.Publish(context.Background(), st.service.bus, eventbus.Audio.IngressSegment, eventbus.SourceAudioIngress, evt)
	st.lastSent = final && len(segmentCopy) > 0
}

func (st *Stream) emitTerminalLocked() {
	st.segmentSeq++
	now := time.Now().UTC()
	start := st.segmentStart
	if start.IsZero() {
		start = now
	}

	evt := eventbus.AudioIngressSegmentEvent{
		SessionID: st.sessionID,
		StreamID:  st.streamID,
		Sequence:  st.segmentSeq,
		Format:    st.format,
		Data:      nil,
		Duration:  0,
		First:     st.first,
		Last:      true,
		StartedAt: start,
		EndedAt:   start,
		Metadata:  copyMetadata(st.metadata),
	}
	st.first = false
	st.lastSent = true
	st.segmentStart = start
	eventbus.Publish(context.Background(), st.service.bus, eventbus.Audio.IngressSegment, eventbus.SourceAudioIngress, evt)
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
	return maps.Clone(src)
}

// Metrics reports aggregated ingress statistics.
type Metrics struct {
	BytesTotal uint64
}

// Metrics returns the current ingress metrics snapshot.
func (s *Service) Metrics() Metrics {
	return Metrics{
		BytesTotal: s.bytesTotal.Load(),
	}
}
