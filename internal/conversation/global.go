package conversation

import (
	"sort"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

const (
	defaultGlobalMaxTurns = 20
	defaultGlobalTTL      = 10 * time.Minute
)

// GlobalStore maintains conversation history for messages without an assigned session.
// This enables "sessionless" voice commands like asking about available sessions,
// general questions, or system commands that don't target a specific tool.
//
// IMPORTANT: GlobalStore supports periodic TTL-based pruning via Start()/Stop() methods.
// When used within daemon.Daemon, the daemon automatically calls Start() and Stop().
// When used standalone (e.g., in tests or custom integrations), call Start() to enable
// periodic pruning, or rely on manual Prune() calls. Without Start(), expired turns
// are only removed during AddTurn() operations.
type GlobalStore struct {
	mu       sync.RWMutex
	turns    []eventbus.ConversationTurn
	maxTurns int
	ttl      time.Duration

	// Periodic prune goroutine control
	cancel chan struct{}
	wg     sync.WaitGroup
}

// GlobalStoreOption configures the GlobalStore.
type GlobalStoreOption func(*GlobalStore)

// WithGlobalMaxTurns sets the maximum number of turns to keep.
func WithGlobalMaxTurns(max int) GlobalStoreOption {
	return func(g *GlobalStore) {
		if max > 0 {
			g.maxTurns = max
		}
	}
}

// WithGlobalTTL sets the TTL for old turns.
func WithGlobalTTL(ttl time.Duration) GlobalStoreOption {
	return func(g *GlobalStore) {
		if ttl > 0 {
			g.ttl = ttl
		}
	}
}

// NewGlobalStore creates a new global conversation store.
func NewGlobalStore(opts ...GlobalStoreOption) *GlobalStore {
	g := &GlobalStore{
		turns:    make([]eventbus.ConversationTurn, 0),
		maxTurns: defaultGlobalMaxTurns,
		ttl:      defaultGlobalTTL,
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// AddTurn adds a new turn to the global history.
func (g *GlobalStore) AddTurn(turn eventbus.ConversationTurn) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Set timestamp if not provided
	if turn.At.IsZero() {
		turn.At = time.Now().UTC()
	}

	g.turns = append(g.turns, turn)

	// Sort by timestamp
	sort.SliceStable(g.turns, func(i, j int) bool {
		return g.turns[i].At.Before(g.turns[j].At)
	})

	// Prune old entries
	g.pruneLockedUnsafe()
}

// GetContext returns a snapshot of the global conversation history.
func (g *GlobalStore) GetContext() []eventbus.ConversationTurn {
	g.mu.RLock()
	defer g.mu.RUnlock()

	// Return a copy
	result := make([]eventbus.ConversationTurn, len(g.turns))
	copy(result, g.turns)
	return result
}

// Slice returns a paginated view of the global history.
func (g *GlobalStore) Slice(offset, limit int) (int, []eventbus.ConversationTurn) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	total := len(g.turns)

	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}

	end := total
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}

	window := g.turns[offset:end]
	result := make([]eventbus.ConversationTurn, len(window))
	copy(result, window)

	return total, result
}

// Prune removes expired turns based on TTL.
func (g *GlobalStore) Prune() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLockedUnsafe()
}

// pruneLockedUnsafe removes expired and excess turns. Must be called with lock held.
func (g *GlobalStore) pruneLockedUnsafe() {
	now := time.Now().UTC()
	cutoff := now.Add(-g.ttl)

	// Remove expired turns
	validFrom := 0
	for i, turn := range g.turns {
		if turn.At.After(cutoff) {
			validFrom = i
			break
		}
		if i == len(g.turns)-1 {
			// All turns are expired
			validFrom = len(g.turns)
		}
	}

	if validFrom > 0 {
		g.turns = g.turns[validFrom:]
	}

	// Enforce max turns limit
	if len(g.turns) > g.maxTurns {
		g.turns = g.turns[len(g.turns)-g.maxTurns:]
	}
}

// Clear removes all turns from the global history.
func (g *GlobalStore) Clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.turns = make([]eventbus.ConversationTurn, 0)
}

// Len returns the number of turns in the store.
func (g *GlobalStore) Len() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.turns)
}

// LastTurn returns the most recent turn, or nil if empty.
func (g *GlobalStore) LastTurn() *eventbus.ConversationTurn {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if len(g.turns) == 0 {
		return nil
	}

	last := g.turns[len(g.turns)-1]
	return &last
}

// TurnsSince returns all turns after the given timestamp.
func (g *GlobalStore) TurnsSince(since time.Time) []eventbus.ConversationTurn {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var result []eventbus.ConversationTurn
	for _, turn := range g.turns {
		if turn.At.After(since) {
			result = append(result, turn)
		}
	}
	return result
}

// Start begins periodic pruning of expired turns.
// The prune interval is set to half the TTL to ensure timely cleanup.
func (g *GlobalStore) Start() {
	g.mu.Lock()
	if g.cancel != nil {
		g.mu.Unlock()
		return // Already running
	}
	g.cancel = make(chan struct{})
	cancelCh := g.cancel
	g.mu.Unlock()

	// Prune interval is half the TTL (minimum 30 seconds)
	pruneInterval := g.ttl / 2
	if pruneInterval < 30*time.Second {
		pruneInterval = 30 * time.Second
	}

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		ticker := time.NewTicker(pruneInterval)
		defer ticker.Stop()

		for {
			select {
			case <-cancelCh:
				return
			case <-ticker.C:
				g.Prune()
			}
		}
	}()
}

// Stop halts the periodic pruning goroutine.
func (g *GlobalStore) Stop() {
	g.mu.Lock()
	if g.cancel == nil {
		g.mu.Unlock()
		return
	}
	close(g.cancel)
	g.cancel = nil
	g.mu.Unlock()

	g.wg.Wait()
}
