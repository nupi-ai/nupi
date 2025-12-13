package contentpipeline

import (
	"sync"
	"testing"
	"time"
)

func TestOutputBuffer_Write(t *testing.T) {
	buf := NewOutputBuffer()

	// First write - should not overflow
	overflow := buf.Write([]byte("hello"))
	if overflow {
		t.Error("first write should not overflow")
	}
	if buf.Peek() != "hello" {
		t.Errorf("expected 'hello', got %q", buf.Peek())
	}

	// Second write - should append
	overflow = buf.Write([]byte(" world"))
	if overflow {
		t.Error("second write should not overflow")
	}
	if buf.Peek() != "hello world" {
		t.Errorf("expected 'hello world', got %q", buf.Peek())
	}
}

func TestOutputBuffer_WriteOverflow(t *testing.T) {
	buf := NewOutputBufferWithOptions(WithMaxSize(10))

	// Write more than max size
	overflow := buf.Write([]byte("0123456789ABCDE"))
	if !overflow {
		t.Error("expected overflow when writing more than maxSize")
	}

	// Buffer should contain only the last maxSize bytes
	content := buf.Peek()
	if len(content) != 10 {
		t.Errorf("expected buffer length 10, got %d", len(content))
	}
	if content != "56789ABCDE" {
		t.Errorf("expected last 10 bytes, got %q", content)
	}

	// WasOverflowed should report true
	if !buf.WasOverflowed() {
		t.Error("WasOverflowed should return true after overflow")
	}
}

func TestOutputBuffer_WriteMultipleOverflows(t *testing.T) {
	buf := NewOutputBufferWithOptions(WithMaxSize(5))

	// First write - no overflow ("abc" = 3 bytes, maxSize = 5)
	buf.Write([]byte("abc"))

	// Second write - triggers overflow
	// "abc" (3) + "defgh" (5) = "abcdefgh" (8 bytes)
	// After truncation to last 5 bytes: "defgh"
	overflow := buf.Write([]byte("defgh"))
	if !overflow {
		t.Error("expected overflow")
	}

	content := buf.Peek()
	if content != "defgh" {
		t.Errorf("expected 'defgh', got %q", content)
	}
}

func TestOutputBuffer_Flush(t *testing.T) {
	buf := NewOutputBuffer()
	buf.Write([]byte("test data"))

	text, wasOverflowed := buf.Flush()
	if text != "test data" {
		t.Errorf("expected 'test data', got %q", text)
	}
	if wasOverflowed {
		t.Error("wasOverflowed should be false")
	}

	// Buffer should be empty after flush
	if !buf.IsEmpty() {
		t.Error("buffer should be empty after flush")
	}
	if buf.Len() != 0 {
		t.Errorf("expected len 0, got %d", buf.Len())
	}
}

func TestOutputBuffer_FlushWithOverflow(t *testing.T) {
	buf := NewOutputBufferWithOptions(WithMaxSize(5))
	buf.Write([]byte("0123456789"))

	text, wasOverflowed := buf.Flush()
	if text != "56789" {
		t.Errorf("expected '56789', got %q", text)
	}
	if !wasOverflowed {
		t.Error("wasOverflowed should be true")
	}

	// Overflow flag should be reset after flush
	if buf.WasOverflowed() {
		t.Error("WasOverflowed should be false after flush")
	}
}

func TestOutputBuffer_Reset(t *testing.T) {
	buf := NewOutputBufferWithOptions(WithMaxSize(5))
	buf.Write([]byte("0123456789")) // trigger overflow

	// Verify state before reset
	if buf.Generation() == 0 {
		t.Error("generation should be non-zero before reset")
	}

	buf.Reset()

	if !buf.IsEmpty() {
		t.Error("buffer should be empty after reset")
	}
	if buf.WasOverflowed() {
		t.Error("overflow flag should be cleared after reset")
	}
	// TimeSinceLastWrite returns 0 if lastWrite is zero
	if buf.TimeSinceLastWrite() != 0 {
		t.Errorf("lastWrite should be zero after reset, but TimeSinceLastWrite=%v", buf.TimeSinceLastWrite())
	}
	// Generation should be reset to 0 (invalidates pending timers)
	if buf.Generation() != 0 {
		t.Errorf("generation should be 0 after reset, got %d", buf.Generation())
	}
}

func TestOutputBuffer_TimeSinceLastWrite(t *testing.T) {
	buf := NewOutputBuffer()

	// Before any write, should return 0
	if buf.TimeSinceLastWrite() != 0 {
		t.Error("TimeSinceLastWrite should be 0 before any write")
	}

	buf.Write([]byte("test"))
	time.Sleep(10 * time.Millisecond)

	since := buf.TimeSinceLastWrite()
	if since < 10*time.Millisecond {
		t.Errorf("expected TimeSinceLastWrite >= 10ms, got %v", since)
	}
}

func TestOutputBuffer_IsIdleByTimeout(t *testing.T) {
	buf := NewOutputBufferWithOptions(WithIdleTimeout(50 * time.Millisecond))

	// Before any write
	if buf.IsIdleByTimeout() {
		t.Error("should not be idle before any write")
	}

	buf.Write([]byte("test"))

	// Immediately after write - not idle
	if buf.IsIdleByTimeout() {
		t.Error("should not be idle immediately after write")
	}

	// Wait for idle timeout
	time.Sleep(60 * time.Millisecond)

	if !buf.IsIdleByTimeout() {
		t.Error("should be idle after timeout")
	}
}

