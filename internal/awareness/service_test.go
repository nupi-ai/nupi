package awareness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
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
		filepath.Join(dir, "awareness", "memory", "sessions"),
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
	if err := os.WriteFile(filepath.Join(dailyDir, "2026-02-19.md"), []byte("## Test\n\nSearchable content here."), 0o600); err != nil {
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
	if err := os.WriteFile(dbPath, []byte("corrupt data here"), 0o600); err != nil {
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

func TestServiceSearchVectorWithProvider(t *testing.T) {
	dir := t.TempDir()

	// Create a file in the expected memory directory structure.
	dailyDir := filepath.Join(dir, "awareness", "memory", "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dailyDir, "2026-02-19.md"), []byte("## Test\n\nVector searchable content here."), 0o600); err != nil {
		t.Fatal(err)
	}

	mock := &mockEmbeddingProvider{dims: 3}
	svc := NewService(dir)
	svc.SetEmbeddingProvider(mock)
	ctx := context.Background()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	results, err := svc.SearchVector(ctx, "vector searchable", SearchOptions{})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected at least 1 result from Service.SearchVector()")
	}
}

func TestServiceSearchVectorWithoutProvider(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Without embedding provider, should return nil, nil (graceful degradation).
	results, err := svc.SearchVector(ctx, "test query", SearchOptions{})
	if err != nil {
		t.Fatalf("SearchVector without provider should not error: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results without provider, got %d results", len(results))
	}
}

func TestServiceSearchVectorBeforeStart(t *testing.T) {
	svc := NewService(t.TempDir())
	mock := &mockEmbeddingProvider{dims: 3}
	svc.SetEmbeddingProvider(mock)

	// Without Start, indexer is nil — should return nil, nil.
	results, err := svc.SearchVector(context.Background(), "test", SearchOptions{})
	if err != nil {
		t.Fatalf("SearchVector before Start should not error: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results before Start, got %d results", len(results))
	}
}

func TestServiceHasEmbeddingsWithProvider(t *testing.T) {
	dir := t.TempDir()

	dailyDir := filepath.Join(dir, "awareness", "memory", "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dailyDir, "test.md"), []byte("## Test\n\nContent."), 0o600); err != nil {
		t.Fatal(err)
	}

	mock := &mockEmbeddingProvider{dims: 3}
	svc := NewService(dir)
	svc.SetEmbeddingProvider(mock)
	ctx := context.Background()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	if !svc.HasEmbeddings() {
		t.Error("expected HasEmbeddings() to return true after Sync with provider")
	}
}

func TestServiceHasEmbeddingsWithoutProvider(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	if svc.HasEmbeddings() {
		t.Error("expected HasEmbeddings() to return false without provider")
	}
}

func TestServiceHasEmbeddingsBeforeStart(t *testing.T) {
	svc := NewService(t.TempDir())
	if svc.HasEmbeddings() {
		t.Error("expected HasEmbeddings() to return false before Start")
	}
}

func TestServiceSearchVectorProviderError(t *testing.T) {
	dir := t.TempDir()

	dailyDir := filepath.Join(dir, "awareness", "memory", "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dailyDir, "test.md"), []byte("## Test\n\nContent."), 0o600); err != nil {
		t.Fatal(err)
	}

	// Provider that fails on GenerateEmbeddings calls.
	mock := &mockEmbeddingProvider{err: fmt.Errorf("provider unavailable")}
	svc := NewService(dir)
	svc.SetEmbeddingProvider(mock)
	ctx := context.Background()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// SearchVector should gracefully degrade: nil results, no error (NFR33).
	results, err := svc.SearchVector(ctx, "test query", SearchOptions{})
	if err != nil {
		t.Fatalf("SearchVector with failing provider should not error: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results when provider fails, got %d results", len(results))
	}
}

// --- Service SearchHybrid tests (Tasks 6.11-6.14) ---

func TestServiceSearchHybrid(t *testing.T) {
	dir := t.TempDir()

	dailyDir := filepath.Join(dir, "awareness", "memory", "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dailyDir, "2026-02-19.md"), []byte("## Test\n\nHybrid searchable content about databases."), 0o600); err != nil {
		t.Fatal(err)
	}

	mock := &mockEmbeddingProvider{dims: 3}
	svc := NewService(dir)
	svc.SetEmbeddingProvider(mock)
	ctx := context.Background()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	results, err := svc.SearchHybrid(ctx, "databases", SearchOptions{Query: "databases"})
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected at least 1 result from Service.SearchHybrid()")
	}
	for i, r := range results {
		if r.Score < 0 || r.Score > 1.0 {
			t.Errorf("result %d score %f not in [0, 1]", i, r.Score)
		}
	}
}

