package egress

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/voice/slots"
)

var (
	// ErrFactoryUnavailable indicates the synthesizer factory has not been configured.
	ErrFactoryUnavailable = errors.New("tts: synthesizer factory unavailable")
	// ErrAdapterUnavailable indicates no active TTS adapter is bound.
	ErrAdapterUnavailable = errors.New("tts: adapter unavailable")
	// errStreamRebuffering is returned by enqueue when a stream is being rebuffered.
	errStreamRebuffering = errors.New("tts: stream rebuffering")
)

const (
	defaultRequestBuffer = 16
	defaultRetryInitial  = 200 * time.Millisecond
	defaultRetryMax      = 5 * time.Second
	maxPendingRequests   = 100

	defaultStreamID = slots.TTS
)

// SpeakRequest represents a text-to-speech invocation.
type SpeakRequest struct {
	SessionID string
	StreamID  string
	PromptID  string
	Text      string
	Metadata  map[string]string
}

// SynthesisChunk contains PCM data to emit on the event bus.
type SynthesisChunk struct {
	Data     []byte
	Duration time.Duration
	Final    bool
	Format   *eventbus.AudioFormat
	Metadata map[string]string
}

// Synthesizer generates audio for speak requests.
type Synthesizer interface {
	Speak(ctx context.Context, req SpeakRequest) ([]SynthesisChunk, error)
	Close(ctx context.Context) ([]SynthesisChunk, error)
}

// Factory constructs synthesizers for a given session.
type Factory interface {
	Create(ctx context.Context, params SessionParams) (Synthesizer, error)
}

// FactoryFunc adapts a function to the Factory interface.
type FactoryFunc func(ctx context.Context, params SessionParams) (Synthesizer, error)

// Create invokes the underlying function.
func (f FactoryFunc) Create(ctx context.Context, params SessionParams) (Synthesizer, error) {
	return f(ctx, params)
}

// SessionParams describes synthesizer initialisation parameters.
type SessionParams struct {
	SessionID string
	StreamID  string
	Format    eventbus.AudioFormat
	Metadata  map[string]string
	AdapterID string
	Config    map[string]any
}

// Option configures the Service.
type Option func(*Service)

