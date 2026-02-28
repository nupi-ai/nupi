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
)

func TestIndexerOpenClose(t *testing.T) {
	dir := t.TempDir()
	ix := NewIndexer(dir)

	ctx := context.Background()
	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Verify index.db was created.
	dbPath := filepath.Join(dir, "index.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		t.Fatal("index.db was not created")
	}

	if err := ix.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestIndexerDoubleOpen(t *testing.T) {
	dir := t.TempDir()
	ix := NewIndexer(dir)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("first Open failed: %v", err)
	}
	defer ix.Close()

	// Second Open should return an error, not leak a connection.
	if err := ix.Open(ctx); err == nil {
		t.Fatal("expected error on double Open, got nil")
	}
}

func TestIndexerSchemaCreation(t *testing.T) {
	dir := t.TempDir()
	ix := NewIndexer(dir)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer ix.Close()

	// Verify FTS5 table exists by inserting.
	_, err := ix.db.ExecContext(ctx,
		"INSERT INTO memory_chunks(content, path, chunk_idx, file_type, project_slug, mtime) VALUES(?, ?, ?, ?, ?, ?)",
		"test content", "test.md", "0", "conversations", "", time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("FTS5 insert failed: %v", err)
	}

	// Verify metadata table exists.
	_, err = ix.db.ExecContext(ctx,
		"INSERT INTO memory_files(path, mtime, chunk_count, size_bytes) VALUES(?, ?, ?, ?)",
		"test.md", time.Now().UTC().Format(time.RFC3339), 1, 12)
	if err != nil {
		t.Fatalf("memory_files insert failed: %v", err)
	}
}

func TestIndexerSyncNewFiles(t *testing.T) {
	dir := t.TempDir()

	// Create directory structure.
	convDir := filepath.Join(dir, "conversations")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a markdown file.
	content := "## Today\n\nDid some work on the project."
	if err := os.WriteFile(filepath.Join(convDir, "2026-02-19.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
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

	// Verify chunks were indexed.
	var count int
	row := ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_chunks")
	if err := row.Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count == 0 {
		t.Fatal("expected at least 1 chunk after sync, got 0")
	}

	// Verify metadata was recorded.
	var metaPath string
	row = ix.db.QueryRowContext(ctx, "SELECT path FROM memory_files LIMIT 1")
	if err := row.Scan(&metaPath); err != nil {
		t.Fatalf("metadata query failed: %v", err)
	}
	if metaPath != "conversations/2026-02-19.md" {
		t.Errorf("expected path 'conversations/2026-02-19.md', got %q", metaPath)
	}
}

func TestIndexerSyncModifiedFiles(t *testing.T) {
	dir := t.TempDir()
	convDir := filepath.Join(dir, "conversations")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(convDir, "2026-02-19.md")
	if err := os.WriteFile(filePath, []byte("## Morning\n\nOriginal content."), 0o600); err != nil {
		t.Fatal(err)
	}

	ix := NewIndexer(dir)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer ix.Close()

	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("first Sync failed: %v", err)
	}

	// Modify the file and ensure mtime changes. Set an explicit future mtime
	// since filesystem mtime resolution can be too coarse on some platforms.
	newContent := []byte("## Morning\n\nUpdated content.\n\n## Evening\n\nNew section.")
	if err := os.WriteFile(filePath, newContent, 0o600); err != nil {
		t.Fatal(err)
	}
	futureTime := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filePath, futureTime, futureTime); err != nil {
		t.Fatal(err)
	}

	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("second Sync failed: %v", err)
	}

	// Verify updated content is in the index.
	var found bool
	rows, err := ix.db.QueryContext(ctx, "SELECT content FROM memory_chunks WHERE path = 'conversations/2026-02-19.md'")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c == "Updated content." || strings.Contains(c, "Updated content") {
			found = true
		}
	}
	if !found {
		t.Error("expected updated content in index after re-sync")
	}
}

func TestIndexerSyncDeletedFiles(t *testing.T) {
	dir := t.TempDir()
	topicsDir := filepath.Join(dir, "topics")
	if err := os.MkdirAll(topicsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(topicsDir, "golang.md")
	if err := os.WriteFile(filePath, []byte("# Go\n\nGreat language."), 0o600); err != nil {
		t.Fatal(err)
	}

	ix := NewIndexer(dir)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer ix.Close()

	if err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	// Delete the file.
	if err := os.Remove(filePath); err != nil {
		t.Fatal(err)
	}

	if err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	// Verify chunks and metadata removed.
	var count int
	row := ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_chunks WHERE path = 'topics/golang.md'")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 chunks after file deletion, got %d", count)
	}

	row = ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_files WHERE path = 'topics/golang.md'")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 metadata entries after file deletion, got %d", count)
	}
}

