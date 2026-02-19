package awareness

import (
	"context"
	"encoding/binary"
	"log"
	"math"
	"time"
)

// serializeEmbedding converts a float32 vector to a compact byte representation.
// Uses LittleEndian encoding: 4 bytes per float32.
func serializeEmbedding(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

// deserializeEmbedding converts a byte representation back to a float32 vector.
func deserializeEmbedding(data []byte) []float32 {
	n := len(data) / 4
	vec := make([]float32, n)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	return vec
}

// vectorNorm computes the L2 norm of a float32 vector.
func vectorNorm(vec []float32) float64 {
	var sum float64
	for _, v := range vec {
		f := float64(v)
		sum += f * f
	}
	return math.Sqrt(sum)
}

// cosineSimilarity computes the cosine similarity between two float32 vectors.
// Returns 0 for zero-length, mismatched, or zero-norm vectors.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// cosineSimilarityWithNorm computes cosine similarity using a pre-computed norm for b.
// The bNorm parameter is the L2 norm of vector b (stored in DB).
func cosineSimilarityWithNorm(a, b []float32, bNorm float64) float64 {
	if len(a) != len(b) || len(a) == 0 || bNorm == 0 {
		return 0
	}
	var dot, normA float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
	}
	if normA == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * bNorm)
}

// embedChunks generates and stores embeddings for the given chunks.
// Returns true if embedding is in degraded mode (provider error occurred).
func (ix *Indexer) embedChunks(ctx context.Context, chunks []chunkForEmbedding) bool {
	if ix.embeddingProvider == nil || len(chunks) == 0 {
		return false
	}

	degraded := false
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Process in batches of embeddingBatchSize.
	for batchStart := 0; batchStart < len(chunks); batchStart += embeddingBatchSize {
		if err := ctx.Err(); err != nil {
			return degraded
		}

		batchEnd := batchStart + embeddingBatchSize
		if batchEnd > len(chunks) {
			batchEnd = len(chunks)
		}
		batch := chunks[batchStart:batchEnd]

		// Collect texts for this batch.
		texts := make([]string, len(batch))
		for i, ch := range batch {
			texts[i] = ch.content
		}

		result, err := ix.embeddingProvider.GenerateEmbeddings(ctx, texts)
		if err != nil {
			if !degraded {
				log.Printf("[Awareness] WARNING: embedding generation failed, continuing FTS-only: %v", err)
				degraded = true
			}
			continue // Skip this batch, try remaining batches.
		}

		if len(result.Vectors) != len(batch) {
			log.Printf("[Awareness] WARNING: embedding count mismatch: got %d, expected %d", len(result.Vectors), len(batch))
			continue
		}

		// Store embeddings in database.
		ix.storeEmbeddings(ctx, batch, result.Vectors, result.Dimensions, result.Model, now)
	}

	return degraded
}

// storeEmbeddings persists a batch of embedding vectors to the database.
func (ix *Indexer) storeEmbeddings(ctx context.Context, chunks []chunkForEmbedding, vectors [][]float32, dims int, model string, timestamp string) {
	stmt, err := ix.db.PrepareContext(ctx,
		`INSERT OR REPLACE INTO memory_embeddings(path, chunk_idx, embedding, model, dimensions, norm, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		log.Printf("[Awareness] WARNING: prepare embedding insert: %v", err)
		return
	}
	defer stmt.Close()

	for i, vec := range vectors {
		blob := serializeEmbedding(vec)
		norm := vectorNorm(vec)
		if _, err := stmt.ExecContext(ctx, chunks[i].path, chunks[i].chunkIdx, blob, model, dims, norm, timestamp); err != nil {
			log.Printf("[Awareness] WARNING: store embedding %s:%d: %v", chunks[i].path, chunks[i].chunkIdx, err)
		}
	}
}

// backfillEmbeddings generates embeddings for chunks that are missing them.
// This handles the scenario where embeddings were skipped during a previous
// degraded sync cycle (NFR33).
func (ix *Indexer) backfillEmbeddings(ctx context.Context) {
	if ix.embeddingProvider == nil || ix.db == nil {
		return
	}

	rows, err := ix.db.QueryContext(ctx,
		`SELECT mc.path, mc.chunk_idx, mc.content
		 FROM memory_chunks mc
		 LEFT JOIN memory_embeddings me ON mc.path = me.path AND mc.chunk_idx = me.chunk_idx
		 WHERE me.path IS NULL`)
	if err != nil {
		log.Printf("[Awareness] WARNING: backfill query: %v", err)
		return
	}
	defer rows.Close()

	var missing []chunkForEmbedding
	for rows.Next() {
		var ch chunkForEmbedding
		if err := rows.Scan(&ch.path, &ch.chunkIdx, &ch.content); err != nil {
			log.Printf("[Awareness] WARNING: backfill scan: %v", err)
			return
		}
		missing = append(missing, ch)
	}
	if err := rows.Err(); err != nil {
		log.Printf("[Awareness] WARNING: backfill rows: %v", err)
		return
	}

	if len(missing) == 0 {
		return
	}

	log.Printf("[Awareness] Backfilling %d missing embeddings", len(missing))

	now := time.Now().UTC().Format(time.RFC3339Nano)
	var backfilled, errors int

	for batchStart := 0; batchStart < len(missing); batchStart += embeddingBatchSize {
		if err := ctx.Err(); err != nil {
			break
		}

		batchEnd := batchStart + embeddingBatchSize
		if batchEnd > len(missing) {
			batchEnd = len(missing)
		}
		batch := missing[batchStart:batchEnd]

		texts := make([]string, len(batch))
		for i, ch := range batch {
			texts[i] = ch.content
		}

		result, err := ix.embeddingProvider.GenerateEmbeddings(ctx, texts)
		if err != nil {
			errors += len(batch)
			log.Printf("[Awareness] WARNING: backfill embedding failed: %v", err)
			continue
		}

		if len(result.Vectors) != len(batch) {
			errors += len(batch)
			continue
		}

		ix.storeEmbeddings(ctx, batch, result.Vectors, result.Dimensions, result.Model, now)
		backfilled += len(batch)
	}

	log.Printf("[Awareness] Backfill complete: %d embedded, %d errors", backfilled, errors)
}
