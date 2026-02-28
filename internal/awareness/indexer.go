package awareness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// Indexer manages the FTS5 archival memory index stored in index.db.
// It is a sub-component of the awareness Service, NOT a standalone daemon service.
type Indexer struct {
	db                *sql.DB
	memoryDir         string // Path to awareness/memory/ directory.
	bus               *eventbus.Bus
	embeddingProvider EmbeddingProvider
}

// NewIndexer creates a new archival memory indexer.
// memoryDir is the absolute path to the awareness/memory/ directory.
func NewIndexer(memoryDir string) *Indexer {
	return &Indexer{memoryDir: memoryDir}
}

// SetEventBus wires the event bus for publishing AwarenessSyncEvent notifications.
// Must be called before Open.
func (ix *Indexer) SetEventBus(bus *eventbus.Bus) {
	if ix.db != nil {
		panic("awareness: Indexer.SetEventBus called after Open")
	}
	ix.bus = bus
}

// SetEmbeddingProvider wires the embedding provider for vector operations during sync.
func (ix *Indexer) SetEmbeddingProvider(provider EmbeddingProvider) {
	ix.embeddingProvider = provider
}

// Open creates or opens the index.db database and applies the FTS5 schema.
// If the schema is corrupted or missing, it rebuilds from scratch.
func (ix *Indexer) Open(ctx context.Context) error {
	if ix.db != nil {
		return fmt.Errorf("awareness: indexer already open")
	}

	dbPath := filepath.Join(ix.memoryDir, "index.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("awareness: open index.db: %w", err)
	}

	db.SetMaxOpenConns(1)

	if err := ix.applyPragmas(ctx, db); err != nil {
		db.Close()
		return err
	}

	ix.db = db

	if err := ix.ensureSchema(ctx); err != nil {
		db.Close()
		ix.db = nil
		return err
	}

	return nil
}

// Close closes the database connection.
func (ix *Indexer) Close() error {
	if ix.db == nil {
		return nil
	}
	err := ix.db.Close()
	ix.db = nil
	return err
}

// applyPragmas sets SQLite pragmas matching the project convention.
func (ix *Indexer) applyPragmas(ctx context.Context, db *sql.DB) error {
	pragmas := []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA temp_store = MEMORY",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("awareness: apply pragma %q: %w", p, err)
		}
	}
	return nil
}

// ensureSchema creates FTS5, metadata, and embeddings tables.
// If the FTS5 table is corrupt or missing, it drops and recreates everything.
// If only the embeddings table is missing (upgrade from pre-vector schema),
// it creates just the embeddings table additively.
func (ix *Indexer) ensureSchema(ctx context.Context) error {
	// Verify FTS5 table health with a lightweight query.
	if ix.schemaHealthy(ctx) {
		// FTS5 and metadata healthy — ensure embeddings table exists (additive migration).
		ix.ensureEmbeddingsTable(ctx)
		return nil
	}

	log.Printf("[Awareness] Index schema missing or corrupt — recreating")

	// Drop everything and recreate.
	stmts := []string{
		"DROP TABLE IF EXISTS memory_chunks",
		"DROP TABLE IF EXISTS memory_files",
		"DROP TABLE IF EXISTS memory_embeddings",
	}
	for _, s := range stmts {
		if _, err := ix.db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("awareness: drop tables: %w", err)
		}
	}

	return ix.createTables(ctx)
}

