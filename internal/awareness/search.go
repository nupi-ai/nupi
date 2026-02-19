package awareness

import (
	"context"
	"fmt"
	"strings"
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
	Score       float64 // BM25 score (higher = more relevant).
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
