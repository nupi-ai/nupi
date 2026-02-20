package awareness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

func TestServiceSearchVectorWithProvider(t *testing.T) {
	dir := t.TempDir()

	// Create a file in the expected memory directory structure.
	dailyDir := filepath.Join(dir, "awareness", "memory", "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dailyDir, "2026-02-19.md"), []byte("## Test\n\nVector searchable content here."), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(dailyDir, "test.md"), []byte("## Test\n\nContent."), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(dailyDir, "test.md"), []byte("## Test\n\nContent."), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(dailyDir, "2026-02-19.md"), []byte("## Test\n\nHybrid searchable content about databases."), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(dailyDir, "2026-02-19.md"), []byte("## Test\n\nFTS fallback content."), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(dailyDir, "2026-02-19.md"), []byte("## Test\n\nProvider error content."), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(dailyDir, "2026-02-19.md"), []byte("## Test\n\nSearchable query defaulting content."), 0o644); err != nil {
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

// --- Flush Handler Tests ---

func TestHandleFlushRequest(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Subscribe to conversation.prompt to capture the flush prompt
	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt)
	defer promptSub.Close()

	turns := []eventbus.ConversationTurn{
		{Origin: eventbus.OriginUser, Text: "What is Go?", At: time.Now().UTC()},
		{Origin: eventbus.OriginAI, Text: "Go is a programming language.", At: time.Now().UTC()},
	}

	// Directly call handleFlushRequest
	svc.handleFlushRequest(ctx, eventbus.MemoryFlushRequestEvent{
		SessionID: "test-session",
		Turns:     turns,
	})

	// Verify ConversationPromptEvent was published
	select {
	case env := <-promptSub.C():
		prompt := env.Payload
		if prompt.SessionID != "test-session" {
			t.Fatalf("expected session test-session, got %s", prompt.SessionID)
		}
		if prompt.Metadata["event_type"] != "memory_flush" {
			t.Fatalf("expected event_type=memory_flush, got %s", prompt.Metadata["event_type"])
		}
		if prompt.PromptID == "" {
			t.Fatal("expected non-empty PromptID")
		}
		if !strings.Contains(prompt.NewMessage.Text, "[user] What is Go?") {
			t.Fatalf("expected serialized turns, got: %s", prompt.NewMessage.Text)
		}
		if len(prompt.Context) != 2 {
			t.Fatalf("expected 2 turns in Context, got %d", len(prompt.Context))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for conversation prompt")
	}
}

func TestHandleFlushReplyWithContent(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Subscribe to flush response
	responseSub := eventbus.SubscribeTo(bus, eventbus.Memory.FlushResponse)
	defer responseSub.Close()

	// Simulate a flush request to populate pendingFlush
	promptID := "test-prompt-id"
	state := &flushState{
		sessionID: "content-session",
		promptID:  promptID,
		timer:     time.AfterFunc(30*time.Second, func() {}),
	}
	svc.pendingFlush.Store(promptID, state)

	// Simulate AI reply with content
	svc.handleFlushReply(ctx, eventbus.ConversationReplyEvent{
		SessionID: "content-session",
		PromptID:  promptID,
		Text:      "User decided to use PostgreSQL for the database.",
		Metadata:  map[string]string{"event_type": "memory_flush"},
	})

	// Verify file was written (use UTC to match production code in flush.go:199)
	date := time.Now().UTC().Format("2006-01-02")
	dailyFile := filepath.Join(dir, "awareness", "memory", "daily", date+".md")
	content, err := os.ReadFile(dailyFile)
	if err != nil {
		t.Fatalf("expected daily file to exist: %v", err)
	}
	if !strings.Contains(string(content), "User decided to use PostgreSQL") {
		t.Fatalf("expected flush content in file, got: %s", string(content))
	}
	if !strings.Contains(string(content), "# Daily Log") {
		t.Fatalf("expected daily log header, got: %s", string(content))
	}

	// Verify flush response published with Saved=true
	select {
	case env := <-responseSub.C():
		resp := env.Payload
		if resp.SessionID != "content-session" {
			t.Fatalf("expected session content-session, got %s", resp.SessionID)
		}
		if !resp.Saved {
			t.Fatal("expected Saved=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for flush response")
	}

	// Verify pendingFlush was cleaned up
	if _, ok := svc.pendingFlush.Load(promptID); ok {
		t.Fatal("expected pendingFlush to be cleaned up")
	}
}

func TestHandleFlushReplyNoReply(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	responseSub := eventbus.SubscribeTo(bus, eventbus.Memory.FlushResponse)
	defer responseSub.Close()

	// Populate pendingFlush
	promptID := "noreply-prompt"
	state := &flushState{
		sessionID: "noreply-session",
		promptID:  promptID,
		timer:     time.AfterFunc(30*time.Second, func() {}),
	}
	svc.pendingFlush.Store(promptID, state)

	// Simulate AI reply with NO_REPLY
	svc.handleFlushReply(ctx, eventbus.ConversationReplyEvent{
		SessionID: "noreply-session",
		PromptID:  promptID,
		Text:      "NO_REPLY",
		Metadata:  map[string]string{"event_type": "memory_flush"},
	})

	// Verify NO file was written (use UTC to match production code in flush.go:199)
	date := time.Now().UTC().Format("2006-01-02")
	dailyFile := filepath.Join(dir, "awareness", "memory", "daily", date+".md")
	if _, err := os.Stat(dailyFile); err == nil {
		t.Fatal("expected no file written for NO_REPLY")
	}

	// Verify flush response with Saved=false
	select {
	case env := <-responseSub.C():
		resp := env.Payload
		if resp.SessionID != "noreply-session" {
			t.Fatalf("expected session noreply-session, got %s", resp.SessionID)
		}
		if resp.Saved {
			t.Fatal("expected Saved=false for NO_REPLY")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for flush response")
	}
}

func TestHandleFlushTimeout(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)
	svc.SetFlushTimeout(100 * time.Millisecond) // Short timeout for testing

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	responseSub := eventbus.SubscribeTo(bus, eventbus.Memory.FlushResponse)
	defer responseSub.Close()

	// Trigger a flush request
	svc.handleFlushRequest(ctx, eventbus.MemoryFlushRequestEvent{
		SessionID: "timeout-session",
		Turns: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "hello", At: time.Now().UTC()},
		},
	})

	// Do NOT send a reply — wait for timeout
	select {
	case env := <-responseSub.C():
		resp := env.Payload
		if resp.SessionID != "timeout-session" {
			t.Fatalf("expected session timeout-session, got %s", resp.SessionID)
		}
		if resp.Saved {
			t.Fatal("expected Saved=false for timeout")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for flush timeout response")
	}
}

func TestHandleFlushRequestEmptyTurns(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	responseSub := eventbus.SubscribeTo(bus, eventbus.Memory.FlushResponse)
	defer responseSub.Close()

	// Send flush request with empty turns
	svc.handleFlushRequest(ctx, eventbus.MemoryFlushRequestEvent{
		SessionID: "empty-turns",
		Turns:     nil,
	})

	// Should immediately publish Saved=false without calling AI
	select {
	case env := <-responseSub.C():
		if env.Payload.SessionID != "empty-turns" {
			t.Fatalf("expected session empty-turns, got %s", env.Payload.SessionID)
		}
		if env.Payload.Saved {
			t.Fatal("expected Saved=false for empty turns")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for flush response")
	}

	// Verify no pendingFlush was created
	count := 0
	svc.pendingFlush.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Fatalf("expected 0 pending flush entries, got %d", count)
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

func TestHandleFlushReplyWriteError(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	responseSub := eventbus.SubscribeTo(bus, eventbus.Memory.FlushResponse)
	defer responseSub.Close()

	// Make the daily directory read-only to force a write error.
	dailyDir := filepath.Join(dir, "awareness", "memory", "daily")
	if err := os.Chmod(dailyDir, 0o444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dailyDir, 0o755)

	// Populate pendingFlush
	promptID := "write-error-prompt"
	state := &flushState{
		sessionID: "write-error-session",
		promptID:  promptID,
		timer:     time.AfterFunc(30*time.Second, func() {}),
	}
	svc.pendingFlush.Store(promptID, state)

	// Simulate AI reply with content (write will fail due to read-only dir)
	svc.handleFlushReply(ctx, eventbus.ConversationReplyEvent{
		SessionID: "write-error-session",
		PromptID:  promptID,
		Text:      "Content that should fail to write",
		Metadata:  map[string]string{"event_type": "memory_flush"},
	})

	// Verify flush response published with Saved=false
	select {
	case env := <-responseSub.C():
		resp := env.Payload
		if resp.SessionID != "write-error-session" {
			t.Fatalf("expected session write-error-session, got %s", resp.SessionID)
		}
		if resp.Saved {
			t.Fatal("expected Saved=false when write fails")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for flush response")
	}

	// Verify pendingFlush was cleaned up
	if _, ok := svc.pendingFlush.Load(promptID); ok {
		t.Fatal("expected pendingFlush to be cleaned up after error")
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

// TestWriteFlushContentConcurrent verifies that flushWriteMu correctly
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

// --- Flush Subscription Integration Tests ---

// TestFlushSubscriptionEndToEnd verifies the full event bus path:
// publish MemoryFlushRequestEvent → consumeFlushRequests → handleFlushRequest → prompt published.
// This exercises the subscription wiring that unit tests bypass by calling handlers directly.
func TestFlushSubscriptionEndToEnd(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Subscribe to the prompt topic to observe the awareness service's output.
	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt)
	defer promptSub.Close()

	// Publish a flush request through the bus (not calling handler directly).
	eventbus.Publish(ctx, bus, eventbus.Memory.FlushRequest, eventbus.SourceConversation, eventbus.MemoryFlushRequestEvent{
		SessionID: "sub-e2e",
		Turns: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "important decision", At: time.Now().UTC()},
		},
	})

	// Verify the awareness service processed it and published a ConversationPromptEvent.
	select {
	case env := <-promptSub.C():
		if env.Payload.Metadata["event_type"] != "memory_flush" {
			t.Fatalf("expected event_type=memory_flush, got %s", env.Payload.Metadata["event_type"])
		}
		if env.Payload.SessionID != "sub-e2e" {
			t.Fatalf("expected session sub-e2e, got %s", env.Payload.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for prompt from subscription path")
	}
}

// TestFlushReplySubscriptionFiltersNonFlush verifies that consumeFlushReplies
// ignores ConversationReplyEvents that are NOT event_type=memory_flush.
func TestFlushReplySubscriptionFiltersNonFlush(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Subscribe to flush response to detect any accidental processing.
	responseSub := eventbus.SubscribeTo(bus, eventbus.Memory.FlushResponse)
	defer responseSub.Close()

	// Seed a fake pendingFlush so that IF the filter fails, handleFlushReply
	// would find the entry and publish a response (making the bug visible).
	fakePromptID := "filter-test-prompt"
	state := &flushState{
		sessionID: "filter-test",
		promptID:  fakePromptID,
		timer:     time.AfterFunc(30*time.Second, func() {}),
	}
	svc.pendingFlush.Store(fakePromptID, state)

	// Publish a NON-flush reply (user_intent) through the bus.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
		SessionID: "filter-test",
		PromptID:  fakePromptID,
		Text:      "This should be ignored by awareness",
		Metadata:  map[string]string{"event_type": "user_intent"},
	})

	// Give the consumer goroutine time to process the message.
	// Then verify NO flush response was published (filter worked).
	select {
	case env := <-responseSub.C():
		t.Fatalf("non-flush reply should have been filtered, but got response: %+v", env.Payload)
	case <-time.After(200 * time.Millisecond):
		// Expected: no response published — filter correctly ignored non-flush reply.
	}

	// Verify the pendingFlush entry was NOT consumed (still present).
	if _, ok := svc.pendingFlush.Load(fakePromptID); !ok {
		t.Fatal("expected pendingFlush entry to still exist (non-flush reply should be ignored)")
	}

	// Clean up
	state.timer.Stop()
	svc.pendingFlush.Delete(fakePromptID)
}

// TestHandleFlushRequestEmptySessionID verifies the early-return path at flush.go:46-49.
// An empty SessionID should be silently ignored (with log warning) — no prompt published,
// no flush response published, no pendingFlush entry created.
func TestHandleFlushRequestEmptySessionID(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt)
	defer promptSub.Close()
	responseSub := eventbus.SubscribeTo(bus, eventbus.Memory.FlushResponse)
	defer responseSub.Close()

	svc.handleFlushRequest(ctx, eventbus.MemoryFlushRequestEvent{
		SessionID: "",
		Turns: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "hello", At: time.Now().UTC()},
		},
	})

	// Verify NO prompt was published
	select {
	case <-promptSub.C():
		t.Fatal("expected no prompt for empty SessionID")
	case <-time.After(100 * time.Millisecond):
		// Expected: no prompt
	}

	// Verify NO flush response was published
	select {
	case <-responseSub.C():
		t.Fatal("expected no flush response for empty SessionID")
	case <-time.After(100 * time.Millisecond):
		// Expected: no response
	}

	// Verify no pendingFlush entry was created
	count := 0
	svc.pendingFlush.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Fatalf("expected 0 pending flush entries, got %d", count)
	}
}

