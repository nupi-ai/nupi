package awareness

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// SearchOptions controls the FTS5 search query.
type SearchOptions struct {
	Query       string // Search query (required).
	Scope       string // "project", "global", or "all" (default "all").
	ProjectSlug string // Project slug for scope="project".
	MaxResults  int    // Maximum results to return (default 10).
	DateFrom    string // Inclusive lower bound (RFC 3339), optional.
	DateTo      string // Inclusive upper bound (RFC 3339), optional.
}

// SearchResult represents a single FTS5 search hit.
type SearchResult struct {
	Path        string  // File path relative to awareness/memory/.
	ChunkIndex  int     // Chunk index within the file.
	Snippet     string  // FTS5-generated snippet with highlighted matches.
	Score       float64 // Relevance score (higher = more relevant). BM25 for FTS, cosine similarity for vector search.
	FileType    string  // "daily", "topic", "session".
	ProjectSlug string  // Project slug (empty for global).
	Mtime       string  // File modification time (RFC 3339).
}

// SearchFTS performs a keyword search across the archival memory index using FTS5.
func (ix *Indexer) SearchFTS(ctx context.Context, opts SearchOptions) ([]SearchResult, error) {
	if ix.db == nil {
		return nil, fmt.Errorf("awareness: indexer not open")
	}

	query := strings.TrimSpace(opts.Query)
	if query == "" {
		return nil, nil
	}

	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = 10
	}
	if maxResults > 100 {
		maxResults = 100
	}

	ftsQuery := sanitizeFTSQuery(query)
	if ftsQuery == "" {
		return nil, nil
	}

	// Build WHERE clause for scope and date filtering.
	var conditions []string
	var args []any

	conditions = append(conditions, "memory_chunks MATCH ?")
	args = append(args, ftsQuery)

	switch opts.Scope {
	case "project":
		if opts.ProjectSlug == "" {
			return nil, nil // No slug specified — no results for project scope.
		}
		conditions = append(conditions, "project_slug = ?")
		args = append(args, opts.ProjectSlug)
	case "global":
		conditions = append(conditions, "project_slug = ?")
		args = append(args, "")
	default:
		// "all" or empty — no scope filter.
	}

	if opts.DateFrom != "" {
		conditions = append(conditions, "mtime >= ?")
		args = append(args, opts.DateFrom)
	}
	if opts.DateTo != "" {
		conditions = append(conditions, "mtime <= ?")
		args = append(args, opts.DateTo)
	}

	where := strings.Join(conditions, " AND ")
	args = append(args, maxResults)

	sql := fmt.Sprintf(`SELECT path, chunk_idx, snippet(memory_chunks, 0, '', '', '...', 64),
		-bm25(memory_chunks) as score, file_type, project_slug, mtime
		FROM memory_chunks
		WHERE %s
		ORDER BY bm25(memory_chunks)
		LIMIT ?`, where)

	rows, err := ix.db.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("awareness: FTS5 search: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.Path, &r.ChunkIndex, &r.Snippet, &r.Score, &r.FileType, &r.ProjectSlug, &r.Mtime); err != nil {
			return nil, err
		}
		results = append(results, r)
	}

	return results, rows.Err()
}

