package conversation

import (
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

func TestNewGlobalStore(t *testing.T) {
	g := NewGlobalStore()
	if g == nil {
		t.Fatal("NewGlobalStore returned nil")
	}
	if g.maxTurns != defaultGlobalMaxTurns {
		t.Errorf("expected maxTurns=%d, got %d", defaultGlobalMaxTurns, g.maxTurns)
	}
	if g.ttl != defaultGlobalTTL {
		t.Errorf("expected ttl=%v, got %v", defaultGlobalTTL, g.ttl)
	}
}

func TestNewGlobalStore_WithOptions(t *testing.T) {
	g := NewGlobalStore(
		WithGlobalMaxTurns(5),
		WithGlobalTTL(1*time.Minute),
	)

	if g.maxTurns != 5 {
		t.Errorf("expected maxTurns=5, got %d", g.maxTurns)
	}
	if g.ttl != 1*time.Minute {
		t.Errorf("expected ttl=1m, got %v", g.ttl)
	}
}

func TestGlobalStore_AddTurn(t *testing.T) {
	g := NewGlobalStore()

	turn := eventbus.ConversationTurn{
		Origin: eventbus.OriginUser,
		Text:   "hello",
		At:     time.Now(),
	}

	g.AddTurn(turn)

	if g.Len() != 1 {
		t.Errorf("expected 1 turn, got %d", g.Len())
	}
}

func TestGlobalStore_AddTurn_AutoTimestamp(t *testing.T) {
	g := NewGlobalStore()

	turn := eventbus.ConversationTurn{
		Origin: eventbus.OriginUser,
		Text:   "hello",
		// At is zero
	}

	before := time.Now()
	g.AddTurn(turn)
	after := time.Now()

	ctx := g.GetContext()
	if len(ctx) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(ctx))
	}

	if ctx[0].At.Before(before) || ctx[0].At.After(after) {
		t.Error("timestamp should be set automatically")
	}
}

func TestGlobalStore_GetContext(t *testing.T) {
	g := NewGlobalStore()

	// Add turns
	g.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginUser, Text: "one", At: time.Now()})
	g.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginAI, Text: "two", At: time.Now()})
	g.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginUser, Text: "three", At: time.Now()})

	ctx := g.GetContext()
	if len(ctx) != 3 {
		t.Errorf("expected 3 turns, got %d", len(ctx))
	}

	// Verify it's a copy (modifying shouldn't affect store)
	ctx[0].Text = "modified"
	original := g.GetContext()
	if original[0].Text == "modified" {
		t.Error("GetContext should return a copy")
	}
}

func TestGlobalStore_Slice(t *testing.T) {
	g := NewGlobalStore()

	// Add 5 turns
	for i := 0; i < 5; i++ {
		g.AddTurn(eventbus.ConversationTurn{
			Origin: eventbus.OriginUser,
			Text:   string(rune('a' + i)),
			At:     time.Now().Add(time.Duration(i) * time.Second),
		})
	}

	// Test full slice
	total, slice := g.Slice(0, 0)
	if total != 5 {
		t.Errorf("expected total=5, got %d", total)
	}
	if len(slice) != 5 {
		t.Errorf("expected 5 items, got %d", len(slice))
	}

	// Test with offset and limit
	_, slice = g.Slice(1, 2)
	if len(slice) != 2 {
		t.Errorf("expected 2 items, got %d", len(slice))
	}
	if slice[0].Text != "b" {
		t.Errorf("expected text='b', got '%s'", slice[0].Text)
	}

	// Test offset beyond length
	_, slice = g.Slice(10, 5)
	if len(slice) != 0 {
		t.Errorf("expected 0 items for offset beyond length, got %d", len(slice))
	}
}

func TestGlobalStore_MaxTurns(t *testing.T) {
	g := NewGlobalStore(WithGlobalMaxTurns(3))

	// Add 5 turns
	for i := 0; i < 5; i++ {
		g.AddTurn(eventbus.ConversationTurn{
			Origin: eventbus.OriginUser,
			Text:   string(rune('a' + i)),
			At:     time.Now().Add(time.Duration(i) * time.Second),
		})
	}

	if g.Len() != 3 {
		t.Errorf("expected 3 turns (max), got %d", g.Len())
	}

	// Should keep the last 3
	ctx := g.GetContext()
	if ctx[0].Text != "c" {
		t.Errorf("expected first text='c', got '%s'", ctx[0].Text)
	}
}