func TestServiceSearchHybridFTSFallback(t *testing.T) {
	dir := t.TempDir()

	dailyDir := filepath.Join(dir, "awareness", "memory", "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dailyDir, "2026-02-19.md"), []byte("## Test\n\nFTS fallback content."), 0o600); err != nil {
		t.Fatal(err)
	}

	// No embedding provider → FTS-only fallback (NFR33).
	svc := NewService(dir)
	ctx := context.Background()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	results, err := svc.SearchHybrid(ctx, "fallback", SearchOptions{Query: "fallback"})
	if err != nil {
		t.Fatalf("SearchHybrid FTS fallback: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected at least 1 FTS-only result from SearchHybrid")
	}
}

func TestServiceSearchHybridProviderError(t *testing.T) {
	dir := t.TempDir()

	dailyDir := filepath.Join(dir, "awareness", "memory", "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dailyDir, "2026-02-19.md"), []byte("## Test\n\nProvider error content."), 0o600); err != nil {
		t.Fatal(err)
	}

	mock := &mockEmbeddingProvider{err: fmt.Errorf("provider unavailable")}
	svc := NewService(dir)
	svc.SetEmbeddingProvider(mock)
	ctx := context.Background()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Should gracefully fall back to FTS-only (NFR33).
	results, err := svc.SearchHybrid(ctx, "provider error", SearchOptions{Query: "provider error"})
	if err != nil {
		t.Fatalf("SearchHybrid with failing provider should not error: %v", err)
	}
	// Should still return FTS results even though embedding failed.
	if len(results) == 0 {
		t.Error("expected FTS fallback results when provider fails")
	}
}

func TestServiceSearchHybridBeforeStart(t *testing.T) {
	svc := NewService(t.TempDir())
	_, err := svc.SearchHybrid(context.Background(), "test", SearchOptions{Query: "test"})
	if err == nil {
		t.Error("expected error when calling SearchHybrid before Start()")
	}
}