// WithLogger overrides the default logger.
func WithLogger(logger *log.Logger) Option {
	return func(s *Service) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// WithFactory sets the synthesizer factory used by the service.
func WithFactory(factory Factory) Option {
	return func(s *Service) {
		if factory != nil {
			s.factory = factory
		}
	}
}

// WithAudioFormat overrides the playback audio format.
func WithAudioFormat(format eventbus.AudioFormat) Option {
	return func(s *Service) {
		s.format = format
	}
}

// WithRetryDelays customises retry backoff for adapter availability.
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

// WithStreamID overrides the default stream identifier used for playback.
func WithStreamID(id string) Option {
	return func(s *Service) {
		if id != "" {
			s.streamID = id
		}
	}
}

// Service consumes conversation replies and manual speak requests, producing audio playback events.
type Service struct {
	bus     *eventbus.Bus
	factory Factory
	logger  *log.Logger

	format       eventbus.AudioFormat
	streamID     string
	retryInitial time.Duration
	retryMax     time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	subs     []*eventbus.Subscription
	bargeSub *eventbus.Subscription
	wg       sync.WaitGroup

	mu      sync.Mutex
	streams map[string]*stream

	pendingMu sync.Mutex
	pending   map[string]*pendingQueue

	activeStreams atomic.Int64
}

// New constructs an audio egress service.
func New(bus *eventbus.Bus, opts ...Option) *Service {
	svc := &Service{
		bus:     bus,
		logger:  log.Default(),
		streams: make(map[string]*stream),
		pending: make(map[string]*pendingQueue),
		format: eventbus.AudioFormat{
			Encoding:      eventbus.AudioEncodingPCM16,
			SampleRate:    16000,
			Channels:      1,
			BitDepth:      16,
			FrameDuration: 20 * time.Millisecond,
		},
		streamID:     defaultStreamID,
		retryInitial: defaultRetryInitial,
		retryMax:     defaultRetryMax,
		factory: FactoryFunc(func(context.Context, SessionParams) (Synthesizer, error) {
			return nil, ErrFactoryUnavailable
		}),
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// DefaultStreamID returns the default playback stream identifier.
func (s *Service) DefaultStreamID() string {
	return s.streamID
}

// PlaybackFormat returns the configured output audio format.
func (s *Service) PlaybackFormat() eventbus.AudioFormat {
	return s.format
}

// Interrupt stops playback for the specified session/stream.
func (s *Service) Interrupt(sessionID, streamID, reason string, metadata map[string]string) {
	if sessionID == "" {
		return
	}
	s.interruptStream(sessionID, streamID, reason, time.Now().UTC(), metadata)
}

// Start subscribes to bus topics and starts processing.
func (s *Service) Start(ctx context.Context) error {
	if s.bus == nil {
		return errors.New("tts: event bus required")
	}

	s.ctx, s.cancel = context.WithCancel(ctx)

	replySub := s.bus.Subscribe(eventbus.TopicConversationReply, eventbus.WithSubscriptionName("audio_egress_reply"))
	speakSub := s.bus.Subscribe(eventbus.TopicConversationSpeak, eventbus.WithSubscriptionName("audio_egress_speak"))
	lifecycleSub := s.bus.Subscribe(eventbus.TopicSessionsLifecycle, eventbus.WithSubscriptionName("audio_egress_lifecycle"))
	bargeSub := s.bus.Subscribe(eventbus.TopicSpeechBargeIn, eventbus.WithSubscriptionName("audio_egress_barge"))
	s.subs = []*eventbus.Subscription{replySub, speakSub, lifecycleSub, bargeSub}
	s.bargeSub = bargeSub

	s.wg.Add(4)
	go s.consumeReplies(replySub)
	go s.consumeSpeak(speakSub)
	go s.consumeLifecycle(lifecycleSub)
	go s.consumeBarge(bargeSub)
	return nil
}

// Shutdown stops background processing and releases resources.
func (s *Service) Shutdown(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	for _, sub := range s.subs {
		if sub != nil {
			sub.Close()
		}
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
	for key, queue := range s.pending {
		if queue != nil {
			queue.stopTimer()
		}
		delete(s.pending, key)
	}
	s.pendingMu.Unlock()
	return nil
}

func (s *Service) closeStreams() []*stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	streams := make([]*stream, 0, len(s.streams))
	for key, st := range s.streams {
		streams = append(streams, st)
		delete(s.streams, key)
	}
	if count := len(streams); count > 0 {
		s.activeStreams.Add(-int64(count))
		if s.logger != nil {
			s.logger.Printf("[TTS] closing %d active streams", count)
		}
	}
	for _, st := range streams {
		st.stop()
	}
	return streams
}

func (s *Service) consumeReplies(sub *eventbus.Subscription) {
	defer s.wg.Done()
	if sub == nil {
		return
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			reply, ok := env.Payload.(eventbus.ConversationReplyEvent)
			if !ok {
				continue
			}
			s.handleSpeakRequest(speakRequest{
				SessionID: reply.SessionID,
				StreamID:  s.streamID,
				PromptID:  reply.PromptID,
				Text:      reply.Text,
				Metadata:  copyMetadata(reply.Metadata),
			})
		}
	}
}

func (s *Service) consumeSpeak(sub *eventbus.Subscription) {
	defer s.wg.Done()
	if sub == nil {
		return
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			event, ok := env.Payload.(eventbus.ConversationSpeakEvent)
			if !ok {
				continue
			}
			s.handleSpeakRequest(speakRequest{
				SessionID: event.SessionID,
				StreamID:  s.streamID,
				PromptID:  event.PromptID,
				Text:      event.Text,
				Metadata:  copyMetadata(event.Metadata),
			})
		}
	}
}

func (s *Service) consumeLifecycle(sub *eventbus.Subscription) {
	defer s.wg.Done()
	if sub == nil {
		return
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			msg, ok := env.Payload.(eventbus.SessionLifecycleEvent)
			if !ok {
				continue
			}
			if msg.State == eventbus.SessionStateStopped {
				s.removeStream(msg.SessionID)
			}
		}
	}
}

func (s *Service) consumeBarge(sub *eventbus.Subscription) {
	defer s.wg.Done()
	if sub == nil {
		return
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			evt, ok := env.Payload.(eventbus.SpeechBargeInEvent)
			if !ok {
				continue
			}
			s.handleBargeEvent(evt)
		}
	}
}

func (s *Service) handleBargeEvent(event eventbus.SpeechBargeInEvent) {
	if event.SessionID == "" || event.StreamID == "" {
		return
	}
	s.interruptStream(event.SessionID, event.StreamID, event.Reason, event.Timestamp, copyMetadata(event.Metadata))
}

type speakRequest struct {
	SessionID string
	StreamID  string
	PromptID  string
	Text      string
	Metadata  map[string]string
}

