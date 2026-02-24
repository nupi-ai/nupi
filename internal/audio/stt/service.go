package stt

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/audio/serviceutil"
	"github.com/nupi-ai/nupi/internal/audio/streammanager"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
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
	defaultSegmentBuffer = serviceutil.DefaultWorkerBuffer
	maxPendingSegments   = serviceutil.DefaultMaxPending
)

type SessionParams = serviceutil.SessionParams

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

type Factory = serviceutil.Factory[Transcriber]
type FactoryFunc = serviceutil.FactoryFunc[Transcriber]

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
		s.retryInitial, s.retryMax = serviceutil.NormalizeRetryDelays(s.retryInitial, s.retryMax, initial, max)
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

	lifecycle eventbus.ServiceLifecycle

	manager *streammanager.Manager[eventbus.AudioIngressSegmentEvent]

	sub *eventbus.TypedSubscription[eventbus.AudioIngressSegmentEvent]
}

// New constructs an STT service bound to the provided event bus.
func New(bus *eventbus.Bus, opts ...Option) *Service {
	svc := &Service{
		bus:           bus,
		logger:        log.Default(),
		segmentBuffer: defaultSegmentBuffer,
		flushTimeout:  constants.Duration2Seconds,
		retryInitial:  serviceutil.DefaultRetryInitial,
		retryMax:      serviceutil.DefaultRetryMax,
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

	s.lifecycle.Start(ctx)

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
		Ctx:    s.lifecycle.Context(),
	})

	s.sub = eventbus.Subscribe[eventbus.AudioIngressSegmentEvent](s.bus,
		eventbus.TopicAudioIngressSegment,
		eventbus.WithSubscriptionName("audio_stt_segments"),
	)
	s.lifecycle.AddSubscriptions(s.sub)
	s.lifecycle.Go(s.consumeSegments)
	return nil
}

// Shutdown stops background processing and waits for streams to finish.
func (s *Service) Shutdown(ctx context.Context) error {
	s.lifecycle.Stop()

	var handles []streammanager.StreamHandle[eventbus.AudioIngressSegmentEvent]
	if s.manager != nil {
		handles = s.manager.CloseAllStreams()
	}

	if err := s.lifecycle.Wait(ctx); err != nil {
		return err
	}

	for _, h := range handles {
		h.Wait(ctx)
	}

	if s.manager != nil {
		s.manager.ShutdownPending()
	}

	return nil
}

func (s *Service) consumeSegments(ctx context.Context) {
	eventbus.Consume(ctx, s.sub, nil, s.handleSegment)
}

func (s *Service) handleSegment(segment eventbus.AudioIngressSegmentEvent) {
	if segment.SessionID == "" || segment.StreamID == "" {
		return
	}

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
		Metadata:  maputil.Clone(segment.Metadata),
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
		Metadata:   maputil.Clone(tr.Metadata),
	}

	td := eventbus.Speech.TranscriptPartial
	if tr.Final {
		td = eventbus.Speech.TranscriptFinal
	}
	eventbus.Publish(context.Background(), s.bus, td, eventbus.SourceAudioSTT, evt)
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

	worker    *streammanager.Worker[eventbus.AudioIngressSegmentEvent]
	closeOnce sync.Once
	seq       uint64
}

func newStream(key string, svc *Service, params SessionParams, transcriber Transcriber) *stream {
	st := &stream{
		service:     svc,
		key:         key,
		sessionID:   params.SessionID,
		streamID:    params.StreamID,
		format:      params.Format,
		metadata:    maputil.Clone(params.Metadata),
		transcriber: transcriber,
		worker:      streammanager.NewWorker[eventbus.AudioIngressSegmentEvent](svc.lifecycle.Context(), svc.segmentBuffer),
	}
	st.worker.Start(st.processSegment, func() {
		st.closeTranscriber("context cancelled")
	}, func() {
		st.service.onStreamEnded(st.key, st)
	})
	return st
}

// Enqueue implements streammanager.StreamHandle.
func (st *stream) Enqueue(segment eventbus.AudioIngressSegmentEvent) error {
	return st.worker.Enqueue(segment, ErrStreamClosed)
}

// Stop implements streammanager.StreamHandle.
func (st *stream) Stop() {
	st.worker.Stop()
}

// Wait implements streammanager.StreamHandle.
func (st *stream) Wait(ctx context.Context) {
	st.worker.Wait(ctx)
}

func (st *stream) processSegment(segment eventbus.AudioIngressSegmentEvent) bool {
	st.handleSegment(segment)
	if segment.Last {
		st.closeTranscriber("last segment")
		return true
	}
	return false
}

func (st *stream) handleSegment(segment eventbus.AudioIngressSegmentEvent) {
	if st.transcriber == nil {
		params := SessionParams{
			SessionID: st.sessionID,
			StreamID:  st.streamID,
			Format:    st.format,
			Metadata:  maputil.Clone(st.metadata),
		}
		transcriber, err := st.service.factory.Create(st.worker.Context(), params)
		if err != nil {
			if !errors.Is(err, ErrAdapterUnavailable) {
				st.service.logger.Printf("[STT] reconnect transcriber session=%s stream=%s: %v", st.sessionID, st.streamID, err)
			}
			return
		}
		st.transcriber = transcriber
		st.service.logger.Printf("[STT] transcriber reconnected session=%s stream=%s", st.sessionID, st.streamID)
	}

	transcripts, err := st.transcriber.OnSegment(st.worker.Context(), segment)

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
	ctx, cancel := context.WithTimeout(context.Background(), constants.Duration2Seconds)
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