func TestOutputBuffer_Len(t *testing.T) {
	buf := NewOutputBuffer()

	if buf.Len() != 0 {
		t.Error("new buffer should have len 0")
	}

	buf.Write([]byte("hello"))
	if buf.Len() != 5 {
		t.Errorf("expected len 5, got %d", buf.Len())
	}

	buf.Write([]byte(" world"))
	if buf.Len() != 11 {
		t.Errorf("expected len 11, got %d", buf.Len())
	}
}

func TestOutputBuffer_IsEmpty(t *testing.T) {
	buf := NewOutputBuffer()

	if !buf.IsEmpty() {
		t.Error("new buffer should be empty")
	}

	buf.Write([]byte("x"))
	if buf.IsEmpty() {
		t.Error("buffer should not be empty after write")
	}

	buf.Flush()
	if !buf.IsEmpty() {
		t.Error("buffer should be empty after flush")
	}
}

func TestOutputBuffer_Options(t *testing.T) {
	// Test WithMaxSize
	buf1 := NewOutputBufferWithOptions(WithMaxSize(100))
	buf1.Write(make([]byte, 150))
	if buf1.Len() != 100 {
		t.Errorf("expected maxSize 100, got len %d", buf1.Len())
	}

	// Test WithIdleTimeout
	buf2 := NewOutputBufferWithOptions(WithIdleTimeout(10 * time.Millisecond))
	buf2.Write([]byte("test"))
	time.Sleep(15 * time.Millisecond)
	if !buf2.IsIdleByTimeout() {
		t.Error("custom idle timeout not respected")
	}

	// Test invalid options (should use defaults)
	buf3 := NewOutputBufferWithOptions(WithMaxSize(-1), WithIdleTimeout(-1))
	if buf3.maxSize != DefaultBufferSize {
		t.Error("invalid maxSize should keep default")
	}
	if buf3.idleTimeout != DefaultIdleTimeout {
		t.Error("invalid idleTimeout should keep default")
	}
}

func TestOutputBuffer_ConcurrentAccess(t *testing.T) {
	buf := NewOutputBuffer()
	var wg sync.WaitGroup

	// Spawn writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				buf.Write([]byte("x"))
			}
		}(i)
	}

	// Spawn readers
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				buf.Peek()
				buf.Len()
				buf.IsEmpty()
				buf.WasOverflowed()
				buf.TimeSinceLastWrite()
				buf.IsIdleByTimeout()
			}
		}()
	}

	wg.Wait()

	// Buffer should have 1000 "x" characters (or be truncated if overflow)
	if buf.Len() > DefaultBufferSize {
		t.Errorf("buffer exceeded max size: %d", buf.Len())
	}
}

func TestOutputBuffer_DefaultSettings(t *testing.T) {
	buf := NewOutputBuffer()

	// Verify default constants are used
	if buf.maxSize != DefaultBufferSize {
		t.Errorf("expected maxSize %d, got %d", DefaultBufferSize, buf.maxSize)
	}
	if buf.idleTimeout != DefaultIdleTimeout {
		t.Errorf("expected idleTimeout %v, got %v", DefaultIdleTimeout, buf.idleTimeout)
	}
}

func TestOutputBuffer_FlushEmpty(t *testing.T) {
	buf := NewOutputBuffer()

	text, wasOverflowed := buf.Flush()
	if text != "" {
		t.Errorf("expected empty string, got %q", text)
	}
	if wasOverflowed {
		t.Error("empty buffer should not report overflow")
	}
}

func TestOutputBuffer_PeekDoesNotClear(t *testing.T) {
	buf := NewOutputBuffer()
	buf.Write([]byte("test"))

	// Peek multiple times
	for i := 0; i < 3; i++ {
		if buf.Peek() != "test" {
			t.Error("Peek should not modify buffer content")
		}
	}

	// Buffer should still have content
	if buf.Len() != 4 {
		t.Error("Peek should not change buffer length")
	}
}

func TestOutputBuffer_Generation(t *testing.T) {
	buf := NewOutputBuffer()

	// Initial generation is 0
	if buf.Generation() != 0 {
		t.Errorf("expected initial generation 0, got %d", buf.Generation())
	}

	// Each write increments generation
	buf.Write([]byte("a"))
	if buf.Generation() != 1 {
		t.Errorf("expected generation 1, got %d", buf.Generation())
	}

	buf.Write([]byte("b"))
	if buf.Generation() != 2 {
		t.Errorf("expected generation 2, got %d", buf.Generation())
	}

	buf.Write([]byte("c"))
	if buf.Generation() != 3 {
		t.Errorf("expected generation 3, got %d", buf.Generation())
	}

	// Flush doesn't change generation (allows detecting new writes after flush)
	buf.Flush()
	if buf.Generation() != 3 {
		t.Errorf("flush should not change generation, got %d", buf.Generation())
	}

	// New write after flush increments generation
	buf.Write([]byte("d"))
	if buf.Generation() != 4 {
		t.Errorf("expected generation 4, got %d", buf.Generation())
	}
}

func TestOutputBuffer_GenerationRaceDetection(t *testing.T) {
	// This test verifies the pattern used in service.go for timer race detection
	buf := NewOutputBuffer()

	buf.Write([]byte("initial"))
	genAtTimerSchedule := buf.Generation()

	// Simulate new data arriving (would happen between timer schedule and fire)
	buf.Write([]byte("new data"))

	// Timer callback would check: if buf.Generation() != expectedGen, skip flush
	currentGen := buf.Generation()
	if currentGen == genAtTimerSchedule {
		t.Error("generation should have changed after new write")
	}

	// This is what the timer callback does to detect race
	shouldSkipFlush := currentGen != genAtTimerSchedule
	if !shouldSkipFlush {
		t.Error("timer callback should skip flush when generation changed")
	}
}
