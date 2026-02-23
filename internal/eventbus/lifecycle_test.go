package eventbus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type testCloser struct {
	mu    sync.Mutex
	count int
}

func (c *testCloser) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
}

func (c *testCloser) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

func TestSubscriptionGroupCloseAllClosesRegisteredSubscriptions(t *testing.T) {
	var group SubscriptionGroup
	var nilCloser *testCloser

	first := &testCloser{}
	second := &testCloser{}

	group.Add(first, nilCloser, second)
	group.CloseAll()

	if first.calls() != 1 {
		t.Fatalf("expected first closer to be called once, got %d", first.calls())
	}
	if second.calls() != 1 {
		t.Fatalf("expected second closer to be called once, got %d", second.calls())
	}
}

func TestSubscriptionGroupCloseAllClearsGroup(t *testing.T) {
	var group SubscriptionGroup
	closer := &testCloser{}

	group.Add(closer)
	group.CloseAll()
	group.CloseAll()

	if closer.calls() != 1 {
		t.Fatalf("expected closer to be called once, got %d", closer.calls())
	}
}

func TestWaitForWorkersWaitsUntilDone(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		time.Sleep(10 * time.Millisecond)
		wg.Done()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := WaitForWorkers(ctx, &wg); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestWaitForWorkersReturnsContextError(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Done()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := WaitForWorkers(ctx, &wg)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
}

func TestWaitForWorkersNilWaitGroup(t *testing.T) {
	if err := WaitForWorkers(context.Background(), nil); err != nil {
		t.Fatalf("expected nil error for nil waitgroup, got %v", err)
	}
}

func TestServiceLifecycleStopClosesSubscriptionsAndCancelsContext(t *testing.T) {
	var lc ServiceLifecycle
	lc.Start(context.Background())

	closer := &testCloser{}
	lc.AddSubscriptions(closer)
	lc.Stop()

	if closer.calls() != 1 {
		t.Fatalf("expected subscription closer to be called once, got %d", closer.calls())
	}
	if lc.Context().Err() == nil {
		t.Fatal("expected lifecycle context to be cancelled")
	}
}

func TestServiceLifecycleGoAndWait(t *testing.T) {
	var lc ServiceLifecycle
	lc.Start(context.Background())

	done := make(chan struct{})
	lc.Go(func(ctx context.Context) {
		select {
		case <-ctx.Done():
			close(done)
		case <-time.After(time.Second):
		}
	})

	lc.Stop()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not observe context cancellation")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := lc.Wait(ctx); err != nil {
		t.Fatalf("expected wait to succeed, got %v", err)
	}
}
