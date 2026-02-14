package streammanager

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type mockHandle struct {
	key     string
	items   []string
	mu      sync.Mutex
	stopped bool
	stopCh  chan struct{}
}

func newMockHandle(key string) *mockHandle {
	return &mockHandle{key: key, stopCh: make(chan struct{})}
}

func (h *mockHandle) Enqueue(item string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return errors.New("stream closed")
	}
	h.items = append(h.items, item)
	return nil
}

func (h *mockHandle) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.stopped {
		h.stopped = true
		close(h.stopCh)
	}
}

func (h *mockHandle) Wait(_ context.Context) {
	<-h.stopCh
}

func (h *mockHandle) getItems() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.items...)
}

var errAdapterUnavailable = errors.New("adapter unavailable")
var errPermanent = errors.New("permanent failure")

func classifyTestError(err error) (bool, bool) {
	if errors.Is(err, errAdapterUnavailable) {
		return true, false
	}
	return false, false
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCreateStreamAndLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var created atomic.Int32
	mgr := New(Config[string]{
		Tag: "TEST",
		Ctx: ctx,
		Factory: StreamFactoryFunc[string](func(_ context.Context, key string, _ SessionParams) (StreamHandle[string], error) {
			created.Add(1)
			return newMockHandle(key), nil
		}),
	})

	key := StreamKey("s1", "mic")
	params := SessionParams{SessionID: "s1", StreamID: "mic"}

	h, err := mgr.CreateStream(key, params)
	if err != nil {
		t.Fatalf("CreateStream: %v", err)
	}
	if h == nil {
		t.Fatal("expected non-nil handle")
	}

	// Second call should return the same handle.
	h2, err := mgr.CreateStream(key, params)
	if err != nil {
		t.Fatalf("CreateStream (2nd): %v", err)
	}
	if any(h) != any(h2) {
		t.Fatal("expected same handle on duplicate create")
	}
	if created.Load() != 1 {
		t.Fatalf("factory called %d times, want 1", created.Load())
	}
}

func TestBufferPendingAndFlush(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := New(Config[string]{
		Tag:        "TEST",
		Ctx:        ctx,
		MaxPending: 10,
		Retry:      RetryConfig{Initial: time.Hour, Max: time.Hour}, // No auto-retry
		Factory: StreamFactoryFunc[string](func(_ context.Context, key string, _ SessionParams) (StreamHandle[string], error) {
			return newMockHandle(key), nil
		}),
	})

	key := StreamKey("s1", "mic")
	params := SessionParams{SessionID: "s1", StreamID: "mic"}

	mgr.BufferPending(key, params, "seg-1")
	mgr.BufferPending(key, params, "seg-2")

	if mgr.PendingCount() != 1 {
		t.Fatalf("expected 1 pending queue, got %d", mgr.PendingCount())
	}

	h := newMockHandle(key)
	mgr.FlushPending(key, h)

	items := h.getItems()
	if len(items) != 2 || items[0] != "seg-1" || items[1] != "seg-2" {
		t.Fatalf("unexpected flushed items: %v", items)
	}
	if mgr.PendingCount() != 0 {
		t.Fatalf("expected 0 pending after flush, got %d", mgr.PendingCount())
	}
}

func TestPendingDropsOldestWhenFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := New(Config[string]{
		Tag:        "TEST",
		Ctx:        ctx,
		MaxPending: 3,
		Retry:      RetryConfig{Initial: time.Hour, Max: time.Hour},
		Factory: StreamFactoryFunc[string](func(_ context.Context, key string, _ SessionParams) (StreamHandle[string], error) {
			return newMockHandle(key), nil
		}),
	})

	key := StreamKey("s1", "mic")
	params := SessionParams{SessionID: "s1", StreamID: "mic"}

	for i := 0; i < 5; i++ {
		mgr.BufferPending(key, params, string(rune('a'+i)))
	}

	pq, ok := mgr.GetPendingQueue(key)
	if !ok {
		t.Fatal("expected pending queue")
	}
	if len(pq.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(pq.Items))
	}
	// First two dropped, last three: c, d, e
	if pq.Items[0] != "c" || pq.Items[1] != "d" || pq.Items[2] != "e" {
		t.Fatalf("unexpected items: %v", pq.Items)
	}
}

