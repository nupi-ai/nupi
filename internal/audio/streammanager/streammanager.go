// Package streammanager provides a generic, race-safe manager for audio
// streams with pending buffering, exponential-backoff retry and lifecycle
// callbacks.  STT, VAD and TTS/Egress services delegate to [Manager] instead
// of duplicating the same ~350 lines each.
package streammanager

import (
	"context"
	"log"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Interfaces – each audio service provides its own implementation.
// ---------------------------------------------------------------------------

// StreamHandle is an opaque handle to an active audio stream.
type StreamHandle[T any] interface {
	// Enqueue adds an item to the stream's processing queue.
	Enqueue(item T) error
	// Stop cancels the stream's context.
	Stop()
	// Wait blocks until the stream's goroutine exits.
	Wait(ctx context.Context)
}

// StreamFactory creates new stream handles.
type StreamFactory[T any] interface {
	CreateStream(ctx context.Context, key string, params SessionParams) (StreamHandle[T], error)
}

// StreamFactoryFunc adapts a plain function to StreamFactory.
type StreamFactoryFunc[T any] func(ctx context.Context, key string, params SessionParams) (StreamHandle[T], error)

func (f StreamFactoryFunc[T]) CreateStream(ctx context.Context, key string, params SessionParams) (StreamHandle[T], error) {
	return f(ctx, key, params)
}

// ---------------------------------------------------------------------------
// Callbacks – services register only the hooks they need.
// ---------------------------------------------------------------------------

// Callbacks allows per-service customisation of manager behaviour.
// All fields are optional – nil callbacks are never invoked.
type Callbacks[T any] struct {
	// OnEnqueueError is called when enqueueing an item into an existing
	// stream fails.  Return true if the error was handled (e.g. rebuffered).
	// TTS uses this for rebuffering on errStreamRebuffering.
	OnEnqueueError func(key string, item T, err error) (handled bool)

	// ClassifyCreateError inspects a factory error and returns whether it
	// should be treated as adapter-unavailable or factory-unavailable.
	// This lets services use their own sentinel errors.
	ClassifyCreateError func(err error) (adapterUnavailable, factoryUnavailable bool)
}

// ---------------------------------------------------------------------------
// PendingQueue – per-key buffer for items that arrived before an adapter.
// ---------------------------------------------------------------------------

// PendingQueue holds items waiting for an adapter to become available.
type PendingQueue[T any] struct {
	Params         SessionParams
	Items          []T
	FailureCount   int
	FirstFailureAt time.Time

	timer *time.Timer
	delay time.Duration
}

func (pq *PendingQueue[T]) scheduleRetry(m *Manager[T], key string) {
	if pq.timer != nil {
		return
	}

	delay := pq.delay
	if delay <= 0 {
		delay = m.retryCfg.Initial
	} else {
		delay *= 2
		if delay > m.retryCfg.Max {
			delay = m.retryCfg.Max
		}
	}
	pq.delay = delay
	pq.timer = time.AfterFunc(delay, func() {
		m.retryPending(key)
	})
	if m.logger != nil {
		sid, strid := SplitStreamKey(key)
		m.logger.Printf("[%s] scheduling adapter retry session=%s stream=%s in %s", m.tag, sid, strid, delay)
	}
}

func (pq *PendingQueue[T]) stopTimer() {
	if pq.timer != nil {
		pq.timer.Stop()
		pq.timer = nil
	}
	pq.delay = 0
}

// ---------------------------------------------------------------------------
// Manager – the generic stream manager.
// ---------------------------------------------------------------------------

// Config configures a Manager instance.
type Config[T any] struct {
	// Tag is used as a prefix in log messages (e.g. "STT", "VAD", "TTS").
	Tag string

	// MaxPending is the maximum number of items buffered per key.
	MaxPending int

	// Retry holds backoff parameters.
	Retry RetryConfig

	// Factory creates stream handles.
	Factory StreamFactory[T]

	// Callbacks hooks for service-specific behaviour.
	Callbacks Callbacks[T]

	// Logger for diagnostics. If nil, log.Default() is used.
	Logger *log.Logger

	// Ctx is the parent context for the manager. When cancelled, pending
	// retries will stop.
	Ctx context.Context
}

// Manager is a generic, concurrency-safe manager for audio streams.
type Manager[T any] struct {
	tag        string
	maxPending int
	retryCfg   RetryConfig
	factory    StreamFactory[T]
	callbacks  Callbacks[T]
	logger     *log.Logger
	ctx        context.Context

	mu      sync.Mutex
	streams map[string]StreamHandle[T]

	pendingMu sync.Mutex
	pending   map[string]*PendingQueue[T]
}

// New constructs a Manager from the provided Config.
func New[T any](cfg Config[T]) *Manager[T] {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	maxPending := cfg.MaxPending
	if maxPending <= 0 {
		maxPending = 100
	}
	ValidateRetryConfig(&cfg.Retry)
	ctx := cfg.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return &Manager[T]{
		tag:        cfg.Tag,
		maxPending: maxPending,
		retryCfg:   cfg.Retry,
		factory:    cfg.Factory,
		callbacks:  cfg.Callbacks,
		logger:     logger,
		ctx:        ctx,
		streams:    make(map[string]StreamHandle[T]),
		pending:    make(map[string]*PendingQueue[T]),
	}
}

// ---------------------------------------------------------------------------
// Stream operations
// ---------------------------------------------------------------------------

// Stream returns an existing stream handle, if any.
func (m *Manager[T]) Stream(key string) (StreamHandle[T], bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.streams[key]
	return st, ok
}

// CreateStream creates a new stream via the factory.  It is race-safe: if
// another goroutine has already registered a stream for key, the duplicate is
// stopped and the existing handle is returned.
//
// After successful registration the manager flushes any pending items.
func (m *Manager[T]) CreateStream(key string, params SessionParams) (StreamHandle[T], error) {
	if m.factory == nil {
		return nil, ErrFactoryNil
	}

	// Fast path: already exists.
	if st, ok := m.Stream(key); ok {
		return st, nil
	}

	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	handle, err := m.factory.CreateStream(ctx, key, params)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if existing, ok := m.streams[key]; ok {
		m.mu.Unlock()
		handle.Stop()
		return existing, nil
	}
	m.streams[key] = handle
	m.mu.Unlock()

	if m.logger != nil {
		sid, strid := SplitStreamKey(key)
		m.logger.Printf("[%s] stream opened session=%s stream=%s", m.tag, sid, strid)
	}

	m.FlushPending(key, handle)
	return handle, nil
}

// RemoveStream removes a stream from the map if it matches the provided handle.
func (m *Manager[T]) RemoveStream(key string, handle StreamHandle[T]) {
	m.mu.Lock()
	existing, ok := m.streams[key]
	if ok && isSameHandle(existing, handle) {
		delete(m.streams, key)
	} else {
		ok = false
	}
	m.mu.Unlock()
}

// RemoveStreamByKey removes whatever stream is registered for key and stops it.
func (m *Manager[T]) RemoveStreamByKey(key string) {
	var handle StreamHandle[T]
	m.mu.Lock()
	if existing, ok := m.streams[key]; ok {
		handle = existing
		delete(m.streams, key)
	}
	m.mu.Unlock()

	if handle != nil {
		handle.Stop()
	}
}

// CloseAllStreams stops and removes every registered stream. Returns the
// list so callers can Wait on them.
func (m *Manager[T]) CloseAllStreams() []StreamHandle[T] {
	type entry struct {
		key    string
		handle StreamHandle[T]
	}
	m.mu.Lock()
	entries := make([]entry, 0, len(m.streams))
	for key, h := range m.streams {
		entries = append(entries, entry{key, h})
		delete(m.streams, key)
	}
	m.mu.Unlock()

	handles := make([]StreamHandle[T], 0, len(entries))
	for _, e := range entries {
		handles = append(handles, e.handle)
		e.handle.Stop()
	}
	return handles
}

// ---------------------------------------------------------------------------
// Pending / retry operations
// ---------------------------------------------------------------------------

// BufferPending stores an item in the pending queue for key, creating the
// queue if necessary. It schedules a retry via the backoff timer.
func (m *Manager[T]) BufferPending(key string, params SessionParams, item T) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()

	pq, ok := m.pending[key]
	if !ok {
		pq = &PendingQueue[T]{
			Params: params,
		}
		m.pending[key] = pq
	}
	if len(pq.Items) >= m.maxPending {
		pq.Items = pq.Items[1:]
		if m.logger != nil {
			sid, strid := SplitStreamKey(key)
			m.logger.Printf("[%s] pending buffer full session=%s stream=%s, dropping oldest item", m.tag, sid, strid)
		}
	}
	pq.Items = append(pq.Items, item)
	if m.logger != nil {
		sid, strid := SplitStreamKey(key)
		m.logger.Printf("[%s] buffered item session=%s stream=%s (pending=%d)", m.tag, sid, strid, len(pq.Items))
	}
	pq.scheduleRetry(m, key)
}

