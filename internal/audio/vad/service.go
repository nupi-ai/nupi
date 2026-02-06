package vad

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

var (
	// ErrFactoryUnavailable indicates the analyzer factory has not been configured.
	ErrFactoryUnavailable = errors.New("vad: analyzer factory unavailable")
	// ErrAdapterUnavailable indicates no active VAD adapter is bound.
	ErrAdapterUnavailable = errors.New("vad: adapter unavailable")
)

const (
	defaultSegmentBuffer = 16
	defaultRetryInitial  = 200 * time.Millisecond
	defaultRetryMax      = 5 * time.Second
	maxPendingSegments   = 100
	maxRetryFailures     = 10
	maxRetryDuration     = 2 * time.Minute
)

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

// Factory constructs analyzers for a given session stream.
type Factory interface {
	Create(ctx context.Context, params SessionParams) (Analyzer, error)
}

// FactoryFunc adapts a function to the Factory interface.
type FactoryFunc func(ctx context.Context, params SessionParams) (Analyzer, error)

// Create invokes the underlying function.
func (f FactoryFunc) Create(ctx context.Context, params SessionParams) (Analyzer, error) {
	return f(ctx, params)
}

// SessionParams captures analyzer initialisation parameters.
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

// Service streams audio segments to VAD adapters and publishes detection events.
type Service struct {
	bus     *eventbus.Bus
	factory Factory
	logger  *log.Logger

	retryInitial time.Duration
	retryMax     time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	sub *eventbus.Subscription
	wg  sync.WaitGroup

	mu      sync.Mutex
	streams map[string]*stream

	pendingMu sync.Mutex
	pending   map[string]*pendingStream

	detectionsTotal    atomic.Uint64
	retryAttemptsTotal atomic.Uint64
	retryFailuresTotal atomic.Uint64
}

// New constructs a VAD bridge service bound to the provided event bus.
func New(bus *eventbus.Bus, opts ...Option) *Service {
	svc := &Service{
		bus:          bus,
		logger:       log.Default(),
		retryInitial: defaultRetryInitial,
		retryMax:     defaultRetryMax,
		streams:      make(map[string]*stream),
		pending:      make(map[string]*pendingStream),
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

	s.ctx, s.cancel = context.WithCancel(ctx)
	s.sub = s.bus.Subscribe(eventbus.TopicAudioIngressSegment, eventbus.WithSubscriptionName("audio_vad_segments"))

	s.wg.Add(1)
	go s.consumeSegments()
	return nil
}

// Shutdown stops background processing and releases resources.
func (s *Service) Shutdown(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.sub != nil {
		s.sub.Close()
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

	streams := s.closeStreams()
	for _, st := range streams {
		st.wait(ctx)
	}

	s.pendingMu.Lock()
	for key, pending := range s.pending {
		if pending != nil {
			pending.stopTimer()
		}
		delete(s.pending, key)
	}
	s.pendingMu.Unlock()
	return nil
}

func (s *Service) closeStreams() []*stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]*stream, 0, len(s.streams))
	for key, st := range s.streams {
		list = append(list, st)
		delete(s.streams, key)
	}
	for _, st := range list {
		st.stop()
	}
	return list
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
	key := streamKey(segment.SessionID, segment.StreamID)

	if st, ok := s.stream(key); ok {
		if err := st.enqueue(segment); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Printf("[VAD] enqueue segment session=%s stream=%s failed: %v", segment.SessionID, segment.StreamID, err)
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
		if err := stream.enqueue(segment); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Printf("[VAD] enqueue segment session=%s stream=%s failed: %v", segment.SessionID, segment.StreamID, err)
		}
	case errors.Is(err, ErrAdapterUnavailable):
		s.bufferPending(key, params, segment)
	case errors.Is(err, ErrFactoryUnavailable):
		s.logger.Printf("[VAD] factory unavailable session=%s stream=%s, dropping segment", segment.SessionID, segment.StreamID)
	default:
		s.logger.Printf("[VAD] create stream session=%s stream=%s failed: %v", segment.SessionID, segment.StreamID, err)
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

	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	if existing, ok := s.streams[key]; ok {
		s.mu.Unlock()
		return existing, nil
	}
	s.mu.Unlock()

	analyzer, err := s.factory.Create(ctx, params)
	if err != nil {
		return nil, err
	}

	stream := newStream(key, s, params, analyzer)

	s.mu.Lock()
	if existing, ok := s.streams[key]; ok {
		s.mu.Unlock()
		stream.stop()
		if stream.analyzer != nil {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = stream.analyzer.Close(closeCtx)
			closeCancel()
		}
		return existing, nil
	}
	s.streams[key] = stream
	s.mu.Unlock()
	if s.logger != nil {
		s.logger.Printf("[VAD] analyzer started session=%s stream=%s", params.SessionID, params.StreamID)
	}

	s.flushPending(key, stream)
	return stream, nil
}