func TestServiceSearchHybridQueryDefaulting(t *testing.T) {
	dir := t.TempDir()

	dailyDir := filepath.Join(dir, "awareness", "memory", "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dailyDir, "2026-02-19.md"), []byte("## Test\n\nSearchable query defaulting content."), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := NewService(dir)
	ctx := context.Background()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Call without opts.Query — should default to the query parameter for FTS.
	results, err := svc.SearchHybrid(ctx, "searchable", SearchOptions{})
	if err != nil {
		t.Fatalf("SearchHybrid without opts.Query: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected results when opts.Query is empty but query param is set")
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

func TestWriteFlushContentNewFile(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	err := svc.writeFlushContent(ctx, "sess-1", "Important decision: use gRPC")
	if err != nil {
		t.Fatalf("writeFlushContent: %v", err)
	}

	date := time.Now().UTC().Format("2006-01-02")
	dailyFile := filepath.Join(dir, "awareness", "memory", "daily", date+".md")
	content, err := os.ReadFile(dailyFile)
	if err != nil {
		t.Fatalf("read daily file: %v", err)
	}

	expected := fmt.Sprintf("# Daily Log %s\n\nImportant decision: use gRPC", date)
	if string(content) != expected {
		t.Fatalf("unexpected content:\ngot:  %q\nwant: %q", string(content), expected)
	}
}

func TestWriteFlushContentAppend(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Write first entry
	if err := svc.writeFlushContent(ctx, "sess-1", "First entry"); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Write second entry (append)
	if err := svc.writeFlushContent(ctx, "sess-1", "Second entry"); err != nil {
		t.Fatalf("second write: %v", err)
	}

	date := time.Now().UTC().Format("2006-01-02")
	dailyFile := filepath.Join(dir, "awareness", "memory", "daily", date+".md")
	content, err := os.ReadFile(dailyFile)
	if err != nil {
		t.Fatalf("read daily file: %v", err)
	}

	if !strings.Contains(string(content), "First entry") {
		t.Fatal("expected first entry in file")
	}
	if !strings.Contains(string(content), "---") {
		t.Fatal("expected separator between entries")
	}
	if !strings.Contains(string(content), "Second entry") {
		t.Fatal("expected second entry in file")
	}
}

func TestWriteFlushContentNoTempFileLeak(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Write multiple entries to exercise both new-file and append paths.
	if err := svc.writeFlushContent(ctx, "sess-1", "First entry"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := svc.writeFlushContent(ctx, "sess-1", "Second entry"); err != nil {
		t.Fatalf("second write: %v", err)
	}

	// Verify no .tmp files remain in the daily directory.
	dailyDir := filepath.Join(dir, "awareness", "memory", "daily")
	entries, err := os.ReadDir(dailyDir)
	if err != nil {
		t.Fatalf("read daily dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

// TestWriteFlushContentConcurrent verifies that memoryWriteMu correctly
// serializes concurrent writes from multiple goroutines. All entries must
// appear in the daily file without corruption or data loss.
func TestWriteFlushContentConcurrent(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	const n = 10
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		go func(idx int) {
			errs <- svc.writeFlushContent(ctx, fmt.Sprintf("sess-%d", idx),
				fmt.Sprintf("concurrent entry %d", idx))
		}(i)
	}

	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("writeFlushContent goroutine error: %v", err)
		}
	}

	date := time.Now().UTC().Format("2006-01-02")
	dailyFile := filepath.Join(dir, "awareness", "memory", "daily", date+".md")
	content, err := os.ReadFile(dailyFile)
	if err != nil {
		t.Fatalf("read daily file: %v", err)
	}

	for i := 0; i < n; i++ {
		expected := fmt.Sprintf("concurrent entry %d", i)
		if !strings.Contains(string(content), expected) {
			t.Fatalf("missing entry %d in daily file", i)
		}
	}

	// Verify no .tmp files leaked.
	dailyDir := filepath.Join(dir, "awareness", "memory", "daily")
	entries, err := os.ReadDir(dailyDir)
	if err != nil {
		t.Fatalf("read daily dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

// TestWriteFlushContentTruncatesOversized verifies that writeFlushContent truncates
// content exceeding maxFlushContentBytes and appends a truncation notice.
func TestWriteFlushContentTruncatesOversized(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Create content larger than maxFlushContentBytes (10KB).
	oversized := strings.Repeat("x", maxFlushContentBytes+500)

	if err := svc.writeFlushContent(ctx, "sess-oversized", oversized); err != nil {
		t.Fatalf("writeFlushContent: %v", err)
	}

	date := time.Now().UTC().Format("2006-01-02")
	dailyFile := filepath.Join(dir, "awareness", "memory", "daily", date+".md")
	content, err := os.ReadFile(dailyFile)
	if err != nil {
		t.Fatalf("read daily file: %v", err)
	}

	if !strings.Contains(string(content), "[truncated") {
		t.Fatal("expected truncation notice in file")
	}

	// File should be smaller than oversized input + header.
	// maxFlushContentBytes + truncation notice + header ≈ maxFlushContentBytes + ~100.
	maxExpected := maxFlushContentBytes + 200
	if len(content) > maxExpected {
		t.Fatalf("file too large: %d bytes (expected < %d)", len(content), maxExpected)
	}
}

// TestWriteFlushContentTruncatesUTF8Safe verifies that truncation does not split
// multi-byte UTF-8 characters at the boundary. A 4-byte emoji placed right at the
// boundary must be fully removed rather than partially included.
func TestWriteFlushContentTruncatesUTF8Safe(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Fill content to exactly maxFlushContentBytes-2 with ASCII, then add a
	// 4-byte emoji (🔥). Total = maxFlushContentBytes+2, triggering truncation.
	// The naive content[:maxFlushContentBytes] would cut the emoji in half.
	prefix := strings.Repeat("a", maxFlushContentBytes-2)
	oversized := prefix + "🔥" // 4-byte emoji → total = maxFlushContentBytes+2

	if err := svc.writeFlushContent(ctx, "sess-utf8", oversized); err != nil {
		t.Fatalf("writeFlushContent: %v", err)
	}

	date := time.Now().UTC().Format("2006-01-02")
	dailyFile := filepath.Join(dir, "awareness", "memory", "daily", date+".md")
	content, err := os.ReadFile(dailyFile)
	if err != nil {
		t.Fatalf("read daily file: %v", err)
	}

	contentStr := string(content)

	// The truncated content written to the file must be valid UTF-8.
	if !utf8.ValidString(contentStr) {
		t.Fatal("daily file contains invalid UTF-8 after truncation")
	}

	if !strings.Contains(contentStr, "[truncated") {
		t.Fatal("expected truncation notice in file")
	}
}

// TestEnsureDirectoriesCleansStaleTemp verifies that stale .tmp files left
// by interrupted atomic writes are cleaned up on service start.
func TestEnsureDirectoriesCleansStaleTemp(t *testing.T) {
	dir := t.TempDir()

	// Pre-create the daily directory with a stale .tmp file.
	dailyDir := filepath.Join(dir, "awareness", "memory", "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	staleTmp := filepath.Join(dailyDir, "2026-02-19.md.tmp")
	if err := os.WriteFile(staleTmp, []byte("partial write"), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Verify the stale .tmp file was cleaned up.
	if _, err := os.Stat(staleTmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stale .tmp file to be removed, got err: %v", err)
	}
}

// --- Session Export Tests ---

func TestParseSlugResponse(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantSlug    string
		wantSummary string
	}{
		{
			name:        "structured format",
			input:       "SLUG: docker-setup\n\nSUMMARY:\nUser configured Docker for the project.",
			wantSlug:    "docker-setup",
			wantSummary: "User configured Docker for the project.",
		},
		{
			name:        "slug only no summary",
			input:       "SLUG: quick-fix",
			wantSlug:    "quick-fix",
			wantSummary: "",
		},
		{
			name:        "multiline summary",
			input:       "SLUG: big-refactor\n\nSUMMARY:\nRefactored the auth module.\nAdded JWT support.\nRemoved legacy sessions.",
			wantSlug:    "big-refactor",
			wantSummary: "Refactored the auth module.\nAdded JWT support.\nRemoved legacy sessions.",
		},
		{
			name:        "inline summary",
			input:       "SLUG: inline-test\n\nSUMMARY: Short inline summary.",
			wantSlug:    "inline-test",
			wantSummary: "Short inline summary.",
		},
		{
			name:        "fallback first 3 words",
			input:       "some random response without structure",
			wantSlug:    "some-random-response",
			wantSummary: "",
		},
		{
			name:        "fallback fewer than 3 words",
			input:       "hello",
			wantSlug:    "hello",
			wantSummary: "",
		},
		{
			name:        "case insensitive slug prefix",
			input:       "slug: my-session\n\nsummary:\nDone.",
			wantSlug:    "my-session",
			wantSummary: "Done.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slug, summary := parseSlugResponse(tt.input)
			if slug != tt.wantSlug {
				t.Errorf("slug = %q, want %q", slug, tt.wantSlug)
			}
			if summary != tt.wantSummary {
				t.Errorf("summary = %q, want %q", summary, tt.wantSummary)
			}
		})
	}
}

func TestSanitizeSlug(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"lowercase", "Docker-Setup", "docker-setup"},
		{"spaces to hyphens", "hello world foo", "hello-world-foo"},
		{"underscores to hyphens", "foo_bar_baz", "foo-bar-baz"},
		{"special chars removed", "hello!@#$world", "helloworld"},
		{"multiple hyphens collapsed", "a---b--c", "a-b-c"},
		{"trim hyphens", "-leading-trailing-", "leading-trailing"},
		{"max length 50", strings.Repeat("a", 60), strings.Repeat("a", 50)},
		{"max length trims trailing hyphen", strings.Repeat("a", 49) + "-b", strings.Repeat("a", 49)},
		{"empty input", "", ""},
		{"only special chars", "!!!@@@", ""},
		{"unicode removed", "héllo-wörld", "hllo-wrld"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeSlug(tt.raw)
			if got != tt.want {
				t.Errorf("sanitizeSlug(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestHandleExportReplyWithContent(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	promptID := "export-reply-prompt"
	state := &exportState{
		sessionID: "reply-session",
		promptID:  promptID,
		turns: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "Set up Docker", At: time.Now().UTC()},
			{Origin: eventbus.OriginAI, Text: "Docker configured.", At: time.Now().UTC()},
		},
	}
	svc.pendingExport.Store(promptID, state)

	svc.handleExportReply(ctx, eventbus.ConversationReplyEvent{
		SessionID: "reply-session",
		PromptID:  promptID,
		Text:      "SLUG: docker-setup\n\nSUMMARY:\nUser configured Docker for local development.",
		Metadata:  map[string]string{constants.MetadataKeyEventType: "session_slug"},
	})

	// Verify file was written in sessions/ directory.
	sessionsDir := filepath.Join(dir, "awareness", "memory", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected session export file to be created")
	}

	// Verify file content.
	content, err := os.ReadFile(filepath.Join(sessionsDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	contentStr := string(content)
	if !strings.Contains(contentStr, "docker-setup") {
		t.Errorf("expected slug in content, got: %s", contentStr)
	}
	if !strings.Contains(contentStr, "Docker for local development") {
		t.Errorf("expected summary in content, got: %s", contentStr)
	}
	if !strings.Contains(contentStr, "reply-session") {
		t.Errorf("expected session ID in content, got: %s", contentStr)
	}
	if !strings.Contains(contentStr, "Set up Docker") {
		t.Errorf("expected conversation log in content, got: %s", contentStr)
	}

	// Verify pendingExport was cleaned up.
	if _, ok := svc.pendingExport.Load(promptID); ok {
		t.Fatal("expected pendingExport to be cleaned up")
	}
}

func TestHandleExportReplyNoReply(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	promptID := "noreply-export"
	state := &exportState{
		sessionID: "trivial-session",
		promptID:  promptID,
		turns: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "hi", At: time.Now().UTC()},
		},
	}
	svc.pendingExport.Store(promptID, state)

	svc.handleExportReply(ctx, eventbus.ConversationReplyEvent{
		SessionID: "trivial-session",
		PromptID:  promptID,
		Text:      "NO_REPLY",
		Metadata:  map[string]string{constants.MetadataKeyEventType: "session_slug"},
	})

	// Verify NO file was written.
	sessionsDir := filepath.Join(dir, "awareness", "memory", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		// Directory might not even be created by writeSessionExport since NO_REPLY skips it.
		return
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			t.Fatalf("expected no export file for NO_REPLY, found: %s", e.Name())
		}
	}
}

func TestHandleExportReplyEmptySlugFallback(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	promptID := "empty-slug-prompt"
	state := &exportState{
		sessionID: "empty-slug-session",
		promptID:  promptID,
		turns: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "hello", At: time.Now().UTC()},
		},
	}
	svc.pendingExport.Store(promptID, state)

	// Reply with a slug that becomes empty after sanitization.
	svc.handleExportReply(ctx, eventbus.ConversationReplyEvent{
		SessionID: "empty-slug-session",
		PromptID:  promptID,
		Text:      "SLUG: !!!@@@\n\nSUMMARY:\nSome summary.",
		Metadata:  map[string]string{constants.MetadataKeyEventType: "session_slug"},
	})

	// Verify a file was still written using fallback timestamp slug.
	sessionsDir := filepath.Join(dir, "awareness", "memory", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected session export file with fallback slug")
	}
}

func TestWriteSessionExportCollision(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	turns := []eventbus.ConversationTurn{
		{Origin: eventbus.OriginUser, Text: "test", At: time.Now().UTC()},
	}

	// Write first file.
	if err := svc.writeSessionExport(ctx, "sess-1", "same-slug", "", turns); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Write second file with the same slug.
	if err := svc.writeSessionExport(ctx, "sess-2", "same-slug", "", turns); err != nil {
		t.Fatalf("second write: %v", err)
	}

	// Verify two distinct files exist.
	sessionsDir := filepath.Join(dir, "awareness", "memory", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}

	mdCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			mdCount++
		}
	}
	if mdCount != 2 {
		t.Fatalf("expected 2 .md files, got %d", mdCount)
	}
}

func TestWriteSessionExportTruncation(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Create a turn with content larger than maxExportContentBytes.
	bigText := strings.Repeat("x", maxExportContentBytes+1000)
	turns := []eventbus.ConversationTurn{
		{Origin: eventbus.OriginUser, Text: bigText, At: time.Now().UTC()},
	}

	if err := svc.writeSessionExport(ctx, "sess-big", "big-session", "", turns); err != nil {
		t.Fatalf("writeSessionExport: %v", err)
	}

	sessionsDir := filepath.Join(dir, "awareness", "memory", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected session file")
	}

	content, err := os.ReadFile(filepath.Join(sessionsDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	if !strings.Contains(string(content), "[truncated") {
		t.Fatal("expected truncation notice")
	}

	if len(content) > maxExportContentBytes {
		t.Fatalf("expected file size <= %d bytes, got %d", maxExportContentBytes, len(content))
	}

	if !utf8.ValidString(string(content)) {
		t.Fatal("file contains invalid UTF-8")
	}
}

func TestWriteSessionExportNoTempFileLeak(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	turns := []eventbus.ConversationTurn{
		{Origin: eventbus.OriginUser, Text: "hello", At: time.Now().UTC()},
	}

	if err := svc.writeSessionExport(ctx, "sess-1", "no-leak", "", turns); err != nil {
		t.Fatalf("writeSessionExport: %v", err)
	}

	sessionsDir := filepath.Join(dir, "awareness", "memory", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

func TestFlushReplySubscriptionFiltersNonMemoryFlush(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Seed a fake pendingFlush so that IF the filter fails,
	// handleFlushReply would find the entry (making the bug visible).
	fakePromptID := "filter-flush-test"
	state := &flushState{
		sessionID: "filter-flush",
		promptID:  fakePromptID,
	}
	svc.pendingFlush.Store(fakePromptID, state)

	// Publish a NON-flush reply (user_intent) through the bus.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
		SessionID: "filter-flush",
		PromptID:  fakePromptID,
		Text:      "This should be ignored by flush consumer",
		Metadata:  map[string]string{constants.MetadataKeyEventType: constants.PromptEventUserIntent},
	})

	// Wait long enough for the consumer goroutine to process (and ignore) the message.
	// The entry must NOT be consumed because the event_type doesn't match.
	select {
	case <-time.After(200 * time.Millisecond):
		// Expected: timeout means the entry was not consumed.
	}

	if _, ok := svc.pendingFlush.Load(fakePromptID); !ok {
		t.Fatal("expected pendingFlush entry to still exist (non-flush reply should be ignored)")
	}

	// Clean up.
	svc.pendingFlush.Delete(fakePromptID)
}

func TestExportReplySubscriptionFiltersNonExport(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Seed a fake pendingExport so that IF the filter fails,
	// handleExportReply would find the entry (making the bug visible).
	fakePromptID := "filter-export-test"
	state := &exportState{
		sessionID: "filter-export",
		promptID:  fakePromptID,
		turns: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "test", At: time.Now().UTC()},
		},
	}
	svc.pendingExport.Store(fakePromptID, state)

	// Publish a NON-export reply (user_intent) through the bus.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
		SessionID: "filter-export",
		PromptID:  fakePromptID,
		Text:      "This should be ignored by export consumer",
		Metadata:  map[string]string{constants.MetadataKeyEventType: constants.PromptEventUserIntent},
	})

	// Wait long enough for the consumer goroutine to process (and ignore) the message.
	// The entry must NOT be consumed because the event_type doesn't match.
	select {
	case <-time.After(200 * time.Millisecond):
		// Expected: timeout means the entry was not consumed.
	}

	if _, ok := svc.pendingExport.Load(fakePromptID); !ok {
		t.Fatal("expected pendingExport entry to still exist (non-export reply should be ignored)")
	}

	// Clean up — timer is fire-and-forget, just remove the map entry.
	svc.pendingExport.Delete(fakePromptID)
}

