package jsruntime

import (
	"context"
	"log"
	"sync"
	"time"
)

// SupervisedRuntime wraps Runtime with automatic restart on failure.
type SupervisedRuntime struct {
	bunPath    string
	hostScript string
	opts       []Option

	mu      sync.RWMutex
	runtime *Runtime

	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	restarts  int
	lastStart time.Time

	onRestart func() // callback invoked after successful restart
}

// SupervisedConfig configures the supervised runtime behavior.
type SupervisedConfig struct {
	// MaxRestarts is the maximum number of restarts before giving up.
	// 0 means unlimited restarts.
	MaxRestarts int

	// MinRestartInterval is the minimum time between restarts.
	// Default is 1 second.
	MinRestartInterval time.Duration

	// MaxRestartInterval is the maximum backoff interval.
	// Default is 30 seconds.
	MaxRestartInterval time.Duration
}

var defaultSupervisedConfig = SupervisedConfig{
	MaxRestarts:        0, // Unlimited
	MinRestartInterval: 1 * time.Second,
	MaxRestartInterval: 30 * time.Second,
}

// NewSupervised creates a supervised runtime that auto-restarts on failure.
func NewSupervised(ctx context.Context, bunPath, hostScript string, opts ...Option) (*SupervisedRuntime, error) {
	sctx, cancel := context.WithCancel(ctx)

	sr := &SupervisedRuntime{
		bunPath:    bunPath,
		hostScript: hostScript,
		opts:       opts,
		ctx:        sctx,
		cancel:     cancel,
		done:       make(chan struct{}),
	}

	// Start the initial runtime
	if err := sr.startRuntime(); err != nil {
		cancel()
		return nil, err
	}

	// Start the supervisor goroutine
	go sr.supervise()

	return sr, nil
}

// Runtime returns the current runtime instance.
// The returned instance may change if a restart occurs.
func (sr *SupervisedRuntime) Runtime() *Runtime {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	return sr.runtime
}

// Restarts returns the number of restarts that have occurred.
func (sr *SupervisedRuntime) Restarts() int {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	return sr.restarts
}

// Shutdown stops the supervised runtime and prevents further restarts.
func (sr *SupervisedRuntime) Shutdown(ctx context.Context) error {
	sr.cancel()

	sr.mu.Lock()
	rt := sr.runtime
	sr.runtime = nil
	sr.mu.Unlock()

	if rt != nil {
		return rt.Shutdown(ctx)
	}

	// Wait for supervisor to exit
	select {
	case <-sr.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done returns a channel that's closed when the supervisor stops.
func (sr *SupervisedRuntime) Done() <-chan struct{} {
	return sr.done
}

// OnRestart registers a callback to be invoked after successful restart.
// The callback is called asynchronously (in a goroutine) after the runtime restarts.
// This can be used to reload plugins or perform other recovery actions.
func (sr *SupervisedRuntime) OnRestart(fn func()) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.onRestart = fn
}

func (sr *SupervisedRuntime) startRuntime() error {
	// Don't use sr.ctx for the runtime - we want to be able to restart it
	rt, err := New(context.Background(), sr.bunPath, sr.hostScript, sr.opts...)
	if err != nil {
		return err
	}

	sr.mu.Lock()
	sr.runtime = rt
	sr.lastStart = time.Now()
	sr.mu.Unlock()

	return nil
}

func (sr *SupervisedRuntime) supervise() {
	defer close(sr.done)

	cfg := defaultSupervisedConfig
	backoff := cfg.MinRestartInterval

	for {
		sr.mu.RLock()
		rt := sr.runtime
		sr.mu.RUnlock()

		if rt == nil {
			return // Shutdown was called
		}

		// Wait for runtime to stop or context to be cancelled
		select {
		case <-sr.ctx.Done():
			return
		case <-rt.Done():
			// Runtime stopped - check if we should restart
		}

		// Check if context is done (shutdown)
		if sr.ctx.Err() != nil {
			return
		}

		// Get runtime error
		err := rt.Err()

		sr.mu.Lock()
		// Check if runtime was nil'd (shutdown)
		if sr.runtime == nil {
			sr.mu.Unlock()
			return
		}

		sr.restarts++
		restartNum := sr.restarts
		sr.mu.Unlock()

		if cfg.MaxRestarts > 0 && restartNum > cfg.MaxRestarts {
			log.Printf("[jsruntime] Max restarts (%d) exceeded, giving up", cfg.MaxRestarts)
			return
		}

		log.Printf("[jsruntime] Runtime crashed (restart #%d): %v", restartNum, err)

		// Apply backoff
		select {
		case <-sr.ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Increase backoff for next time
		backoff = backoff * 2
		if backoff > cfg.MaxRestartInterval {
			backoff = cfg.MaxRestartInterval
		}

		// Attempt restart
		log.Printf("[jsruntime] Attempting restart #%d...", restartNum)
		if err := sr.startRuntime(); err != nil {
			log.Printf("[jsruntime] Restart #%d failed: %v", restartNum, err)
			// Continue loop - will try again with backoff
			continue
		}

		log.Printf("[jsruntime] Restart #%d successful", restartNum)
		// Reset backoff on successful restart
		backoff = cfg.MinRestartInterval

		// Invoke restart callback (e.g., to reload plugins)
		sr.mu.RLock()
		callback := sr.onRestart
		sr.mu.RUnlock()
		if callback != nil {
			go callback()
		}
	}
}

// LoadPlugin loads a JS plugin via the supervised runtime.
func (sr *SupervisedRuntime) LoadPlugin(ctx context.Context, path string) (*PluginMetadata, error) {
	rt := sr.Runtime()
	if rt == nil {
		return nil, ErrRuntimeStopped
	}
	return rt.LoadPlugin(ctx, path)
}

// LoadPluginWithOptions loads a JS plugin with options via the supervised runtime.
func (sr *SupervisedRuntime) LoadPluginWithOptions(ctx context.Context, path string, opts LoadPluginOptions) (*PluginMetadata, error) {
	rt := sr.Runtime()
	if rt == nil {
		return nil, ErrRuntimeStopped
	}
	return rt.LoadPluginWithOptions(ctx, path, opts)
}

// Call invokes a function on a loaded plugin via the supervised runtime.
func (sr *SupervisedRuntime) Call(ctx context.Context, pluginPath, fnName string, args ...any) (any, error) {
	rt := sr.Runtime()
	if rt == nil {
		return nil, ErrRuntimeStopped
	}
	return rt.Call(ctx, pluginPath, fnName, args...)
}

// CallWithTimeout invokes a function with a specific timeout via the supervised runtime.
func (sr *SupervisedRuntime) CallWithTimeout(pluginPath, fnName string, timeout time.Duration, args ...any) (any, error) {
	rt := sr.Runtime()
	if rt == nil {
		return nil, ErrRuntimeStopped
	}
	return rt.CallWithTimeout(pluginPath, fnName, timeout, args...)
}
