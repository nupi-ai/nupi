package stt

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
	// ErrFactoryUnavailable indicates no factory is configured to provide transcribers.
	ErrFactoryUnavailable = errors.New("stt: transcriber factory unavailable")
	// ErrStreamClosed is returned when a stream is no longer accepting segments.
	ErrStreamClosed = errors.New("stt: stream closed")
	// ErrAdapterUnavailable is returned when no active STT adapter is bound.
	ErrAdapterUnavailable = errors.New("stt: no active stt adapter")
)

const (
	defaultSegmentBuffer = 16
	defaultFlushTimeout  = 2 * time.Second
	defaultRetryInitial  = 200 * time.Millisecond
	defaultRetryMax      = 5 * time.Second
	maxPendingSegments   = 100
)

// Transcription represents a recognised speech segment returned by a transcriber.
type Transcription struct {
	Text       string
	Confidence float32
	Final      bool
	StartedAt  time.Time
	EndedAt    time.Time
	Metadata   map[string]string
}

// Transcriber consumes audio segments and yields transcriptions.
type Transcriber interface {
	OnSegment(ctx context.Context, segment eventbus.AudioIngressSegmentEvent) ([]Transcription, error)
	Close(ctx context.Context) ([]Transcription, error)
}

// Factory constructs transcribers for a specific audio stream.
type Factory interface {
	Create(ctx context.Context, params SessionParams) (Transcriber, error)
}

// FactoryFunc adapts a function to the Factory interface.
type FactoryFunc func(ctx context.Context, params SessionParams) (Transcriber, error)

// Create invokes the underlying function.
func (f FactoryFunc) Create(ctx context.Context, params SessionParams) (Transcriber, error) {
	return f(ctx, params)
}

// SessionParams describes context for establishing a transcriber.
type SessionParams struct {
	SessionID string
	StreamID  string
	Format    eventbus.AudioFormat
	Metadata  map[string]string
	AdapterID string
	Config    map[string]any
}

// Option configures the Service behaviour.
type Option func(*Service)

