package tooldetectors

import (
	"sync"
)

const RingBufferSize = 50 * 1024 // 50KB sliding window

// RingBuffer holds the last RingBufferSize bytes of data.
// When new data is added, old data is discarded to maintain size limit.
type RingBuffer struct {
	data []byte
	mu   sync.RWMutex
}

// NewRingBuffer creates a new ring buffer with 50KB capacity.
func NewRingBuffer() *RingBuffer {
	return &RingBuffer{
		data: make([]byte, 0, RingBufferSize),
	}
}

// Append adds data to buffer, maintaining sliding window.
func (b *RingBuffer) Append(chunk []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.data = append(b.data, chunk...)

	if len(b.data) > RingBufferSize {
		b.data = b.data[len(b.data)-RingBufferSize:]
	}
}

// String returns the buffer content as string.
func (b *RingBuffer) String() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return string(b.data)
}

// Bytes returns the buffer content as byte slice.
func (b *RingBuffer) Bytes() []byte {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make([]byte, len(b.data))
	copy(result, b.data)
	return result
}

// Len returns current buffer size.
func (b *RingBuffer) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.data)
}

// IsFull returns true if buffer reached max size.
func (b *RingBuffer) IsFull() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.data) >= RingBufferSize
}

// Clear empties the buffer.
func (b *RingBuffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = b.data[:0]
}