// SearchVector performs a vector similarity search using cosine similarity.
// Returns results ranked by similarity score (descending).
func (ix *Indexer) SearchVector(ctx context.Context, queryVec []float32, opts SearchOptions) ([]SearchResult, error) {
	if ix.db == nil {
		return nil, fmt.Errorf("awareness: indexer not open")
	}
	if len(queryVec) == 0 {
		return nil, nil
	}

	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = 10
	}
	if maxResults > 100 {
		maxResults = 100
	}

	// Build query to load candidate embeddings with scope/date filtering via JOIN.
	var conditions []string
	var args []any

	conditions = append(conditions, "1=1") // base condition

	switch opts.Scope {
	case "project":
		if opts.ProjectSlug == "" {
			return nil, nil
		}
		conditions = append(conditions, "mc.project_slug = ?")
		args = append(args, opts.ProjectSlug)
	case "global":
		conditions = append(conditions, "mc.project_slug = ?")
		args = append(args, "")
	}

	if opts.DateFrom != "" {
		conditions = append(conditions, "mc.mtime >= ?")
		args = append(args, opts.DateFrom)
	}
	if opts.DateTo != "" {
		conditions = append(conditions, "mc.mtime <= ?")
		args = append(args, opts.DateTo)
	}

	where := strings.Join(conditions, " AND ")

	query := fmt.Sprintf(`SELECT me.path, me.chunk_idx, me.embedding, me.norm,
		mc.content, mc.file_type, mc.project_slug, mc.mtime
		FROM memory_embeddings me
		JOIN memory_chunks mc ON me.path = mc.path AND me.chunk_idx = mc.chunk_idx
		WHERE %s`, where)

	rows, err := ix.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("awareness: vector search: %w", err)
	}
	defer rows.Close()

	var results []SearchResult

	for rows.Next() {
		var (
			path, content, fileType, projectSlug, mtime string
			chunkIdx                                    int
			embBlob                                     []byte
			norm                                        float64
		)
		if err := rows.Scan(&path, &chunkIdx, &embBlob, &norm, &content, &fileType, &projectSlug, &mtime); err != nil {
			return nil, err
		}

		vec := deserializeEmbedding(embBlob)
		score := cosineSimilarityWithNorm(queryVec, vec, norm)

		// Use content as snippet (truncate at rune boundary for valid UTF-8).
		snippet := content
		if utf8.RuneCountInString(snippet) > 256 {
			runes := []rune(snippet)
			snippet = string(runes[:256]) + "..."
		}

		results = append(results, SearchResult{
			Path:        path,
			ChunkIndex:  chunkIdx,
			Snippet:     snippet,
			Score:       score,
			FileType:    fileType,
			ProjectSlug: projectSlug,
			Mtime:       mtime,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Sort by score descending.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	// Apply MaxResults limit.
	if len(results) > maxResults {
		results = results[:maxResults]
	}

	return results, nil
}

// hybridCandidateKey identifies a unique chunk for merging FTS and vector results.
type hybridCandidateKey struct {
	path     string
	chunkIdx int
}

// hybridCandidate holds merged scores from FTS and vector searches.
type hybridCandidate struct {
	result   SearchResult
	ftsScore float64
	vecScore float64
	hasFTS   bool
	hasVec   bool
}

// SearchHybrid combines FTS5 keyword search and vector semantic search into
// a unified ranking pipeline with score normalization, temporal decay, and
// MMR diversity reranking. If queryVec is nil, operates in FTS-only mode (NFR33).
func (ix *Indexer) SearchHybrid(ctx context.Context, queryVec []float32, opts SearchOptions) ([]SearchResult, error) {
	if ix.db == nil {
		return nil, fmt.Errorf("awareness: indexer not open")
	}

	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = 5
	}

	// Over-fetch for merging and MMR diversity.
	overMax := maxResults * 5
	if overMax < 50 {
		overMax = 50
	}
	if overMax > 100 {
		overMax = 100
	}
	overOpts := opts
	overOpts.MaxResults = overMax

	// Run FTS search.
	ftsResults, err := ix.SearchFTS(ctx, overOpts)
	if err != nil {
		return nil, fmt.Errorf("awareness: hybrid FTS: %w", err)
	}

	// Run vector search (skip if FTS-only mode).
	var vecResults []SearchResult
	if len(queryVec) > 0 {
		vecResults, err = ix.SearchVector(ctx, queryVec, overOpts)
		if err != nil {
			return nil, fmt.Errorf("awareness: hybrid vector: %w", err)
		}
	}

	// Normalize scores to [0, 1].
	ftsResults = normalizeScores(ftsResults, false)
	vecResults = normalizeScores(vecResults, true)

	// Merge by (path, chunkIdx).
	candidates := make(map[hybridCandidateKey]*hybridCandidate)

	for _, r := range ftsResults {
		key := hybridCandidateKey{path: r.Path, chunkIdx: r.ChunkIndex}
		c, ok := candidates[key]
		if !ok {
			c = &hybridCandidate{result: r}
			candidates[key] = c
		}
		c.ftsScore = r.Score
		c.hasFTS = true
	}

	for _, r := range vecResults {
		key := hybridCandidateKey{path: r.Path, chunkIdx: r.ChunkIndex}
		c, ok := candidates[key]
		if !ok {
			c = &hybridCandidate{result: r}
			candidates[key] = c
		} else {
			// Prefer vector snippet (full content, up to 256 chars) over FTS snippet.
			c.result.Snippet = r.Snippet
		}
		c.vecScore = r.Score
		c.hasVec = true
	}

	// Compute combined scores and apply temporal decay.
	now := time.Now()
	results := make([]SearchResult, 0, len(candidates))

	for _, c := range candidates {
		var combined float64
		switch {
		case c.hasFTS && c.hasVec:
			combined = 0.7*c.vecScore + 0.3*c.ftsScore
		case c.hasFTS:
			combined = 0.3 * c.ftsScore
		case c.hasVec:
			combined = 0.7 * c.vecScore
		}

		if !isEvergreenFile(c.result.Path) {
			combined = temporalDecay(combined, c.result.Mtime, now)
		}

		c.result.Score = combined
		results = append(results, c.result)
	}

	// Sort by score descending.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	// MMR reranking for diversity.
	return mmrRerank(results, 0.7, maxResults), nil
}

// sanitizeFTSQuery wraps each search term in double quotes to prevent
// FTS5 syntax injection (AND, OR, NOT, NEAR, *, etc.).
func sanitizeFTSQuery(query string) string {
	fields := strings.Fields(query)
	var terms []string
	for _, term := range fields {
		// Strip quotes and null bytes to prevent FTS5 syntax errors.
		term = strings.ReplaceAll(term, `"`, "")
		term = strings.ReplaceAll(term, "\x00", "")
		if term != "" {
			terms = append(terms, `"`+term+`"`)
		}
	}
	return strings.Join(terms, " ")
}