// ensureEmbeddingsTable creates the memory_embeddings table if it doesn't exist.
// This is an additive migration for existing index.db files that were created
// before vector embeddings support was added.
func (ix *Indexer) ensureEmbeddingsTable(ctx context.Context) {
	// Check if table exists via sqlite_master (explicit check avoids misinterpreting DB errors).
	var name string
	row := ix.db.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name='memory_embeddings'")
	if err := row.Scan(&name); err == nil {
		return // Table already exists.
	}

	log.Printf("[Awareness] Adding memory_embeddings table (schema upgrade)")
	embeddings := `CREATE TABLE IF NOT EXISTS memory_embeddings (
		path TEXT NOT NULL,
		chunk_idx INTEGER NOT NULL,
		embedding BLOB NOT NULL,
		model TEXT NOT NULL,
		dimensions INTEGER NOT NULL,
		norm REAL NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY (path, chunk_idx)
	)`
	if _, err := ix.db.ExecContext(ctx, embeddings); err != nil {
		log.Printf("[Awareness] WARNING: create memory_embeddings table: %v", err)
		return
	}
	embIdx := `CREATE INDEX IF NOT EXISTS idx_embeddings_model ON memory_embeddings(model)`
	if _, err := ix.db.ExecContext(ctx, embIdx); err != nil {
		log.Printf("[Awareness] WARNING: create embeddings model index: %v", err)
	}
}

// schemaHealthy returns true when both tables exist and are queryable.
func (ix *Indexer) schemaHealthy(ctx context.Context) bool {
	// SELECT 1 LIMIT 1 avoids full table scan (unlike count(*)).
	// sql.ErrNoRows means table exists but is empty — still healthy.
	var n int
	if err := ix.db.QueryRowContext(ctx, "SELECT 1 FROM memory_chunks LIMIT 1").Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err := ix.db.QueryRowContext(ctx, "SELECT 1 FROM memory_files LIMIT 1").Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false
	}
	return true
}

// createTables creates the FTS5 virtual table, metadata table, and embeddings table.
func (ix *Indexer) createTables(ctx context.Context) error {
	fts := `CREATE VIRTUAL TABLE IF NOT EXISTS memory_chunks USING fts5(
		content,
		path UNINDEXED,
		chunk_idx UNINDEXED,
		file_type UNINDEXED,
		project_slug UNINDEXED,
		mtime UNINDEXED,
		tokenize='porter unicode61'
	)`
	if _, err := ix.db.ExecContext(ctx, fts); err != nil {
		return fmt.Errorf("awareness: create FTS5 table: %w", err)
	}

	meta := `CREATE TABLE IF NOT EXISTS memory_files (
		path TEXT PRIMARY KEY,
		mtime TEXT NOT NULL,
		chunk_count INTEGER NOT NULL,
		size_bytes INTEGER NOT NULL
	)`
	if _, err := ix.db.ExecContext(ctx, meta); err != nil {
		return fmt.Errorf("awareness: create memory_files table: %w", err)
	}

	embeddings := `CREATE TABLE IF NOT EXISTS memory_embeddings (
		path TEXT NOT NULL,
		chunk_idx INTEGER NOT NULL,
		embedding BLOB NOT NULL,
		model TEXT NOT NULL,
		dimensions INTEGER NOT NULL,
		norm REAL NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY (path, chunk_idx)
	)`
	if _, err := ix.db.ExecContext(ctx, embeddings); err != nil {
		return fmt.Errorf("awareness: create memory_embeddings table: %w", err)
	}

	embIdx := `CREATE INDEX IF NOT EXISTS idx_embeddings_model ON memory_embeddings(model)`
	if _, err := ix.db.ExecContext(ctx, embIdx); err != nil {
		return fmt.Errorf("awareness: create embeddings model index: %w", err)
	}

	return nil
}

// embeddingBatchSize is the maximum number of texts per GenerateEmbeddings call.
const embeddingBatchSize = 100

// chunkForEmbedding tracks a chunk that needs embedding generation.
type chunkForEmbedding struct {
	path     string
	chunkIdx int
	content  string
}

