package tooldetectors

import (
	"strings"
	"testing"
)

func TestRingBuffer(t *testing.T) {
	t.Run("basic append", func(t *testing.T) {
		buf := NewRingBuffer()
		buf.Append([]byte("Hello "))
		buf.Append([]byte("World"))

		if got := buf.String(); got != "Hello World" {
			t.Errorf("expected 'Hello World', got %q", got)
		}
	})

	t.Run("respects size limit and slides window", func(t *testing.T) {
		buf := NewRingBuffer()

		largeData := strings.Repeat("A", RingBufferSize+1000)
		buf.Append([]byte(largeData))

		if buf.Len() != RingBufferSize {
			t.Errorf("expected buffer size %d, got %d", RingBufferSize, buf.Len())
		}

		if !buf.IsFull() {
			t.Error("expected buffer to be full")
		}
	})

	t.Run("ring buffer sliding window", func(t *testing.T) {
		buf := NewRingBuffer()

		chunk := strings.Repeat("B", 25*1024)
		buf.Append([]byte(chunk))
		buf.Append([]byte(chunk))

		if buf.Len() != RingBufferSize {
			t.Errorf("expected buffer size %d, got %d", RingBufferSize, buf.Len())
		}

		buf.Append([]byte("MARKER"))

		if buf.Len() != RingBufferSize {
			t.Errorf("expected buffer size %d after append, got %d", RingBufferSize, buf.Len())
		}

		if !strings.HasSuffix(buf.String(), "MARKER") {
			t.Errorf("expected buffer to end with MARKER, got suffix %q", buf.String()[len(buf.String())-6:])
		}

		buf.Clear()
		if buf.Len() != 0 {
			t.Errorf("expected buffer length 0 after clear, got %d", buf.Len())
		}
	})
}