// FlushPending replays buffered items into the given stream handle.
func (m *Manager[T]) FlushPending(key string, handle StreamHandle[T]) {
	m.pendingMu.Lock()
	pq, ok := m.pending[key]
	if ok {
		pq.stopTimer()
		items := append([]T(nil), pq.Items...)
		delete(m.pending, key)
		m.pendingMu.Unlock()
		if len(items) > 0 && m.logger != nil {
			sid, strid := SplitStreamKey(key)
			m.logger.Printf("[%s] replaying %d buffered items session=%s stream=%s", m.tag, len(items), sid, strid)
		}
		for _, item := range items {
			if err := handle.Enqueue(item); err != nil {
				if cb := m.callbacks.OnEnqueueError; cb != nil {
					cb(key, item, err)
				} else if m.logger != nil {
					sid, strid := SplitStreamKey(key)
					m.logger.Printf("[%s] enqueue pending item session=%s stream=%s failed: %v", m.tag, sid, strid, err)
				}
			}
		}
		return
	}
	m.pendingMu.Unlock()
}

func (m *Manager[T]) retryPending(key string) {
	m.pendingMu.Lock()
	pq, ok := m.pending[key]
	if !ok {
		m.pendingMu.Unlock()
		return
	}
	pq.timer = nil
	params := pq.Params
	m.pendingMu.Unlock()

	handle, err := m.CreateStream(key, params)
	if err != nil {
		adapterUnavail, _ := m.classifyError(err)
		if adapterUnavail {
			m.pendingMu.Lock()
			if pq, ok := m.pending[key]; ok {
				pq.scheduleRetry(m, key)
			}
			m.pendingMu.Unlock()
			return
		}

		// Non-recoverable error — update failure tracking.
		m.pendingMu.Lock()
		pq, ok = m.pending[key]
		if ok {
			pq.FailureCount++
			if pq.FirstFailureAt.IsZero() {
				pq.FirstFailureAt = time.Now()
			}

			abandon := false
			if m.retryCfg.MaxFailures <= 0 {
				abandon = true // default: abandon on first permanent error
			} else if pq.FailureCount >= m.retryCfg.MaxFailures {
				abandon = true
			}
			if !abandon && m.retryCfg.MaxDuration > 0 && time.Since(pq.FirstFailureAt) >= m.retryCfg.MaxDuration {
				abandon = true
			}

			if abandon {
				if m.logger != nil {
					sid, strid := SplitStreamKey(key)
					m.logger.Printf("[%s] retry stream session=%s stream=%s permanent error, dropping pending: %v", m.tag, sid, strid, err)
				}
				pq.stopTimer()
				delete(m.pending, key)
			} else {
				pq.scheduleRetry(m, key)
			}
		}
		m.pendingMu.Unlock()
		return
	}

	// CreateStream already called FlushPending.
	_ = handle
}

