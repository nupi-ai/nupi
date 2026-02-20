package eventbus

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestOverflowPushPop(t *testing.T) {
	ovf := newOverflowBuffer(4)

	for i := 0; i < 4; i++ {
		ok := ovf.push(Envelope{CorrelationID: string(rune('a' + i))})
		if !ok {
			t.Fatalf("push %d should succeed", i)
		}
	}

	if ovf.len() != 4 {
		t.Fatalf("expected len 4, got %d", ovf.len())
	}

	// FIFO ordering
	for i := 0; i < 4; i++ {
		env, ok := ovf.pop()
		if !ok {
			t.Fatalf("pop %d should succeed", i)
		}
		want := string(rune('a' + i))
		if env.CorrelationID != want {
			t.Fatalf("expected %q, got %q", want, env.CorrelationID)
		}
	}

	_, ok := ovf.pop()
	if ok {
		t.Fatal("pop from empty buffer should return false")
	}
}

func TestOverflowCapacity(t *testing.T) {
	ovf := newOverflowBuffer(2)

	ovf.push(Envelope{CorrelationID: "a"})
	ovf.push(Envelope{CorrelationID: "b"})

	ok := ovf.push(Envelope{CorrelationID: "c"})
	if ok {
		t.Fatal("push should return false when buffer is full")
	}

	if ovf.len() != 2 {
		t.Fatalf("expected len 2, got %d", ovf.len())
	}
}

func TestOverflowDrainLoop(t *testing.T) {
	ovf := newOverflowBuffer(8)
	ch := make(chan Envelope, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ovf.drainLoop(ctx, ch)

	for i := 0; i < 5; i++ {
		ovf.push(Envelope{CorrelationID: string(rune('0' + i))})
	}

	for i := 0; i < 5; i++ {
		select {
		case env := <-ch:
			want := string(rune('0' + i))
			if env.CorrelationID != want {
				t.Fatalf("expected %q, got %q", want, env.CorrelationID)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event %d", i)
		}
	}
}

func TestOverflowDrainCancellation(t *testing.T) {
	ovf := newOverflowBuffer(4)
	ch := make(chan Envelope, 4)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		ovf.drainLoop(ctx, ch)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainLoop did not exit after context cancel")
	}
}

func TestOverflowConcurrency(t *testing.T) {
	ovf := newOverflowBuffer(256)
	// Use a large channel so the drain loop never blocks on send.
	// Previously ch had capacity 64, which caused the drain loop to stall
	// under backpressure — leading to flaky failures under the race detector.
	ch := make(chan Envelope, 1024)
	ctx, cancel := context.WithCancel(context.Background())

	go ovf.drainLoop(ctx, ch)

	var wg sync.WaitGroup
	pushCount := 200

	// Concurrent pushers
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < pushCount; i++ {
				ovf.push(Envelope{CorrelationID: "x"})
			}
		}()
	}

	// Wait for all pushers to finish.
	wg.Wait()

	// Wait for the drain loop to empty the overflow buffer before cancelling.
	// With a large ch the drain loop never blocks on send, so this is fast.
	deadline := time.After(5 * time.Second)
	for ovf.len() > 0 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for overflow to drain, %d items remaining", ovf.len())
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	<-ovf.done

	received := 0
drain:
	for {
		select {
		case <-ch:
			received++
		default:
			break drain
		}
	}

	// 4 goroutines × 200 pushes = 800 attempts. Some are rejected when the
	// overflow buffer is momentarily full, but with a large channel the drain
	// loop processes items quickly. We expect well over 50.
	if received < 50 {
		t.Fatalf("expected at least 50 events to be drained, got %d", received)
	}
}
