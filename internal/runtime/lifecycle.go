package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// Service defines a unit that can be started and stopped as part of the daemon runtime.
type Service interface {
	Start(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

// Lifecycle coordinates shutdown signalling across runtime services.
type Lifecycle struct {
	shutdownOnce sync.Once
	shutdownChan chan struct{}
}

// NewLifecycle creates a lifecycle controller with its own shutdown channel.
func NewLifecycle() *Lifecycle {
	return &Lifecycle{shutdownChan: make(chan struct{})}
}

// Done returns a channel that is closed when the lifecycle is shutting down.
func (l *Lifecycle) Done() <-chan struct{} {
	return l.shutdownChan
}

// Shutdown signals all listeners that the lifecycle is terminating.
func (l *Lifecycle) Shutdown() {
	l.shutdownOnce.Do(func() { close(l.shutdownChan) })
}

// WritePIDFile writes the given PID into the provided file path with secure permissions.
func WritePIDFile(pidFile string, pid int) error {
	if pidFile == "" {
		return fmt.Errorf("pid file path is empty")
	}

	if err := os.MkdirAll(filepath.Dir(pidFile), 0o755); err != nil {
		return fmt.Errorf("failed to create pid directory: %w", err)
	}

	data := []byte(strconv.Itoa(pid))
	if err := os.WriteFile(pidFile, data, 0o600); err != nil {
		return fmt.Errorf("failed to write pid file: %w", err)
	}

	return nil
}

// RemovePIDFile removes the pid file if it exists.
func RemovePIDFile(pidFile string) {
	if pidFile == "" {
		return
	}
	_ = os.Remove(pidFile)
}
