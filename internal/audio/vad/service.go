package vad

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/nupi-ai/nupi/internal/audio/serviceutil"
	"github.com/nupi-ai/nupi/internal/audio/streammanager"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/mapper"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
)

var (
	// ErrFactoryUnavailable indicates the analyzer factory has not been configured.
	ErrFactoryUnavailable = errors.New("vad: analyzer factory unavailable")
	// ErrAdapterUnavailable indicates no active VAD adapter is bound.
	ErrAdapterUnavailable = errors.New("vad: adapter unavailable")
)

const (
	defaultSegmentBuffer = serviceutil.DefaultWorkerBuffer
	maxPendingSegments   = serviceutil.DefaultMaxPending
	maxRetryFailures     = 10
	maxRetryDuration     = constants.Duration2Minutes
)

type SessionParams = serviceutil.SessionParams

// Detection represents the outcome of processing a segment.
type Detection struct {
	Active     bool
	Confidence float32
	Metadata   map[string]string
}

// Analyzer processes audio segments and emits voice-activity detections.
type Analyzer interface {
	OnSegment(ctx context.Context, segment eventbus.AudioIngressSegmentEvent) ([]Detection, error)
	Close(ctx context.Context) ([]Detection, error)
}

type Factory = serviceutil.Factory[Analyzer]
type FactoryFunc = serviceutil.FactoryFunc[Analyzer]

// Option configures the Service behaviour.
type Option func(*Service)

