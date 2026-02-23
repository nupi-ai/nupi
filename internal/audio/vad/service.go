package vad

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
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
	latencyDegradeFrames = 10
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

// WithLatencyThreshold configures the per-frame latency threshold for diagnostics.
func WithLatencyThreshold(threshold time.Duration) Option {
	return func(s *Service) {
		if threshold > 0 {
			s.latencyThreshold = threshold
		}
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

	detectionsTotal  atomic.Uint64
	framesTotal      atomic.Uint64
	latencyHistogram *latencyHistogram
	latencyThreshold time.Duration
}

// New constructs a VAD bridge service bound to the provided event bus.
func New(bus *eventbus.Bus, opts ...Option) *Service {
	svc := &Service{
		bus:              bus,
		logger:           log.Default(),
		retryInitial:     serviceutil.DefaultRetryInitial,
		retryMax:         serviceutil.DefaultRetryMax,
		latencyHistogram: newLatencyHistogram(defaultLatencyBuckets()),
		latencyThreshold: constants.Duration100Milliseconds,
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

	s.detectionsTotal.Add(1)

	eventbus.Publish(context.Background(), s.bus, eventbus.Speech.VADDetected, eventbus.SourceSpeechVAD, event)
}

func (s *Service) recordLatency(latency time.Duration) {
	s.framesTotal.Add(1)
	if s.latencyHistogram != nil {
		s.latencyHistogram.Observe(latency)
	}
}

// stream is the internal per-key processing goroutine.
// It implements streammanager.StreamHandle[eventbus.AudioIngressSegmentEvent].
type stream struct {
	service   *Service
	key       string
	sessionID string
	streamID  string
	analyzer  Analyzer

	worker *streammanager.Worker[eventbus.AudioIngressSegmentEvent]

	slowConsecutive int
}

func newStream(key string, svc *Service, params SessionParams, analyzer Analyzer) *stream {
	st := &stream{
		service:   svc,
		key:       key,
		sessionID: params.SessionID,
		streamID:  params.StreamID,
		analyzer:  analyzer,
		worker:    streammanager.NewWorker[eventbus.AudioIngressSegmentEvent](svc.lifecycle.Context(), defaultSegmentBuffer),
	}
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
	if st.analyzer == nil {
		params := SessionParams{
			SessionID: st.sessionID,
			StreamID:  st.streamID,
			Format:    segment.Format,
			Metadata:  maputil.Clone(segment.Metadata),
		}
		analyzer, err := st.service.factory.Create(st.worker.Context(), params)
		if err != nil {
			if !errors.Is(err, ErrAdapterUnavailable) {
				st.service.logger.Printf("[VAD] reconnect analyzer session=%s stream=%s: %v", st.sessionID, st.streamID, err)
			}
			return
		}
		st.analyzer = analyzer
		st.service.logger.Printf("[VAD] analyzer reconnected session=%s stream=%s", st.sessionID, st.streamID)
	}

	startedAt := time.Now()
	detections, err := st.analyzer.OnSegment(st.worker.Context(), segment)
	latency := time.Since(startedAt)
	st.service.recordLatency(latency)
	st.handleLatencyDiagnostics(latency, segment)

	// Publish any detections returned alongside the error — the NAP
	// contract allows partial results + error (see nap_analyzer.go:162).
	for _, det := range detections {
		st.service.publishDetection(st.sessionID, st.streamID, segment, det)
	}

	if err != nil {
		if errors.Is(err, ErrAdapterUnavailable) {
			st.service.logger.Printf("[VAD] analyzer unavailable session=%s stream=%s, will retry: %v", st.sessionID, st.streamID, err)
			st.closeAnalyzerForRecovery("adapter unavailable")
			return
		}
		st.service.logger.Printf("[VAD] analyze segment session=%s stream=%s failed: %v", st.sessionID, st.streamID, err)
	}
}

func (st *stream) handleLatencyDiagnostics(latency time.Duration, segment eventbus.AudioIngressSegmentEvent) {
	threshold := st.service.latencyThreshold
	if threshold <= 0 {
		return
	}

	if latency > threshold {
		st.slowConsecutive++
	} else {
		st.slowConsecutive = 0
	}

	if st.slowConsecutive < latencyDegradeFrames {
		return
	}

	st.slowConsecutive = 0
	message := "VAD latency degradation detected"
	if st.service.logger != nil {
		st.service.logger.Printf("[VAD] %s session=%s stream=%s latency_ms=%d threshold_ms=%d", message, st.sessionID, st.streamID, latency.Milliseconds(), threshold.Milliseconds())
	}

	if st.service.bus == nil {
		return
	}

	evt := eventbus.VoiceDiagnosticsEvent{
		SessionID:   st.sessionID,
		StreamID:    st.streamID,
		Type:        eventbus.VoiceDiagnosticTypeVADLatencyDegradation,
		Message:     message,
		ThresholdMs: threshold.Milliseconds(),
		ObservedMs:  latency.Milliseconds(),
		Consecutive: latencyDegradeFrames,
		Timestamp:   segment.EndedAt,
		Metadata:    maputil.Clone(segment.Metadata),
	}
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}

	eventbus.Publish(context.Background(), st.service.bus, eventbus.Voice.Diagnostics, eventbus.SourceSpeechVAD, evt)
}

func (st *stream) closeAnalyzerForRecovery(reason string) {
	if st.analyzer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), constants.Duration2Seconds)
	defer cancel()
	_, err := st.analyzer.Close(ctx)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		st.service.logger.Printf("[VAD] close broken analyzer session=%s stream=%s (%s): %v", st.sessionID, st.streamID, reason, err)
	}
	st.analyzer = nil
}