func TestRetryCreatesStreamWhenAdapterBecomesAvailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu    sync.Mutex
		ready bool
	)

	mgr := New(Config[string]{
		Tag: "TEST",
		Ctx: ctx,
		Retry: RetryConfig{
			Initial: 5 * time.Millisecond,
			Max:     20 * time.Millisecond,
		},
		Factory: StreamFactoryFunc[string](func(_ context.Context, key string, _ SessionParams) (StreamHandle[string], error) {
			mu.Lock()
			defer mu.Unlock()
			if !ready {
				return nil, errAdapterUnavailable
			}
			return newMockHandle(key), nil
		}),
		Callbacks: Callbacks[string]{
			ClassifyCreateError: classifyTestError,
		},
	})

	key := StreamKey("s1", "mic")
	params := SessionParams{SessionID: "s1", StreamID: "mic"}

	mgr.BufferPending(key, params, "seg-1")

	// Should not have a stream yet.
	if _, ok := mgr.Stream(key); ok {
		t.Fatal("expected no stream before adapter ready")
	}

	// Make adapter available.
	mu.Lock()
	ready = true
	mu.Unlock()

	// Wait for retry to fire.
	deadline := time.After(time.Second)
	for {
		if _, ok := mgr.Stream(key); ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for stream creation via retry")
		case <-time.After(2 * time.Millisecond):
		}
	}

	if mgr.PendingCount() != 0 {
		t.Fatalf("expected pending cleared after retry, got %d", mgr.PendingCount())
	}
}

func TestRetryDropsPendingOnPermanentError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu       sync.Mutex
		attempts int
	)

	mgr := New(Config[string]{
		Tag: "TEST",
		Ctx: ctx,
		Retry: RetryConfig{
			Initial: 5 * time.Millisecond,
			Max:     20 * time.Millisecond,
		},
		Factory: StreamFactoryFunc[string](func(_ context.Context, _ string, _ SessionParams) (StreamHandle[string], error) {
			mu.Lock()
			defer mu.Unlock()
			attempts++
			if attempts == 1 {
				return nil, errAdapterUnavailable
			}
			return nil, errPermanent
		}),
		Callbacks: Callbacks[string]{
			ClassifyCreateError: classifyTestError,
		},
	})

	key := StreamKey("s1", "mic")
	params := SessionParams{SessionID: "s1", StreamID: "mic"}

	mgr.BufferPending(key, params, "seg-1")

	deadline := time.After(time.Second)
	for {
		if mgr.PendingCount() == 0 {
			mu.Lock()
			a := attempts
			mu.Unlock()
			if a >= 2 {
				break
			}
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for pending to be dropped")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func TestRetryAbandonWithMaxFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu       sync.Mutex
		attempts int
	)

	mgr := New(Config[string]{
		Tag: "TEST",
		Ctx: ctx,
		Retry: RetryConfig{
			Initial:     5 * time.Millisecond,
			Max:         20 * time.Millisecond,
			MaxFailures: 1,
		},
		Factory: StreamFactoryFunc[string](func(_ context.Context, _ string, _ SessionParams) (StreamHandle[string], error) {
			mu.Lock()
			defer mu.Unlock()
			attempts++
			if attempts == 1 {
				return nil, errAdapterUnavailable
			}
			return nil, errPermanent
		}),
		Callbacks: Callbacks[string]{
			ClassifyCreateError: classifyTestError,
		},
	})

	key := StreamKey("s1", "mic")
	params := SessionParams{SessionID: "s1", StreamID: "mic"}

	mgr.BufferPending(key, params, "seg-1")

	deadline := time.After(time.Second)
	for {
		if mgr.PendingCount() == 0 {
			mu.Lock()
			a := attempts
			mu.Unlock()
			if a >= 2 {
				break
			}
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for abandon")
		case <-time.After(2 * time.Millisecond):
		}
	}

	if mgr.RetryAbandoned() != 1 {
		t.Fatalf("expected RetryAbandoned() = 1, got %d", mgr.RetryAbandoned())
	}
}

func TestRemoveStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := New(Config[string]{
		Tag: "TEST",
		Ctx: ctx,
		Factory: StreamFactoryFunc[string](func(_ context.Context, key string, _ SessionParams) (StreamHandle[string], error) {
			return newMockHandle(key), nil
		}),
	})

	key := StreamKey("s1", "mic")
	params := SessionParams{SessionID: "s1", StreamID: "mic"}

	h, _ := mgr.CreateStream(key, params)
	if mgr.ActiveStreamCount() != 1 {
		t.Fatalf("expected ActiveStreamCount() = 1, got %d", mgr.ActiveStreamCount())
	}

	mgr.RemoveStream(key, h)

	if _, ok := mgr.Stream(key); ok {
		t.Fatal("expected stream to be removed")
	}
	if mgr.ActiveStreamCount() != 0 {
		t.Fatalf("expected ActiveStreamCount() = 0, got %d", mgr.ActiveStreamCount())
	}

	// Removing again is a no-op.
	mgr.RemoveStream(key, h)
	if mgr.ActiveStreamCount() != 0 {
		t.Fatalf("expected ActiveStreamCount() still 0, got %d", mgr.ActiveStreamCount())
	}
}

