// Package awareness implements the AI awareness system — long-term memory and identity.
// It manages core memory files (SOUL.md, IDENTITY.md, USER.md, GLOBAL.md, PROJECT.md)
// and injects their content into the AI system prompt on every request.
package awareness

import (
	"context"
	"errors"
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
	memoryWriteMu sync.Mutex // serializes all memory file writes (flush, daily, topic)

	exportSub      *eventbus.TypedSubscription[eventbus.SessionExportRequestEvent]
	exportReplySub *eventbus.TypedSubscription[eventbus.ConversationReplyEvent]
	pendingExport  sync.Map // promptID → *exportState
	slugTimeout    time.Duration
	exportWriteMu  sync.Mutex // serializes session export file writes
	coreWriteMu    sync.Mutex // serializes core memory file writes in UpdateCoreMemory

	shuttingDown atomic.Bool

	onboardingMu        sync.Mutex
	onboardingSessionID string // session that claimed onboarding
	onboardingActive    bool   // cached: true while BOOTSTRAP.md exists (avoids I/O under lock)

	lifecycleSub *eventbus.TypedSubscription[eventbus.SessionLifecycleEvent]
	lifecycle    eventbus.ServiceLifecycle
}

// NewService creates a new awareness service for the given instance directory.
func NewService(instanceDir string) *Service {
	return &Service{
		instanceDir:  instanceDir,
		awarenessDir: filepath.Join(instanceDir, "awareness"),
	}
}

