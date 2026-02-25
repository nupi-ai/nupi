package eventbus

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Bus orchestrates topic-based publish/subscribe messaging.
type Bus struct {
	logger        *log.Logger
	mu            sync.RWMutex
	subscribers   map[Topic]map[uint64]*Subscription
	topicBuffers  map[Topic]int
	topicPolicies map[Topic]DeliveryPolicy
	nextID        uint64
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
		TopicConversationTurn:        256,
		TopicPairingCreated:          64,
		TopicPairingClaimed:          64,
		TopicAwarenessSync:           64,
	}

	bus := &Bus{
		logger:        log.Default(),
		subscribers:   make(map[Topic]map[uint64]*Subscription),
		topicBuffers:  defaults,
		topicPolicies: make(map[Topic]DeliveryPolicy),
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

// WithTopicPolicy overrides the delivery policy for a specific topic.
func WithTopicPolicy(topic Topic, policy DeliveryPolicy) BusOption {
	return func(b *Bus) {
		if b.topicPolicies == nil {
			b.topicPolicies = make(map[Topic]DeliveryPolicy)
		}
		b.topicPolicies[topic] = policy
	}
}

// publish sends the envelope to all subscribers of the topic.
func (b *Bus) publish(ctx context.Context, env Envelope) {
	if env.Topic == "" {
		return
	}
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now().UTC()
	}
	if env.Source == "" {
		env.Source = SourceUnknown
	}

	b.mu.RLock()
	subs := b.subscribers[env.Topic]
	for _, sub := range subs {
		sub.deliver(ctx, env, b.logger)
	}
	b.mu.RUnlock()
}

// Subscribe registers a subscriber for the given topic.
// If b is nil the returned Subscription has a closed channel and Close is a no-op.
func (b *Bus) Subscribe(topic Topic, opts ...SubscriptionOption) *Subscription {
	if b == nil {
		ch := make(chan Envelope)
		close(ch)
		done := make(chan struct{})
		close(done)
		sub := &Subscription{ch: ch, done: done}
		sub.closed.Store(true)
		return sub
	}
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
		done:   make(chan struct{}),
		bus:    b,
		policy: policy,
	}

	if policy.Strategy == StrategyOverflow {
		ovfCap := policy.MaxOverflow
		if ovfCap <= 0 {
			ovfCap = defaultMaxOverflow
		}
		sub.ovf = newOverflowBuffer(ovfCap)
		ovfCtx, cancel := context.WithCancel(context.Background())
		sub.ovfCancel = cancel
		go sub.ovf.drainLoop(ovfCtx, sub.ch)
	}

	b.mu.Lock()
	if _, exists := b.subscribers[topic]; !exists {
		b.subscribers[topic] = make(map[uint64]*Subscription)
	}
	b.subscribers[topic][id] = sub
	b.mu.Unlock()

	if cfg.ctx != nil {
		go func() {
			select {
			case <-cfg.ctx.Done():
				sub.Close()
			case <-sub.done:
			}
		}()
	}

	return sub
}

// Shutdown closes all subscriptions and empties routing tables.
// If b is nil the call is a no-op.
func (b *Bus) Shutdown() {
	if b == nil {
		return
	}
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
	ctx        context.Context
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

// WithContext ties the subscription lifecycle to a context.
// When the context is cancelled the subscription is automatically closed.
// A nil context is ignored.
func WithContext(ctx context.Context) SubscriptionOption {
	return func(cfg *subscriptionConfig) {
		if ctx != nil {
			cfg.ctx = ctx
		}
	}
}

// Subscription represents a consumer listening to a topic.
type Subscription struct {
	topic Topic
	id    uint64
	name  string
	ch    chan Envelope
	done  chan struct{} // closed when the subscription is closed

	bus       *Bus
	closed    atomic.Bool
	dropped   atomic.Uint64
	policy    DeliveryPolicy
	ovf       *overflowBuffer
	ovfCancel context.CancelFunc
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
	close(s.done)

	if s.bus == nil {
		close(s.ch)
		return
	}

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
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	s.stopOverflow()
	close(s.done)
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
}