// WithLogger overrides the logger used for diagnostics.
func WithLogger(logger *log.Logger) Option {
	return func(s *Service) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// WithFactory sets the transcriber factory.
func WithFactory(factory Factory) Option {
	return func(s *Service) {
		if factory != nil {
			s.factory = factory
		}
	}
}

// WithSegmentBuffer overrides the per-stream segment channel buffer size.
func WithSegmentBuffer(size int) Option {
	return func(s *Service) {
		if size > 0 {
			s.segmentBuffer = size
		}
	}
}

// WithFlushTimeout overrides the timeout used when flushing transcribers during shutdown.
func WithFlushTimeout(timeout time.Duration) Option {
	return func(s *Service) {
		if timeout > 0 {
			s.flushTimeout = timeout
		}
	}
}

// WithRetryDelays overrides retry backoff used when adapters are temporarily unavailable.
func WithRetryDelays(initial, max time.Duration) Option {
	return func(s *Service) {
		if initial > 0 {
			s.retryInitial = initial
		}
		if max > 0 && max >= s.retryInitial {
			s.retryMax = max
		}
		if s.retryMax < s.retryInitial {
			s.retryMax = s.retryInitial
		}
	}
}

// Service bridges audio ingress segments to STT adapters and publishes transcripts.
type Service struct {
	bus           *eventbus.Bus
	factory       Factory
	logger        *log.Logger
	segmentBuffer int
	flushTimeout  time.Duration
	retryInitial  time.Duration
	retryMax      time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	streams map[string]*stream

	pendingMu sync.Mutex
	pending   map[string]*pendingStream

	sub *eventbus.Subscription
	wg  sync.WaitGroup

	segmentsTotal atomic.Uint64
}

// New constructs an STT service bound to the provided event bus.
func New(bus *eventbus.Bus, opts ...Option) *Service {
	svc := &Service{
		bus:           bus,
		logger:        log.Default(),
		segmentBuffer: defaultSegmentBuffer,
		flushTimeout:  defaultFlushTimeout,
		retryInitial:  defaultRetryInitial,
		retryMax:      defaultRetryMax,
		streams:       make(map[string]*stream),
		pending:       make(map[string]*pendingStream),
		factory: FactoryFunc(func(context.Context, SessionParams) (Transcriber, error) {
			return nil, ErrFactoryUnavailable
		}),
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// Start subscribes to audio ingress segments and begins processing.
func (s *Service) Start(ctx context.Context) error {
	if s.bus == nil {
		return errors.New("stt: event bus is required")
	}

	s.ctx, s.cancel = context.WithCancel(ctx)

	s.sub = s.bus.Subscribe(
		eventbus.TopicAudioIngressSegment,
		eventbus.WithSubscriptionName("audio_stt_segments"),
	)

	s.wg.Add(1)
	go s.consumeSegments()
	return nil
}

// Shutdown stops background processing and waits for streams to finish.
func (s *Service) Shutdown(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.sub != nil {
		s.sub.Close()
	}

	s.mu.Lock()
	streams := make([]*stream, 0, len(s.streams))
	for _, st := range s.streams {
		streams = append(streams, st)
	}
	s.mu.Unlock()

	for _, st := range streams {
		st.stop()
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

	for _, st := range streams {
		st.wait(ctx)
	}

	s.pendingMu.Lock()
	for key, ps := range s.pending {
		if ps != nil {
			ps.stopTimer()
		}
		delete(s.pending, key)
	}
	s.pendingMu.Unlock()

	return nil
}

func (s *Service) consumeSegments() {
	defer s.wg.Done()
	if s.sub == nil {
		return
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case env, ok := <-s.sub.C():
			if !ok {
				return
			}
			segment, ok := env.Payload.(eventbus.AudioIngressSegmentEvent)
			if !ok {
				continue
			}
			s.handleSegment(segment)
		}
	}
}

func (s *Service) handleSegment(segment eventbus.AudioIngressSegmentEvent) {
	if segment.SessionID == "" || segment.StreamID == "" {
		return
	}

	s.segmentsTotal.Add(1)

	key := streamKey(segment.SessionID, segment.StreamID)

	if stream, ok := s.stream(key); ok {
		if err := stream.enqueue(segment); err != nil && !errors.Is(err, ErrStreamClosed) {
			s.logger.Printf("[STT] enqueue segment for %s/%s failed: %v", segment.SessionID, segment.StreamID, err)
		}
		return
	}

	params := SessionParams{
		SessionID: segment.SessionID,
		StreamID:  segment.StreamID,
		Format:    segment.Format,
		Metadata:  copyMetadata(segment.Metadata),
	}

	stream, err := s.createStream(key, params)
	switch {
	case err == nil:
		if err := stream.enqueue(segment); err != nil && !errors.Is(err, ErrStreamClosed) {
			s.logger.Printf("[STT] enqueue segment for %s/%s failed: %v", segment.SessionID, segment.StreamID, err)
		}
	case errors.Is(err, ErrAdapterUnavailable):
		s.bufferPending(key, params, segment)
	case errors.Is(err, ErrFactoryUnavailable):
		s.logger.Printf("[STT] factory unavailable for %s/%s, dropping segment", segment.SessionID, segment.StreamID)
	default:
		s.logger.Printf("[STT] create stream %s/%s failed: %v", segment.SessionID, segment.StreamID, err)
	}
}

func (s *Service) stream(key string) (*stream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.streams[key]
	return st, ok
}

func (s *Service) createStream(key string, params SessionParams) (*stream, error) {
	if s.factory == nil {
		return nil, ErrFactoryUnavailable
	}

	if st, ok := s.stream(key); ok {
		return st, nil
	}

	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	transcriber, err := s.factory.Create(ctx, params)
	if err != nil {
		return nil, err
	}

	stream := newStream(key, s, params, transcriber)

	s.mu.Lock()
	if existing, ok := s.streams[key]; ok {
		s.mu.Unlock()
		stream.stop()
		return existing, nil
	}
	s.streams[key] = stream
	s.mu.Unlock()

	s.flushPendingForStream(key, stream)
	return stream, nil
}

func (s *Service) bufferPending(key string, params SessionParams, segment eventbus.AudioIngressSegmentEvent) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	ps, ok := s.pending[key]
	if !ok {
		ps = &pendingStream{
			params: params,
		}
		s.pending[key] = ps
	}
	if len(ps.segments) >= maxPendingSegments {
		ps.segments = ps.segments[1:]
		s.logger.Printf("[STT] pending buffer full for %s, dropping oldest segment", key)
	}
	ps.segments = append(ps.segments, segment)
	ps.scheduleRetry(s, key)
}

func (s *Service) flushPendingForStream(key string, st *stream) {
	s.pendingMu.Lock()
	ps, ok := s.pending[key]
	if ok {
		ps.stopTimer()
		segments := append([]eventbus.AudioIngressSegmentEvent(nil), ps.segments...)
		delete(s.pending, key)
		s.pendingMu.Unlock()

		for _, segment := range segments {
			if err := st.enqueue(segment); err != nil && !errors.Is(err, ErrStreamClosed) {
				s.logger.Printf("[STT] enqueue pending segment for %s/%s failed: %v", segment.SessionID, segment.StreamID, err)
			}
		}
		return
	}
	s.pendingMu.Unlock()
}

func (s *Service) retryPending(key string) {
	s.pendingMu.Lock()
	ps, ok := s.pending[key]
	if !ok {
		s.pendingMu.Unlock()
		return
	}
	ps.timer = nil
	params := ps.params
	s.pendingMu.Unlock()

	stream, err := s.createStream(key, params)
	if err != nil {
		if !errors.Is(err, ErrAdapterUnavailable) && !errors.Is(err, ErrFactoryUnavailable) && !errors.Is(err, context.Canceled) {
			s.logger.Printf("[STT] retry stream %s failed: %v", key, err)
		}
		s.pendingMu.Lock()
		if ps, ok := s.pending[key]; ok {
			ps.scheduleRetry(s, key)
		}
		s.pendingMu.Unlock()
		return
	}

	s.flushPendingForStream(key, stream)
}

func (s *Service) onStreamEnded(key string, st *stream) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.streams[key]; ok && existing == st {
		delete(s.streams, key)
	}
}

func (s *Service) publishTranscript(st *stream, tr Transcription) {
	if s.bus == nil {
		return
	}

	st.seq++
	evt := eventbus.SpeechTranscriptEvent{
		SessionID:  st.sessionID,
		StreamID:   st.streamID,
		Sequence:   st.seq,
		Text:       tr.Text,
		Confidence: tr.Confidence,
		Final:      tr.Final,
		StartedAt:  tr.StartedAt,
		EndedAt:    tr.EndedAt,
		Metadata:   copyMetadata(tr.Metadata),
	}

	s.bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicSpeechTranscript,
		Source:  eventbus.SourceAudioSTT,
		Payload: evt,
	})
}