// TestExportReplyWriteEndToEnd exercises the full event bus path for session export replies:
// publish ConversationReplyEvent with event_type=session_slug → consumeExportReplies subscription
// → filter → handleExportReply → writeSessionExport → file written.
func TestExportReplyWriteEndToEnd(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Seed a pendingExport entry so handleExportReply has something to match.
	promptID := "e2e-export-reply"
	state := &exportState{
		sessionID: "e2e-export-session",
		promptID:  promptID,
		turns: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "Deploy to staging", At: time.Now().UTC()},
			{Origin: eventbus.OriginAI, Text: "Deployed successfully.", At: time.Now().UTC()},
		},
	}
	svc.pendingExport.Store(promptID, state)

	// Publish a ConversationReplyEvent with event_type=session_slug through the bus.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
		SessionID: "e2e-export-session",
		PromptID:  promptID,
		Text:      "SLUG: staging-deploy\n\nSUMMARY:\nDeployed application to staging environment.",
		Metadata:  map[string]string{constants.MetadataKeyEventType: "session_slug"},
	})

	// Poll until the consumer goroutine processes the event and writes the file.
	sessionsDir := filepath.Join(dir, "awareness", "memory", "sessions")
	deadline := time.After(2 * time.Second)
	for {
		entries, err := os.ReadDir(sessionsDir)
		if err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".md") {
					// Verify file content.
					content, readErr := os.ReadFile(filepath.Join(sessionsDir, e.Name()))
					if readErr != nil {
						t.Fatalf("read session file: %v", readErr)
					}
					contentStr := string(content)
					if !strings.Contains(contentStr, "staging-deploy") {
						t.Errorf("expected slug in content, got: %s", contentStr)
					}
					if !strings.Contains(contentStr, "staging environment") {
						t.Errorf("expected summary in content, got: %s", contentStr)
					}

					// Verify pendingExport was cleaned up.
					if _, ok := svc.pendingExport.Load(promptID); ok {
						t.Fatal("expected pendingExport to be cleaned up")
					}
					return
				}
			}
		}
		select {
		case <-deadline:
			t.Fatal("expected session export file written via subscription path")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestStartCreatesSessionsDirectory(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer svc.Shutdown(context.Background())

	sessionsDir := filepath.Join(dir, "awareness", "memory", "sessions")
	info, err := os.Stat(sessionsDir)
	if err != nil {
		t.Fatalf("sessions directory not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", sessionsDir)
	}
}

func TestEnsureDirectoriesCleansStaleSessionsTemp(t *testing.T) {
	dir := t.TempDir()

	// Pre-create sessions directory with a stale .tmp file.
	sessionsDir := filepath.Join(dir, "awareness", "memory", "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	staleTmp := filepath.Join(sessionsDir, "2026-02-20-test.md.tmp")
	if err := os.WriteFile(staleTmp, []byte("partial write"), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	if _, err := os.Stat(staleTmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stale .tmp file in sessions/ to be removed, got err: %v", err)
	}
}

func TestSetSlugTimeout(t *testing.T) {
	svc := NewService(t.TempDir())

	// Default should be zero (gets defaultSlugTimeout on Start).
	if svc.slugTimeout != 0 {
		t.Fatalf("expected default slugTimeout=0, got %v", svc.slugTimeout)
	}

	svc.SetSlugTimeout(5 * time.Second)
	if svc.slugTimeout != 5*time.Second {
		t.Fatalf("expected slugTimeout=5s, got %v", svc.slugTimeout)
	}

	// Negative/zero should not change value.
	svc.SetSlugTimeout(0)
	if svc.slugTimeout != 5*time.Second {
		t.Fatalf("expected slugTimeout unchanged at 5s, got %v", svc.slugTimeout)
	}
	svc.SetSlugTimeout(-1)
	if svc.slugTimeout != 5*time.Second {
		t.Fatalf("expected slugTimeout unchanged at 5s, got %v", svc.slugTimeout)
	}
}

func TestHandleExportReplyNoReply_CaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	sessionsDir := filepath.Join(dir, "awareness", "memory", "sessions")

	for _, text := range []string{"no_reply", "No_Reply", " NO_REPLY "} {
		promptID := fmt.Sprintf("export-noreply-%s", text)
		state := &exportState{
			sessionID: "noreply-export-ci",
			promptID:  promptID,
			turns: []eventbus.ConversationTurn{
				{Origin: eventbus.OriginUser, Text: "hello", At: time.Now().UTC()},
			},
		}
		svc.pendingExport.Store(promptID, state)

		svc.handleExportReply(ctx, eventbus.ConversationReplyEvent{
			SessionID: "noreply-export-ci",
			PromptID:  promptID,
			Text:      text,
			Metadata:  map[string]string{constants.MetadataKeyEventType: "session_slug"},
		})

		// Verify no file was written (trivial session — no export).
		entries, err := os.ReadDir(sessionsDir)
		if err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".md") {
					t.Fatalf("expected no export file for NO_REPLY variant %q, found %s", text, e.Name())
				}
			}
		}

		// Verify pendingExport was cleaned up.
		if _, ok := svc.pendingExport.Load(promptID); ok {
			t.Fatalf("expected pendingExport cleaned up for %q", text)
		}
	}
}

