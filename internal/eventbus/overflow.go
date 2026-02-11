package eventbus

import (
	"context"
	"sync"
)

// overflowBuffer is a mutex-protected circular buffer of Envelopes.
// It acts as a shock absorber for bursts on critical topics.
type overflowBuffer struct {
	mu     sync.Mutex
	buf    []Envelope
	head   int // index of oldest item
	count  int
	cap    int
	notify chan struct{} // signalled on push so drainLoop wakes up
	done   chan struct{} // closed when drainLoop exits
}

// newOverflowBuffer creates a ring buffer with the given capacity.
func newOverflowBuffer(maxSize int) *overflowBuffer {
	if maxSize <= 0 {
		maxSize = defaultMaxOverflow
	}
	return &overflowBuffer{
		buf:    make([]Envelope, maxSize),
		cap:    maxSize,
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

// push appends an envelope to the ring. Returns false if the ring is full.
func (o *overflowBuffer) push(env Envelope) bool {
	o.mu.Lock()
	if o.count >= o.cap {
		o.mu.Unlock()
		return false
	}
	idx := (o.head + o.count) % o.cap
	o.buf[idx] = env
	o.count++
	o.mu.Unlock()

	// Non-blocking signal to wake drainLoop.
	select {
	case o.notify <- struct{}{}:
	default:
	}
	return true
}

// pop removes and returns the oldest envelope. Returns false if empty.
func (o *overflowBuffer) pop() (Envelope, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.count == 0 {
		return Envelope{}, false
	}
	env := o.buf[o.head]
	o.buf[o.head] = Envelope{} // allow GC of payload
	o.head = (o.head + 1) % o.cap
	o.count--
	return env, true
}

// len returns the number of items currently buffered.
func (o *overflowBuffer) len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.count
}

// drainLoop moves envelopes from the ring buffer into ch until ctx is cancelled.
// It blocks on the notify channel between drain sweeps to avoid busy-spinning.
func (o *overflowBuffer) drainLoop(ctx context.Context, ch chan<- Envelope) {
	defer close(o.done)
	for {
		// Drain everything currently in the buffer.
		for {
			env, ok := o.pop()
			if !ok {
				break
			}
			select {
			case ch <- env:
			case <-ctx.Done():
				return
			}
		}

		// Wait for a signal or cancellation.
		select {
		case <-ctx.Done():
			return
		case <-o.notify:
		}
	}
}