// WithLogger overrides the default logging sink.
func WithLogger(logger *log.Logger) Option {
	return func(s *Service) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// WithFactory sets the analyzer factory used by the service.
func WithFactory(factory Factory) Option {
	return func(s *Service) {
		if factory != nil {
			s.factory = factory
		}
	}
}

// WithRetryDelays configures the retry backoff when adapters are temporarily unavailable.
func WithRetryDelays(initial, max time.Duration) Option {
	return func(s *Service) {
		s.retryInitial, s.retryMax = serviceutil.NormalizeRetryDelays(s.retryInitial, s.retryMax, initial, max)
	}
}

// Service streams audio segments to VAD adapters and publishes detection events.
type Service struct {
	bus     *eventbus.Bus
	factory Factory
	logger  *log.Logger

	retryInitial time.Duration
	retryMax     time.Duration

	lifecycle eventbus.ServiceLifecycle

	sub *eventbus.TypedSubscription[eventbus.AudioIngressSegmentEvent]

	manager *streammanager.Manager[eventbus.AudioIngressSegmentEvent]
}

// New constructs a VAD bridge service bound to the provided event bus.
func New(bus *eventbus.Bus, opts ...Option) *Service {
	svc := &Service{
		bus:          bus,
		logger:       log.Default(),
		retryInitial: serviceutil.DefaultRetryInitial,
		retryMax:     serviceutil.DefaultRetryMax,
		factory: FactoryFunc(func(context.Context, SessionParams) (Analyzer, error) {
			return nil, ErrFactoryUnavailable
		}),
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// Start subscribes to audio ingress segments and begins dispatching them.
func (s *Service) Start(ctx context.Context) error {
	if s.bus == nil {
		return errors.New("vad: event bus required")
	}

	s.lifecycle.Start(ctx)

	s.manager = streammanager.New(streammanager.Config[eventbus.AudioIngressSegmentEvent]{
		Tag:        "VAD",
		MaxPending: maxPendingSegments,
		Retry: streammanager.RetryConfig{
			Initial:     s.retryInitial,
			Max:         s.retryMax,
			MaxFailures: maxRetryFailures,
			MaxDuration: maxRetryDuration,
		},
		Factory: streammanager.StreamFactoryFunc[eventbus.AudioIngressSegmentEvent](s.createStreamHandle),
		Callbacks: streammanager.Callbacks[eventbus.AudioIngressSegmentEvent]{
			ClassifyCreateError: s.classifyError,
		},
		Logger: s.logger,
		Ctx:    s.lifecycle.Context(),
	})

	s.sub = eventbus.Subscribe[eventbus.AudioIngressSegmentEvent](s.bus, eventbus.TopicAudioIngressSegment, eventbus.WithSubscriptionName("audio_vad_segments"))
	s.lifecycle.AddSubscriptions(s.sub)
	s.lifecycle.Go(s.consumeSegments)
	return nil
}

// Shutdown stops background processing and releases resources.
func (s *Service) Shutdown(ctx context.Context) error {
	s.lifecycle.Stop()

	if err := s.lifecycle.Wait(ctx); err != nil {
		return err
	}

	var handles []streammanager.StreamHandle[eventbus.AudioIngressSegmentEvent]
	if s.manager != nil {
		handles = s.manager.CloseAllStreams()
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
		if err := h.Enqueue(segment); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Printf("[VAD] enqueue segment session=%s stream=%s failed: %v", segment.SessionID, segment.StreamID, err)
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
		if err := h.Enqueue(segment); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Printf("[VAD] enqueue segment session=%s stream=%s failed: %v", segment.SessionID, segment.StreamID, err)
		}
	case errors.Is(err, ErrAdapterUnavailable):
		s.manager.BufferPending(key, params, segment)
	case errors.Is(err, ErrFactoryUnavailable):
		s.logger.Printf("[VAD] factory unavailable session=%s stream=%s, dropping segment", segment.SessionID, segment.StreamID)
	default:
		s.logger.Printf("[VAD] create stream session=%s stream=%s failed: %v", segment.SessionID, segment.StreamID, err)
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

	analyzer, err := s.factory.Create(ctx, params)
	if err != nil {
		return nil, err
	}

	return newStream(key, s, params, analyzer), nil
}

func (s *Service) publishDetection(sessionID, streamID string, segment eventbus.AudioIngressSegmentEvent, det Detection) {
	if s.bus == nil {
		return
	}

	event := eventbus.SpeechVADEvent{
		SessionID:   sessionID,
		StreamID:    streamID,
		Active:      det.Active,
		Confidence:  det.Confidence,
		Timestamp:   segment.EndedAt,
		Metadata:    mergeMetadata(maputil.Clone(segment.Metadata), det.Metadata),
		EnergyLevel: 0,
	}

	if s.logger != nil && det.Active {
		s.logger.Printf("[VAD] detection active session=%s stream=%s confidence=%.2f", sessionID, streamID, det.Confidence)
	}

	eventbus.Publish(context.Background(), s.bus, eventbus.Speech.VADDetected, eventbus.SourceSpeechVAD, event)
}

// stream is the internal per-key processing goroutine.
// It implements streammanager.StreamHandle[eventbus.AudioIngressSegmentEvent].
type stream struct {
	service   *Service
	key       string
	sessionID string
	streamID  string
	format    eventbus.AudioFormat
	metadata  map[string]string
	adapter   *streammanager.RetryableAdapter[Analyzer, Detection]

	worker *streammanager.Worker[eventbus.AudioIngressSegmentEvent]
}

func newStream(key string, svc *Service, params SessionParams, analyzer Analyzer) *stream {
	st := &stream{
		service:   svc,
		key:       key,
		sessionID: params.SessionID,
		streamID:  params.StreamID,
		format:    params.Format,
		metadata:  maputil.Clone(params.Metadata),
		worker:    streammanager.NewWorker[eventbus.AudioIngressSegmentEvent](svc.lifecycle.Context(), defaultSegmentBuffer),
	}
	st.adapter = streammanager.NewRetryableAdapter(streammanager.RetryableAdapterConfig[Analyzer, Detection]{
		Create: func(ctx context.Context) (Analyzer, error) {
			reconnected, err := st.service.factory.Create(ctx, SessionParams{
				SessionID: st.sessionID,
				StreamID:  st.streamID,
				Format:    st.format,
				Metadata:  maputil.Clone(st.metadata),
			})
			return reconnected, err
		},
		Close: func(ctx context.Context, adapter Analyzer) ([]Detection, error) {
			return adapter.Close(ctx)
		},
		IsAdapterUnavailable: func(err error) bool {
			return errors.Is(err, ErrAdapterUnavailable)
		},
		OnCreateError: func(err error) {
			st.service.logger.Printf("[VAD] reconnect analyzer session=%s stream=%s: %v", st.sessionID, st.streamID, err)
		},
		OnReconnected: func() {
			st.service.logger.Printf("[VAD] analyzer reconnected session=%s stream=%s", st.sessionID, st.streamID)
		},
		OnAdapterUnavailable: func(err error) {
			st.service.logger.Printf("[VAD] analyzer unavailable session=%s stream=%s, will retry: %v", st.sessionID, st.streamID, err)
		},
		OnProcessError: func(err error) {
			st.service.logger.Printf("[VAD] analyze segment session=%s stream=%s failed: %v", st.sessionID, st.streamID, err)
		},
		OnCloseError: func(reason string, err error) {
			st.service.logger.Printf("[VAD] close analyzer session=%s stream=%s (%s): %v", st.sessionID, st.streamID, reason, err)
		},
		RecoveryCloseTimeout: constants.Duration2Seconds,
	})
	st.adapter.SetActive(analyzer)
	st.worker.Start(st.processSegment, func() {
		st.closeAnalyzer("context cancelled")
	}, func() {
		if st.service.manager != nil {
			st.service.manager.RemoveStream(st.key, st)
		}
	})
	return st
}

// Enqueue implements streammanager.StreamHandle.
func (st *stream) Enqueue(segment eventbus.AudioIngressSegmentEvent) error {
	return st.worker.Enqueue(segment, context.Canceled)
}

// Stop implements streammanager.StreamHandle.
func (st *stream) Stop() {
	st.worker.Stop()
	if st.service.logger != nil {
		st.service.logger.Printf("[VAD] analyzer stopping session=%s stream=%s", st.sessionID, st.streamID)
	}
}

// Wait implements streammanager.StreamHandle.
func (st *stream) Wait(ctx context.Context) {
	st.worker.Wait(ctx)
}

func (st *stream) processSegment(segment eventbus.AudioIngressSegmentEvent) bool {
	st.handleSegment(segment)
	return false
}

func (st *stream) handleSegment(segment eventbus.AudioIngressSegmentEvent) {
	st.format = segment.Format
	st.metadata = maputil.Clone(segment.Metadata)

	st.adapter.Process(st.worker.Context(), func(ctx context.Context, analyzer Analyzer) ([]Detection, error) {
		return analyzer.OnSegment(ctx, segment)
	}, func(det Detection) {
		// Publish any detections returned alongside the error — the NAP
		// contract allows partial results + error (see nap_analyzer.go:162).
		st.service.publishDetection(st.sessionID, st.streamID, segment, det)
	})
}

func (st *stream) closeAnalyzer(reason string) {
	lastSegment := eventbus.AudioIngressSegmentEvent{
		SessionID: st.sessionID,
		StreamID:  st.streamID,
		EndedAt:   time.Now().UTC(),
	}
	st.adapter.Close(reason, constants.Duration2Seconds, func(det Detection) {
		st.service.publishDetection(st.sessionID, st.streamID, lastSegment, det)
	})
}

func mergeMetadata(base map[string]string, override map[string]string) map[string]string {
	return mapper.MergeStringMaps(base, override)
}