func TestHandleExportReplyEmptyReplySessionID(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	promptID := "empty-sid-reply"
	state := &exportState{
		sessionID: "original-session",
		promptID:  promptID,
		turns: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "test content", At: time.Now().UTC()},
		},
	}
	svc.pendingExport.Store(promptID, state)

	// Reply has empty SessionID — should log warning but still write file
	// using the original state.sessionID.
	svc.handleExportReply(ctx, eventbus.ConversationReplyEvent{
		SessionID: "", // empty
		PromptID:  promptID,
		Text:      "SLUG: empty-sid-test\n\nSUMMARY:\nTest summary.",
		Metadata:  map[string]string{constants.MetadataKeyEventType: "session_slug"},
	})

	sessionsDir := filepath.Join(dir, "awareness", "memory", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}

	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			content, readErr := os.ReadFile(filepath.Join(sessionsDir, e.Name()))
			if readErr != nil {
				t.Fatalf("read file: %v", readErr)
			}
			// File should reference the original session ID, not the empty one.
			if !strings.Contains(string(content), "original-session") {
				t.Errorf("expected original-session in content, got: %s", string(content))
			}
			found = true
		}
	}
	if !found {
		t.Fatal("expected export file to be written despite empty reply SessionID")
	}
}

func TestHandleExportReplySessionIDMismatch(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	promptID := "mismatch-sid-reply"
	state := &exportState{
		sessionID: "correct-session",
		promptID:  promptID,
		turns: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "test content", At: time.Now().UTC()},
		},
	}
	svc.pendingExport.Store(promptID, state)

	// Reply has a different SessionID — should log warning but still write file
	// using the original state.sessionID.
	svc.handleExportReply(ctx, eventbus.ConversationReplyEvent{
		SessionID: "wrong-session",
		PromptID:  promptID,
		Text:      "SLUG: mismatch-test\n\nSUMMARY:\nMismatch summary.",
		Metadata:  map[string]string{constants.MetadataKeyEventType: "session_slug"},
	})

	sessionsDir := filepath.Join(dir, "awareness", "memory", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}

	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			content, readErr := os.ReadFile(filepath.Join(sessionsDir, e.Name()))
			if readErr != nil {
				t.Fatalf("read file: %v", readErr)
			}
			contentStr := string(content)
			// File should use the original state.sessionID, not the reply's.
			if !strings.Contains(contentStr, "correct-session") {
				t.Errorf("expected correct-session in content, got: %s", contentStr)
			}
			if strings.Contains(contentStr, "wrong-session") {
				t.Errorf("file should not contain wrong-session")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("expected export file to be written despite SessionID mismatch")
	}
}