// TestHandleFlushReplyNoReplySubstring verifies that an AI response containing
// "NO_REPLY" as a substring (not an exact match) is treated as saveable content.
func TestHandleFlushReplyNoReplySubstring(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	responseSub := eventbus.SubscribeTo(bus, eventbus.Memory.FlushResponse)
	defer responseSub.Close()

	promptID := "noreply-substring"
	state := &flushState{
		sessionID: "substring-session",
		promptID:  promptID,
		timer:     time.AfterFunc(30*time.Second, func() {}),
	}
	svc.pendingFlush.Store(promptID, state)

	// AI response contains "NO_REPLY" as substring — should be saved, not discarded
	svc.handleFlushReply(ctx, eventbus.ConversationReplyEvent{
		SessionID: "substring-session",
		PromptID:  promptID,
		Text:      "NO_REPLY is not needed. Here is the important context:\n- User chose PostgreSQL.",
		Metadata:  map[string]string{"event_type": "memory_flush"},
	})

	// Verify file was written (content was saved, not treated as NO_REPLY)
	date := time.Now().UTC().Format("2006-01-02")
	dailyFile := filepath.Join(dir, "awareness", "memory", "daily", date+".md")
	content, err := os.ReadFile(dailyFile)
	if err != nil {
		t.Fatalf("expected daily file to exist: %v", err)
	}
	if !strings.Contains(string(content), "User chose PostgreSQL") {
		t.Fatalf("expected content in file, got: %s", string(content))
	}

	// Verify flush response with Saved=true
	select {
	case env := <-responseSub.C():
		if !env.Payload.Saved {
			t.Fatal("expected Saved=true for non-exact NO_REPLY match")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for flush response")
	}
}

// TestShutdownStartResetsShuttingDown verifies that after Shutdown→Start,
// the shuttingDown flag is reset so flush timeouts still fire and publish
// MemoryFlushResponseEvent. Without the reset, timer callbacks bail out early.
func TestShutdownStartResetsShuttingDown(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)
	svc.SetFlushTimeout(100 * time.Millisecond)

	ctx := context.Background()

	// First Start
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("first start: %v", err)
	}

	// Shutdown sets shuttingDown=true
	if err := svc.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Re-Start should reset shuttingDown=false
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("second start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Subscribe BEFORE injecting the flush request so we don't miss events.
	responseSub := eventbus.SubscribeTo(bus, eventbus.Memory.FlushResponse)
	defer responseSub.Close()
	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt)
	defer promptSub.Close()

	// Inject a flush request — the 100ms timeout should fire normally.
	svc.handleFlushRequest(ctx, eventbus.MemoryFlushRequestEvent{
		SessionID: "restart-test",
		Turns: []eventbus.ConversationTurn{
			{Origin: eventbus.OriginUser, Text: "hello", At: time.Now().UTC()},
		},
	})

	// Drain the prompt (we don't care about it, just need to prevent bus backup)
	select {
	case <-promptSub.C():
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for prompt")
	}

	// Wait for flush timeout to fire and publish response.
	// If shuttingDown was NOT reset, the timer callback would bail out
	// and never publish a MemoryFlushResponseEvent.
	select {
	case env := <-responseSub.C():
		if env.Payload.SessionID != "restart-test" {
			t.Fatalf("expected session restart-test, got %s", env.Payload.SessionID)
		}
		if env.Payload.Saved {
			t.Fatal("expected Saved=false for timeout response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: flush timer did not fire after restart (shuttingDown not reset?)")
	}
}

