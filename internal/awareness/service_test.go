package awareness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewService(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if svc.instanceDir != dir {
		t.Errorf("instanceDir = %q, want %q", svc.instanceDir, dir)
	}
	if svc.awarenessDir != filepath.Join(dir, "awareness") {
		t.Errorf("awarenessDir = %q, want %q", svc.awarenessDir, filepath.Join(dir, "awareness"))
	}
}

func TestStartCreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer svc.Shutdown(context.Background())

	expected := []string{
		filepath.Join(dir, "awareness"),
		filepath.Join(dir, "awareness", "memory"),
		filepath.Join(dir, "awareness", "memory", "daily"),
		filepath.Join(dir, "awareness", "memory", "topics"),
		filepath.Join(dir, "awareness", "memory", "projects"),
	}

	for _, d := range expected {
		info, err := os.Stat(d)
		if err != nil {
			t.Errorf("directory %s not created: %v", d, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", d)
		}
	}
}

func TestStartWithEmptyAwarenessDir(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer svc.Shutdown(context.Background())

	// No core memory files → empty string
	if got := svc.CoreMemory(); got != "" {
		t.Errorf("CoreMemory() = %q, want empty string", got)
	}
}

func TestShutdownWithTimeout(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := svc.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown() error = %v", err)
	}
}

func TestShutdownWithoutStart(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	// Shutdown before Start should not panic or error
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown() without Start error = %v", err)
	}
}

func TestServiceSearch(t *testing.T) {
	dir := t.TempDir()

	// Create a file in the expected memory directory structure.
	dailyDir := filepath.Join(dir, "awareness", "memory", "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dailyDir, "2026-02-19.md"), []byte("## Test\n\nSearchable content here."), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	results, err := svc.Search(ctx, SearchOptions{Query: "searchable"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected at least 1 result from Service.Search()")
	}
}

func TestServiceDoubleStart(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Second Start should return an error, not leak resources.
	if err := svc.Start(ctx); err == nil {
		t.Fatal("expected error on double Start, got nil")
	}
}

func TestServiceSearchBeforeStart(t *testing.T) {
	svc := NewService(t.TempDir())
	_, err := svc.Search(context.Background(), SearchOptions{Query: "test"})
	if err == nil {
		t.Error("expected error when searching before Start()")
	}
}

func TestServiceStartFailureRecovery(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()

	// Create the awareness/memory directory so ensureDirectories passes,
	// then place a corrupt index.db to force Open to fail.
	memDir := filepath.Join(dir, "awareness", "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a non-empty file that is not a valid SQLite DB. The driver
	// may succeed in sql.Open but fail during schema health check or
	// pragma application; either way, Start should clean up s.indexer.
	dbPath := filepath.Join(memDir, "index.db")
	if err := os.WriteFile(dbPath, []byte("corrupt data here"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First Start may or may not fail (depends on driver behavior with corrupt DB).
	err := svc.Start(ctx)
	if err != nil {
		// Open failed — verify we can retry Start after removing corrupt file.
		os.Remove(dbPath)
		if err := svc.Start(ctx); err != nil {
			t.Fatalf("second Start after removing corrupt DB failed: %v", err)
		}
		svc.Shutdown(ctx)
	} else {
		// Open succeeded (driver tolerated corrupt data, schema recreated).
		svc.Shutdown(ctx)
	}
}

func TestServiceStartShutdownRestart(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()

	// First lifecycle: Start → Shutdown.
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := svc.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}

	// Second lifecycle: Start should succeed after Shutdown cleaned up.
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("second Start after Shutdown: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Verify the service is functional after restart.
	_, err := svc.Search(ctx, SearchOptions{Query: "test"})
	if err != nil {
		t.Fatalf("Search after restart: %v", err)
	}
}