func TestConcurrentWriteSessionExport(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	turns := []eventbus.ConversationTurn{
		{Origin: eventbus.OriginUser, Text: "concurrent test", At: time.Now().UTC()},
	}

	const n = 10
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			slug := fmt.Sprintf("concurrent-%d", idx)
			errs <- svc.writeSessionExport(ctx, fmt.Sprintf("sess-%d", idx), slug, "", turns)
		}(i)
	}

	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent writeSessionExport failed: %v", err)
		}
	}

	sessionsDir := filepath.Join(dir, "awareness", "memory", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}

	mdCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			mdCount++
		}
	}
	if mdCount != n {
		t.Fatalf("expected %d .md files from concurrent writes, got %d", n, mdCount)
	}
}

func TestHandleExportReplyDuplicate(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	promptID := "dup-export-reply"
	state := &exportState{
		sessionID: "dup-session",
		promptID:  promptID,
		turns: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "test dup", At: time.Now().UTC()},
		},
	}
	svc.pendingExport.Store(promptID, state)

	reply := eventbus.ConversationReplyEvent{
		SessionID: "dup-session",
		PromptID:  promptID,
		Text:      "SLUG: dup-test\n\nSUMMARY:\nDuplicate test.",
		Metadata:  map[string]string{constants.MetadataKeyEventType: "session_slug"},
	}

	// First reply — should write file and clean up pendingExport.
	svc.handleExportReply(ctx, reply)

	if _, ok := svc.pendingExport.Load(promptID); ok {
		t.Fatal("expected pendingExport cleaned up after first reply")
	}

	sessionsDir := filepath.Join(dir, "awareness", "memory", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}
	firstCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			firstCount++
		}
	}
	if firstCount != 1 {
		t.Fatalf("expected 1 file after first reply, got %d", firstCount)
	}

	// Second reply with the same PromptID — should be silently ignored (no-op).
	svc.handleExportReply(ctx, reply)

	entries2, err := os.ReadDir(sessionsDir)
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}
	secondCount := 0
	for _, e := range entries2 {
		if strings.HasSuffix(e.Name(), ".md") {
			secondCount++
		}
	}
	if secondCount != firstCount {
		t.Fatalf("expected duplicate reply to be ignored (still %d files), got %d", firstCount, secondCount)
	}
}
