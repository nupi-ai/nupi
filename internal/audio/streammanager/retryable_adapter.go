package streammanager

import (
	"context"
	"errors"
	"time"
)

// RetryableAdapterConfig configures RetryableAdapter behaviour.
type RetryableAdapterConfig[A any, O any] struct {
	Create               func(ctx context.Context) (A, error)
	Close                func(ctx context.Context, adapter A) ([]O, error)
	IsAdapterUnavailable func(err error) bool
	OnCreateError        func(err error)
	OnReconnected        func()
	OnAdapterUnavailable func(err error)
	OnProcessError       func(err error)
	OnCloseError         func(reason string, err error)
	RecoveryCloseTimeout time.Duration
}

// RetryableAdapter wraps single-adapter lifecycle with reconnect-on-demand and
// adapter-unavailable recovery semantics. It is intended for single-goroutine
// stream processing loops.
type RetryableAdapter[A any, O any] struct {
	cfg     RetryableAdapterConfig[A, O]
	adapter A
	active  bool
}

// NewRetryableAdapter constructs a retryable adapter helper.
func NewRetryableAdapter[A any, O any](cfg RetryableAdapterConfig[A, O]) *RetryableAdapter[A, O] {
	return &RetryableAdapter[A, O]{cfg: cfg}
}

// SetActive seeds the helper with an already-created adapter.
func (r *RetryableAdapter[A, O]) SetActive(adapter A) {
	r.adapter = adapter
	r.active = true
}

// Process ensures an adapter exists, runs process, publishes outputs, and
// handles adapter-unavailable recovery.
func (r *RetryableAdapter[A, O]) Process(
	ctx context.Context,
	process func(ctx context.Context, adapter A) ([]O, error),
	publish func(out O),
) {
	if process == nil {
		return
	}
	adapter, ok := r.ensureAdapter(ctx)
	if !ok {
		return
	}

	outputs, err := process(ctx, adapter)
	if publish != nil {
		for _, out := range outputs {
			publish(out)
		}
	}

	if err == nil {
		return
	}
	if r.isAdapterUnavailable(err) {
		if r.cfg.OnAdapterUnavailable != nil {
			r.cfg.OnAdapterUnavailable(err)
		}
		r.CloseForRecovery("adapter unavailable")
		return
	}
	if r.cfg.OnProcessError != nil {
		r.cfg.OnProcessError(err)
	}
}

// CloseForRecovery closes current adapter and clears it so a subsequent
// Process call can reconnect.
func (r *RetryableAdapter[A, O]) CloseForRecovery(reason string) {
	timeout := r.cfg.RecoveryCloseTimeout
	if timeout <= 0 {
		timeout = time.Second
	}
	r.close(reason, timeout, nil)
}

// Close closes current adapter and optionally publishes outputs.
func (r *RetryableAdapter[A, O]) Close(reason string, timeout time.Duration, publish func(out O)) {
	if timeout <= 0 {
		timeout = time.Second
	}
	r.close(reason, timeout, publish)
}

func (r *RetryableAdapter[A, O]) ensureAdapter(ctx context.Context) (A, bool) {
	if r.active {
		return r.adapter, true
	}
	var zero A
	if r.cfg.Create == nil {
		return zero, false
	}

	adapter, err := r.cfg.Create(ctx)
	if err != nil {
		if !r.isAdapterUnavailable(err) && r.cfg.OnCreateError != nil {
			r.cfg.OnCreateError(err)
		}
		return zero, false
	}

	r.adapter = adapter
	r.active = true
	if r.cfg.OnReconnected != nil {
		r.cfg.OnReconnected()
	}
	return r.adapter, true
}

func (r *RetryableAdapter[A, O]) close(reason string, timeout time.Duration, publish func(out O)) {
	if !r.active {
		return
	}

	adapter := r.adapter
	var zero A
	r.adapter = zero
	r.active = false

	if r.cfg.Close == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	outputs, err := r.cfg.Close(ctx, adapter)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		if r.cfg.OnCloseError != nil {
			r.cfg.OnCloseError(reason, err)
		}
	}
	if publish != nil {
		for _, out := range outputs {
			publish(out)
		}
	}
}

func (r *RetryableAdapter[A, O]) isAdapterUnavailable(err error) bool {
	return r.cfg.IsAdapterUnavailable != nil && r.cfg.IsAdapterUnavailable(err)
}
