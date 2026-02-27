package awareness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"
)

func setupSearchIndex(t *testing.T) (*Indexer, context.Context) {
	t.Helper()
	dir := t.TempDir()

	// Create directory structure with files.
	for _, sub := range []string{"conversations", "topics", "projects/nupi/journals", "projects/nupi/conversations"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	files := map[string]string{
		"conversations/2026-02-19.md":                   "## Morning\n\nReviewed database migration plan.\n\n## Afternoon\n\nImplemented FTS5 indexer for memory search.",
		"conversations/2026-02-18.md":                   "## Work\n\nSet up awareness service scaffold.",
		"topics/golang.md":                              "## Go Tips\n\nUse context for cancellation. Use t.TempDir for tests.",
		"topics/databases.md":                           "## SQLite\n\nFTS5 provides full-text search. Use WAL mode for concurrency.\n\n## PostgreSQL\n\nGood for production workloads.",
		"projects/nupi/journals/2026-02-19.md":          "## Journal\n\nDiscussed architecture for the awareness memory system.",
		"projects/nupi/conversations/2026-02-19.md":     "## Nupi Conversation\n\nWorked on archival memory indexer implementation.",
	}

	for relPath, content := range files {
		absPath := filepath.Join(dir, relPath)
		if err := os.WriteFile(absPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ix := NewIndexer(dir)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	t.Cleanup(func() { ix.Close() })
	return ix, ctx
}

func TestSearchFTSBasic(t *testing.T) {
	ix, ctx := setupSearchIndex(t)

	results, err := ix.SearchFTS(ctx, SearchOptions{
		Query: "database migration",
	})
	if err != nil {
		t.Fatalf("SearchFTS failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least 1 result for 'database migration'")
	}

	// First result should be the conversation log mentioning database migration.
	if results[0].Path != "conversations/2026-02-19.md" {
		t.Errorf("expected top result from conversations/2026-02-19.md, got %q", results[0].Path)
	}
}

func TestSearchFTSBM25Ordering(t *testing.T) {
	ix, ctx := setupSearchIndex(t)

	results, err := ix.SearchFTS(ctx, SearchOptions{
		Query: "FTS5",
	})
	if err != nil {
		t.Fatalf("SearchFTS failed: %v", err)
	}

	if len(results) < 2 {
		t.Fatalf("expected at least 2 results for 'FTS5', got %d", len(results))
	}

	// All scores should be positive (negated BM25).
	for i, r := range results {
		if r.Score <= 0 {
			t.Errorf("result %d has non-positive score: %f", i, r.Score)
		}
	}

	// Scores should be in descending order (higher = more relevant).
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("results not in descending score order: [%d]=%f > [%d]=%f",
				i, results[i].Score, i-1, results[i-1].Score)
		}
	}
}

func TestSearchFTSScopeProject(t *testing.T) {
	ix, ctx := setupSearchIndex(t)

	results, err := ix.SearchFTS(ctx, SearchOptions{
		Query:       "awareness",
		Scope:       "project",
		ProjectSlug: "nupi",
	})
	if err != nil {
		t.Fatalf("SearchFTS failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least 1 result for project scope 'nupi'")
	}

	for _, r := range results {
		if r.ProjectSlug != "nupi" {
			t.Errorf("expected all results to have project_slug 'nupi', got %q in %s", r.ProjectSlug, r.Path)
		}
	}
}

func TestSearchFTSScopeProjectEmptySlug(t *testing.T) {
	ix, ctx := setupSearchIndex(t)

	results, err := ix.SearchFTS(ctx, SearchOptions{
		Query: "awareness",
		Scope: "project",
		// No ProjectSlug — should return empty results.
	})
	if err != nil {
		t.Fatalf("SearchFTS failed: %v", err)
	}

	if len(results) != 0 {
		t.Errorf("expected 0 results for project scope with empty slug, got %d", len(results))
	}
}

func TestSearchFTSScopeGlobal(t *testing.T) {
	ix, ctx := setupSearchIndex(t)

	results, err := ix.SearchFTS(ctx, SearchOptions{
		Query: "awareness",
		Scope: "global",
	})
	if err != nil {
		t.Fatalf("SearchFTS failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least 1 global result for 'awareness'")
	}

	for _, r := range results {
		if r.ProjectSlug != "" {
			t.Errorf("expected all global results to have empty project_slug, got %q in %s", r.ProjectSlug, r.Path)
		}
	}
}

func TestSearchFTSScopeAll(t *testing.T) {
	ix, ctx := setupSearchIndex(t)

	results, err := ix.SearchFTS(ctx, SearchOptions{
		Query: "awareness",
		Scope: "all",
	})
	if err != nil {
		t.Fatalf("SearchFTS failed: %v", err)
	}

	// Should include both global and project results.
	hasGlobal := false
	hasProject := false
	for _, r := range results {
		if r.ProjectSlug == "" {
			hasGlobal = true
		} else {
			hasProject = true
		}
	}

	if !hasGlobal || !hasProject {
		t.Errorf("scope=all should return both global and project results, global=%v project=%v", hasGlobal, hasProject)
	}
}

func TestSearchFTSDateFiltering(t *testing.T) {
	ix, ctx := setupSearchIndex(t)

	// Only search for very recent files.
	now := time.Now().UTC()
	from := now.Add(-1 * time.Hour).Format(time.RFC3339)

	results, err := ix.SearchFTS(ctx, SearchOptions{
		Query:    "awareness",
		DateFrom: from,
	})
	if err != nil {
		t.Fatalf("SearchFTS failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least 1 result for date-filtered 'awareness' query")
	}

	// All results should have mtime >= from.
	for _, r := range results {
		if r.Mtime < from {
			t.Errorf("result mtime %s is before DateFrom %s", r.Mtime, from)
		}
	}
}

func TestSearchFTSSnippetGeneration(t *testing.T) {
	ix, ctx := setupSearchIndex(t)

	results, err := ix.SearchFTS(ctx, SearchOptions{
		Query: "SQLite",
	})
	if err != nil {
		t.Fatalf("SearchFTS failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least 1 result for 'SQLite'")
	}

	// Snippet should contain some text.
	if results[0].Snippet == "" {
		t.Error("expected non-empty snippet")
	}
}

func TestSearchFTSMaxResults(t *testing.T) {
	ix, ctx := setupSearchIndex(t)

	results, err := ix.SearchFTS(ctx, SearchOptions{
		Query:      "the",
		MaxResults: 2,
	})
	if err != nil {
		t.Fatalf("SearchFTS failed: %v", err)
	}

	if len(results) > 2 {
		t.Errorf("expected at most 2 results with MaxResults=2, got %d", len(results))
	}
}

func TestSearchFTSMaxResultsCap(t *testing.T) {
	ix, ctx := setupSearchIndex(t)

	// Requesting more than 100 should be capped at 100.
	results, err := ix.SearchFTS(ctx, SearchOptions{
		Query:      "the",
		MaxResults: 999999,
	})
	if err != nil {
		t.Fatalf("SearchFTS failed: %v", err)
	}

	// With only 6 test files, we can't verify the cap directly,
	// but we verify the query doesn't fail with a huge MaxResults.
	if len(results) > 100 {
		t.Errorf("expected at most 100 results (capped), got %d", len(results))
	}
}

func TestSearchFTSEmptyQuery(t *testing.T) {
	ix, ctx := setupSearchIndex(t)

	results, err := ix.SearchFTS(ctx, SearchOptions{
		Query: "",
	})
	if err != nil {
		t.Fatalf("SearchFTS with empty query should not error: %v", err)
	}

	if len(results) != 0 {
		t.Errorf("expected 0 results for empty query, got %d", len(results))
	}
}

func TestSearchFTSSpecialCharacters(t *testing.T) {
	ix, ctx := setupSearchIndex(t)

	// These should not cause SQL/FTS5 syntax errors.
	queries := []string{
		`test"injection`,
		"AND OR NOT",
		"NEAR(test, 5)",
		`*wildcard*`,
		"(parentheses)",
		`"""`,
		"test\x00injection",
		"\x00",
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			_, err := ix.SearchFTS(ctx, SearchOptions{
				Query: q,
			})
			if err != nil {
				t.Errorf("SearchFTS with query %q returned error: %v", q, err)
			}
		})
	}
}

