package eventbus

import (
	"context"
	"log"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Bus orchestrates topic-based publish/subscribe messaging.
const defaultLatencySampleSize = 512

type Bus struct {
	logger        *log.Logger
	mu            sync.RWMutex
	subscribers   map[Topic]map[uint64]*Subscription
	topicBuffers  map[Topic]int
	topicPolicies map[Topic]DeliveryPolicy
	nextID        uint64
	observerMu    sync.RWMutex
	observers     []Observer

	publishTotal  atomic.Uint64
	droppedTotal  atomic.Uint64
	overflowTotal atomic.Uint64

	latencyMu      sync.Mutex
	latencySamples []time.Duration
	latencyIndex   int
	latencyCount   int
}

// Observer receives notifications for every published envelope.
type Observer interface {
	OnPublish(Envelope)
}

// ObserverFunc adapts a function to the Observer interface.
type ObserverFunc func(Envelope)

// OnPublish invokes the underlying function.
func (f ObserverFunc) OnPublish(env Envelope) {
	f(env)
}

// New constructs a bus with default topic buffer sizes.
func New(opts ...BusOption) *Bus {
	defaults := map[Topic]int{
		TopicSessionsOutput:          1024,
		TopicSessionsLifecycle:       256,
		TopicSessionsTool:            128,
		TopicSessionsToolChanged:     64,
		TopicPipelineCleaned:         512,
		TopicPipelineError:           64,
		TopicConversationPrompt:      256,
		TopicConversationReply:       256,
		TopicAdaptersStatus:          128,
		TopicAdaptersLog:             256,
		TopicAudioIngressRaw:         256,
		TopicAudioIngressSegment:     10000,
		TopicAudioEgressPlayback:     256,
		TopicAudioInterrupt:          64,
		TopicSpeechTranscriptPartial: 256,
		TopicSpeechTranscriptFinal:   256,
		TopicSpeechVADDetected:       128,
		TopicSpeechBargeIn:           64,
		TopicConversationSpeak:       128,
		TopicIntentRouterDiagnostics: 64,
	}

	bus := &Bus{
		logger:         log.Default(),
		subscribers:    make(map[Topic]map[uint64]*Subscription),
		topicBuffers:   defaults,
		topicPolicies:  make(map[Topic]DeliveryPolicy),
		latencySamples: make([]time.Duration, defaultLatencySampleSize),
	}

	for _, opt := range opts {
		opt(bus)
	}

	return bus
}

// BusOption customises bus behaviour.
type BusOption func(*Bus)

// WithLogger overrides the logger used for drop warnings.
func WithLogger(logger *log.Logger) BusOption {
	return func(b *Bus) {
		if logger != nil {
			b.logger = logger
		}
	}
}

// WithTopicBuffer sets the buffer size for a given topic.
func WithTopicBuffer(topic Topic, size int) BusOption {
	return func(b *Bus) {
		if size <= 0 {
			size = 1
		}
		if b.topicBuffers == nil {
			b.topicBuffers = make(map[Topic]int)
		}
		b.topicBuffers[topic] = size
	}
}

// WithObserver registers an observer invoked for every published event.
func WithObserver(observer Observer) BusOption {
	return func(b *Bus) {
		if observer != nil {
			b.observers = append(b.observers, observer)
		}
	}
}

// WithTopicPolicy overrides the delivery policy for a specific topic.
func WithTopicPolicy(topic Topic, policy DeliveryPolicy) BusOption {
	return func(b *Bus) {
		if b.topicPolicies == nil {
			b.topicPolicies = make(map[Topic]DeliveryPolicy)
		}
		b.topicPolicies[topic] = policy
	}
}

// Publish sends the envelope to all subscribers of the topic.
func (b *Bus) Publish(ctx context.Context, env Envelope) {
	if env.Topic == "" {
		return
	}
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now().UTC()
	}
	if env.Source == "" {
		env.Source = SourceUnknown
	}

	start := time.Now()
	b.publishTotal.Add(1)

	b.mu.RLock()
	subs := b.subscribers[env.Topic]
	for _, sub := range subs {
		sub.deliver(ctx, env, b.logger)
	}
	b.mu.RUnlock()

	b.notifyObservers(env)
	b.storeLatency(time.Since(start))
}