func (s *Service) handleSpeakRequest(req speakRequest) {
	if req.SessionID == "" || req.StreamID == "" || req.Text == "" {
		return
	}
	key := streamKey(req.SessionID, req.StreamID)

	if st, ok := s.stream(key); ok {
		if err := st.enqueue(req); err == nil {
			return
		}
		// Enqueue failed — stream is dying (rebuffer/interrupt) or service
		// shutting down.  If the service is still running, buffer the request
		// so it survives the stream transition rather than being dropped.
		if s.ctx.Err() == nil {
			s.bufferPending(key, SessionParams{
				SessionID: req.SessionID,
				StreamID:  req.StreamID,
				Format:    s.format,
				Metadata:  copyMetadata(req.Metadata),
			}, req)
		}
		return
	}

	params := SessionParams{
		SessionID: req.SessionID,
		StreamID:  req.StreamID,
		Format:    s.format,
		Metadata:  copyMetadata(req.Metadata),
	}

	stream, err := s.createStream(key, params)
	switch {
	case err == nil:
		if err := stream.enqueue(req); err != nil {
			if errors.Is(err, errStreamRebuffering) {
				s.bufferPending(key, params, req)
			} else if !errors.Is(err, context.Canceled) {
				s.logger.Printf("[TTS] enqueue request session=%s stream=%s failed: %v", req.SessionID, req.StreamID, err)
			}
		}
	case errors.Is(err, ErrAdapterUnavailable):
		s.bufferPending(key, params, req)
	case errors.Is(err, ErrFactoryUnavailable):
		s.logger.Printf("[TTS] factory unavailable session=%s stream=%s, dropping request", params.SessionID, params.StreamID)
	default:
		s.logger.Printf("[TTS] create stream session=%s stream=%s failed: %v", params.SessionID, params.StreamID, err)
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

	synth, err := s.factory.Create(ctx, params)
	if err != nil {
		return nil, err
	}

	stream := newStream(key, s, params, synth)

	s.mu.Lock()
	if existing, ok := s.streams[key]; ok {
		s.mu.Unlock()
		stream.stop()
		return existing, nil
	}
	s.streams[key] = stream
	s.activeStreams.Add(1)
	s.mu.Unlock()
	if s.logger != nil {
		s.logger.Printf("[TTS] stream opened session=%s stream=%s", params.SessionID, params.StreamID)
	}

	s.flushPending(key, stream)
	return stream, nil
}

func (s *Service) removeStream(key string) {
	var (
		st      *stream
		removed bool
	)
	s.mu.Lock()
	if existing, ok := s.streams[key]; ok {
		st = existing
		delete(s.streams, key)
		removed = true
	}
	s.mu.Unlock()
	if removed {
		s.activeStreams.Add(-1)
		if s.logger != nil {
			s.logger.Printf("[TTS] stream stop requested session=%s stream=%s", st.sessionID, st.streamID)
		}
	}
	if st != nil {
		st.stop()
	}
}

func (s *Service) interruptStream(sessionID, streamID, reason string, ts time.Time, metadata map[string]string) {
	if streamID == "" {
		streamID = s.streamID
	}
	key := streamKey(sessionID, streamID)
	st, ok := s.stream(key)
	if !ok {
		return
	}
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	s.clearPending(key)
	st.interrupt(reasonOrDefault(reason), ts, metadata)
}

func (s *Service) clearPending(key string) {
	s.pendingMu.Lock()
	if queue, ok := s.pending[key]; ok {
		queue.stopTimer()
		delete(s.pending, key)
	}
	s.pendingMu.Unlock()
}

func (s *Service) onStreamClosed(key string, st *stream) {
	removed := false
	s.mu.Lock()
	if current, ok := s.streams[key]; ok && current == st {
		delete(s.streams, key)
		removed = true
	}
	s.mu.Unlock()
	if removed {
		s.activeStreams.Add(-1)
		if s.logger != nil {
			s.logger.Printf("[TTS] stream closed session=%s stream=%s interrupted=%t", st.sessionID, st.streamID, st.interrupted)
		}
	}
	if !st.keepPending {
		s.clearPending(key)
		return
	}
	// Drain any requests that arrived between rebufferPending and stream
	// removal from the map. Without this, requests enqueued after the
	// drain in rebufferPending but before onStreamClosed would be lost.
	params := SessionParams{
		SessionID: st.sessionID,
		StreamID:  st.streamID,
		Format:    st.format,
		Metadata:  copyMetadata(st.metadata),
	}
	for {
		select {
		case req, ok := <-st.requestCh:
			if !ok {
				return
			}
			s.bufferPending(st.key, params, req)
		default:
			return
		}
	}
}

type pendingQueue struct {
	params  SessionParams
	records []speakRequest
	timer   *time.Timer
	delay   time.Duration
}

func (pq *pendingQueue) scheduleRetry(s *Service, key string) {
	if pq.timer != nil {
		return
	}

	delay := pq.delay
	if delay <= 0 {
		delay = s.retryInitial
	} else {
		delay *= 2
		if delay > s.retryMax {
			delay = s.retryMax
		}
	}
	pq.delay = delay
	pq.timer = time.AfterFunc(delay, func() {
		s.retryPending(key)
	})
	if s.logger != nil {
		sessionID, streamID := splitStreamKey(key)
		s.logger.Printf("[TTS] scheduling adapter retry session=%s stream=%s in %s", sessionID, streamID, delay)
	}
}

func (pq *pendingQueue) stopTimer() {
	if pq.timer != nil {
		pq.timer.Stop()
		pq.timer = nil
	}
	pq.delay = 0
}

func (s *Service) bufferPending(key string, params SessionParams, req speakRequest) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	queue, ok := s.pending[key]
	if !ok {
		queue = &pendingQueue{
			params: params,
		}
		s.pending[key] = queue
	}
	if len(queue.records) >= maxPendingRequests {
		queue.records = queue.records[1:]
		s.logger.Printf("[TTS] pending buffer full session=%s stream=%s, dropping oldest request", queue.params.SessionID, queue.params.StreamID)
	}
	queue.records = append(queue.records, req)
	if s.logger != nil {
		s.logger.Printf("[TTS] buffered speak request session=%s stream=%s pending=%d prompt=%s", req.SessionID, req.StreamID, len(queue.records), strings.TrimSpace(req.PromptID))
	}
	queue.scheduleRetry(s, key)
}