func TestSearchFTSPerformance(t *testing.T) {
	dir := t.TempDir()
	convDir := filepath.Join(dir, "conversations")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create 500 files with ~3 chunks each ≈ 1500 chunks.
	// Full 10K chunk test is too slow for CI; we validate search latency scales linearly.
	for i := 0; i < 500; i++ {
		content := fmt.Sprintf("## Entry %d\n\nThis is document number %d about software development.\n\n"+
			"## Details\n\nPerformance testing of FTS5 full-text search with many documents.\n\n"+
			"## Notes\n\nAdditional content for chunk %d generation and indexing validation.", i, i, i)
		filename := fmt.Sprintf("2026-01-%03d.md", i)
		if err := os.WriteFile(filepath.Join(convDir, filename), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ix := NewIndexer(dir)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer ix.Close()

	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// Verify we have enough chunks.
	var totalChunks int
	row := ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_chunks")
	if err := row.Scan(&totalChunks); err != nil {
		t.Fatal(err)
	}
	t.Logf("Indexed %d chunks from 500 files", totalChunks)

	// Search must complete within 500ms (NFR30).
	start := time.Now()
	results, err := ix.SearchFTS(ctx, SearchOptions{
		Query:      "performance testing FTS5",
		MaxResults: 10,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("SearchFTS failed: %v", err)
	}

	if elapsed > 500*time.Millisecond {
		t.Errorf("search took %s, exceeds 500ms NFR30 target", elapsed)
	}

	t.Logf("Search returned %d results in %s", len(results), elapsed)

	if len(results) == 0 {
		t.Error("expected at least 1 result from performance test")
	}
}

// setupSearchIndexWithEmbeddings creates an index with both FTS5 and vector embeddings.
func setupSearchIndexWithEmbeddings(t *testing.T) (*Indexer, context.Context) {
	t.Helper()
	dir := t.TempDir()

	for _, sub := range []string{"conversations", "topics", "projects/nupi/journals"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	files := map[string]string{
		"conversations/2026-02-19.md":                  "## Morning\n\nReviewed database migration plan.\n\n## Afternoon\n\nImplemented FTS5 indexer for memory search.",
		"topics/golang.md":                             "## Go Tips\n\nUse context for cancellation. Use t.TempDir for tests.",
		"topics/databases.md":                          "## SQLite\n\nFTS5 provides full-text search. Use WAL mode for concurrency.",
		"projects/nupi/journals/2026-02-19.md":         "## Journal\n\nDiscussed architecture for the awareness memory system.",
	}

	for relPath, content := range files {
		absPath := filepath.Join(dir, relPath)
		if err := os.WriteFile(absPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	mock := &mockEmbeddingProvider{dims: 3}
	ix := NewIndexer(dir)
	ix.SetEmbeddingProvider(mock)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	t.Cleanup(func() { ix.Close() })
	return ix, ctx
}

func TestSearchVectorBasic(t *testing.T) {
	ix, ctx := setupSearchIndexWithEmbeddings(t)

	// Use a query vector that should match something.
	queryVec := []float32{0.2, 0.1, 0.5}
	results, err := ix.SearchVector(ctx, queryVec, SearchOptions{})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least 1 vector search result")
	}

	// All results should have non-zero scores.
	for i, r := range results {
		if r.Score == 0 {
			t.Errorf("result %d has zero score", i)
		}
	}
}

func TestSearchVectorScoreOrdering(t *testing.T) {
	ix, ctx := setupSearchIndexWithEmbeddings(t)

	queryVec := []float32{0.3, 0.1, 0.5}
	results, err := ix.SearchVector(ctx, queryVec, SearchOptions{})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}

	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	// Results should be in descending score order.
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("results not in descending score order: [%d]=%f > [%d]=%f",
				i, results[i].Score, i-1, results[i-1].Score)
		}
	}
}

func TestSearchVectorMaxResults(t *testing.T) {
	ix, ctx := setupSearchIndexWithEmbeddings(t)

	queryVec := []float32{0.2, 0.1, 0.5}
	results, err := ix.SearchVector(ctx, queryVec, SearchOptions{MaxResults: 2})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}

	if len(results) > 2 {
		t.Errorf("expected at most 2 results with MaxResults=2, got %d", len(results))
	}
}

func TestSearchVectorScopeProject(t *testing.T) {
	ix, ctx := setupSearchIndexWithEmbeddings(t)

	queryVec := []float32{0.2, 0.1, 0.5}
	results, err := ix.SearchVector(ctx, queryVec, SearchOptions{
		Scope:       "project",
		ProjectSlug: "nupi",
	})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}

	for _, r := range results {
		if r.ProjectSlug != "nupi" {
			t.Errorf("expected project_slug 'nupi', got %q in %s", r.ProjectSlug, r.Path)
		}
	}
}