func (st *stream) closeAnalyzer(reason string) {
	if st.analyzer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), constants.Duration2Seconds)
	defer cancel()

	detections, err := st.analyzer.Close(ctx)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		st.service.logger.Printf("[VAD] close analyzer session=%s stream=%s (%s): %v", st.sessionID, st.streamID, reason, err)
	}
	if len(detections) == 0 {
		return
	}
	lastSegment := eventbus.AudioIngressSegmentEvent{
		SessionID: st.sessionID,
		StreamID:  st.streamID,
		EndedAt:   time.Now().UTC(),
	}
	for _, det := range detections {
		st.service.publishDetection(st.sessionID, st.streamID, lastSegment, det)
	}
}

func mergeMetadata(base map[string]string, override map[string]string) map[string]string {
	return mapper.MergeStringMaps(base, override)
}

type latencyHistogram struct {
	mu       sync.Mutex
	bounds   []time.Duration
	buckets  []atomic.Uint64
	sumNanos atomic.Uint64
	count    atomic.Uint64
}

func defaultLatencyBuckets() []time.Duration {
	return []time.Duration{
		time.Millisecond,
		constants.Duration5Milliseconds,
		constants.Duration10Milliseconds,
		constants.Duration25Milliseconds,
		constants.Duration50Milliseconds,
		constants.Duration100Milliseconds,
		constants.Duration250Milliseconds,
	}
}

func newLatencyHistogram(bounds []time.Duration) *latencyHistogram {
	clone := make([]time.Duration, len(bounds))
	copy(clone, bounds)
	return &latencyHistogram{
		bounds:  clone,
		buckets: make([]atomic.Uint64, len(bounds)),
	}
}

func (h *latencyHistogram) Observe(d time.Duration) {
	if h == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count.Add(1)
	h.sumNanos.Add(uint64(d.Nanoseconds()))
	for i, bound := range h.bounds {
		if d <= bound {
			h.buckets[i].Add(1)
			return
		}
	}
}

type LatencyHistogramSnapshot struct {
	Bounds []time.Duration
	Counts []uint64
	Sum    time.Duration
	Count  uint64
}

func (h *latencyHistogram) Snapshot() LatencyHistogramSnapshot {
	if h == nil {
		return LatencyHistogramSnapshot{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	count := h.count.Load()
	sum := h.sumNanos.Load()
	counts := make([]uint64, len(h.buckets))
	for i := range h.buckets {
		counts[i] = h.buckets[i].Load()
	}
	bounds := make([]time.Duration, len(h.bounds))
	copy(bounds, h.bounds)
	return LatencyHistogramSnapshot{
		Bounds: bounds,
		Counts: counts,
		Sum:    time.Duration(sum),
		Count:  count,
	}
}

// Metrics aggregates counters for the VAD service.
type Metrics struct {
	DetectionsTotal     uint64
	RetryAttemptsTotal  uint64
	RetryAbandonedTotal uint64
	FramesTotal         uint64
	ProcessingLatency   LatencyHistogramSnapshot
}

// Metrics returns the current metrics snapshot for the VAD service.
func (s *Service) Metrics() Metrics {
	m := Metrics{
		DetectionsTotal: s.detectionsTotal.Load(),
		FramesTotal:     s.framesTotal.Load(),
	}
	if s.manager != nil {
		m.RetryAttemptsTotal = s.manager.RetryAttempts()
		m.RetryAbandonedTotal = s.manager.RetryAbandoned()
	}
	if s.latencyHistogram != nil {
		m.ProcessingLatency = s.latencyHistogram.Snapshot()
	}
	return m
}
