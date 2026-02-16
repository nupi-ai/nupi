package daemon

import (
	"sync"
	"time"
)

// RuntimeInfo stores runtime metadata exposed to clients.
type RuntimeInfo struct {
	mu        sync.RWMutex
	grpcPort  int
	startTime time.Time
}

// SetGRPCPort updates the active gRPC port.
func (r *RuntimeInfo) SetGRPCPort(port int) {
	r.mu.Lock()
	r.grpcPort = port
	r.mu.Unlock()
}

// GRPCPort returns the current gRPC port.
func (r *RuntimeInfo) GRPCPort() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.grpcPort
}

// SetStartTime records the daemon start time.
func (r *RuntimeInfo) SetStartTime(t time.Time) {
	r.mu.Lock()
	r.startTime = t
	r.mu.Unlock()
}

// StartTime returns the daemon start time.
func (r *RuntimeInfo) StartTime() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.startTime
}
