package store

import (
	"strings"
	"testing"
	"time"
)

func TestInsertHeartbeat(t *testing.T) {
	s, ctx := openTestStore(t)

	if err := s.InsertHeartbeat(ctx, "daily-summary", "0 9 * * *", "Summarize today", 0); err != nil {
		t.Fatal(err)
	}

	heartbeats, err := s.ListHeartbeats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(heartbeats) != 1 {
		t.Fatalf("expected 1 heartbeat, got %d", len(heartbeats))
	}
	h := heartbeats[0]
	if h.Name != "daily-summary" {
		t.Fatalf("expected name 'daily-summary', got %q", h.Name)
	}
	if h.CronExpr != "0 9 * * *" {
		t.Fatalf("expected cron '0 9 * * *', got %q", h.CronExpr)
	}
	if h.Prompt != "Summarize today" {
		t.Fatalf("expected prompt 'Summarize today', got %q", h.Prompt)
	}
	if h.LastRunAt != nil {
		t.Fatalf("expected nil LastRunAt, got %v", h.LastRunAt)
	}
	if h.CreatedAt.IsZero() {
		t.Fatal("expected non-zero CreatedAt")
	}
}

func TestInsertHeartbeatDuplicateName(t *testing.T) {
	s, ctx := openTestStore(t)

	if err := s.InsertHeartbeat(ctx, "dup", "* * * * *", "first", 0); err != nil {
		t.Fatal(err)
	}
	err := s.InsertHeartbeat(ctx, "dup", "0 * * * *", "second", 0)
	if err == nil {
		t.Fatal("expected error on duplicate name")
	}
	if !IsDuplicate(err) {
		t.Fatalf("expected DuplicateError, got: %v", err)
	}
}

func TestDeleteHeartbeat(t *testing.T) {
	s, ctx := openTestStore(t)

	if err := s.InsertHeartbeat(ctx, "to-delete", "* * * * *", "delete me", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteHeartbeat(ctx, "to-delete"); err != nil {
		t.Fatal(err)
	}

	heartbeats, err := s.ListHeartbeats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(heartbeats) != 0 {
		t.Fatalf("expected 0 heartbeats after delete, got %d", len(heartbeats))
	}
}

func TestDeleteHeartbeatNotFound(t *testing.T) {
	s, ctx := openTestStore(t)

	err := s.DeleteHeartbeat(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for deleting nonexistent heartbeat")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got: %v", err)
	}
}

func TestListHeartbeatsEmpty(t *testing.T) {
	s, ctx := openTestStore(t)

	heartbeats, err := s.ListHeartbeats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(heartbeats) != 0 {
		t.Fatalf("expected 0 heartbeats, got %d", len(heartbeats))
	}
}

func TestListHeartbeatsMultiple(t *testing.T) {
	s, ctx := openTestStore(t)

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := s.InsertHeartbeat(ctx, name, "* * * * *", "prompt-"+name, 0); err != nil {
			t.Fatal(err)
		}
	}

	heartbeats, err := s.ListHeartbeats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(heartbeats) != 3 {
		t.Fatalf("expected 3 heartbeats, got %d", len(heartbeats))
	}

	// Verify ordered by id (insertion order)
	for i, name := range []string{"alpha", "beta", "gamma"} {
		if heartbeats[i].Name != name {
			t.Fatalf("heartbeat[%d] expected name %q, got %q", i, name, heartbeats[i].Name)
		}
	}
}

func TestUpdateHeartbeatLastRun(t *testing.T) {
	s, ctx := openTestStore(t)

	if err := s.InsertHeartbeat(ctx, "test-run", "0 * * * *", "run test", 0); err != nil {
		t.Fatal(err)
	}

	heartbeats, err := s.ListHeartbeats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(heartbeats) != 1 {
		t.Fatal("expected 1 heartbeat")
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.UpdateHeartbeatLastRun(ctx, heartbeats[0].ID, now); err != nil {
		t.Fatal(err)
	}

	heartbeats, err = s.ListHeartbeats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if heartbeats[0].LastRunAt == nil {
		t.Fatal("expected non-nil LastRunAt after update")
	}

	diff := heartbeats[0].LastRunAt.Sub(now)
	if diff < -time.Second || diff > time.Second {
		t.Fatalf("LastRunAt %v differs from expected %v by %v", heartbeats[0].LastRunAt, now, diff)
	}
}

func TestInsertHeartbeatMaxCountEnforced(t *testing.T) {
	s, ctx := openTestStore(t)

	// Insert 2 heartbeats with maxCount=2 — should succeed.
	if err := s.InsertHeartbeat(ctx, "hb-1", "* * * * *", "p1", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertHeartbeat(ctx, "hb-2", "* * * * *", "p2", 2); err != nil {
		t.Fatal(err)
	}

	// Third insert with maxCount=2 — should fail.
	err := s.InsertHeartbeat(ctx, "hb-3", "* * * * *", "p3", 2)
	if err == nil {
		t.Fatal("expected error when max count reached")
	}
	if !strings.Contains(err.Error(), "maximum number") {
		t.Fatalf("expected max count error, got: %v", err)
	}
}

func TestUpdateHeartbeatLastRunNotFound(t *testing.T) {
	s, ctx := openTestStore(t)

	err := s.UpdateHeartbeatLastRun(ctx, 99999, time.Now())
	if err == nil {
		t.Fatal("expected error for non-existent heartbeat ID")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got: %v", err)
	}
}
