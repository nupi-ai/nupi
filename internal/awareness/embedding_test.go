package awareness

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mockEmbeddingProvider implements EmbeddingProvider for testing.
type mockEmbeddingProvider struct {
	vectors    [][]float32
	dims       int
	model      string
	err        error
	callCount  int
	lastTexts  []string
}

func (m *mockEmbeddingProvider) GenerateEmbeddings(_ context.Context, texts []string) (EmbeddingResult, error) {
	m.callCount++
	m.lastTexts = texts
	if m.err != nil {
		return EmbeddingResult{}, m.err
	}
	// If vectors are pre-set, return them (up to len(texts)).
	if len(m.vectors) > 0 {
		n := len(texts)
		if n > len(m.vectors) {
			n = len(m.vectors)
		}
		return EmbeddingResult{Vectors: m.vectors[:n], Dimensions: m.dims, Model: m.model}, nil
	}
	// Default: generate deterministic embeddings based on text length.
	vecs := make([][]float32, len(texts))
	for i, t := range texts {
		vec := make([]float32, 3)
		vec[0] = float32(len(t)) / 100.0
		vec[1] = float32(i) / 10.0
		vec[2] = 0.5
		vecs[i] = vec
	}
	return EmbeddingResult{Vectors: vecs, Dimensions: 3, Model: "test-model"}, nil
}

func TestSerializeDeserializeRoundtrip(t *testing.T) {
	original := []float32{0.1, 0.2, 0.3, -0.4, 0.0, 1.0, -1.0, 3.14}
	blob := serializeEmbedding(original)

	if len(blob) != len(original)*4 {
		t.Fatalf("expected %d bytes, got %d", len(original)*4, len(blob))
	}

	restored := deserializeEmbedding(blob)
	if len(restored) != len(original) {
		t.Fatalf("expected %d elements, got %d", len(original), len(restored))
	}

	for i := range original {
		if restored[i] != original[i] {
			t.Errorf("mismatch at index %d: got %v, want %v", i, restored[i], original[i])
		}
	}
}

func TestSerializeEmptyVector(t *testing.T) {
	blob := serializeEmbedding(nil)
	if len(blob) != 0 {
		t.Fatalf("expected empty blob for nil input, got %d bytes", len(blob))
	}

	restored := deserializeEmbedding(blob)
	if len(restored) != 0 {
		t.Fatalf("expected empty vector for empty blob, got %d elements", len(restored))
	}
}

func TestCosineSimilarityIdenticalVectors(t *testing.T) {
	a := []float32{1.0, 2.0, 3.0}
	score := cosineSimilarity(a, a)
	if math.Abs(score-1.0) > 1e-6 {
		t.Errorf("identical vectors should have similarity 1.0, got %f", score)
	}
}

func TestCosineSimilarityOrthogonalVectors(t *testing.T) {
	a := []float32{1.0, 0.0, 0.0}
	b := []float32{0.0, 1.0, 0.0}
	score := cosineSimilarity(a, b)
	if math.Abs(score) > 1e-6 {
		t.Errorf("orthogonal vectors should have similarity 0.0, got %f", score)
	}
}

func TestCosineSimilarityOppositeVectors(t *testing.T) {
	a := []float32{1.0, 2.0, 3.0}
	b := []float32{-1.0, -2.0, -3.0}
	score := cosineSimilarity(a, b)
	if math.Abs(score-(-1.0)) > 1e-6 {
		t.Errorf("opposite vectors should have similarity -1.0, got %f", score)
	}
}

func TestCosineSimilarityZeroVector(t *testing.T) {
	a := []float32{0.0, 0.0, 0.0}
	b := []float32{1.0, 2.0, 3.0}
	score := cosineSimilarity(a, b)
	if score != 0 {
		t.Errorf("zero vector should give similarity 0, got %f", score)
	}
}

func TestCosineSimilarityMismatchedLength(t *testing.T) {
	a := []float32{1.0, 2.0}
	b := []float32{1.0, 2.0, 3.0}
	score := cosineSimilarity(a, b)
	if score != 0 {
		t.Errorf("mismatched lengths should give similarity 0, got %f", score)
	}
}