func TestGlobalStore_TTL(t *testing.T) {
	g := NewGlobalStore(WithGlobalTTL(100 * time.Millisecond))

	// Add an old turn
	g.AddTurn(eventbus.ConversationTurn{
		Origin: eventbus.OriginUser,
		Text:   "old",
		At:     time.Now().Add(-200 * time.Millisecond),
	})

	// Add a new turn
	g.AddTurn(eventbus.ConversationTurn{
		Origin: eventbus.OriginUser,
		Text:   "new",
		At:     time.Now(),
	})

	// The old turn should be pruned
	if g.Len() != 1 {
		t.Errorf("expected 1 turn after TTL pruning, got %d", g.Len())
	}

	ctx := g.GetContext()
	if ctx[0].Text != "new" {
		t.Errorf("expected text='new', got '%s'", ctx[0].Text)
	}
}

func TestGlobalStore_Clear(t *testing.T) {
	g := NewGlobalStore()

	g.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginUser, Text: "test", At: time.Now()})
	g.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginAI, Text: "response", At: time.Now()})

	if g.Len() != 2 {
		t.Errorf("expected 2 turns, got %d", g.Len())
	}

	g.Clear()

	if g.Len() != 0 {
		t.Errorf("expected 0 turns after clear, got %d", g.Len())
	}
}

func TestGlobalStore_LastTurn(t *testing.T) {
	g := NewGlobalStore()

	// Empty store
	if g.LastTurn() != nil {
		t.Error("expected nil for empty store")
	}

	// Add turns
	g.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginUser, Text: "first", At: time.Now()})
	g.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginAI, Text: "second", At: time.Now().Add(time.Second)})

	last := g.LastTurn()
	if last == nil {
		t.Fatal("expected non-nil last turn")
	}
	if last.Text != "second" {
		t.Errorf("expected text='second', got '%s'", last.Text)
	}
}

func TestGlobalStore_TurnsSince(t *testing.T) {
	g := NewGlobalStore()

	now := time.Now()
	g.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginUser, Text: "old", At: now.Add(-5 * time.Second)})
	g.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginUser, Text: "recent", At: now.Add(-1 * time.Second)})
	g.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginUser, Text: "newest", At: now})

	// Get turns from last 2 seconds
	since := now.Add(-2 * time.Second)
	turns := g.TurnsSince(since)

	if len(turns) != 2 {
		t.Errorf("expected 2 turns, got %d", len(turns))
	}
}

func TestGlobalStore_Prune(t *testing.T) {
	g := NewGlobalStore(WithGlobalTTL(50 * time.Millisecond))

	// Add a turn
	g.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginUser, Text: "test", At: time.Now()})

	if g.Len() != 1 {
		t.Errorf("expected 1 turn, got %d", g.Len())
	}

	// Wait for TTL to expire
	time.Sleep(100 * time.Millisecond)

	// Explicitly prune
	g.Prune()

	if g.Len() != 0 {
		t.Errorf("expected 0 turns after prune, got %d", g.Len())
	}
}

func TestGlobalStore_SortByTimestamp(t *testing.T) {
	g := NewGlobalStore()

	now := time.Now()
	// Add out of order
	g.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginUser, Text: "third", At: now.Add(2 * time.Second)})
	g.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginUser, Text: "first", At: now})
	g.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginUser, Text: "second", At: now.Add(1 * time.Second)})

	ctx := g.GetContext()
	if ctx[0].Text != "first" {
		t.Errorf("expected first text='first', got '%s'", ctx[0].Text)
	}
	if ctx[1].Text != "second" {
		t.Errorf("expected second text='second', got '%s'", ctx[1].Text)
	}
	if ctx[2].Text != "third" {
		t.Errorf("expected third text='third', got '%s'", ctx[2].Text)
	}
}