// Sync walks the memory directory, indexes new/modified markdown files,
// removes index entries for deleted files, and generates vector embeddings.
func (ix *Indexer) Sync(ctx context.Context) error {
	start := time.Now()

	// Collect files currently on disk.
	onDisk := make(map[string]os.FileInfo)
	err := filepath.WalkDir(ix.memoryDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Printf("[Awareness] WARNING: walk: %s: %v", path, err)
			return nil // Skip this entry, continue walking.
		}
		if d.IsDir() {
			// Skip logs/ and archives/ subtrees under journals/ or conversations/ —
			// these contain raw append-only output and archived summaries that must
			// not be indexed by FTS5. Only skip when the parent is journals or
			// conversations to avoid accidentally excluding legitimate directories
			// elsewhere in the memory tree.
			name := d.Name()
			if name == "logs" || name == "archives" {
				parent := filepath.Base(filepath.Dir(path))
				if parent == "journals" || parent == "conversations" {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			log.Printf("[Awareness] WARNING: file info: %s: %v", path, infoErr)
			return nil // Skip this file, continue walking.
		}
		rel, relErr := filepath.Rel(ix.memoryDir, path)
		if relErr != nil {
			return relErr
		}
		onDisk[filepath.ToSlash(rel)] = info
		return nil
	})
	if err != nil {
		return fmt.Errorf("awareness: walk memory dir: %w", err)
	}

	// Load currently indexed files from metadata table.
	indexed, err := ix.loadIndexedFiles(ctx)
	if err != nil {
		return err
	}

	var statsNew, statsUpdated, statsDeleted, statsChunks int
	var chunksToEmbed []chunkForEmbedding

	// Index new/modified files.
	for relPath, info := range onDisk {
		if err := ctx.Err(); err != nil {
			return err
		}

		mtime := info.ModTime().UTC().Format(time.RFC3339Nano)

		isUpdate := false
		if prev, exists := indexed[relPath]; exists {
			if prev == mtime {
				continue // Unchanged.
			}
			isUpdate = true
		}

		chunks, idxErr := ix.indexFile(ctx, relPath, mtime)
		if idxErr != nil {
			log.Printf("[Awareness] WARNING: index file %s: %v", relPath, idxErr)
			continue
		}

		// Collect chunks for embedding generation.
		for _, ch := range chunks {
			chunksToEmbed = append(chunksToEmbed, chunkForEmbedding{
				path:     relPath,
				chunkIdx: ch.Index,
				content:  ch.Content,
			})
		}

		if isUpdate {
			statsUpdated++
			ix.publishSync(relPath, "updated")
		} else {
			statsNew++
			ix.publishSync(relPath, "created")
		}
	}

	// Remove deleted files.
	for relPath := range indexed {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, exists := onDisk[relPath]; !exists {
			if err := ix.removeFile(ctx, relPath); err != nil {
				log.Printf("[Awareness] WARNING: remove indexed file %s: %v", relPath, err)
				continue
			}
			statsDeleted++
		}
	}

	// Generate and store embeddings for new/modified chunks.
	embeddingDegraded := ix.embedChunks(ctx, chunksToEmbed)

	// Backfill missing embeddings if provider is healthy.
	if !embeddingDegraded && ix.embeddingProvider != nil {
		ix.backfillEmbeddings(ctx)
	}

	// Count total chunks.
	row := ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_chunks")
	_ = row.Scan(&statsChunks)

	log.Printf("[Awareness] Sync complete in %s: %d new, %d updated, %d deleted, %d total chunks",
		time.Since(start).Round(time.Millisecond), statsNew, statsUpdated, statsDeleted, statsChunks)

	return nil
}

// RebuildIndex drops all data and re-indexes everything from markdown files (NFR31).
// Note: during a rebuild, concurrent SearchFTS calls may return partial results
// because the clear and re-index span multiple transactions. Callers should
// avoid searching during a rebuild or accept best-effort results.
func (ix *Indexer) RebuildIndex(ctx context.Context) error {
	log.Printf("[Awareness] Rebuilding index from scratch (NFR31)")

	if _, err := ix.db.ExecContext(ctx, "DELETE FROM memory_chunks"); err != nil {
		return fmt.Errorf("awareness: clear FTS5 table: %w", err)
	}
	if _, err := ix.db.ExecContext(ctx, "DELETE FROM memory_files"); err != nil {
		return fmt.Errorf("awareness: clear memory_files table: %w", err)
	}
	if _, err := ix.db.ExecContext(ctx, "DELETE FROM memory_embeddings"); err != nil {
		return fmt.Errorf("awareness: clear memory_embeddings table: %w", err)
	}

	return ix.Sync(ctx)
}