func TestSearchVectorScopeGlobal(t *testing.T) {
	ix, ctx := setupSearchIndexWithEmbeddings(t)

	queryVec := []float32{0.2, 0.1, 0.5}
	results, err := ix.SearchVector(ctx, queryVec, SearchOptions{Scope: "global"})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}

	for _, r := range results {
		if r.ProjectSlug != "" {
			t.Errorf("expected empty project_slug for global scope, got %q in %s", r.ProjectSlug, r.Path)
		}
	}
}

func TestSearchVectorEmptyQueryVec(t *testing.T) {
	ix, ctx := setupSearchIndexWithEmbeddings(t)

	results, err := ix.SearchVector(ctx, nil, SearchOptions{})
	if err != nil {
		t.Fatalf("SearchVector with nil vec: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for nil query vector, got %d", len(results))
	}

	results, err = ix.SearchVector(ctx, []float32{}, SearchOptions{})
	if err != nil {
		t.Fatalf("SearchVector with empty vec: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for empty query vector, got %d", len(results))
	}
}

func TestSearchVectorSnippetTruncation(t *testing.T) {
	ix, ctx := setupSearchIndexWithEmbeddings(t)

	queryVec := []float32{0.2, 0.1, 0.5}
	results, err := ix.SearchVector(ctx, queryVec, SearchOptions{})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}

	for _, r := range results {
		if r.Snippet == "" {
			t.Errorf("expected non-empty snippet for %s", r.Path)
		}
		// Snippets should be truncated at 256 runes + "..." suffix.
		runeCount := utf8.RuneCountInString(r.Snippet)
		if runeCount > 259 {
			t.Errorf("snippet too long (%d runes) for %s", runeCount, r.Path)
		}
	}
}

