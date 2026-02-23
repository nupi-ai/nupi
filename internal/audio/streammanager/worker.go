package streammanager

import (
	"context"
	"sync"
)

// Worker manages a single goroutine that consumes queued items for a stream.
// It centralizes queueing and shutdown mechanics shared by STT/VAD/TTS streams.
type Worker[T any] struct {
	ctx    context.Context
	cancel context.CancelFunc
	ch     chan T
	wg     sync.WaitGroup
}

// NewWorker constructs a worker bound to parent context.
func NewWorker[T any](parent context.Context, buffer int) *Worker[T] {
	if parent == nil {
		parent = context.Background()
	}
	if buffer <= 0 {
		buffer = 1
	}
	ctx, cancel := context.WithCancel(parent)
	return &Worker[T]{
		ctx:    ctx,
		cancel: cancel,
		ch:     make(chan T, buffer),
	}
}

// Start launches the worker loop.
// handle should return true to stop the loop after processing an item.
func (w *Worker[T]) Start(handle func(item T) (stop bool), onContextDone, onExit func()) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer w.cancel()
		if onExit != nil {
			defer onExit()
		}

		for {
			select {
			case <-w.ctx.Done():
				if onContextDone != nil {
					onContextDone()
				}
				return
			case item := <-w.ch:
				if handle(item) {
					return
				}
			}
		}
	}()
}

// Context returns worker context.
func (w *Worker[T]) Context() context.Context {
	return w.ctx
}

// Enqueue appends an item to the worker queue.
func (w *Worker[T]) Enqueue(item T, closedErr error) error {
	select {
	case <-w.ctx.Done():
		return closedErr
	default:
	}

	select {
	case w.ch <- item:
		return nil
	case <-w.ctx.Done():
		return closedErr
	}
}

// Stop cancels worker context.
func (w *Worker[T]) Stop() {
	w.cancel()
}

// Wait blocks until worker exits or caller context is canceled.
func (w *Worker[T]) Wait(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.wg.Wait()
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// DrainNonBlocking drains all currently buffered items without blocking.
func (w *Worker[T]) DrainNonBlocking(consume func(item T)) {
	for {
		select {
		case item := <-w.ch:
			if consume != nil {
				consume(item)
			}
		default:
			return
		}
	}
}
