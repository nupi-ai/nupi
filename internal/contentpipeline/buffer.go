package contentpipeline

import (
	"sync"
	"time"
)

const (
	// DefaultBufferSize is the maximum buffer size per session (64KB).
	DefaultBufferSize = 64 * 1024
	// DefaultIdleTimeout is the duration after which a buffer is considered idle.
	DefaultIdleTimeout = 500 * time.Millisecond
)

// OutputBuffer accumulates streaming chunks until idle is detected.
// It is safe for concurrent use.
type OutputBuffer struct {
	mu          sync.Mutex
	data        []byte
	lastWrite   time.Time
	idleTimeout time.Duration
	maxSize     int
	overflowed  bool   // true if data was truncated due to overflow
	generation  uint64 // incremented on each write, used for timer race detection
}

// NewOutputBuffer creates a new OutputBuffer with default settings.
func NewOutputBuffer() *OutputBuffer {
	return &OutputBuffer{
		data:        make([]byte, 0, 4096),
		idleTimeout: DefaultIdleTimeout,
		maxSize:     DefaultBufferSize,
	}
}

// OutputBufferOption configures an OutputBuffer.
type OutputBufferOption func(*OutputBuffer)

// WithIdleTimeout sets the idle timeout for the buffer.
func WithIdleTimeout(d time.Duration) OutputBufferOption {
	return func(b *OutputBuffer) {
		if d > 0 {
			b.idleTimeout = d
		}
	}
}

// WithMaxSize sets the maximum buffer size.
func WithMaxSize(size int) OutputBufferOption {
	return func(b *OutputBuffer) {
		if size > 0 {
			b.maxSize = size
		}
	}
}

// NewOutputBufferWithOptions creates a new OutputBuffer with custom options.
func NewOutputBufferWithOptions(opts ...OutputBufferOption) *OutputBuffer {
	b := NewOutputBuffer()
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Write appends chunk to buffer. Returns true if buffer overflowed and oldest data was dropped.
func (b *OutputBuffer) Write(chunk []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.data = append(b.data, chunk...)
	b.lastWrite = time.Now()
	b.generation++ // increment for timer race detection

	// Enforce max size - drop oldest data
	if len(b.data) > b.maxSize {
		b.data = b.data[len(b.data)-b.maxSize:]
		b.overflowed = true // remember that truncation occurred
		return true         // signal overflow
	}
	return false
}

// WasOverflowed returns true if the buffer was truncated due to overflow.
// The flag is reset when the buffer is flushed or reset.
func (b *OutputBuffer) WasOverflowed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.overflowed
}

// Generation returns the current generation counter.
// This counter increments on each Write and can be used to detect
// whether new data arrived between timer scheduling and firing.
func (b *OutputBuffer) Generation() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.generation
}

// TimeSinceLastWrite returns the duration since the last chunk was written.
func (b *OutputBuffer) TimeSinceLastWrite() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastWrite.IsZero() {
		return 0
	}
	return time.Since(b.lastWrite)
}

// IsIdleByTimeout returns true if no output has been received for idleTimeout duration.
func (b *OutputBuffer) IsIdleByTimeout() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastWrite.IsZero() {
		return false
	}
	return time.Since(b.lastWrite) > b.idleTimeout
}

// Flush returns accumulated data, overflow status, and clears the buffer.
// The overflow flag indicates if data was truncated during accumulation.
func (b *OutputBuffer) Flush() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	result := string(b.data)
	wasOverflowed := b.overflowed
	b.data = b.data[:0]
	b.overflowed = false
	return result, wasOverflowed
}

// Peek returns current buffer content without clearing.
func (b *OutputBuffer) Peek() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// Len returns current buffer length in bytes.
func (b *OutputBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.data)
}

// MaxSize returns the configured maximum buffer size.
func (b *OutputBuffer) MaxSize() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxSize
}

// IdleTimeout returns the configured idle timeout duration.
func (b *OutputBuffer) IdleTimeout() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.idleTimeout
}

// IsEmpty returns true if the buffer contains no data.
func (b *OutputBuffer) IsEmpty() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.data) == 0
}

// Reset clears the buffer, overflow flag, generation counter, and resets lastWrite time.
// Resetting generation invalidates any pending idle timers.
func (b *OutputBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = b.data[:0]
	b.lastWrite = time.Time{}
	b.overflowed = false
	b.generation = 0
}