func TestCosineSimilarityEmpty(t *testing.T) {
	score := cosineSimilarity(nil, nil)
	if score != 0 {
		t.Errorf("empty vectors should give similarity 0, got %f", score)
	}
}

func TestCosineSimilarityWithNormMatchesRegular(t *testing.T) {
	a := []float32{0.5, 0.3, 0.8}
	b := []float32{0.2, 0.7, 0.4}
	bNorm := vectorNorm(b)

	regular := cosineSimilarity(a, b)
	withNorm := cosineSimilarityWithNorm(a, b, bNorm)

	if math.Abs(regular-withNorm) > 1e-6 {
		t.Errorf("cosineSimilarityWithNorm (%f) should match cosineSimilarity (%f)", withNorm, regular)
	}
}

func TestVectorNorm(t *testing.T) {
	vec := []float32{3.0, 4.0}
	norm := vectorNorm(vec)
	if math.Abs(norm-5.0) > 1e-6 {
		t.Errorf("norm([3,4]) should be 5.0, got %f", norm)
	}
}

func TestIndexerSyncWithEmbeddings(t *testing.T) {
	dir := t.TempDir()
	dailyDir := filepath.Join(dir, "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dailyDir, "test.md"), []byte("## Test\n\nSome content for embedding."), 0o600); err != nil {
		t.Fatal(err)
	}

	mock := &mockEmbeddingProvider{dims: 3}
	ix := NewIndexer(dir)
	ix.SetEmbeddingProvider(mock)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ix.Close()

	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if mock.callCount == 0 {
		t.Fatal("expected embedding provider to be called during Sync")
	}

	// Verify embeddings stored in DB.
	var count int
	row := ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_embeddings")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("expected embeddings to be stored after Sync")
	}
}

func TestIndexerSyncWithoutProvider(t *testing.T) {
	dir := t.TempDir()
	dailyDir := filepath.Join(dir, "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dailyDir, "test.md"), []byte("## Test\n\nFTS only."), 0o600); err != nil {
		t.Fatal(err)
	}

	ix := NewIndexer(dir)
	// No embedding provider set — FTS-only mode.
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ix.Close()

	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// FTS should work.
	var chunkCount int
	row := ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_chunks")
	if err := row.Scan(&chunkCount); err != nil {
		t.Fatal(err)
	}
	if chunkCount == 0 {
		t.Fatal("expected FTS chunks even without embedding provider")
	}

	// No embeddings should exist.
	var embCount int
	row = ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_embeddings")
	if err := row.Scan(&embCount); err != nil {
		t.Fatal(err)
	}
	if embCount != 0 {
		t.Errorf("expected 0 embeddings without provider, got %d", embCount)
	}
}

func TestIndexerFileDeleteClearsEmbeddings(t *testing.T) {
	dir := t.TempDir()
	dailyDir := filepath.Join(dir, "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(dailyDir, "test.md")
	if err := os.WriteFile(filePath, []byte("## Test\n\nContent to embed."), 0o600); err != nil {
		t.Fatal(err)
	}

	mock := &mockEmbeddingProvider{dims: 3}
	ix := NewIndexer(dir)
	ix.SetEmbeddingProvider(mock)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ix.Close()

	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	// Verify embeddings exist.
	var count int
	row := ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_embeddings WHERE path = 'daily/test.md'")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("expected embeddings before deletion")
	}

	// Delete the file and re-sync.
	os.Remove(filePath)
	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("second Sync: %v", err)
	}

	// Embeddings should be cleared.
	row = ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_embeddings WHERE path = 'daily/test.md'")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 embeddings after file deletion, got %d", count)
	}
}