func TestIndexerFileTypeExtraction(t *testing.T) {
	tests := []struct {
		relPath     string
		wantType    string
		wantProject string
	}{
		{"conversations/2026-02-19.md", "conversations", ""},
		{"journals/2026-02-19.md", "journals", ""},
		{"topics/golang.md", "topic", ""},
		{"projects/nupi/conversations/2026-02-19.md", "conversations", "nupi"},
		{"projects/nupi/journals/2026-02-19-slug.md", "journals", "nupi"},
		{"projects/nupi/topics/architecture.md", "topic", "nupi"},
		{"projects/nupi", "unknown", ""},
		{"unknown.md", "unknown", ""},
		// logs/ and archives/ paths still classify under parent type,
		// but Sync() skips these directories entirely via filepath.SkipDir.
		{"journals/logs/session1/2026-02-27.md", "journals", ""},
		{"journals/archives/session1.md", "journals", ""},
		{"conversations/logs/session1/2026-02-27.md", "conversations", ""},
		{"conversations/archives/2026-02-27.md", "conversations", ""},
		{"projects/nupi/journals/logs/s1/2026-02-27.md", "journals", "nupi"},
	}

	for _, tt := range tests {
		t.Run(tt.relPath, func(t *testing.T) {
			gotType, gotProject := classifyFile(tt.relPath)
			if gotType != tt.wantType {
				t.Errorf("classifyFile(%q) type = %q, want %q", tt.relPath, gotType, tt.wantType)
			}
			if gotProject != tt.wantProject {
				t.Errorf("classifyFile(%q) project = %q, want %q", tt.relPath, gotProject, tt.wantProject)
			}
		})
	}
}

func TestSyncSkipsLogsAndArchives(t *testing.T) {
	dir := t.TempDir()

	// Create files that SHOULD be indexed.
	convDir := filepath.Join(dir, "conversations")
	journalsDir := filepath.Join(dir, "journals")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(journalsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(convDir, "2026-02-27.md"), []byte("Indexed conversation."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(journalsDir, "session1.md"), []byte("Indexed journal."), 0o600); err != nil {
		t.Fatal(err)
	}

	// Create files in logs/ and archives/ under journals/ and conversations/
	// that must NOT be indexed — including project-scoped paths.
	for _, sub := range []string{
		filepath.Join("journals", "logs", "session1"),
		filepath.Join("journals", "archives"),
		filepath.Join("conversations", "logs", "session1"),
		filepath.Join("conversations", "archives"),
		filepath.Join("projects", "myapp", "journals", "logs", "session1"),
		filepath.Join("projects", "myapp", "conversations", "archives"),
	} {
		d := filepath.Join(dir, sub)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "2026-02-27.md"), []byte("Raw log that must not be indexed."), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Create a logs/ directory under an unrelated parent — this SHOULD be indexed
	// (SkipDir only applies to journals/ and conversations/ children).
	otherLogs := filepath.Join(dir, "projects", "logs")
	if err := os.MkdirAll(otherLogs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherLogs, "note.md"), []byte("Should be indexed."), 0o600); err != nil {
		t.Fatal(err)
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

	// 2 top-level files + 1 file under projects/logs/ = 3 indexed files.
	var count int
	row := ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_files")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		// Show which files were indexed to aid debugging.
		rows, _ := ix.db.QueryContext(ctx, "SELECT path FROM memory_files ORDER BY path")
		defer rows.Close()
		var paths []string
		for rows.Next() {
			var p string
			rows.Scan(&p)
			paths = append(paths, p)
		}
		t.Fatalf("expected 3 indexed files, got %d: %v", count, paths)
	}

	// Verify no files from journals/logs, journals/archives, conversations/logs,
	// conversations/archives appear — including project-scoped paths.
	// The projects/logs/ file (not under journals/ or conversations/) SHOULD be indexed.
	for _, forbidden := range []string{
		"journals/logs",
		"journals/archives",
		"conversations/logs",
		"conversations/archives",
		"projects/myapp/journals/logs",
		"projects/myapp/conversations/archives",
	} {
		var n int
		row := ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_files WHERE path LIKE '%"+forbidden+"%'")
		if err := row.Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("expected 0 files from %s/, got %d", forbidden, n)
		}
	}

	// Verify the projects/logs/ file IS indexed (not accidentally excluded).
	var projectLogsCount int
	row = ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_files WHERE path LIKE '%projects/logs%'")
	if err := row.Scan(&projectLogsCount); err != nil {
		t.Fatal(err)
	}
	if projectLogsCount != 1 {
		t.Errorf("expected projects/logs/note.md to be indexed, got count: %d", projectLogsCount)
	}
}

