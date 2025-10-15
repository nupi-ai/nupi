package pty

import "testing"

func TestWrapperGetBufferReturnsCopy(t *testing.T) {
	w := New()

	w.bufferMutex.Lock()
	if _, err := w.outputBuffer.WriteString("abc"); err != nil {
		t.Fatalf("failed to seed buffer: %v", err)
	}
	w.bufferMutex.Unlock()

	snapshot := w.GetBuffer()
	if got := string(snapshot); got != "abc" {
		t.Fatalf("expected snapshot \"abc\", got %q", got)
	}

	w.bufferMutex.Lock()
	if _, err := w.outputBuffer.WriteString("def"); err != nil {
		t.Fatalf("failed to append buffer: %v", err)
	}
	w.bufferMutex.Unlock()

	if got := string(snapshot); got != "abc" {
		t.Fatalf("expected cached snapshot to remain \"abc\", got %q", got)
	}

	snapshot[0] = 'X'
	if got := string(w.GetBuffer()); got != "abcdef" {
		t.Fatalf("expected live buffer to remain \"abcdef\", got %q", got)
	}
}