func TestIndexerFileUpdateReEmbeds(t *testing.T) {
	dir := t.TempDir()
	dailyDir := filepath.Join(dir, "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(dailyDir, "test.md")
	if err := os.WriteFile(filePath, []byte("## V1\n\nOriginal."), 0o600); err != nil {
		t.Fatal(err)
	}

	mock := &mockEmbeddingProvider{dims: 3}
	ix := NewIndexer(dir)
	ix.SetEmbeddingProvider(mock)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ix.Close()

	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	callsBefore := mock.callCount

	// Modify file with explicit future mtime.
	if err := os.WriteFile(filePath, []byte("## V2\n\nUpdated content.\n\n## New Section\n\nMore content."), 0o600); err != nil {
		t.Fatal(err)
	}
	// Use os.Chtimes to ensure mtime change is detected.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filePath, future, future); err != nil {
		t.Fatal(err)
	}

	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("second Sync: %v", err)
	}

	if mock.callCount <= callsBefore {
		t.Error("expected embedding provider to be called again after file update")
	}

	// Verify new embeddings stored (old ones replaced).
	var count int
	row := ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_embeddings WHERE path = 'daily/test.md'")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("expected new embeddings after file update")
	}
}

func TestIndexerRebuildClearsEmbeddings(t *testing.T) {
	dir := t.TempDir()
	dailyDir := filepath.Join(dir, "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dailyDir, "test.md"), []byte("## Test\n\nContent."), 0o600); err != nil {
		t.Fatal(err)
	}

	mock := &mockEmbeddingProvider{dims: 3}
	ix := NewIndexer(dir)
	ix.SetEmbeddingProvider(mock)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ix.Close()

	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Insert a stale embedding.
	_, _ = ix.db.ExecContext(ctx,
		`INSERT INTO memory_embeddings(path, chunk_idx, embedding, model, dimensions, norm, created_at)
		 VALUES('stale.md', 0, X'00000000', 'test', 1, 0.0, '2026-01-01T00:00:00Z')`)

	if err := ix.RebuildIndex(ctx); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	// Stale embedding should be gone.
	var count int
	row := ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_embeddings WHERE path = 'stale.md'")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected stale embedding removed after rebuild, got %d", count)
	}
}

func TestBackfillMissingEmbeddings(t *testing.T) {
	dir := t.TempDir()
	dailyDir := filepath.Join(dir, "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dailyDir, "test.md"), []byte("## Test\n\nContent for backfill."), 0o600); err != nil {
		t.Fatal(err)
	}

	// First sync without embedding provider.
	ix := NewIndexer(dir)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ix.Close()

	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	// Verify no embeddings.
	var count int
	row := ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_embeddings")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 embeddings before backfill, got %d", count)
	}

	// Set embedding provider and manually trigger backfill.
	mock := &mockEmbeddingProvider{dims: 3}
	ix.SetEmbeddingProvider(mock)
	ix.backfillEmbeddings(ctx)

	// Verify embeddings now exist.
	row = ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_embeddings")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("expected embeddings after backfill")
	}
}

func TestBackfillWithErrors(t *testing.T) {
	dir := t.TempDir()
	dailyDir := filepath.Join(dir, "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dailyDir, "test.md"), []byte("## Test\n\nContent."), 0o600); err != nil {
		t.Fatal(err)
	}

	// Sync without provider first.
	ix := NewIndexer(dir)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ix.Close()

	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Backfill with failing provider — should not crash.
	mock := &mockEmbeddingProvider{err: fmt.Errorf("provider unavailable")}
	ix.SetEmbeddingProvider(mock)
	ix.backfillEmbeddings(ctx) // Should log warning but not error.

	// No embeddings should exist (all failed).
	var count int
	row := ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_embeddings")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 embeddings when provider fails, got %d", count)
	}
}

func TestBackfillNoOpWhenAllEmbedded(t *testing.T) {
	dir := t.TempDir()
	dailyDir := filepath.Join(dir, "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dailyDir, "test.md"), []byte("## Test\n\nContent."), 0o600); err != nil {
		t.Fatal(err)
	}

	mock := &mockEmbeddingProvider{dims: 3}
	ix := NewIndexer(dir)
	ix.SetEmbeddingProvider(mock)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ix.Close()

	// Sync will embed everything.
	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	callsBefore := mock.callCount

	// Backfill should be a no-op.
	ix.backfillEmbeddings(ctx)

	if mock.callCount != callsBefore {
		t.Error("expected no additional embedding calls when all chunks are embedded")
	}
}