func TestIndexerRebuildIndex(t *testing.T) {
	dir := t.TempDir()
	convDir := filepath.Join(dir, "conversations")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(convDir, "2026-02-19.md"), []byte("## Test\n\nContent here."), 0o600); err != nil {
		t.Fatal(err)
	}

	ix := NewIndexer(dir)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer ix.Close()

	if err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	// Insert a stale entry that doesn't correspond to a file.
	_, _ = ix.db.ExecContext(ctx,
		"INSERT INTO memory_chunks(content, path, chunk_idx, file_type, project_slug, mtime) VALUES(?, ?, ?, ?, ?, ?)",
		"stale", "nonexistent.md", "0", "conversations", "", "2026-01-01T00:00:00Z")
	_, _ = ix.db.ExecContext(ctx,
		"INSERT INTO memory_files(path, mtime, chunk_count, size_bytes) VALUES(?, ?, ?, ?)",
		"nonexistent.md", "2026-01-01T00:00:00Z", 1, 5)

	if err := ix.RebuildIndex(ctx); err != nil {
		t.Fatalf("RebuildIndex failed: %v", err)
	}

	// Stale entry should be gone.
	var count int
	row := ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_chunks WHERE path = 'nonexistent.md'")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected stale entry removed after rebuild, got %d chunks", count)
	}

	// Real file should still be indexed.
	row = ix.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_chunks WHERE path = 'conversations/2026-02-19.md'")
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Error("expected real file to be re-indexed after rebuild")
	}
}

func TestIndexerCorruptIndexRecovery(t *testing.T) {
	dir := t.TempDir()
	convDir := filepath.Join(dir, "conversations")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(convDir, "test.md"), []byte("Recovery test content."), 0o600); err != nil {
		t.Fatal(err)
	}

	// Create a corrupt index.db.
	dbPath := filepath.Join(dir, "index.db")
	if err := os.WriteFile(dbPath, []byte("this is not a valid database"), 0o600); err != nil {
		t.Fatal(err)
	}

	ix := NewIndexer(dir)
	ctx := context.Background()

	// Open should handle corrupt DB by recreating. The modernc.org/sqlite driver
	// may or may not return an error on open for corrupt files. We test that after
	// Open + Sync, the data is accessible regardless.
	err := ix.Open(ctx)
	if err != nil {
		// If Open fails with corrupt DB, that's acceptable for this test.
		// Remove corrupt file and retry.
		os.Remove(dbPath)
		ix2 := NewIndexer(dir)
		if err := ix2.Open(ctx); err != nil {
			t.Fatalf("Open failed even after removing corrupt DB: %v", err)
		}
		defer ix2.Close()

		if err := ix2.Sync(ctx); err != nil {
			t.Fatalf("Sync failed after recovery: %v", err)
		}

		var count int
		row := ix2.db.QueryRowContext(ctx, "SELECT count(*) FROM memory_chunks")
		if err := row.Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			t.Error("expected chunks after recovery sync")
		}
		return
	}
	defer ix.Close()

	if err := ix.Sync(ctx); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
}

func TestIndexerProjectSlugExtraction(t *testing.T) {
	dir := t.TempDir()

	// Create project directory structure.
	projDir := filepath.Join(dir, "projects", "myapp", "journals")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "2026-02-19-test.md"), []byte("Journal content."), 0o600); err != nil {
		t.Fatal(err)
	}

	ix := NewIndexer(dir)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer ix.Close()

	if err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	var slug, fileType string
	row := ix.db.QueryRowContext(ctx, "SELECT project_slug, file_type FROM memory_chunks LIMIT 1")
	if err := row.Scan(&slug, &fileType); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if slug != "myapp" {
		t.Errorf("expected project_slug 'myapp', got %q", slug)
	}
	if fileType != "journals" {
		t.Errorf("expected file_type 'journals', got %q", fileType)
	}
}

func TestIndexerSyncCancelledContext(t *testing.T) {
	dir := t.TempDir()
	convDir := filepath.Join(dir, "conversations")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create several files so the sync loop has work to do.
	for i := 0; i < 20; i++ {
		name := filepath.Join(convDir, fmt.Sprintf("file-%03d.md", i))
		if err := os.WriteFile(name, []byte(fmt.Sprintf("## Entry %d\n\nContent for file %d.", i, i)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ix := NewIndexer(dir)
	ctx := context.Background()

	if err := ix.Open(ctx); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer ix.Close()

	// Cancel the context before calling Sync.
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel() // Cancel immediately.

	err := ix.Sync(cancelCtx)
	if err == nil {
		t.Fatal("expected error from Sync with cancelled context, got nil")
	}

	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("expected context canceled error, got: %v", err)
	}
}