// indexFile reads a markdown file, chunks it, and upserts into the index.
// Returns the chunks for embedding generation.
func (ix *Indexer) indexFile(ctx context.Context, relPath, mtime string) ([]Chunk, error) {
	absPath := filepath.Join(ix.memoryDir, filepath.FromSlash(relPath))
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}

	content := string(data)
	chunks := ChunkMarkdown(content)

	fileType, projectSlug := classifyFile(relPath)

	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Delete old chunks for this file.
	if _, err := tx.ExecContext(ctx, "DELETE FROM memory_chunks WHERE path = ?", relPath); err != nil {
		return nil, err
	}

	// Delete old embeddings for this file (chunk_idx may change on update).
	if _, err := tx.ExecContext(ctx, "DELETE FROM memory_embeddings WHERE path = ?", relPath); err != nil {
		return nil, err
	}

	// Insert new chunks.
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO memory_chunks(content, path, chunk_idx, file_type, project_slug, mtime) VALUES(?, ?, ?, ?, ?, ?)")
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	for _, ch := range chunks {
		if _, err := stmt.ExecContext(ctx, ch.Content, relPath, ch.Index, fileType, projectSlug, mtime); err != nil {
			return nil, err
		}
	}

	// Upsert metadata.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memory_files(path, mtime, chunk_count, size_bytes) VALUES(?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET mtime=excluded.mtime, chunk_count=excluded.chunk_count, size_bytes=excluded.size_bytes`,
		relPath, mtime, len(chunks), len(data)); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return chunks, nil
}

// removeFile deletes all chunks, embeddings, and metadata for a file.
func (ix *Indexer) removeFile(ctx context.Context, relPath string) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "DELETE FROM memory_chunks WHERE path = ?", relPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM memory_embeddings WHERE path = ?", relPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM memory_files WHERE path = ?", relPath); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	ix.publishSync(relPath, "deleted")
	return nil
}

// loadIndexedFiles returns a map of relPath → mtime from the metadata table.
func (ix *Indexer) loadIndexedFiles(ctx context.Context) (map[string]string, error) {
	rows, err := ix.db.QueryContext(ctx, "SELECT path, mtime FROM memory_files")
	if err != nil {
		return nil, fmt.Errorf("awareness: query memory_files: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var path, mtime string
		if err := rows.Scan(&path, &mtime); err != nil {
			return nil, err
		}
		result[path] = mtime
	}
	return result, rows.Err()
}

// classifyFile determines the file type and project slug from a relative path.
// Expected paths (relative to awareness/memory/):
//
//	conversations/YYYY-MM-DD.md            → ("conversations", "")
//	journals/x.md                          → ("journals", "")
//	topics/foo.md                          → ("topic", "")
//	projects/<slug>/conversations/x.md     → ("conversations", "<slug>")
//	projects/<slug>/journals/x.md          → ("journals", "<slug>")
//	projects/<slug>/topics/x.md            → ("topic", "<slug>")
func classifyFile(relPath string) (fileType, projectSlug string) {
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	if len(parts) == 0 {
		return "unknown", ""
	}

	switch parts[0] {
	case "conversations":
		return "conversations", ""
	case "journals":
		return "journals", ""
	case "topics":
		return "topic", ""
	case "projects":
		if len(parts) < 3 {
			return "unknown", ""
		}
		slug := parts[1]
		switch parts[2] {
		case "conversations":
			return "conversations", slug
		case "journals":
			return "journals", slug
		case "topics":
			return "topic", slug
		default:
			return "unknown", slug
		}
	default:
		return "unknown", ""
	}
}

// publishSync sends an AwarenessSyncEvent on the event bus.
func (ix *Indexer) publishSync(filePath, syncType string) {
	if ix.bus == nil {
		return
	}
	eventbus.Publish(context.Background(), ix.bus, eventbus.Memory.Sync, eventbus.SourceAwareness,
		eventbus.AwarenessSyncEvent{
			FilePath: filePath,
			SyncType: syncType,
		})
}
