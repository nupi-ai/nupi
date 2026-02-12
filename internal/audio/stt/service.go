package stt

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nupi-ai/nupi/internal/audio/streammanager"
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

// SessionParams is an alias for backward compatibility.
type SessionParams = streammanager.SessionParams

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

	manager *streammanager.Manager[eventbus.AudioIngressSegmentEvent]

	sub *eventbus.TypedSubscription[eventbus.AudioIngressSegmentEvent]
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

	s.manager = streammanager.New(streammanager.Config[eventbus.AudioIngressSegmentEvent]{
		Tag:        "STT",
		MaxPending: maxPendingSegments,
		Retry: streammanager.RetryConfig{
			Initial: s.retryInitial,
			Max:     s.retryMax,
		},
		Factory: streammanager.StreamFactoryFunc[eventbus.AudioIngressSegmentEvent](s.createStreamHandle),
		Callbacks: streammanager.Callbacks[eventbus.AudioIngressSegmentEvent]{
			ClassifyCreateError: s.classifyError,
		},
		Logger: s.logger,
		Ctx:    s.ctx,
	})

	s.sub = eventbus.Subscribe[eventbus.AudioIngressSegmentEvent](s.bus,
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

	var handles []streammanager.StreamHandle[eventbus.AudioIngressSegmentEvent]
	if s.manager != nil {
		handles = s.manager.CloseAllStreams()
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

	for _, h := range handles {
		h.Wait(ctx)
	}

	if s.manager != nil {
		s.manager.ShutdownPending()
	}

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
			s.handleSegment(env.Payload)
		}
	}
}

func (s *Service) handleSegment(segment eventbus.AudioIngressSegmentEvent) {
	if segment.SessionID == "" || segment.StreamID == "" {
		return
	}

	s.segmentsTotal.Add(1)

	key := streammanager.StreamKey(segment.SessionID, segment.StreamID)

	if h, ok := s.manager.Stream(key); ok {
		if err := h.Enqueue(segment); err != nil && !errors.Is(err, ErrStreamClosed) {
			s.logger.Printf("[STT] enqueue segment session=%s stream=%s failed: %v", segment.SessionID, segment.StreamID, err)
		}
		return
	}

	params := SessionParams{
		SessionID: segment.SessionID,
		StreamID:  segment.StreamID,
		Format:    segment.Format,
		Metadata:  streammanager.CopyMetadata(segment.Metadata),
	}

	h, err := s.manager.CreateStream(key, params)
	switch {
	case err == nil:
		if err := h.Enqueue(segment); err != nil && !errors.Is(err, ErrStreamClosed) {
			s.logger.Printf("[STT] enqueue segment session=%s stream=%s failed: %v", segment.SessionID, segment.StreamID, err)
		}
	case errors.Is(err, ErrAdapterUnavailable):
		s.manager.BufferPending(key, params, segment)
	case errors.Is(err, ErrFactoryUnavailable):
		s.logger.Printf("[STT] factory unavailable session=%s stream=%s, dropping segment", segment.SessionID, segment.StreamID)
	default:
		s.logger.Printf("[STT] create stream session=%s stream=%s failed: %v", segment.SessionID, segment.StreamID, err)
	}
}

func (s *Service) classifyError(err error) (adapterUnavailable, factoryUnavailable bool) {
	return errors.Is(err, ErrAdapterUnavailable), errors.Is(err, ErrFactoryUnavailable)
}

// createStreamHandle is the StreamFactory callback for the manager.
func (s *Service) createStreamHandle(ctx context.Context, key string, params SessionParams) (streammanager.StreamHandle[eventbus.AudioIngressSegmentEvent], error) {
	if s.factory == nil {
		return nil, ErrFactoryUnavailable
	}

	transcriber, err := s.factory.Create(ctx, params)
	if err != nil {
		return nil, err
	}

	return newStream(key, s, params, transcriber), nil
}