type stream struct {
	service *Service
	key     string

	sessionID string
	streamID  string
	format    eventbus.AudioFormat
	metadata  map[string]string

	transcriber Transcriber

	segmentCh chan eventbus.AudioIngressSegmentEvent
	ctx       context.Context
	cancel    context.CancelFunc

	wg        sync.WaitGroup
	closeOnce sync.Once
	seq       uint64
}

type pendingStream struct {
	params   SessionParams
	segments []eventbus.AudioIngressSegmentEvent
	timer    *time.Timer
	delay    time.Duration
}

func (ps *pendingStream) scheduleRetry(s *Service, key string) {
	if ps.timer != nil {
		return
	}

	delay := ps.delay
	if delay <= 0 {
		delay = s.retryInitial
	} else {
		delay *= 2
		if delay > s.retryMax {
			delay = s.retryMax
		}
	}
	ps.delay = delay
	ps.timer = time.AfterFunc(delay, func() {
		s.retryPending(key)
	})
}

func (ps *pendingStream) stopTimer() {
	if ps.timer != nil {
		ps.timer.Stop()
		ps.timer = nil
	}
	ps.delay = 0
}

func newStream(key string, svc *Service, params SessionParams, transcriber Transcriber) *stream {
	ctx, cancel := context.WithCancel(svc.ctx)
	st := &stream{
		service:     svc,
		key:         key,
		sessionID:   params.SessionID,
		streamID:    params.StreamID,
		format:      params.Format,
		metadata:    copyMetadata(params.Metadata),
		transcriber: transcriber,
		segmentCh:   make(chan eventbus.AudioIngressSegmentEvent, svc.segmentBuffer),
		ctx:         ctx,
		cancel:      cancel,
	}

	st.wg.Add(1)
	go st.run()
	return st
}

func (st *stream) enqueue(segment eventbus.AudioIngressSegmentEvent) error {
	select {
	case <-st.ctx.Done():
		return ErrStreamClosed
	default:
	}

	select {
	case st.segmentCh <- segment:
		return nil
	case <-st.ctx.Done():
		return ErrStreamClosed
	}
}

func (st *stream) run() {
	defer st.wg.Done()
	defer st.cancel()
	defer st.service.onStreamEnded(st.key, st)

	for {
		select {
		case <-st.ctx.Done():
			st.closeTranscriber("context cancelled")
			return
		case segment := <-st.segmentCh:
			st.handleSegment(segment)
			if segment.Last {
				st.closeTranscriber("last segment")
				return
			}
		}
	}
}

func (st *stream) handleSegment(segment eventbus.AudioIngressSegmentEvent) {
	transcripts, err := st.transcriber.OnSegment(st.ctx, segment)
	if err != nil {
		st.service.logger.Printf("[STT] transcribe segment %s/%s seq=%d: %v", st.sessionID, st.streamID, segment.Sequence, err)
	}

	for _, tr := range transcripts {
		st.service.publishTranscript(st, tr)
	}
}

func (st *stream) closeTranscriber(reason string) {
	st.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), st.service.flushTimeout)
		defer cancel()

		transcripts, err := st.transcriber.Close(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			st.service.logger.Printf("[STT] close transcriber %s/%s (%s): %v", st.sessionID, st.streamID, reason, err)
		}
		for _, tr := range transcripts {
			st.service.publishTranscript(st, tr)
		}
	})
}

func (st *stream) stop() {
	st.cancel()
}

func (st *stream) wait(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		st.wg.Wait()
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

func streamKey(sessionID, streamID string) string {
	return fmt.Sprintf("%s::%s", sessionID, streamID)
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

// Metrics represents aggregated statistics for the STT service.
type Metrics struct {
	SegmentsTotal uint64
}

// Metrics returns the current STT metrics snapshot.
func (s *Service) Metrics() Metrics {
	return Metrics{
		SegmentsTotal: s.segmentsTotal.Load(),
	}
}
