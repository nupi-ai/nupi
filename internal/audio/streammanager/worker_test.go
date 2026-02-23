package streammanager

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestWorkerEnqueueAndStopOnHandler(t *testing.T) {
	w := NewWorker[string](context.Background(), 2)

	var (
		mu    sync.Mutex
		items []string
	)
	done := make(chan struct{})
	w.Start(func(item string) bool {
		mu.Lock()
		items = append(items, item)
		mu.Unlock()
		return item == "stop"
	}, nil, func() { close(done) })

	if err := w.Enqueue("a", errors.New("closed")); err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	if err := w.Enqueue("stop", errors.New("closed")); err != nil {
		t.Fatalf("enqueue stop: %v", err)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for worker exit")
	}

	w.Wait(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(items) != 2 || items[0] != "a" || items[1] != "stop" {
		t.Fatalf("unexpected items: %+v", items)
	}
}

func TestWorkerContextDoneEnqueueError(t *testing.T) {
	closedErr := errors.New("closed")
	w := NewWorker[string](context.Background(), 1)
	w.Start(func(item string) bool { return false }, nil, nil)
	w.Stop()
	w.Wait(context.Background())

	err := w.Enqueue("x", closedErr)
	if !errors.Is(err, closedErr) {
		t.Fatalf("expected closed error, got %v", err)
	}
}

func TestWorkerDrainNonBlocking(t *testing.T) {
	w := NewWorker[int](context.Background(), 4)
	if err := w.Enqueue(1, errors.New("closed")); err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	if err := w.Enqueue(2, errors.New("closed")); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}

	var drained []int
	w.DrainNonBlocking(func(item int) {
		drained = append(drained, item)
	})

	if len(drained) != 2 || drained[0] != 1 || drained[1] != 2 {
		t.Fatalf("unexpected drained values: %+v", drained)
	}
}
