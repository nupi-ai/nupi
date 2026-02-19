// Package awareness implements the AI awareness system — long-term memory and identity.
// It manages core memory files (SOUL.md, IDENTITY.md, USER.md, GLOBAL.md, PROJECT.md)
// and injects their content into the AI system prompt on every request.
package awareness

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// Service manages the awareness system: core memory loading, directory structure,
// and event bus integration. Implements runtime.Service.
type Service struct {
	instanceDir  string
	awarenessDir string

	bus *eventbus.Bus

	mu         sync.RWMutex
	coreMemory string

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewService creates a new awareness service for the given instance directory.
func NewService(instanceDir string) *Service {
	return &Service{
		instanceDir:  instanceDir,
		awarenessDir: filepath.Join(instanceDir, "awareness"),
	}
}

// SetEventBus wires the event bus for publish/subscribe.
func (s *Service) SetEventBus(bus *eventbus.Bus) {
	s.bus = bus
}

// Start initializes the awareness service: ensures directory structure exists
// and loads core memory files.
func (s *Service) Start(ctx context.Context) error {
	if err := s.ensureDirectories(); err != nil {
		return err
	}

	s.loadCoreMemory("")

	log.Printf("[Awareness] Service started (core memory: %d chars)", utf8.RuneCountInString(s.CoreMemory()))
	return nil
}

// Shutdown gracefully stops the awareness service.
func (s *Service) Shutdown(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.wg.Wait()
	}()

	select {
	case <-done:
		log.Printf("[Awareness] Service shutdown complete")
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}

// CoreMemory returns the current combined core memory content.
// Thread-safe for concurrent access.
func (s *Service) CoreMemory() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.coreMemory
}

// ensureDirectories creates the awareness directory structure if it doesn't exist.
func (s *Service) ensureDirectories() error {
	dirs := []string{
		s.awarenessDir,
		filepath.Join(s.awarenessDir, "memory"),
		filepath.Join(s.awarenessDir, "memory", "daily"),
		filepath.Join(s.awarenessDir, "memory", "topics"),
		filepath.Join(s.awarenessDir, "memory", "projects"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	return nil
}
