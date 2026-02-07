package recording

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestNewStoreCreatesDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(store.recordingsDir) })

	expectedDir := filepath.Join(home, ".nupi", "recordings")
	if store.recordingsDir != expectedDir {
		t.Fatalf("expected recordings dir %s, got %s", expectedDir, store.recordingsDir)
	}
	if _, err := os.Stat(expectedDir); err != nil {
		t.Fatalf("expected recordings dir to exist: %v", err)
	}
}

func TestSaveAndLoadMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(store.recordingsDir) })

	later := time.Now()
	earlier := later.Add(-time.Hour)

	metaA := Metadata{
		SessionID: "A",
		Filename:  "a.cast",
		StartTime: later,
		Duration:  10,
	}
	metaB := Metadata{
		SessionID: "B",
		Filename:  "b.cast",
		StartTime: earlier,
		Duration:  20,
	}

	if err := store.SaveMetadata(metaA); err != nil {
		t.Fatalf("SaveMetadata A: %v", err)
	}
	if err := store.SaveMetadata(metaB); err != nil {
		t.Fatalf("SaveMetadata B: %v", err)
	}

	items, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].SessionID != "A" {
		t.Fatalf("expected newest entry first, got %+v", items)
	}

	metaA.Title = "updated"
	if err := store.SaveMetadata(metaA); err != nil {
		t.Fatalf("SaveMetadata update: %v", err)
	}

	items, err = store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll after update: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items after update, got %d", len(items))
	}
	if items[0].Title != "updated" {
		t.Fatalf("expected updated metadata at head, got %+v", items[0])
	}
}

func TestLoadAllInvalidJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(store.recordingsDir) })

	if err := os.WriteFile(store.metadataFile, []byte("{broken"), 0o644); err != nil {
		t.Fatalf("write invalid metadata: %v", err)
	}

	if _, err := store.LoadAll(); err == nil {
		t.Fatalf("expected LoadAll to fail on invalid JSON")
	}
}

func TestGetBySessionIDReturnsCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(store.recordingsDir) })

	meta := Metadata{
		SessionID: "session",
		Title:     "original",
		StartTime: time.Now(),
	}
	if err := store.SaveMetadata(meta); err != nil {
		t.Fatalf("SaveMetadata: %v", err)
	}

	result, err := store.GetBySessionID("session")
	if err != nil {
		t.Fatalf("GetBySessionID: %v", err)
	}
	result.Title = "mutated"

	fresh, err := store.GetBySessionID("session")
	if err != nil {
		t.Fatalf("GetBySessionID second read: %v", err)
	}
	if fresh.Title != "original" {
		t.Fatalf("expected store value unaffected by mutation, got %s", fresh.Title)
	}
}

func TestDeleteRemovesMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(store.recordingsDir) })

	now := time.Now()
	for _, id := range []string{"keep", "remove"} {
		if err := store.SaveMetadata(Metadata{SessionID: id, StartTime: now}); err != nil {
			t.Fatalf("SaveMetadata %s: %v", id, err)
		}
	}

	if err := store.Delete("remove"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	items, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(items) != 1 || items[0].SessionID != "keep" {
		t.Fatalf("expected only keep entry, got %+v", items)
	}
}

func TestMetadataStoreMultiSessionOrdering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(store.recordingsDir) })

	base := time.Now()
	for i := 0; i < 5; i++ {
		meta := Metadata{
			SessionID: fmt.Sprintf("sess-%d", i),
			Filename:  fmt.Sprintf("sess-%d.cast", i),
			StartTime: base.Add(time.Duration(i) * time.Minute),
			Duration:  float64(i + 1),
		}
		if err := store.SaveMetadata(meta); err != nil {
			t.Fatalf("SaveMetadata sess-%d: %v", i, err)
		}
	}

	items, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("expected 5 items, got %d", len(items))
	}

	// Verify newest-first ordering.
	if items[0].SessionID != "sess-4" {
		t.Fatalf("expected newest (sess-4) first, got %s", items[0].SessionID)
	}
	if items[4].SessionID != "sess-0" {
		t.Fatalf("expected oldest (sess-0) last, got %s", items[4].SessionID)
	}

	// Verify GetBySessionID returns correct metadata for each.
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("sess-%d", i)
		meta, err := store.GetBySessionID(id)
		if err != nil {
			t.Fatalf("GetBySessionID(%s): %v", id, err)
		}
		if meta.Duration != float64(i+1) {
			t.Fatalf("expected duration %f for %s, got %f", float64(i+1), id, meta.Duration)
		}
	}
}

func TestMetadataStoreSaveUpdateExisting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(store.recordingsDir) })

	meta := Metadata{
		SessionID: "update-test",
		Filename:  "update.cast",
		StartTime: time.Now(),
		Duration:  5.0,
		Title:     "original",
	}
	if err := store.SaveMetadata(meta); err != nil {
		t.Fatalf("SaveMetadata initial: %v", err)
	}

	// Update with same SessionID but different Duration.
	meta.Duration = 42.5
	meta.Title = "updated"
	if err := store.SaveMetadata(meta); err != nil {
		t.Fatalf("SaveMetadata update: %v", err)
	}

	items, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item (update, not duplicate), got %d", len(items))
	}
	if items[0].Duration != 42.5 {
		t.Fatalf("expected updated duration 42.5, got %f", items[0].Duration)
	}
	if items[0].Title != "updated" {
		t.Fatalf("expected updated title, got %q", items[0].Title)
	}
}

func TestScanRecordingsMatchesMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(store.recordingsDir) })

	// Create 3 .cast files + 1 .txt file.
	castFiles := []string{"a.cast", "b.cast", "c.cast"}
	for _, name := range castFiles {
		path := filepath.Join(store.recordingsDir, name)
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		meta := Metadata{
			SessionID:     strings.TrimSuffix(name, ".cast"),
			Filename:      name,
			StartTime:     time.Now(),
			RecordingPath: path,
		}
		if err := store.SaveMetadata(meta); err != nil {
			t.Fatalf("SaveMetadata %s: %v", name, err)
		}
	}
	// Write a non-.cast file.
	if err := os.WriteFile(filepath.Join(store.recordingsDir, "notes.txt"), []byte("txt"), 0o644); err != nil {
		t.Fatalf("write notes.txt: %v", err)
	}

	// ScanRecordings should return only .cast files.
	paths, err := store.ScanRecordings()
	if err != nil {
		t.Fatalf("ScanRecordings: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("expected 3 .cast files from scan, got %d", len(paths))
	}

	// Metadata count should match.
	metadata, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(metadata) != len(paths) {
		t.Fatalf("metadata count (%d) != scan count (%d)", len(metadata), len(paths))
	}
}

func TestScanRecordingsFiltersCastFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(store.recordingsDir) })

	files := []string{"one.cast", "two.cast", "notes.txt"}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(store.recordingsDir, name), []byte("data"), 0o644); err != nil {
			t.Fatalf("write file %s: %v", name, err)
		}
	}

	paths, err := store.ScanRecordings()
	if err != nil {
		t.Fatalf("ScanRecordings: %v", err)
	}
	sort.Strings(paths)
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "one.cast") || !strings.HasSuffix(paths[1], "two.cast") {
		t.Fatalf("unexpected recordings: %+v", paths)
	}
}
