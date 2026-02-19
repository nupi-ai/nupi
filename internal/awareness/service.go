// Package awareness implements the AI awareness system — long-term memory and identity.
// It manages core memory files (SOUL.md, IDENTITY.md, USER.md, GLOBAL.md, PROJECT.md)
// and injects their content into the AI system prompt on every request.
package awareness

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// Service manages the awareness system: core memory loading, archival memory
// indexing, directory structure, and event bus integration. Implements runtime.Service.
type Service struct {
	instanceDir  string
	awarenessDir string

	bus *eventbus.Bus

	mu         sync.RWMutex
	coreMemory string

	indexer            *Indexer
	embeddingProvider  EmbeddingProvider

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

// SetEmbeddingProvider wires the embedding provider for vector search.
func (s *Service) SetEmbeddingProvider(provider EmbeddingProvider) {
	s.embeddingProvider = provider
}

// Start initializes the awareness service: ensures directory structure exists,
// loads core memory files, and opens the archival memory indexer.
func (s *Service) Start(ctx context.Context) error {
	if s.indexer != nil {
		return fmt.Errorf("awareness: service already started")
	}

	if err := s.ensureDirectories(); err != nil {
		return err
	}

	s.loadCoreMemory("")

	// Open archival memory indexer and sync files.
	memoryDir := filepath.Join(s.awarenessDir, "memory")
	s.indexer = NewIndexer(memoryDir)
	s.indexer.SetEventBus(s.bus)
	s.indexer.SetEmbeddingProvider(s.embeddingProvider)

	if err := s.indexer.Open(ctx); err != nil {
		s.indexer = nil
		return fmt.Errorf("awareness: open indexer: %w", err)
	}

	if err := s.indexer.Sync(ctx); err != nil {
		log.Printf("[Awareness] WARNING: initial index sync failed: %v", err)
	}

	log.Printf("[Awareness] Service started (core memory: %d chars)", utf8.RuneCountInString(s.CoreMemory()))
	return nil
}

// Shutdown gracefully stops the awareness service.
func (s *Service) Shutdown(ctx context.Context) error {
	// Close the indexer before cancelling context / waiting on goroutines.
	if s.indexer != nil {
		if err := s.indexer.Close(); err != nil {
			log.Printf("[Awareness] WARNING: close indexer: %v", err)
		}
		s.indexer = nil
	}

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

// Search performs a keyword search across the archival memory index.
// Delegates to the indexer's FTS5 search.
func (s *Service) Search(ctx context.Context, opts SearchOptions) ([]SearchResult, error) {
	if s.indexer == nil {
		return nil, fmt.Errorf("awareness: indexer not initialized")
	}
	return s.indexer.SearchFTS(ctx, opts)
}

// SearchVector performs a semantic vector similarity search.
// If the embedding provider is nil or unavailable, returns nil, nil (graceful degradation).
// The caller should fall back to FTS-only search.
func (s *Service) SearchVector(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error) {
	if s.embeddingProvider == nil {
		return nil, nil
	}
	if s.indexer == nil {
		return nil, nil
	}

	// Generate query embedding.
	result, err := s.embeddingProvider.GenerateEmbeddings(ctx, []string{query})
	if err != nil {
		// Graceful degradation (NFR33): embedding failure is not a search error.
		log.Printf("[Awareness] WARNING: vector search embedding failed, falling back to FTS-only: %v", err)
		return nil, nil
	}
	if len(result.Vectors) == 0 || len(result.Vectors[0]) == 0 {
		return nil, nil
	}

	return s.indexer.SearchVector(ctx, result.Vectors[0], opts)
}

// HasEmbeddings returns true if the embedding provider is set and embeddings exist in the database.
func (s *Service) HasEmbeddings() bool {
	if s.embeddingProvider == nil || s.indexer == nil || s.indexer.db == nil {
		return false
	}
	var exists int
	row := s.indexer.db.QueryRow("SELECT 1 FROM memory_embeddings LIMIT 1")
	if err := row.Scan(&exists); err != nil {
		return false
	}
	return true
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
