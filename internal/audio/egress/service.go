package egress

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/audio/serviceutil"
	"github.com/nupi-ai/nupi/internal/audio/streammanager"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/mapper"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
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
	defaultRequestBuffer = serviceutil.DefaultWorkerBuffer
	maxPendingRequests   = serviceutil.DefaultMaxPending

	defaultStreamID = slots.TTS
)

type SessionParams = serviceutil.SessionParams

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

type Factory = serviceutil.Factory[Synthesizer]
type FactoryFunc = serviceutil.FactoryFunc[Synthesizer]

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
		s.retryInitial, s.retryMax = serviceutil.NormalizeRetryDelays(s.retryInitial, s.retryMax, initial, max)
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

	lifecycle eventbus.ServiceLifecycle

	replySub     *eventbus.TypedSubscription[eventbus.ConversationReplyEvent]
	speakSub     *eventbus.TypedSubscription[eventbus.ConversationSpeakEvent]
	lifecycleSub *eventbus.TypedSubscription[eventbus.SessionLifecycleEvent]
	bargeSub     *eventbus.TypedSubscription[eventbus.SpeechBargeInEvent]

	manager *streammanager.Manager[speakRequest]
}

// New constructs an audio egress service.
func New(bus *eventbus.Bus, opts ...Option) *Service {
	svc := &Service{
		bus:    bus,
		logger: log.Default(),
		format: eventbus.AudioFormat{
			Encoding:      eventbus.AudioEncodingPCM16,
			SampleRate:    16000,
			Channels:      1,
			BitDepth:      16,
			FrameDuration: 20 * time.Millisecond,
		},
		streamID:     defaultStreamID,
		retryInitial: serviceutil.DefaultRetryInitial,
		retryMax:     serviceutil.DefaultRetryMax,
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

	s.lifecycle.Start(ctx)

	s.manager = streammanager.New(streammanager.Config[speakRequest]{
		Tag:        "TTS",
		MaxPending: maxPendingRequests,
		Retry: streammanager.RetryConfig{
			Initial: s.retryInitial,
			Max:     s.retryMax,
		},
		Factory: streammanager.StreamFactoryFunc[speakRequest](s.createStreamHandle),
		Callbacks: streammanager.Callbacks[speakRequest]{
			ClassifyCreateError: s.classifyError,
			OnEnqueueError:      s.onEnqueueError,
		},
		Logger: s.logger,
		Ctx:    s.lifecycle.Context(),
	})

	s.replySub = eventbus.Subscribe[eventbus.ConversationReplyEvent](s.bus, eventbus.TopicConversationReply, eventbus.WithSubscriptionName("audio_egress_reply"))
	s.speakSub = eventbus.Subscribe[eventbus.ConversationSpeakEvent](s.bus, eventbus.TopicConversationSpeak, eventbus.WithSubscriptionName("audio_egress_speak"))
	s.lifecycleSub = eventbus.Subscribe[eventbus.SessionLifecycleEvent](s.bus, eventbus.TopicSessionsLifecycle, eventbus.WithSubscriptionName("audio_egress_lifecycle"))
	s.bargeSub = eventbus.Subscribe[eventbus.SpeechBargeInEvent](s.bus, eventbus.TopicSpeechBargeIn, eventbus.WithSubscriptionName("audio_egress_barge"))
	s.lifecycle.AddSubscriptions(s.replySub, s.speakSub, s.lifecycleSub, s.bargeSub)
	s.lifecycle.Go(s.consumeReplies)
	s.lifecycle.Go(s.consumeSpeak)
	s.lifecycle.Go(s.consumeLifecycle)
	s.lifecycle.Go(s.consumeBarge)
	return nil
}

// Shutdown stops background processing and releases resources.
func (s *Service) Shutdown(ctx context.Context) error {
	s.lifecycle.Stop()

	if err := s.lifecycle.Wait(ctx); err != nil {
		return err
	}

	var handles []streammanager.StreamHandle[speakRequest]
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

func (s *Service) consumeReplies(ctx context.Context) {
	eventbus.Consume(ctx, s.replySub, nil, func(reply eventbus.ConversationReplyEvent) {
		s.handleSpeakRequest(speakRequest{
			SessionID: reply.SessionID,
			StreamID:  s.streamID,
			PromptID:  reply.PromptID,
			Text:      reply.Text,
			Metadata:  maputil.Clone(reply.Metadata),
		})
	})
}

func (s *Service) consumeSpeak(ctx context.Context) {
	eventbus.Consume(ctx, s.speakSub, nil, func(event eventbus.ConversationSpeakEvent) {
		s.handleSpeakRequest(speakRequest{
			SessionID: event.SessionID,
			StreamID:  s.streamID,
			PromptID:  event.PromptID,
			Text:      event.Text,
			Metadata:  maputil.Clone(event.Metadata),
		})
	})
}

func (s *Service) consumeLifecycle(ctx context.Context) {
	eventbus.Consume(ctx, s.lifecycleSub, nil, func(msg eventbus.SessionLifecycleEvent) {
		if msg.State == eventbus.SessionStateStopped && s.manager != nil {
			s.manager.RemoveStreamByKey(streammanager.StreamKey(msg.SessionID, s.streamID))
		}
	})
}

func (s *Service) consumeBarge(ctx context.Context) {
	eventbus.Consume(ctx, s.bargeSub, nil, s.handleBargeEvent)
}

func (s *Service) handleBargeEvent(event eventbus.SpeechBargeInEvent) {
	if event.SessionID == "" || event.StreamID == "" {
		return
	}
	s.interruptStream(event.SessionID, event.StreamID, event.Reason, event.Timestamp, maputil.Clone(event.Metadata))
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
	key := streammanager.StreamKey(req.SessionID, req.StreamID)

	if h, ok := s.manager.Stream(key); ok {
		if err := h.Enqueue(req); err == nil {
			return
		}
		// Enqueue failed — stream is dying (rebuffer/interrupt) or service
		// shutting down.  If the service is still running, buffer the request
		// so it survives the stream transition rather than being dropped.
		if s.lifecycle.Context().Err() == nil {
			s.manager.BufferPending(key, SessionParams{
				SessionID: req.SessionID,
				StreamID:  req.StreamID,
				Format:    s.format,
				Metadata:  maputil.Clone(req.Metadata),
			}, req)
		}
		return
	}

	params := SessionParams{
		SessionID: req.SessionID,
		StreamID:  req.StreamID,
		Format:    s.format,
		Metadata:  maputil.Clone(req.Metadata),
	}

	h, err := s.manager.CreateStream(key, params)
	switch {
	case err == nil:
		if err := h.Enqueue(req); err != nil {
			if errors.Is(err, errStreamRebuffering) {
				s.manager.BufferPending(key, params, req)
			} else if !errors.Is(err, context.Canceled) {
				s.logger.Printf("[TTS] enqueue request session=%s stream=%s failed: %v", req.SessionID, req.StreamID, err)
			}
		}
	case errors.Is(err, ErrAdapterUnavailable):
		s.manager.BufferPending(key, params, req)
	case errors.Is(err, ErrFactoryUnavailable):
		s.logger.Printf("[TTS] factory unavailable session=%s stream=%s, dropping request", params.SessionID, params.StreamID)
	default:
		s.logger.Printf("[TTS] create stream session=%s stream=%s failed: %v", params.SessionID, params.StreamID, err)
	}
}

func (s *Service) interruptStream(sessionID, streamID, reason string, ts time.Time, metadata map[string]string) {
	if s.manager == nil {
		return
	}
	if streamID == "" {
		streamID = s.streamID
	}
	key := streammanager.StreamKey(sessionID, streamID)
	h, ok := s.manager.Stream(key)
	if !ok {
		return
	}
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	s.manager.ClearPending(key)
	st := h.(*stream)
	st.interrupt(reasonOrDefault(reason), ts, metadata)
}

func (s *Service) classifyError(err error) (adapterUnavailable, factoryUnavailable bool) {
	return errors.Is(err, ErrAdapterUnavailable), errors.Is(err, ErrFactoryUnavailable)
}

func (s *Service) onEnqueueError(key string, item speakRequest, err error) (handled bool) {
	if errors.Is(err, errStreamRebuffering) {
		s.manager.BufferPending(key, SessionParams{
			SessionID: item.SessionID,
			StreamID:  item.StreamID,
			Format:    s.format,
			Metadata:  maputil.Clone(item.Metadata),
		}, item)
		return true
	}
	if !errors.Is(err, context.Canceled) {
		s.logger.Printf("[TTS] enqueue pending request session=%s stream=%s failed: %v", item.SessionID, item.StreamID, err)
	}
	return true
}

// createStreamHandle is the StreamFactory callback for the manager.
func (s *Service) createStreamHandle(ctx context.Context, key string, params SessionParams) (streammanager.StreamHandle[speakRequest], error) {
	if s.factory == nil {
		return nil, ErrFactoryUnavailable
	}

	synth, err := s.factory.Create(ctx, params)
	if err != nil {
		return nil, err
	}

	return newStream(key, s, params, synth), nil
}

// onStreamClosed is called from the stream worker goroutine when it exits.
func (s *Service) onStreamClosed(key string, st *stream) {
	if s.manager != nil {
		s.manager.RemoveStream(key, st)
	}
	if !st.keepPending {
		if s.manager != nil {
			s.manager.ClearPending(key)
		}
		return
	}
	// Drain any requests that arrived between rebufferPending and stream
	// removal from the map. Without this, requests enqueued after the
	// drain in rebufferPending but before onStreamClosed would be lost.
	params := SessionParams{
		SessionID: st.sessionID,
		StreamID:  st.streamID,
		Format:    st.format,
		Metadata:  maputil.Clone(st.metadata),
	}
	st.worker.DrainNonBlocking(func(req speakRequest) {
		s.manager.BufferPending(st.key, params, req)
	})
}

type stream struct {
	service *Service
	key     string

	sessionID   string
	streamID    string
	format      eventbus.AudioFormat
	metadata    map[string]string
	synthesizer Synthesizer

	worker *streammanager.Worker[speakRequest]

	mu sync.RWMutex

	seq         uint64
	stopped     bool
	keepPending bool

	interrupted        bool
	interruptReason    string
	interruptTimestamp time.Time
	interruptMeta      map[string]string
}

func newStream(key string, svc *Service, params SessionParams, synth Synthesizer) *stream {
	st := &stream{
		service:     svc,
		key:         key,
		sessionID:   params.SessionID,
		streamID:    params.StreamID,
		format:      params.Format,
		metadata:    maputil.Clone(params.Metadata),
		synthesizer: synth,
		worker:      streammanager.NewWorker[speakRequest](svc.lifecycle.Context(), defaultRequestBuffer),
	}
	st.worker.Start(st.processRequest, func() {
		st.closeSynthesizer("context cancelled")
	}, func() {
		st.service.onStreamClosed(st.key, st)
	})
	return st
}

func (st *stream) interrupt(reason string, ts time.Time, metadata map[string]string) {
	st.service.logger.Printf("[TTS] barge-in interrupt session=%s stream=%s (%s)", st.sessionID, st.streamID, reason)
	st.mu.Lock()
	st.interrupted = true
	st.interruptReason = reason
	st.interruptTimestamp = ts
	st.interruptMeta = maputil.Clone(metadata)
	st.mu.Unlock()
	st.worker.Stop()
}

func (st *stream) decorateMetadata(meta map[string]string) map[string]string {
	st.mu.RLock()
	interrupted := st.interrupted
	reason := st.interruptReason
	ts := st.interruptTimestamp
	extras := maputil.Clone(st.interruptMeta)
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

// Enqueue implements streammanager.StreamHandle.
func (st *stream) Enqueue(req speakRequest) error {
	st.mu.RLock()
	stopped := st.stopped
	st.mu.RUnlock()
	if stopped {
		return errStreamRebuffering
	}

	return st.worker.Enqueue(req, context.Canceled)
}

// Stop implements streammanager.StreamHandle.
func (st *stream) Stop() {
	st.mu.Lock()
	st.stopped = true
	st.mu.Unlock()
	st.worker.Stop()
}

// Wait implements streammanager.StreamHandle.
func (st *stream) Wait(ctx context.Context) {
	st.worker.Wait(ctx)
}

func (st *stream) processRequest(req speakRequest) bool {
	if st.handleRequest(req) {
		// Close synthesizer quietly — don't publish final events
		// since the request is being rebuffered for retry.
		if st.synthesizer != nil {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), constants.Duration2Seconds)
			st.synthesizer.Close(closeCtx)
			closeCancel()
		}
		st.rebufferPending(req)
		return true
	}
	return false
}

// handleRequest returns true if the error is ErrAdapterUnavailable, signalling
// the caller to rebuffer the request and close the stream.
func (st *stream) handleRequest(req speakRequest) bool {
	ctx := st.worker.Context()
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
	metadata := maputil.Clone(st.metadata)
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
		Metadata:  maputil.Clone(st.metadata),
	}
	st.service.manager.BufferPending(st.key, params, failedReq)

	// Cancel context BEFORE draining so that racing enqueue calls
	// see ctx.Done in their first select and fail fast.  Any request
	// that still slips through the nondeterministic second select will
	// land in the channel buffer and be caught by the drain below.
	st.worker.Stop()

	// Drain any remaining queued requests into the pending buffer.
	st.worker.DrainNonBlocking(func(req speakRequest) {
		st.service.manager.BufferPending(st.key, params, req)
	})
}

func (st *stream) closeSynthesizer(reason string) {
	if st.synthesizer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), constants.Duration2Seconds)
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
	eventbus.Publish(context.Background(), s.bus, eventbus.Audio.EgressPlayback, eventbus.SourceAudioEgress, evt)
}

func mergeMetadata(base map[string]string, chunk map[string]string, req map[string]string) map[string]string {
	return mapper.MergeStringMaps(base, req, chunk)
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