func (s *Service) bufferPending(key string, params SessionParams, segment eventbus.AudioIngressSegmentEvent) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	queue, ok := s.pending[key]
	if !ok {
		queue = &pendingStream{
			params: params,
		}
		s.pending[key] = queue
	}
	if len(queue.segments) >= maxPendingSegments {
		queue.segments = queue.segments[1:]
		s.logger.Printf("[VAD] pending buffer full session=%s stream=%s, dropping oldest segment", params.SessionID, params.StreamID)
	}
	queue.segments = append(queue.segments, segment)
	if s.logger != nil {
		s.logger.Printf("[VAD] buffered segment %d session=%s stream=%s (pending=%d)", segment.Sequence, params.SessionID, params.StreamID, len(queue.segments))
	}
	queue.scheduleRetry(s, key)
}

func (s *Service) flushPending(key string, st *stream) {
	s.pendingMu.Lock()
	queue, ok := s.pending[key]
	if ok {
		queue.stopTimer()
		segments := append([]eventbus.AudioIngressSegmentEvent(nil), queue.segments...)
		delete(s.pending, key)
		s.pendingMu.Unlock()
		if len(segments) > 0 && s.logger != nil {
			s.logger.Printf("[VAD] replaying %d buffered segments session=%s stream=%s", len(segments), st.sessionID, st.streamID)
		}

		for _, segment := range segments {
			if err := st.enqueue(segment); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Printf("[VAD] enqueue pending segment session=%s stream=%s failed: %v", segment.SessionID, segment.StreamID, err)
			}
		}
		return
	}
	s.pendingMu.Unlock()
}

func (s *Service) retryPending(key string) {
	s.pendingMu.Lock()
	queue, ok := s.pending[key]
	if !ok {
		s.pendingMu.Unlock()
		return
	}
	queue.timer = nil
	params := queue.params
	s.pendingMu.Unlock()

	s.retryAttemptsTotal.Add(1)

	stream, err := s.createStream(key, params)
	if err != nil {
		permanentFailure := !errors.Is(err, ErrAdapterUnavailable) && !errors.Is(err, context.Canceled)
		if permanentFailure {
			s.retryFailuresTotal.Add(1)
			s.pendingMu.Lock()
			if queue, ok := s.pending[key]; ok {
				queue.failureCount++
				if queue.firstFailureAt.IsZero() {
					queue.firstFailureAt = time.Now()
				}
				elapsed := time.Since(queue.firstFailureAt)
				if queue.failureCount >= maxRetryFailures || elapsed >= maxRetryDuration {
					delete(s.pending, key)
					s.pendingMu.Unlock()
					if s.logger != nil {
						s.logger.Printf("[VAD] abandoning retry session=%s stream=%s after %d failures", params.SessionID, params.StreamID, queue.failureCount)
					}
					return
				}
			}
			s.pendingMu.Unlock()
			if s.logger != nil {
				s.logger.Printf("[VAD] retry stream session=%s stream=%s failed: %v", params.SessionID, params.StreamID, err)
			}
		}
		s.pendingMu.Lock()
		if queue, ok := s.pending[key]; ok {
			queue.scheduleRetry(s, key)
		}
		s.pendingMu.Unlock()
		return
	}

	s.flushPending(key, stream)
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
		Metadata:    mergeMetadata(copyMetadata(segment.Metadata), det.Metadata),
		EnergyLevel: 0,
	}

	if s.logger != nil && det.Active {
		s.logger.Printf("[VAD] detection active session=%s stream=%s confidence=%.2f", sessionID, streamID, det.Confidence)
	}

	s.detectionsTotal.Add(1)

	s.bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicSpeechVADDetected,
		Source:  eventbus.SourceSpeechVAD,
		Payload: event,
	})
}