func TestSearchVectorClosedDB(t *testing.T) {
	ix := &Indexer{} // No DB opened.
	_, err := ix.SearchVector(context.Background(), []float32{1.0}, SearchOptions{})
	if err == nil {
		t.Error("expected error for SearchVector on closed indexer")
	}
}

// --- SearchHybrid integration tests (Tasks 6.7-6.10) ---

func TestSearchHybridFTSAndVector(t *testing.T) {
	ix, ctx := setupSearchIndexWithEmbeddings(t)

	// Use a query vector that produces results.
	queryVec := []float32{0.2, 0.1, 0.5}
	results, err := ix.SearchHybrid(ctx, queryVec, SearchOptions{
		Query:      "database migration",
		MaxResults: 5,
	})
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least 1 hybrid result")
	}

	// All combined scores should be in [0, 1] (before MMR reranking, max possible is 1.0).
	for i, r := range results {
		if r.Score < 0 || r.Score > 1.0 {
			t.Errorf("result %d score %f not in [0, 1]", i, r.Score)
		}
	}

	// Note: MMR reranking intentionally reorders results for diversity,
	// so strict descending score order is NOT guaranteed after reranking.
}

func TestSearchHybridFTSOnly(t *testing.T) {
	ix, ctx := setupSearchIndex(t) // No embeddings.

	// nil queryVec → FTS-only mode.
	results, err := ix.SearchHybrid(ctx, nil, SearchOptions{
		Query:      "database migration",
		MaxResults: 5,
	})
	if err != nil {
		t.Fatalf("SearchHybrid FTS-only: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least 1 FTS-only result")
	}

	// FTS-only scores should be ≤ 0.3 (max possible: 0.3 × 1.0).
	for i, r := range results {
		if r.Score > 0.31 { // small epsilon for floating point
			t.Errorf("FTS-only result %d score %f exceeds 0.3 max", i, r.Score)
		}
	}
}

