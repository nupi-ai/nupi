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
	"strings"
	"sync"
	"sync/atomic"
	"time"
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

	indexer           *Indexer
	embeddingProvider EmbeddingProvider

	flushSub      *eventbus.TypedSubscription[eventbus.MemoryFlushRequestEvent]
	flushReplySub *eventbus.TypedSubscription[eventbus.ConversationReplyEvent]
	pendingFlush  sync.Map // promptID → *flushState
	flushTimeout  time.Duration
	flushWriteMu  sync.Mutex // serializes daily file writes in writeFlushContent
	shuttingDown  atomic.Bool

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

// SetFlushTimeout overrides the default timeout for memory flush operations.
// Must be called before Start.
func (s *Service) SetFlushTimeout(timeout time.Duration) {
	if timeout > 0 {
		s.flushTimeout = timeout
	}
}

// Start initializes the awareness service: ensures directory structure exists,
// loads core memory files, and opens the archival memory indexer.
func (s *Service) Start(ctx context.Context) error {
	if s.indexer != nil {
		return fmt.Errorf("awareness: service already started")
	}

	// Reset shutdown flag so timer callbacks work correctly after a
	// Shutdown → Start restart cycle.
	s.shuttingDown.Store(false)

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

	// Start flush consumer goroutines if event bus is available.
	if s.bus != nil {
		if s.flushTimeout == 0 {
			s.flushTimeout = eventbus.DefaultFlushTimeout
		}

		s.flushSub = eventbus.Subscribe[eventbus.MemoryFlushRequestEvent](s.bus, eventbus.TopicMemoryFlushRequest, eventbus.WithSubscriptionName("awareness_flush_request"))
		s.flushReplySub = eventbus.Subscribe[eventbus.ConversationReplyEvent](s.bus, eventbus.TopicConversationReply, eventbus.WithSubscriptionName("awareness_flush_reply"))

		var derivedCtx context.Context
		derivedCtx, s.cancel = context.WithCancel(ctx)

		s.wg.Add(2)
		go s.consumeFlushRequests(derivedCtx)
		go s.consumeFlushReplies(derivedCtx)
	}

	log.Printf("[Awareness] Service started (core memory: %d chars)", utf8.RuneCountInString(s.CoreMemory()))
	return nil
}

// Shutdown gracefully stops the awareness service.
func (s *Service) Shutdown(ctx context.Context) error {
	// Signal timer callbacks to bail out early, preventing log noise
	// and publishes to a draining bus after Shutdown returns.
	s.shuttingDown.Store(true)

	// Close flush subscriptions first to unblock consumer goroutines.
	// This must happen BEFORE closing the indexer so that in-flight
	// handleFlushReply calls (which may call writeFlushContent → indexer.Sync)
	// finish before the indexer is closed.
	if s.flushSub != nil {
		s.flushSub.Close()
	}
	if s.flushReplySub != nil {
		s.flushReplySub.Close()
	}

	// Cancel pending flush timers.
	s.pendingFlush.Range(func(key, val any) bool {
		if state, ok := val.(*flushState); ok && state.timer != nil {
			state.timer.Stop()
		}
		s.pendingFlush.Delete(key)
		return true
	})

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
	case <-ctx.Done():
		return ctx.Err()
	}

	// Close the indexer after goroutines have stopped to prevent
	// data races on s.indexer between writeFlushContent and Shutdown.
	if s.indexer != nil {
		if err := s.indexer.Close(); err != nil {
			log.Printf("[Awareness] WARNING: close indexer: %v", err)
		}
		s.indexer = nil
	}

	log.Printf("[Awareness] Service shutdown complete")
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

// SearchHybrid performs a combined FTS5 + vector search with score normalization,
// temporal decay, and MMR diversity reranking. Falls back to FTS-only when the
// embedding provider is unavailable (NFR33 graceful degradation).
func (s *Service) SearchHybrid(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error) {
	if s.indexer == nil {
		return nil, fmt.Errorf("awareness: indexer not initialized")
	}

	if opts.MaxResults <= 0 {
		opts.MaxResults = 5
	}

	// Default opts.Query to the query parameter so FTS doesn't silently return nothing.
	if opts.Query == "" {
		opts.Query = query
	}

	// Attempt to generate query embedding for vector component.
	var queryVec []float32
	if s.embeddingProvider != nil {
		result, err := s.embeddingProvider.GenerateEmbeddings(ctx, []string{query})
		if err != nil {
			log.Printf("[Awareness] WARNING: hybrid search embedding failed, using FTS-only: %v", err)
		} else if len(result.Vectors) > 0 && len(result.Vectors[0]) > 0 {
			queryVec = result.Vectors[0]
		}
	}

	return s.indexer.SearchHybrid(ctx, queryVec, opts)
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

// ensureDirectories creates the awareness directory structure if it doesn't exist,
// and cleans up stale .tmp files left behind by incomplete atomic writes.
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

	// Clean up stale .tmp files from interrupted atomic writes in writeFlushContent.
	// These can remain if the process crashes between os.WriteFile and os.Rename.
	dailyDir := filepath.Join(s.awarenessDir, "memory", "daily")
	entries, err := os.ReadDir(dailyDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".tmp") {
				tmpPath := filepath.Join(dailyDir, e.Name())
				if err := os.Remove(tmpPath); err != nil {
					log.Printf("[Awareness] WARNING: failed to clean up stale tmp file %s: %v", tmpPath, err)
				} else {
					log.Printf("[Awareness] Cleaned up stale tmp file: %s", e.Name())
				}
			}
		}
	}

	return nil
}