type stream struct {
	service   *Service
	key       string
	sessionID string
	streamID  string
	analyzer  Analyzer

	requestCh chan eventbus.AudioIngressSegmentEvent
	ctx       context.Context
	cancel    context.CancelFunc

	wg sync.WaitGroup
}

func newStream(key string, svc *Service, params SessionParams, analyzer Analyzer) *stream {
	ctx, cancel := context.WithCancel(svc.ctx)
	st := &stream{
		service:   svc,
		key:       key,
		sessionID: params.SessionID,
		streamID:  params.StreamID,
		analyzer:  analyzer,
		requestCh: make(chan eventbus.AudioIngressSegmentEvent, defaultSegmentBuffer),
		ctx:       ctx,
		cancel:    cancel,
	}
	st.wg.Add(1)
	go st.run()
	return st
}

func (st *stream) enqueue(segment eventbus.AudioIngressSegmentEvent) error {
	select {
	case <-st.ctx.Done():
		return context.Canceled
	default:
	}

	select {
	case st.requestCh <- segment:
		return nil
	case <-st.ctx.Done():
		return context.Canceled
	}
}

func (st *stream) run() {
	defer st.wg.Done()
	for {
		select {
		case <-st.ctx.Done():
			st.closeAnalyzer("context cancelled")
			return
		case segment, ok := <-st.requestCh:
			if !ok {
				st.closeAnalyzer("queue closed")
				return
			}
			st.handleSegment(segment)
		}
	}
}

func (st *stream) handleSegment(segment eventbus.AudioIngressSegmentEvent) {
	if st.analyzer == nil {
		params := SessionParams{
			SessionID: st.sessionID,
			StreamID:  st.streamID,
			Format:    segment.Format,
			Metadata:  copyMetadata(segment.Metadata),
		}
		analyzer, err := st.service.factory.Create(st.ctx, params)
		if err != nil {
			if !errors.Is(err, ErrAdapterUnavailable) {
				st.service.logger.Printf("[VAD] reconnect analyzer session=%s stream=%s: %v", st.sessionID, st.streamID, err)
			}
			return
		}
		st.analyzer = analyzer
		st.service.logger.Printf("[VAD] analyzer reconnected session=%s stream=%s", st.sessionID, st.streamID)
	}

	detections, err := st.analyzer.OnSegment(st.ctx, segment)

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

func (st *stream) stop() {
	st.cancel()
	close(st.requestCh)
	if st.service.logger != nil {
		st.service.logger.Printf("[VAD] analyzer stopping session=%s stream=%s", st.sessionID, st.streamID)
	}
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

func (st *stream) closeAnalyzerForRecovery(reason string) {
	if st.analyzer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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

type pendingStream struct {
	params         SessionParams
	segments       []eventbus.AudioIngressSegmentEvent
	timer          *time.Timer
	delay          time.Duration
	failureCount   int
	firstFailureAt time.Time
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
	if s.logger != nil {
		sessionID, streamID := splitStreamKey(key)
		s.logger.Printf("[VAD] scheduling adapter retry session=%s stream=%s in %s", sessionID, streamID, delay)
	}
}

func (ps *pendingStream) stopTimer() {
	if ps.timer != nil {
		ps.timer.Stop()
		ps.timer = nil
	}
	ps.delay = 0
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

func mergeMetadata(base map[string]string, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

func splitStreamKey(key string) (string, string) {
	const sep = "::"
	if idx := strings.Index(key, sep); idx >= 0 {
		return key[:idx], key[idx+len(sep):]
	}
	return key, ""
}

func streamKey(sessionID, streamID string) string {
	return sessionID + "::" + streamID
}

// Metrics aggregates counters for the VAD service.
//
// Fields:
//   - DetectionsTotal: total number of VAD detection events published (active and inactive)
//   - RetryAttemptsTotal: total number of adapter reconnection attempts triggered by the service
//   - RetryFailuresTotal: total number of retry attempts that failed with a non-recoverable error
type Metrics struct {
	DetectionsTotal    uint64
	RetryAttemptsTotal uint64
	RetryFailuresTotal uint64
}

// Metrics returns the current metrics snapshot for the VAD service.
func (s *Service) Metrics() Metrics {
	return Metrics{
		DetectionsTotal:    s.detectionsTotal.Load(),
		RetryAttemptsTotal: s.retryAttemptsTotal.Load(),
		RetryFailuresTotal: s.retryFailuresTotal.Load(),
	}
}