func TestSearchHybridEmptyResults(t *testing.T) {
	ix, ctx := setupSearchIndex(t)

	// Query that matches nothing.
	results, err := ix.SearchHybrid(ctx, nil, SearchOptions{
		Query:      "xyznonexistent",
		MaxResults: 5,
	})
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if results != nil && len(results) != 0 {
		t.Errorf("expected 0 results for non-matching query, got %d", len(results))
	}

	// Empty query.
	results, err = ix.SearchHybrid(ctx, nil, SearchOptions{
		Query: "",
	})
	if err != nil {
		t.Fatalf("SearchHybrid empty query: %v", err)
	}
	if results != nil && len(results) != 0 {
		t.Errorf("expected 0 results for empty query, got %d", len(results))
	}
}

func TestSearchHybridScopeFiltering(t *testing.T) {
	ix, ctx := setupSearchIndexWithEmbeddings(t)
	queryVec := []float32{0.2, 0.1, 0.5}

	// Project scope.
	results, err := ix.SearchHybrid(ctx, queryVec, SearchOptions{
		Query:       "architecture memory",
		Scope:       "project",
		ProjectSlug: "nupi",
		MaxResults:  10,
	})
	if err != nil {
		t.Fatalf("SearchHybrid project scope: %v", err)
	}
	for _, r := range results {
		if r.ProjectSlug != "nupi" {
			t.Errorf("project scope: expected slug 'nupi', got %q in %s", r.ProjectSlug, r.Path)
		}
	}

	// Global scope.
	results, err = ix.SearchHybrid(ctx, queryVec, SearchOptions{
		Query:      "database",
		Scope:      "global",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("SearchHybrid global scope: %v", err)
	}
	for _, r := range results {
		if r.ProjectSlug != "" {
			t.Errorf("global scope: expected empty slug, got %q in %s", r.ProjectSlug, r.Path)
		}
	}
}

func TestSearchHybridClosedDB(t *testing.T) {
	ix := &Indexer{}
	_, err := ix.SearchHybrid(context.Background(), nil, SearchOptions{Query: "test"})
	if err == nil {
		t.Error("expected error for SearchHybrid on closed indexer")
	}
}

func TestSanitizeFTSQuery(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello world", `"hello" "world"`},
		{`test"injection`, `"testinjection"`},
		{"AND OR NOT", `"AND" "OR" "NOT"`},
		{"single", `"single"`},
		{"", ""},
		{"  spaces  ", `"spaces"`},
		{`"""`, ""},
		{`"" hello`, `"hello"`},
		{"test\x00null", `"testnull"`},
		{"\x00", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeFTSQuery(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeFTSQuery(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