func (s *Service) onStreamEnded(key string, st *stream) {
	if s.manager != nil {
		s.manager.RemoveStream(key, st)
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
		Metadata:   streammanager.CopyMetadata(tr.Metadata),
	}

	specificTopic := eventbus.TopicSpeechTranscriptPartial
	if tr.Final {
		specificTopic = eventbus.TopicSpeechTranscriptFinal
	}
	s.bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   specificTopic,
		Source:  eventbus.SourceAudioSTT,
		Payload: evt,
	})
}

// stream is the internal per-key audio processing goroutine.
// It implements streammanager.StreamHandle[eventbus.AudioIngressSegmentEvent].
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

func newStream(key string, svc *Service, params SessionParams, transcriber Transcriber) *stream {
	ctx, cancel := context.WithCancel(svc.ctx)
	st := &stream{
		service:     svc,
		key:         key,
		sessionID:   params.SessionID,
		streamID:    params.StreamID,
		format:      params.Format,
		metadata:    streammanager.CopyMetadata(params.Metadata),
		transcriber: transcriber,
		segmentCh:   make(chan eventbus.AudioIngressSegmentEvent, svc.segmentBuffer),
		ctx:         ctx,
		cancel:      cancel,
	}

	st.wg.Add(1)
	go st.run()
	return st
}

// Enqueue implements streammanager.StreamHandle.
func (st *stream) Enqueue(segment eventbus.AudioIngressSegmentEvent) error {
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

// Stop implements streammanager.StreamHandle.
func (st *stream) Stop() {
	st.cancel()
}

// Wait implements streammanager.StreamHandle.
func (st *stream) Wait(ctx context.Context) {
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
	if st.transcriber == nil {
		params := SessionParams{
			SessionID: st.sessionID,
			StreamID:  st.streamID,
			Format:    st.format,
			Metadata:  streammanager.CopyMetadata(st.metadata),
		}
		transcriber, err := st.service.factory.Create(st.ctx, params)
		if err != nil {
			if !errors.Is(err, ErrAdapterUnavailable) {
				st.service.logger.Printf("[STT] reconnect transcriber session=%s stream=%s: %v", st.sessionID, st.streamID, err)
			}
			return
		}
		st.transcriber = transcriber
		st.service.logger.Printf("[STT] transcriber reconnected session=%s stream=%s", st.sessionID, st.streamID)
	}

	transcripts, err := st.transcriber.OnSegment(st.ctx, segment)

	// Publish any transcripts returned alongside the error — the NAP
	// contract allows partial results + error (see nap_transcriber.go:182).
	for _, tr := range transcripts {
		st.service.publishTranscript(st, tr)
	}

	if err != nil {
		if errors.Is(err, ErrAdapterUnavailable) {
			st.service.logger.Printf("[STT] transcriber unavailable session=%s stream=%s, will retry: %v", st.sessionID, st.streamID, err)
			st.closeTranscriberForRecovery("adapter unavailable")
			return
		}
		st.service.logger.Printf("[STT] transcribe segment session=%s stream=%s seq=%d: %v", st.sessionID, st.streamID, segment.Sequence, err)
	}
}

func (st *stream) closeTranscriberForRecovery(reason string) {
	if st.transcriber == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := st.transcriber.Close(ctx)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		st.service.logger.Printf("[STT] close broken transcriber session=%s stream=%s (%s): %v", st.sessionID, st.streamID, reason, err)
	}
	st.transcriber = nil
}

func (st *stream) closeTranscriber(reason string) {
	st.closeOnce.Do(func() {
		if st.transcriber == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), st.service.flushTimeout)
		defer cancel()

		transcripts, err := st.transcriber.Close(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			st.service.logger.Printf("[STT] close transcriber session=%s stream=%s (%s): %v", st.sessionID, st.streamID, reason, err)
		}
		for _, tr := range transcripts {
			st.service.publishTranscript(st, tr)
		}
	})
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