// SetEventBus wires the event bus for publish/subscribe.
// Must be called before Start.
func (s *Service) SetEventBus(bus *eventbus.Bus) {
	if s.indexer != nil {
		panic("awareness: SetEventBus called after Start")
	}
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

// SetSlugTimeout overrides the default timeout for session slug generation.
// Must be called before Start.
func (s *Service) SetSlugTimeout(timeout time.Duration) {
	if timeout > 0 {
		s.slugTimeout = timeout
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

	if err := s.scaffoldCoreFiles(); err != nil {
		return fmt.Errorf("awareness: scaffold core files: %w", err)
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
		s.lifecycle.Start(ctx)

		if s.flushTimeout == 0 {
			s.flushTimeout = eventbus.DefaultFlushTimeout
		}

		s.flushSub = eventbus.Subscribe[eventbus.MemoryFlushRequestEvent](s.bus, eventbus.TopicMemoryFlushRequest, eventbus.WithSubscriptionName("awareness_flush_request"))
		s.flushReplySub = eventbus.Subscribe[eventbus.ConversationReplyEvent](s.bus, eventbus.TopicConversationReply, eventbus.WithSubscriptionName("awareness_flush_reply"))
		s.exportSub = eventbus.Subscribe[eventbus.SessionExportRequestEvent](s.bus, eventbus.TopicSessionExportRequest, eventbus.WithSubscriptionName("awareness_export_request"))
		s.exportReplySub = eventbus.Subscribe[eventbus.ConversationReplyEvent](s.bus, eventbus.TopicConversationReply, eventbus.WithSubscriptionName("awareness_export_reply"))
		s.lifecycleSub = eventbus.Subscribe[eventbus.SessionLifecycleEvent](s.bus, eventbus.TopicSessionsLifecycle, eventbus.WithSubscriptionName("awareness_lifecycle"))

		if s.slugTimeout == 0 {
			s.slugTimeout = defaultSlugTimeout
		}

		s.lifecycle.AddSubscriptions(s.flushSub, s.flushReplySub, s.exportSub, s.exportReplySub, s.lifecycleSub)
		s.lifecycle.Go(s.consumeFlushRequests)
		s.lifecycle.Go(s.consumeFlushReplies)
		s.lifecycle.Go(s.consumeExportRequests)
		s.lifecycle.Go(s.consumeExportReplies)
		s.lifecycle.Go(s.consumeLifecycleEvents)
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
	s.lifecycle.Stop()

	// Clear pending flush entries. Timers are fire-and-forget: the
	// shuttingDown flag (set above) makes callbacks no-op.
	s.pendingFlush.Range(func(key, _ any) bool {
		s.pendingFlush.Delete(key)
		return true
	})

	// Clear pending export entries. Timers are fire-and-forget: the
	// shuttingDown flag (set above) makes callbacks no-op.
	s.pendingExport.Range(func(key, _ any) bool {
		s.pendingExport.Delete(key)
		return true
	})

	if err := s.lifecycle.Wait(ctx); err != nil {
		return err
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

// isOnboarding returns true if BOOTSTRAP.md exists in the awareness directory,
// indicating the AI has not yet completed its first-time setup conversation.
//
// NOTE: This performs a raw os.Stat without holding onboardingMu. For concurrent
// use within the service, prefer the cached onboardingActive field (under
// onboardingMu). The intent router uses IsOnboardingSession() (which reads the
// cache) instead.
func (s *Service) isOnboarding() bool {
	_, err := os.Stat(filepath.Join(s.awarenessDir, "BOOTSTRAP.md"))
	return err == nil
}

// BootstrapContent reads and returns the content of BOOTSTRAP.md.
// Returns "" if the file is missing or unreadable (graceful degradation).
// As a side effect, clears the onboardingActive cache when the file is gone,
// handling manual deletion without requiring a daemon restart.
func (s *Service) BootstrapContent() string {
	data, err := os.ReadFile(filepath.Join(s.awarenessDir, "BOOTSTRAP.md"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.onboardingMu.Lock()
			s.onboardingActive = false
			s.onboardingMu.Unlock()
		}
		return ""
	}
	return string(data)
}

// CompleteOnboarding deletes BOOTSTRAP.md and creates a persistent
// .onboarding_done marker so that scaffoldCoreFiles never recreates it.
// Also clears the in-memory session lock.
// Returns (true, nil) if BOOTSTRAP.md was removed, (false, nil) if already done (idempotent).
func (s *Service) CompleteOnboarding() (bool, error) {
	s.onboardingMu.Lock()
	defer s.onboardingMu.Unlock()

	bootstrapRemoved := true
	err := os.Remove(filepath.Join(s.awarenessDir, "BOOTSTRAP.md"))
	if errors.Is(err, os.ErrNotExist) {
		bootstrapRemoved = false
	} else if err != nil {
		return false, fmt.Errorf("remove BOOTSTRAP.md: %w", err)
	}

	markerPath := filepath.Join(s.awarenessDir, ".onboarding_done")
	if err := os.WriteFile(markerPath, []byte("done\n"), 0o600); err != nil {
		return false, fmt.Errorf("write .onboarding_done: %w", err)
	}

	s.onboardingActive = false
	s.onboardingSessionID = ""
	if bootstrapRemoved {
		log.Printf("[Awareness] onboarding complete, BOOTSTRAP.md removed, marker created")
	} else {
		log.Printf("[Awareness] onboarding already complete, marker ensured")
	}
	return bootstrapRemoved, nil
}

// IsOnboardingSession checks whether the given session should enter onboarding.
// The first session to call this while onboarding is active claims the lock;
// subsequent sessions with a different ID are rejected. This prevents multiple
// concurrent sessions from running onboarding simultaneously.
//
// Uses the cached onboardingActive flag (set during Start, cleared by
// CompleteOnboarding). When the cache says active, a stat check confirms
// BOOTSTRAP.md still exists — handling manual deletion without a restart.
func (s *Service) IsOnboardingSession(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	s.onboardingMu.Lock()
	defer s.onboardingMu.Unlock()

	if !s.onboardingActive {
		return false
	}

	// Guard against stale cache: if BOOTSTRAP.md was manually deleted,
	// clear the flag so we stop routing to onboarding.
	if _, err := os.Stat(filepath.Join(s.awarenessDir, "BOOTSTRAP.md")); errors.Is(err, os.ErrNotExist) {
		s.onboardingActive = false
		s.onboardingSessionID = ""
		log.Printf("[Awareness] BOOTSTRAP.md gone (manual deletion?), clearing onboarding state")
		return false
	}

	if s.onboardingSessionID == "" {
		s.onboardingSessionID = sessionID
		log.Printf("[Awareness] onboarding claimed by session %s", sessionID)
		return true
	}
	return s.onboardingSessionID == sessionID
}

// ReleaseOnboardingSession clears the onboarding lock if it is held by the given
// session. This allows another session to claim onboarding when the original
// session disconnects before completing onboarding (AC #4).
func (s *Service) ReleaseOnboardingSession(sessionID string) {
	if sessionID == "" {
		return
	}
	s.onboardingMu.Lock()
	defer s.onboardingMu.Unlock()

	if s.onboardingSessionID == sessionID {
		s.onboardingSessionID = ""
		log.Printf("[Awareness] onboarding lock released for disconnected session %s", sessionID)
	}
}

// consumeLifecycleEvents listens for session stop/detach events and releases
// the onboarding lock if the owning session disconnects mid-onboarding.
func (s *Service) consumeLifecycleEvents(ctx context.Context) {
	eventbus.Consume(ctx, s.lifecycleSub, nil, func(event eventbus.SessionLifecycleEvent) {
		switch event.State {
		case eventbus.SessionStateStopped, eventbus.SessionStateDetached:
			s.ReleaseOnboardingSession(event.SessionID)
		}
	})
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
		filepath.Join(s.awarenessDir, "memory", "sessions"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	// Clean up stale .tmp files from interrupted atomic writes.
	// These can remain if the process crashes between os.WriteFile and os.Rename.
	// Covers both top-level (daily/, topics/, sessions/) and project-scoped
	// (projects/<slug>/daily/, projects/<slug>/topics/) directories.
	cleanTmp := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".tmp") {
				tmpPath := filepath.Join(dir, e.Name())
				if err := os.Remove(tmpPath); err != nil {
					log.Printf("[Awareness] WARNING: failed to clean up stale tmp file %s: %v", tmpPath, err)
				} else {
					log.Printf("[Awareness] Cleaned up stale tmp file: %s", e.Name())
				}
			}
		}
	}

	// Clean awareness root dir (.tmp from core memory atomic writes).
	cleanTmp(s.awarenessDir)

	for _, subdir := range []string{"daily", "topics", "sessions"} {
		cleanTmp(filepath.Join(s.awarenessDir, "memory", subdir))
	}

	// Walk project-scoped directories for stale .tmp files.
	projectsDir := filepath.Join(s.awarenessDir, "memory", "projects")
	projectEntries, err := os.ReadDir(projectsDir)
	if err == nil {
		for _, pe := range projectEntries {
			if pe.IsDir() {
				for _, subdir := range []string{"daily", "topics"} {
					cleanTmp(filepath.Join(projectsDir, pe.Name(), subdir))
				}
			}
		}
	}

	return nil
}