func (s *Service) flushPending(key string, st *stream) {
	s.pendingMu.Lock()
	queue, ok := s.pending[key]
	if ok {
		queue.stopTimer()
		records := append([]speakRequest(nil), queue.records...)
		delete(s.pending, key)
		s.pendingMu.Unlock()
		if len(records) > 0 && s.logger != nil {
			s.logger.Printf("[TTS] replaying %d buffered requests session=%s stream=%s", len(records), st.sessionID, st.streamID)
		}

		for _, req := range records {
			if err := st.enqueue(req); err != nil {
				if errors.Is(err, errStreamRebuffering) {
					s.bufferPending(key, SessionParams{
						SessionID: st.sessionID,
						StreamID:  st.streamID,
						Format:    st.format,
						Metadata:  copyMetadata(st.metadata),
					}, req)
				} else if !errors.Is(err, context.Canceled) {
					s.logger.Printf("[TTS] enqueue pending request session=%s stream=%s failed: %v", req.SessionID, req.StreamID, err)
				}
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

	stream, err := s.createStream(key, params)
	if err != nil {
		if errors.Is(err, ErrAdapterUnavailable) {
			s.pendingMu.Lock()
			if queue, ok := s.pending[key]; ok {
				queue.scheduleRetry(s, key)
			}
			s.pendingMu.Unlock()
			return
		}
		if !errors.Is(err, context.Canceled) {
			s.logger.Printf("[TTS] retry stream session=%s stream=%s permanent error, dropping pending: %v", params.SessionID, params.StreamID, err)
		}
		s.pendingMu.Lock()
		if queue, ok := s.pending[key]; ok {
			queue.stopTimer()
			delete(s.pending, key)
		}
		s.pendingMu.Unlock()
		return
	}

	s.flushPending(key, stream)
}

type stream struct {
	service *Service
	key     string

	sessionID   string
	streamID    string
	format      eventbus.AudioFormat
	metadata    map[string]string
	synthesizer Synthesizer

	requestCh chan speakRequest
	ctx       context.Context
	cancel    context.CancelFunc

	mu sync.RWMutex

	wg          sync.WaitGroup
	seq         uint64
	stopped     bool
	keepPending bool

	interrupted        bool
	interruptReason    string
	interruptTimestamp time.Time
	interruptMeta      map[string]string
}

func newStream(key string, svc *Service, params SessionParams, synth Synthesizer) *stream {
	ctx, cancel := context.WithCancel(svc.ctx)
	st := &stream{
		service:     svc,
		key:         key,
		sessionID:   params.SessionID,
		streamID:    params.StreamID,
		format:      params.Format,
		metadata:    copyMetadata(params.Metadata),
		synthesizer: synth,
		requestCh:   make(chan speakRequest, defaultRequestBuffer),
		ctx:         ctx,
		cancel:      cancel,
	}
	st.wg.Add(1)
	go st.run()
	return st
}

func (st *stream) interrupt(reason string, ts time.Time, metadata map[string]string) {
	st.service.logger.Printf("[TTS] barge-in interrupt session=%s stream=%s (%s)", st.sessionID, st.streamID, reason)
	st.mu.Lock()
	st.interrupted = true
	st.interruptReason = reason
	st.interruptTimestamp = ts
	st.interruptMeta = copyMetadata(metadata)
	st.mu.Unlock()
	st.cancel()
}

func (st *stream) decorateMetadata(meta map[string]string) map[string]string {
	st.mu.RLock()
	interrupted := st.interrupted
	reason := st.interruptReason
	ts := st.interruptTimestamp
	extras := copyMetadata(st.interruptMeta)
	st.mu.RUnlock()

	if !interrupted {
		return meta
	}
	if meta == nil {
		meta = make(map[string]string)
	}
	meta["barge_in"] = "true"
	if reason != "" {
		meta["barge_in_reason"] = reason
	}
	if !ts.IsZero() {
		meta["barge_in_timestamp"] = ts.Format(time.RFC3339Nano)
	}
	for k, v := range extras {
		if _, exists := meta[k]; !exists {
			meta[k] = v
		}
	}
	return meta
}

func (st *stream) enqueue(req speakRequest) error {
	st.mu.RLock()
	stopped := st.stopped
	st.mu.RUnlock()
	if stopped {
		return errStreamRebuffering
	}

	select {
	case <-st.ctx.Done():
		return context.Canceled
	default:
	}

	select {
	case st.requestCh <- req:
		return nil
	case <-st.ctx.Done():
		return context.Canceled
	}
}

func (st *stream) run() {
	defer st.wg.Done()
	defer st.service.onStreamClosed(st.key, st)
	for {
		select {
		case <-st.ctx.Done():
			st.closeSynthesizer("context cancelled")
			return
		case req, ok := <-st.requestCh:
			if !ok {
				st.closeSynthesizer("queue closed")
				return
			}
			if st.handleRequest(req) {
				// Close synthesizer quietly — don't publish final events
				// since the request is being rebuffered for retry.
				if st.synthesizer != nil {
					closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
					st.synthesizer.Close(closeCtx)
					closeCancel()
				}
				st.rebufferPending(req)
				return
			}
		}
	}
}

// handleRequest returns true if the error is ErrAdapterUnavailable, signalling
// the caller to rebuffer the request and close the stream.
func (st *stream) handleRequest(req speakRequest) bool {
	ctx := st.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	chunks, err := st.synthesizer.Speak(ctx, SpeakRequest{
		SessionID: req.SessionID,
		StreamID:  req.StreamID,
		PromptID:  req.PromptID,
		Text:      req.Text,
		Metadata:  req.Metadata,
	})
	if err != nil {
		if errors.Is(err, ErrAdapterUnavailable) {
			st.service.logger.Printf("[TTS] synthesizer unavailable session=%s stream=%s, rebuffering: %v", st.sessionID, st.streamID, err)
			return true
		}
		st.service.logger.Printf("[TTS] synthesizer speak session=%s stream=%s failed: %v", st.sessionID, st.streamID, err)
		return false
	}

	if len(chunks) == 0 {
		chunks = []SynthesisChunk{{
			Data:     nil,
			Duration: 0,
			Final:    true,
		}}
	}

	for _, chunk := range chunks {
		st.seq++
		format := st.format
		if chunk.Format != nil {
			format = *chunk.Format
		}
		duration := chunkDuration(format, chunk)
		metadata := mergeMetadata(st.metadata, chunk.Metadata, req.Metadata)
		metadata = st.decorateMetadata(metadata)
		evt := eventbus.AudioEgressPlaybackEvent{
			SessionID: st.sessionID,
			StreamID:  st.streamID,
			Sequence:  st.seq,
			Format:    format,
			Duration:  duration,
			Data:      append([]byte(nil), chunk.Data...),
			Final:     chunk.Final,
			Metadata:  metadata,
		}
		st.service.publishPlayback(evt)
	}
	return false
}

func (st *stream) publishFinal() {
	st.seq++
	metadata := copyMetadata(st.metadata)
	metadata = st.decorateMetadata(metadata)
	evt := eventbus.AudioEgressPlaybackEvent{
		SessionID: st.sessionID,
		StreamID:  st.streamID,
		Sequence:  st.seq,
		Format:    st.format,
		Duration:  0,
		Final:     true,
		Metadata:  metadata,
	}
	st.service.publishPlayback(evt)
}

func (st *stream) rebufferPending(failedReq speakRequest) {
	st.mu.Lock()
	st.stopped = true
	st.mu.Unlock()

	st.keepPending = true
	params := SessionParams{
		SessionID: st.sessionID,
		StreamID:  st.streamID,
		Format:    st.format,
		Metadata:  copyMetadata(st.metadata),
	}
	st.service.bufferPending(st.key, params, failedReq)

	// Cancel context BEFORE draining so that racing enqueue calls
	// see ctx.Done in their first select and fail fast.  Any request
	// that still slips through the nondeterministic second select will
	// land in the channel buffer and be caught by the drain below.
	st.cancel()

	// Drain any remaining queued requests into the pending buffer.
	for {
		select {
		case req, ok := <-st.requestCh:
			if !ok {
				return
			}
			st.service.bufferPending(st.key, params, req)
		default:
			return
		}
	}
}

func (st *stream) stop() {
	st.mu.Lock()
	st.stopped = true
	st.mu.Unlock()
	st.cancel()
	close(st.requestCh)
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

func (st *stream) closeSynthesizer(reason string) {
	if st.synthesizer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	chunks, err := st.synthesizer.Close(ctx)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		st.service.logger.Printf("[TTS] close synthesizer session=%s stream=%s (%s): %v", st.sessionID, st.streamID, reason, err)
	}
	for _, chunk := range chunks {
		st.seq++
		metadata := mergeMetadata(st.metadata, chunk.Metadata, nil)
		metadata = st.decorateMetadata(metadata)
		format := st.format
		if chunk.Format != nil {
			format = *chunk.Format
		}
		evt := eventbus.AudioEgressPlaybackEvent{
			SessionID: st.sessionID,
			StreamID:  st.streamID,
			Sequence:  st.seq,
			Format:    format,
			Duration:  chunkDuration(format, chunk),
			Data:      append([]byte(nil), chunk.Data...),
			Final:     chunk.Final,
			Metadata:  metadata,
		}
		st.service.publishPlayback(evt)
	}
	st.mu.RLock()
	stopped := st.stopped
	interrupted := st.interrupted
	st.mu.RUnlock()
	if len(chunks) == 0 && (!stopped || interrupted) {
		st.publishFinal()
	}
}

func (s *Service) publishPlayback(evt eventbus.AudioEgressPlaybackEvent) {
	if s.bus == nil {
		return
	}

	s.bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicAudioEgressPlayback,
		Source:  eventbus.SourceAudioEgress,
		Payload: evt,
	})
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

func mergeMetadata(base map[string]string, chunk map[string]string, req map[string]string) map[string]string {
	size := len(base) + len(chunk) + len(req)
	if size == 0 {
		return nil
	}
	out := make(map[string]string, size)
	for k, v := range base {
		out[k] = v
	}
	for k, v := range req {
		out[k] = v
	}
	for k, v := range chunk {
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

func reasonOrDefault(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "interrupt"
	}
	return reason
}

func chunkDuration(format eventbus.AudioFormat, chunk SynthesisChunk) time.Duration {
	if chunk.Duration > 0 {
		return chunk.Duration
	}
	return durationFromPCM(format, len(chunk.Data))
}

func durationFromPCM(format eventbus.AudioFormat, bytes int) time.Duration {
	if bytes <= 0 || format.SampleRate <= 0 || format.Channels <= 0 || format.BitDepth <= 0 {
		return 0
	}
	bytesPerSample := format.BitDepth / 8
	if bytesPerSample <= 0 {
		return 0
	}
	frameSize := format.Channels * bytesPerSample
	if frameSize <= 0 {
		return 0
	}
	samples := bytes / frameSize
	if samples <= 0 {
		return 0
	}
	seconds := float64(samples) / float64(format.SampleRate)
	return time.Duration(seconds * float64(time.Second))
}

// Metrics reports aggregated statistics for the TTS service.
type Metrics struct {
	ActiveStreams int64
}

// Metrics returns the current TTS metrics snapshot.
func (s *Service) Metrics() Metrics {
	return Metrics{
		ActiveStreams: s.activeStreams.Load(),
	}
}