// Subscribe registers a subscriber for the given topic.
func (b *Bus) Subscribe(topic Topic, opts ...SubscriptionOption) *Subscription {
	cfg := subscriptionConfig{
		bufferSize: b.topicBuffers[topic],
		name:       "",
	}
	if cfg.bufferSize <= 0 {
		cfg.bufferSize = 1
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	policy := policyFor(topic, b.topicPolicies)

	id := atomic.AddUint64(&b.nextID, 1)
	sub := &Subscription{
		topic:  topic,
		id:     id,
		name:   cfg.name,
		ch:     make(chan Envelope, cfg.bufferSize),
		bus:    b,
		policy: policy,
	}

	if policy.Strategy == StrategyOverflow {
		ovfCap := policy.MaxOverflow
		if ovfCap <= 0 {
			ovfCap = defaultMaxOverflow
		}
		sub.ovf = newOverflowBuffer(ovfCap)
		ctx, cancel := context.WithCancel(context.Background())
		sub.ovfCancel = cancel
		go sub.ovf.drainLoop(ctx, sub.ch)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, exists := b.subscribers[topic]; !exists {
		b.subscribers[topic] = make(map[uint64]*Subscription)
	}
	b.subscribers[topic][id] = sub
	return sub
}

// Shutdown closes all subscriptions and empties routing tables.
func (b *Bus) Shutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for topic, subs := range b.subscribers {
		for id, sub := range subs {
			sub.closeLocked()
			delete(subs, id)
		}
		delete(b.subscribers, topic)
	}
}

// SubscriptionOption customises individual subscriptions.
type SubscriptionOption func(*subscriptionConfig)

type subscriptionConfig struct {
	bufferSize int
	name       string
}

// WithSubscriptionBuffer overrides the channel buffer for a subscription.
func WithSubscriptionBuffer(size int) SubscriptionOption {
	return func(cfg *subscriptionConfig) {
		if size > 0 {
			cfg.bufferSize = size
		}
	}
}

// WithSubscriptionName records a human friendly identifier used in logs.
func WithSubscriptionName(name string) SubscriptionOption {
	return func(cfg *subscriptionConfig) {
		cfg.name = name
	}
}

// Subscription represents a consumer listening to a topic.
type Subscription struct {
	topic Topic
	id    uint64
	name  string
	ch    chan Envelope

	bus      *Bus
	closed   atomic.Bool
	dropped  atomic.Uint64
	policy   DeliveryPolicy
	ovf      *overflowBuffer
	ovfCancel context.CancelFunc
	overflow atomic.Uint64
}

// C exposes the event channel.
func (s *Subscription) C() <-chan Envelope {
	return s.ch
}

// Close removes the subscription and closes the channel.
func (s *Subscription) Close() {
	if s.closed.Load() {
		return
	}
	if !s.closed.CompareAndSwap(false, true) {
		return
	}

	s.stopOverflow()

	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()

	if subs, ok := s.bus.subscribers[s.topic]; ok {
		if _, exists := subs[s.id]; exists {
			delete(subs, s.id)
		}
	}
	close(s.ch)
}

func (s *Subscription) closeLocked() {
	if s.closed.Load() {
		return
	}
	s.closed.Store(true)
	s.stopOverflow()
	close(s.ch)
}

// stopOverflow cancels the drain goroutine and waits for it to exit.
func (s *Subscription) stopOverflow() {
	if s.ovfCancel != nil {
		s.ovfCancel()
	}
	if s.ovf != nil {
		<-s.ovf.done
	}
}

func (s *Subscription) deliver(ctx context.Context, env Envelope, logger *log.Logger) {
	if s.closed.Load() {
		return
	}

	select {
	case <-ctx.Done():
		return
	default:
	}

	// Overflow strategy: always route through the overflow buffer to preserve FIFO ordering.
	// Direct channel sends would race with the drain goroutine and break ordering.
	if s.policy.Strategy == StrategyOverflow && s.ovf != nil {
		if s.ovf.push(env) {
			s.overflow.Add(1)
			s.bus.overflowTotal.Add(1)
			return
		}
		// Overflow full — fall back to drop-oldest on the channel.
		s.dropOldestAndEnqueue(env, logger)
		return
	}

	// Fast path: non-blocking send.
	select {
	case s.ch <- env:
		return
	default:
	}

	// Channel full — apply policy.
	switch s.policy.Strategy {
	case StrategyDropNewest:
		s.recordDrop(logger, "drop-newest")
	default: // StrategyDropOldest
		s.dropOldestAndEnqueue(env, logger)
	}
}

func (s *Subscription) dropOldestAndEnqueue(env Envelope, logger *log.Logger) {
	select {
	case <-s.ch:
		s.recordDrop(logger, "drop-oldest")
	default:
	}

	select {
	case s.ch <- env:
	default:
		s.recordDrop(logger, "drop-current")
	}
}

func (s *Subscription) recordDrop(logger *log.Logger, reason string) {
	count := s.dropped.Add(1)
	if logger != nil {
		name := s.name
		if name == "" {
			name = "subscription"
		}
		logger.Printf("[eventbus] dropped event #%d for %s on topic %s (%s)", count, name, s.topic, reason)
	}
	if s.bus != nil {
		s.bus.recordDrop()
	}
}

// Metrics exposes aggregated bus metrics.
type Metrics struct {
	PublishTotal  uint64
	DroppedTotal  uint64
	OverflowTotal uint64
	LatencyP50    time.Duration
	LatencyP99    time.Duration
}

// Metrics returns the current metrics snapshot.
func (b *Bus) Metrics() Metrics {
	metrics := Metrics{
		PublishTotal:  b.publishTotal.Load(),
		DroppedTotal:  b.droppedTotal.Load(),
		OverflowTotal: b.overflowTotal.Load(),
	}

	samples := b.collectLatencySamples()
	if len(samples) == 0 {
		return metrics
	}

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	metrics.LatencyP50 = percentile(samples, 0.50)
	metrics.LatencyP99 = percentile(samples, 0.99)
	return metrics
}

func (b *Bus) collectLatencySamples() []time.Duration {
	b.latencyMu.Lock()
	defer b.latencyMu.Unlock()

	count := b.latencyCount
	if count == 0 || len(b.latencySamples) == 0 {
		return nil
	}

	samples := make([]time.Duration, count)
	size := len(b.latencySamples)
	start := (b.latencyIndex - count + size) % size
	for i := 0; i < count; i++ {
		idx := (start + i) % size
		samples[i] = b.latencySamples[idx]
	}
	return samples
}

func (b *Bus) storeLatency(d time.Duration) {
	if d < 0 {
		d = 0
	}

	b.latencyMu.Lock()
	defer b.latencyMu.Unlock()

	if len(b.latencySamples) == 0 {
		b.latencySamples = make([]time.Duration, defaultLatencySampleSize)
	}

	b.latencySamples[b.latencyIndex] = d
	if b.latencyCount < len(b.latencySamples) {
		b.latencyCount++
	}
	b.latencyIndex = (b.latencyIndex + 1) % len(b.latencySamples)
}

func (b *Bus) recordDrop() {
	b.droppedTotal.Add(1)
}

// AddObserver registers additional observers at runtime.
func (b *Bus) AddObserver(observer Observer) {
	if observer == nil {
		return
	}
	b.observerMu.Lock()
	b.observers = append(b.observers, observer)
	b.observerMu.Unlock()
}

func (b *Bus) notifyObservers(env Envelope) {
	b.observerMu.RLock()
	observers := b.observers
	b.observerMu.RUnlock()

	for _, observer := range observers {
		if observer == nil {
			continue
		}
		observer.OnPublish(env)
	}
}

func percentile(data []time.Duration, quantile float64) time.Duration {
	if len(data) == 0 {
		return 0
	}

	if quantile <= 0 {
		return data[0]
	}
	if quantile >= 1 {
		return data[len(data)-1]
	}

	pos := int(math.Ceil(quantile*float64(len(data)))) - 1
	if pos < 0 {
		pos = 0
	}
	if pos >= len(data) {
		pos = len(data) - 1
	}
	return data[pos]
}

// StartMetricsReporter periodically logs bus metrics until the context is cancelled.
func (b *Bus) StartMetricsReporter(ctx context.Context, interval time.Duration, logger *log.Logger) {
	if interval <= 0 {
		return
	}
	if logger == nil {
		logger = b.logger
	}
	if logger == nil {
		logger = log.Default()
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				metrics := b.Metrics()
				logger.Printf("[eventbus] metrics publish_total=%d dropped_total=%d overflow_total=%d latency_p50=%s latency_p99=%s", metrics.PublishTotal, metrics.DroppedTotal, metrics.OverflowTotal, metrics.LatencyP50, metrics.LatencyP99)
			}
		}
	}()
}