// ClearPending removes the pending queue for a key, cancelling timers.
func (m *Manager[T]) ClearPending(key string) {
	m.pendingMu.Lock()
	if pq, ok := m.pending[key]; ok {
		pq.stopTimer()
		delete(m.pending, key)
	}
	m.pendingMu.Unlock()
}

// ShutdownPending stops all pending timers and clears queues.
func (m *Manager[T]) ShutdownPending() {
	m.pendingMu.Lock()
	for key, pq := range m.pending {
		if pq != nil {
			pq.stopTimer()
		}
		delete(m.pending, key)
	}
	m.pendingMu.Unlock()
}

// GetPendingQueue returns the pending queue for a key, if any.
// Primarily for testing.
func (m *Manager[T]) GetPendingQueue(key string) (*PendingQueue[T], bool) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	pq, ok := m.pending[key]
	return pq, ok
}

// PendingCount returns the number of pending queues.
func (m *Manager[T]) PendingCount() int {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	return len(m.pending)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (m *Manager[T]) classifyError(err error) (adapterUnavailable, factoryUnavailable bool) {
	if cb := m.callbacks.ClassifyCreateError; cb != nil {
		return cb(err)
	}
	return false, false
}

// isSameHandle compares two interface values by identity.
func isSameHandle[T any](a, b StreamHandle[T]) bool {
	return any(a) == any(b)
}

// Sentinel errors.
var ErrFactoryNil = errFactoryNil{}

type errFactoryNil struct{}

func (errFactoryNil) Error() string { return "streammanager: factory is nil" }