func TestCloseAllStreams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := New(Config[string]{
		Tag: "TEST",
		Ctx: ctx,
		Factory: StreamFactoryFunc[string](func(_ context.Context, key string, _ SessionParams) (StreamHandle[string], error) {
			return newMockHandle(key), nil
		}),
	})

	mgr.CreateStream(StreamKey("s1", "mic"), SessionParams{SessionID: "s1", StreamID: "mic"})
	mgr.CreateStream(StreamKey("s2", "mic"), SessionParams{SessionID: "s2", StreamID: "mic"})

	if mgr.ActiveStreamCount() != 2 {
		t.Fatalf("expected ActiveStreamCount() = 2, got %d", mgr.ActiveStreamCount())
	}

	handles := mgr.CloseAllStreams()
	if len(handles) != 2 {
		t.Fatalf("expected 2 handles, got %d", len(handles))
	}
	if mgr.ActiveStreamCount() != 0 {
		t.Fatalf("expected ActiveStreamCount() = 0 after close, got %d", mgr.ActiveStreamCount())
	}
}

func TestClearPending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := New(Config[string]{
		Tag:   "TEST",
		Ctx:   ctx,
		Retry: RetryConfig{Initial: time.Hour, Max: time.Hour},
		Factory: StreamFactoryFunc[string](func(_ context.Context, key string, _ SessionParams) (StreamHandle[string], error) {
			return nil, errAdapterUnavailable
		}),
		Callbacks: Callbacks[string]{
			ClassifyCreateError: classifyTestError,
		},
	})

	key := StreamKey("s1", "mic")
	params := SessionParams{SessionID: "s1", StreamID: "mic"}

	mgr.BufferPending(key, params, "seg-1")
	if mgr.PendingCount() != 1 {
		t.Fatal("expected 1 pending")
	}

	mgr.ClearPending(key)
	if mgr.PendingCount() != 0 {
		t.Fatal("expected 0 pending after clear")
	}
}

func TestShutdownPending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := New(Config[string]{
		Tag:   "TEST",
		Ctx:   ctx,
		Retry: RetryConfig{Initial: time.Hour, Max: time.Hour},
		Factory: StreamFactoryFunc[string](func(_ context.Context, key string, _ SessionParams) (StreamHandle[string], error) {
			return nil, errAdapterUnavailable
		}),
		Callbacks: Callbacks[string]{
			ClassifyCreateError: classifyTestError,
		},
	})

	params := SessionParams{SessionID: "s1", StreamID: "mic"}
	mgr.BufferPending(StreamKey("s1", "mic"), params, "a")
	mgr.BufferPending(StreamKey("s2", "mic"), SessionParams{SessionID: "s2", StreamID: "mic"}, "b")

	mgr.ShutdownPending()
	if mgr.PendingCount() != 0 {
		t.Fatalf("expected all pending cleared, got %d", mgr.PendingCount())
	}
}

func TestActiveStreamCountOnCreate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := New(Config[string]{
		Tag: "TEST",
		Ctx: ctx,
		Factory: StreamFactoryFunc[string](func(_ context.Context, key string, _ SessionParams) (StreamHandle[string], error) {
			return newMockHandle(key), nil
		}),
	})

	key := StreamKey("s1", "mic")
	mgr.CreateStream(key, SessionParams{SessionID: "s1", StreamID: "mic"})
	if mgr.ActiveStreamCount() != 1 {
		t.Fatalf("expected ActiveStreamCount() = 1, got %d", mgr.ActiveStreamCount())
	}

	// Duplicate create should not increment again.
	mgr.CreateStream(key, SessionParams{SessionID: "s1", StreamID: "mic"})
	if mgr.ActiveStreamCount() != 1 {
		t.Fatalf("expected ActiveStreamCount() still 1, got %d", mgr.ActiveStreamCount())
	}
}

func TestOnEnqueueErrorCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var handledItems []string
	mgr := New(Config[string]{
		Tag:   "TEST",
		Ctx:   ctx,
		Retry: RetryConfig{Initial: time.Hour, Max: time.Hour},
		Factory: StreamFactoryFunc[string](func(_ context.Context, key string, _ SessionParams) (StreamHandle[string], error) {
			return newMockHandle(key), nil
		}),
		Callbacks: Callbacks[string]{
			OnEnqueueError: func(key string, item string, err error) bool {
				handledItems = append(handledItems, item)
				return true
			},
		},
	})

	key := StreamKey("s1", "mic")
	params := SessionParams{SessionID: "s1", StreamID: "mic"}

	// Buffer an item.
	mgr.BufferPending(key, params, "seg-1")

	// Create a handle that rejects enqueue.
	h := newMockHandle(key)
	h.Stop() // Make it reject.

	mgr.FlushPending(key, h)

	if len(handledItems) != 1 || handledItems[0] != "seg-1" {
		t.Fatalf("unexpected handled items: %v", handledItems)
	}
}

func TestConcurrentCreateStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var created atomic.Int32
	mgr := New(Config[string]{
		Tag: "TEST",
		Ctx: ctx,
		Factory: StreamFactoryFunc[string](func(_ context.Context, key string, _ SessionParams) (StreamHandle[string], error) {
			created.Add(1)
			// Simulate some work.
			time.Sleep(time.Millisecond)
			return newMockHandle(key), nil
		}),
	})

	key := StreamKey("s1", "mic")
	params := SessionParams{SessionID: "s1", StreamID: "mic"}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.CreateStream(key, params)
		}()
	}
	wg.Wait()

	// At most a few factory calls (race between lock check and creation),
	// but only one stream registered.
	if _, ok := mgr.Stream(key); !ok {
		t.Fatal("expected stream to be registered")
	}
}

func TestRetryAbandonWithMaxDuration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := New(Config[string]{
		Tag: "TEST",
		Ctx: ctx,
		Retry: RetryConfig{
			Initial:     5 * time.Millisecond,
			Max:         10 * time.Millisecond,
			MaxFailures: 1000, // high limit so duration triggers first
			MaxDuration: 50 * time.Millisecond,
		},
		Factory: StreamFactoryFunc[string](func(_ context.Context, _ string, _ SessionParams) (StreamHandle[string], error) {
			return nil, errPermanent
		}),
		Callbacks: Callbacks[string]{
			ClassifyCreateError: classifyTestError,
		},
	})

	key := StreamKey("s1", "mic")
	params := SessionParams{SessionID: "s1", StreamID: "mic"}

	mgr.BufferPending(key, params, "seg-1")

	deadline := time.After(2 * time.Second)
	for {
		if mgr.PendingCount() == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for duration-based abandon")
		case <-time.After(5 * time.Millisecond):
		}
	}

	if mgr.RetryAbandoned() != 1 {
		t.Fatalf("expected RetryAbandoned() = 1, got %d", mgr.RetryAbandoned())
	}
}

func TestRetryAttemptsCounter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu    sync.Mutex
		ready bool
	)

	mgr := New(Config[string]{
		Tag: "TEST",
		Ctx: ctx,
		Retry: RetryConfig{
			Initial: 5 * time.Millisecond,
			Max:     20 * time.Millisecond,
		},
		Factory: StreamFactoryFunc[string](func(_ context.Context, key string, _ SessionParams) (StreamHandle[string], error) {
			mu.Lock()
			defer mu.Unlock()
			if !ready {
				return nil, errAdapterUnavailable
			}
			return newMockHandle(key), nil
		}),
		Callbacks: Callbacks[string]{
			ClassifyCreateError: classifyTestError,
		},
	})

	key := StreamKey("s1", "mic")
	mgr.BufferPending(key, SessionParams{SessionID: "s1", StreamID: "mic"}, "seg-1")

	// Let a few retries fire.
	time.Sleep(30 * time.Millisecond)

	mu.Lock()
	ready = true
	mu.Unlock()

	deadline := time.After(time.Second)
	for {
		if _, ok := mgr.Stream(key); ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for stream")
		case <-time.After(2 * time.Millisecond):
		}
	}

	if mgr.RetryAttempts() == 0 {
		t.Fatal("expected RetryAttempts() > 0")
	}
}

func TestRemoveStreamByKeyUpdatesCounter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := New(Config[string]{
		Tag: "TEST",
		Ctx: ctx,
		Factory: StreamFactoryFunc[string](func(_ context.Context, key string, _ SessionParams) (StreamHandle[string], error) {
			return newMockHandle(key), nil
		}),
	})

	key := StreamKey("s1", "mic")
	mgr.CreateStream(key, SessionParams{SessionID: "s1", StreamID: "mic"})

	if mgr.ActiveStreamCount() != 1 {
		t.Fatalf("expected ActiveStreamCount() = 1, got %d", mgr.ActiveStreamCount())
	}

	mgr.RemoveStreamByKey(key)

	if mgr.ActiveStreamCount() != 0 {
		t.Fatalf("expected ActiveStreamCount() = 0, got %d", mgr.ActiveStreamCount())
	}
	if _, ok := mgr.Stream(key); ok {
		t.Fatal("expected stream to be removed")
	}
}