// TestFlushReplyWriteEndToEnd exercises the complete awareness service flush reply
// pipeline via the event bus: publish ConversationReplyEvent with event_type=memory_flush
// → consumeFlushReplies subscription → filter → handleFlushReply → writeFlushContent
// → file written → MemoryFlushResponseEvent published. Previous tests either called
// handleFlushReply directly or only tested the request path.
func TestFlushReplyWriteEndToEnd(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Subscribe to flush response to verify the final output.
	responseSub := eventbus.SubscribeTo(bus, eventbus.Memory.FlushResponse)
	defer responseSub.Close()

	// Seed a pendingFlush entry so handleFlushReply has something to match.
	promptID := "e2e-reply-prompt"
	state := &flushState{
		sessionID: "e2e-reply-session",
		promptID:  promptID,
		timer:     time.AfterFunc(30*time.Second, func() {}),
	}
	svc.pendingFlush.Store(promptID, state)

	// Publish a ConversationReplyEvent with event_type=memory_flush through
	// the event bus. This must be picked up by consumeFlushReplies goroutine.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
		SessionID: "e2e-reply-session",
		PromptID:  promptID,
		Text:      "User decided to use SQLite for config storage.",
		Metadata:  map[string]string{"event_type": "memory_flush"},
	})

	// Verify MemoryFlushResponseEvent published with Saved=true.
	select {
	case env := <-responseSub.C():
		if env.Payload.SessionID != "e2e-reply-session" {
			t.Fatalf("expected session e2e-reply-session, got %s", env.Payload.SessionID)
		}
		if !env.Payload.Saved {
			t.Fatal("expected Saved=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for flush response via subscription path")
	}

	// Verify the daily file was actually written.
	date := time.Now().UTC().Format("2006-01-02")
	dailyFile := filepath.Join(dir, "awareness", "memory", "daily", date+".md")
	content, err := os.ReadFile(dailyFile)
	if err != nil {
		t.Fatalf("expected daily file to exist: %v", err)
	}
	if !strings.Contains(string(content), "SQLite for config storage") {
		t.Fatalf("expected flush content in file, got: %s", string(content))
	}

	// Verify pendingFlush was cleaned up.
	if _, ok := svc.pendingFlush.Load(promptID); ok {
		t.Fatal("expected pendingFlush to be cleaned up")
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
	if err := os.WriteFile(staleTmp, []byte("partial write"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := NewService(dir)
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	// Verify the stale .tmp file was cleaned up.
	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Fatalf("expected stale .tmp file to be removed, got err: %v", err)
	}
}

func TestHandleFlushReplyNoReply_CaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	svc := NewService(dir)
	svc.SetEventBus(bus)

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Shutdown(ctx)

	responseSub := eventbus.SubscribeTo(bus, eventbus.Memory.FlushResponse)
	defer responseSub.Close()

	// Test case-insensitive NO_REPLY
	for _, text := range []string{"no_reply", "No_Reply", " NO_REPLY "} {
		promptID := fmt.Sprintf("noreply-%s", text)
		state := &flushState{
			sessionID: "noreply-ci",
			promptID:  promptID,
			timer:     time.AfterFunc(30*time.Second, func() {}),
		}
		svc.pendingFlush.Store(promptID, state)

		svc.handleFlushReply(ctx, eventbus.ConversationReplyEvent{
			SessionID: "noreply-ci",
			PromptID:  promptID,
			Text:      text,
			Metadata:  map[string]string{"event_type": "memory_flush"},
		})

		select {
		case env := <-responseSub.C():
			if env.Payload.Saved {
				t.Fatalf("expected Saved=false for %q", text)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout for %q", text)
		}
	}
}
