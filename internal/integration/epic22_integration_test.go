package integration

import (
	"context"
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/awareness"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/conversation"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/intentrouter"
	"github.com/nupi-ai/nupi/internal/server"
)

const (
	// testEventTimeout is the maximum time to wait for an async event in tests.
	testEventTimeout = 3 * time.Second
	// testPollInterval is the polling interval for waitForFile/waitForCondition.
	testPollInterval = 50 * time.Millisecond
)

// ---------------------------------------------------------------------------
// Shared helpers — Task 1
// ---------------------------------------------------------------------------

// publishConversationTurn publishes a ConversationTurnEvent on the bus.
// Timestamps use time.Now() for ordering; no test asserts on exact times.
func publishConversationTurn(bus *eventbus.Bus, sessionID string, origin eventbus.ContentOrigin, text string) {
	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Turn, eventbus.SourceConversation,
		eventbus.ConversationTurnEvent{
			SessionID: sessionID,
			Turn: eventbus.ConversationTurn{
				Origin: origin,
				Text:   text,
				At:     time.Now().UTC(),
			},
		})
}

// publishSessionLifecycle publishes a SessionLifecycleEvent (created).
// Used only by TestEpic22_Persistence (session reload after restart).
func publishSessionLifecycle(bus *eventbus.Bus, sessionID, label string, state eventbus.SessionState) {
	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager,
		eventbus.SessionLifecycleEvent{
			SessionID: sessionID,
			Label:     label,
			State:     state,
		})
}

// publishPipelineCleaned publishes a PipelineMessageEvent.
func publishPipelineCleaned(bus *eventbus.Bus, sessionID string, origin eventbus.ContentOrigin, text string, annotations map[string]string) {
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline,
		eventbus.PipelineMessageEvent{
			SessionID:   sessionID,
			Origin:      origin,
			Text:        text,
			Annotations: annotations,
		})
}

// waitForFile polls until the file exists or timeout expires.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(testPollInterval)
	}
	t.Fatalf("timeout waiting for file %s", path)
}

// waitForCondition polls until check returns true or timeout expires.
func waitForCondition(t *testing.T, timeout time.Duration, msg string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(testPollInterval)
	}
	t.Fatalf("timeout: %s", msg)
}

// createAwarenessTestDirs creates a temporary directory with the standard
// awareness subdirectory layout (journals, conversations, topics).
func createAwarenessTestDirs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{
		"journals/logs", "journals/archives",
		"conversations/logs", "conversations/archives",
		"topics",
	} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	return dir
}

// ---------------------------------------------------------------------------
// AC #1 — Persistence survives restart
// ---------------------------------------------------------------------------

func TestEpic22_Persistence(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	dataDir := createAwarenessTestDirs(t)
	journalsDir := filepath.Join(dataDir, "journals")
	conversationsDir := filepath.Join(dataDir, "conversations")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Phase 1: Start services, populate data ---

	journalSvc := awareness.NewJournalService(bus, journalsDir)
	if err := journalSvc.Start(ctx); err != nil {
		t.Fatalf("start journal: %v", err)
	}

	convLogSvc := awareness.NewConversationLogService(bus, conversationsDir)
	if err := convLogSvc.Start(ctx); err != nil {
		t.Fatalf("start convlog: %v", err)
	}

	sessionID := "persist-test-session"

	// Define expected file paths for barrier-based waiting.
	convRLPath := filepath.Join(conversationsDir, "conversation.md")
	journalRLPath := filepath.Join(journalsDir, sessionID+".md")

	// Create session so JournalService creates a rolling log.
	publishSessionLifecycle(bus, sessionID, "bash", eventbus.SessionStateCreated)

	// Wait for journal RL file creation instead of time.Sleep.
	waitForFile(t, journalRLPath, testEventTimeout)

	// Publish conversation turns for ConversationLogService.
	publishConversationTurn(bus, sessionID, eventbus.OriginUser, "hello from user")
	publishConversationTurn(bus, sessionID, eventbus.OriginAI, "hello from AI")

	// Publish pipeline events for JournalService (OriginTool).
	publishPipelineCleaned(bus, sessionID, eventbus.OriginTool, "command output one", nil)
	publishPipelineCleaned(bus, sessionID, eventbus.OriginTool, "command output two", nil)

	// Wait for files on disk (barrier-based, no time.Sleep).
	// Use condition checks for audit logs to avoid UTC midnight boundary race
	// (the date in the filename may differ from time.Now() at test start).
	waitForFile(t, convRLPath, testEventTimeout)
	waitForCondition(t, testEventTimeout, "conversation audit log not created", func() bool {
		entries, _ := os.ReadDir(filepath.Join(conversationsDir, "logs"))
		return len(entries) > 0
	})
	waitForCondition(t, testEventTimeout, "journal audit log not created", func() bool {
		entries, _ := os.ReadDir(filepath.Join(journalsDir, "logs", sessionID))
		return len(entries) > 0
	})

	// --- Phase 2: Shutdown and re-create ---

	convLogSvc.Shutdown(context.Background())
	journalSvc.Shutdown(context.Background())

	bus2 := eventbus.New()
	journalSvc2 := awareness.NewJournalService(bus2, journalsDir)
	if err := journalSvc2.Start(ctx); err != nil {
		t.Fatalf("restart journal: %v", err)
	}
	defer journalSvc2.Shutdown(context.Background())

	convLogSvc2 := awareness.NewConversationLogService(bus2, conversationsDir)
	if err := convLogSvc2.Start(ctx); err != nil {
		t.Fatalf("restart convlog: %v", err)
	}
	defer convLogSvc2.Shutdown(context.Background())

	// --- Phase 3: Verify data survives ---

	// ConversationLogService loads RL from conversation.md at Start().
	convSummaries, convRaw, err := convLogSvc2.GetContext()
	if err != nil {
		t.Fatalf("convlog GetContext: %v", err)
	}
	if convRaw == "" {
		t.Fatal("conversation raw is empty after restart — data did not survive")
	}
	if !strings.Contains(convRaw, "hello from user") {
		t.Errorf("conversation raw missing 'hello from user', got: %s", convRaw)
	}
	if !strings.Contains(convRaw, "hello from AI") {
		t.Errorf("conversation raw missing 'hello from AI', got: %s", convRaw)
	}

	// JournalService only loads per-session RL on SessionCreated. Re-publishing
	// SessionCreated causes NewRollingLog to reload from the persisted file,
	// recovering all prior entries.
	publishSessionLifecycle(bus2, sessionID, "bash", eventbus.SessionStateCreated)

	// Poll until journal data is available instead of time.Sleep.
	waitForCondition(t, testEventTimeout, "journal raw empty after restart", func() bool {
		_, raw, err := journalSvc2.GetContext(sessionID)
		return err == nil && raw != ""
	})

	journalSummaries, journalRaw, err := journalSvc2.GetContext(sessionID)
	if err != nil {
		t.Fatalf("journal GetContext: %v", err)
	}
	if journalRaw == "" {
		t.Fatal("journal raw is empty after restart — data did not survive")
	}
	if !strings.Contains(journalRaw, "command output") {
		t.Errorf("journal raw missing 'command output', got: %s", journalRaw)
	}

	// Summaries may be empty (no compaction triggered), but verify they are
	// valid strings — not corrupted by the restart cycle.
	if strings.ContainsAny(convSummaries, "\x00\x01\x02\x03") {
		t.Error("conversation summaries contain control characters after restart — possible data corruption")
	}
	if strings.ContainsAny(journalSummaries, "\x00\x01\x02\x03") {
		t.Error("journal summaries contain control characters after restart — possible data corruption")
	}

	// --- Phase 4: Verify AI prompt includes reloaded context (AC #1 completeness) ---
	// Wire restarted services as providers and trigger a prompt build to verify
	// the full path: restart → load data → provider.GetContext() → prompt template.
	engine := newEpic22PromptEngine()
	routerSvc := intentrouter.NewService(bus2,
		intentrouter.WithAdapter(&epic22MockAdapter{}),
		intentrouter.WithPromptEngine(engine),
		intentrouter.WithJournalProvider(journalSvc2),
		intentrouter.WithConversationLogProvider(convLogSvc2),
	)
	if err := routerSvc.Start(ctx); err != nil {
		t.Fatalf("start intent router for restart check: %v", err)
	}
	defer routerSvc.Shutdown(context.Background())

	eventbus.Publish(ctx, bus2, eventbus.Conversation.Prompt, eventbus.SourceConversation,
		eventbus.ConversationPromptEvent{
			SessionID: sessionID,
			PromptID:  "prompt-restart-verify",
			NewMessage: eventbus.ConversationMessage{
				Origin: eventbus.OriginTool,
				Text:   "restart verification",
			},
			Metadata: map[string]string{
				constants.MetadataKeyEventType:     constants.PromptEventSessionOutput,
				constants.MetadataKeySessionOutput: "output",
			},
		})

	select {
	case req := <-engine.ch:
		if !strings.Contains(req.ConversationRaw, "hello from user") {
			t.Error("prompt after restart missing conversation context 'hello from user'")
		}
		if !strings.Contains(req.JournalRaw, "command output") {
			t.Error("prompt after restart missing journal context 'command output'")
		}
	case <-time.After(testEventTimeout):
		t.Fatal("timeout waiting for prompt build after restart — reloaded context not available")
	}
}

// ---------------------------------------------------------------------------
// AC #2 — Compaction triggers and produces summaries
// ---------------------------------------------------------------------------

func TestEpic22_Compaction(t *testing.T) {
	t.Parallel()
	// This test validates the RollingLog compaction primitives directly:
	// ShouldCompact() → OlderHalf() → AppendSummary(). It operates at the
	// RollingLog level rather than through the full service-level flow because:
	//
	//   1. Service-level compaction (JournalService/ConversationLogService)
	//      requires an AI adapter mock for summarization — the adapter calls
	//      an LLM to produce summary text, making it unsuitable for deterministic
	//      integration tests without a full adapter test harness.
	//   2. Service-level compaction is already covered by awareness package
	//      unit tests (TestCompactionTrigger*) which mock the summarizer.
	//   3. The primitives tested here (ShouldCompact, OlderHalf, AppendSummary)
	//      are the exact methods services call internally — the service layer
	//      only adds scheduling and the summarizer callback.
	//
	// A future improvement could add a WithSummarizer option to the services
	// to allow injection of a mock summarizer for end-to-end compaction testing.
	dir := t.TempDir()
	rlPath := filepath.Join(dir, "compaction-test.md")

	rl, err := awareness.NewRollingLog(rlPath, awareness.WithCompactionThreshold(16000))
	if err != nil {
		t.Fatalf("new rolling log: %v", err)
	}

	// Generate >16000 chars of raw entries.
	totalChars := 0
	entryCount := 0
	for totalChars < 20000 {
		entry := fmt.Sprintf("12:00:%02d [tool] %s", entryCount%60, strings.Repeat("x", 200))
		if err := rl.AppendRaw(entry); err != nil {
			t.Fatalf("append raw %d: %v", entryCount, err)
		}
		totalChars += len(entry)
		entryCount++
	}

	if !rl.ShouldCompact() {
		t.Fatal("ShouldCompact() returned false after exceeding threshold")
	}

	rawBefore := rl.RawTailSize()
	if rawBefore == 0 {
		t.Fatal("raw tail size is 0 before compaction")
	}

	// Simulate compaction: extract older half and create a summary.
	olderText, err := rl.OlderHalf()
	if err != nil {
		t.Fatalf("OlderHalf: %v", err)
	}
	if olderText == "" {
		t.Fatal("OlderHalf returned empty string")
	}

	// Mock summarizer.
	summary := "### Compaction Summary\n\nSummary of: " + olderText[:min(50, len(olderText))]
	if err := rl.AppendSummary(summary); err != nil {
		t.Fatalf("AppendSummary: %v", err)
	}

	// Verify summaries section is populated.
	if rl.SummariesSize() == 0 {
		t.Fatal("summaries size is 0 after compaction")
	}

	// Verify raw tail is reduced.
	rawAfter := rl.RawTailSize()
	if rawAfter >= rawBefore {
		t.Errorf("raw tail not reduced after compaction: before=%d, after=%d", rawBefore, rawAfter)
	}

	// Verify summary content is meaningful via GetContext.
	summaries, raw, err := rl.GetContext()
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	if summaries == "" {
		t.Fatal("summaries empty after compaction")
	}
	if !strings.Contains(summaries, "Compaction Summary") {
		t.Errorf("summary doesn't contain expected text, got: %s", summaries[:min(100, len(summaries))])
	}
	if raw == "" {
		t.Fatal("raw tail empty after compaction — should still have recent entries")
	}

	// Verify disk persistence: re-open the file and confirm compacted state
	// was flushed, not just held in memory.
	rl2, err := awareness.NewRollingLog(rlPath)
	if err != nil {
		t.Fatalf("reopen rolling log: %v", err)
	}
	sum2, raw2, err := rl2.GetContext()
	if err != nil {
		t.Fatalf("reopen GetContext: %v", err)
	}
	if sum2 == "" {
		t.Fatal("summaries empty after reopen — compaction not persisted to disk")
	}
	if !strings.Contains(sum2, "Compaction Summary") {
		t.Errorf("reopened summaries missing expected text, got: %s", sum2[:min(100, len(sum2))])
	}
	if raw2 == "" {
		t.Fatal("raw tail empty after reopen — compaction not persisted to disk")
	}
}

// ---------------------------------------------------------------------------
// AC #3 — Archival and FTS5 indexing
// ---------------------------------------------------------------------------

func TestEpic22_ArchivalAndFTS5(t *testing.T) {
	t.Parallel()
	// Part A tests the RollingLog archival mechanism (OlderSummaries → Archive
	// → CommitArchival). Part B tests FTS5 indexing and search with fixture
	// files placed directly in source directories.
	//
	// DESIGN NOTE (AC #3 divergence): The Indexer's Sync() deliberately skips
	// journals/archives/ and conversations/archives/ subtrees (indexer.go:253-262)
	// because they contain raw append-only data. This means archived files are
	// NOT indexed by FTS5, contrary to AC #3's stated requirement that "archived
	// files are indexed in FTS5." In practice, the main rolling log files
	// (journals/<session>.md, conversations/conversation.md) contain recent
	// summaries that ARE indexed; only older summaries moved to archives/ become
	// unsearchable. Part A and Part B therefore use separate directories —
	// Part A verifies the archival mechanism, Part B verifies FTS5 on top-level
	// source files.
	//
	// TODO: If archived content should be searchable, the Indexer needs to walk
	// archives/ subdirectories (or a separate index job for archives is needed).
	dir := t.TempDir()

	// --- Part A: Test RollingLog archival ---
	rlPath := filepath.Join(dir, "archival-test.md")
	rl, err := awareness.NewRollingLog(rlPath, awareness.WithSummaryBudget(10000))
	if err != nil {
		t.Fatalf("new rolling log: %v", err)
	}

	// Generate summaries exceeding 10000 chars budget.
	totalSummaryChars := 0
	summaryCount := 0
	for totalSummaryChars < 15000 {
		summary := fmt.Sprintf("### Summary %d\n\nDetailed summary about indexing and archival processing for test iteration %d with keyword searchable_content.", summaryCount, summaryCount)
		if err := rl.AppendSummary(summary); err != nil {
			t.Fatalf("append summary %d: %v", summaryCount, err)
		}
		totalSummaryChars += len(summary)
		summaryCount++
	}

	if !rl.ShouldArchive() {
		t.Fatal("ShouldArchive() returned false after exceeding budget")
	}

	olderSummaries := rl.OlderSummaries()
	if len(olderSummaries) == 0 {
		t.Fatal("OlderSummaries returned empty slice")
	}

	// Archive to a subdirectory.
	archiveDir := filepath.Join(dir, "journals", "archives", "test-session")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("mkdir archive: %v", err)
	}

	today := time.Now().UTC().Format("2006-01-02")
	if err := rl.Archive(archiveDir, olderSummaries, today); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := rl.CommitArchival(len(olderSummaries)); err != nil {
		t.Fatalf("CommitArchival: %v", err)
	}

	// Verify archive file exists with expected content.
	archiveFile := filepath.Join(archiveDir, today+".md")
	data, err := os.ReadFile(archiveFile)
	if err != nil {
		t.Fatalf("read archive file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("archive file is empty")
	}
	if !strings.Contains(string(data), "searchable_content") {
		t.Error("archive file doesn't contain expected keyword 'searchable_content'")
	}

	// Verify budget is now within limits.
	if rl.ShouldArchive() {
		t.Error("ShouldArchive() still true after archival")
	}

	// --- Part B: Test FTS5 indexing with fixture files ---
	// Indexer.Sync() walks journals/, conversations/, topics/ (skipping
	// archives/ and logs/ subtrees). Place fixtures directly in source dirs.
	indexDir := t.TempDir()
	for _, sub := range []string{"conversations", "journals", "topics"} {
		if err := os.MkdirAll(filepath.Join(indexDir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}

	fixtures := map[string]string{
		"journals/archived-summary.md":     "## Journal Archive\n\nSummary about integration_validation_keyword and archival processing.",
		"conversations/archived-dialog.md": "## Conversation Archive\n\nDialog about integration_validation_keyword and memory search.",
		"topics/test-topic.md":             "## Topic Notes\n\nNotes about integration_validation_keyword and project patterns.",
	}
	for relPath, content := range fixtures {
		absPath := filepath.Join(indexDir, relPath)
		if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", relPath, err)
		}
	}

	indexer := awareness.NewIndexer(indexDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := indexer.Open(ctx); err != nil {
		t.Fatalf("open indexer: %v", err)
	}
	defer indexer.Close()

	if err := indexer.Sync(ctx); err != nil {
		t.Fatalf("sync indexer: %v", err)
	}

	// Negative test: non-existent keyword returns empty results.
	emptyResults, err := indexer.SearchFTS(ctx, awareness.SearchOptions{
		Query:  "xyz_nonexistent_term_42",
		Source: "all",
	})
	if err != nil {
		t.Fatalf("SearchFTS negative: %v", err)
	}
	if len(emptyResults) != 0 {
		t.Errorf("expected 0 results for non-existent keyword, got %d", len(emptyResults))
	}

	// Search for the shared keyword with source=journals.
	results, err := indexer.SearchFTS(ctx, awareness.SearchOptions{
		Query:  "integration_validation_keyword",
		Source: "journals",
	})
	if err != nil {
		t.Fatalf("SearchFTS journals: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected non-empty results for source=journals search")
	}
	for _, r := range results {
		if r.FileType != "journals" {
			t.Errorf("expected file_type 'journals', got %q in %s", r.FileType, r.Path)
		}
	}

	// Search with source=conversations.
	convResults, err := indexer.SearchFTS(ctx, awareness.SearchOptions{
		Query:  "integration_validation_keyword",
		Source: "conversations",
	})
	if err != nil {
		t.Fatalf("SearchFTS conversations: %v", err)
	}
	if len(convResults) == 0 {
		t.Fatal("expected non-empty results for source=conversations in Part B")
	}
	for _, r := range convResults {
		if r.FileType != "conversations" {
			t.Errorf("expected file_type 'conversations', got %q in %s", r.FileType, r.Path)
		}
	}

	// Search with source=topics.
	topicResults, err := indexer.SearchFTS(ctx, awareness.SearchOptions{
		Query:  "integration_validation_keyword",
		Source: "topics",
	})
	if err != nil {
		t.Fatalf("SearchFTS topics: %v", err)
	}
	if len(topicResults) == 0 {
		t.Fatal("expected non-empty results for source=topics in Part B")
	}
	for _, r := range topicResults {
		// "topics" directory maps to FileType "topic" (singular).
		if r.FileType != "topic" {
			t.Errorf("expected file_type 'topic', got %q in %s", r.FileType, r.Path)
		}
	}

	// --- Part C: Verify archived files are NOT indexed (design divergence guard) ---
	// Indexer.Sync() deliberately skips journals/archives/ and conversations/archives/
	// subtrees (indexer.go:259-262). This negative test ensures that behavior holds:
	// if someone removes the SkipDir guard, this test will catch the regression.
	archiveUniqueTerm := "archived_content_must_not_be_indexed"
	for _, sub := range []string{
		filepath.Join("journals", "archives", "test-session"),
		filepath.Join("conversations", "archives"),
	} {
		archDir := filepath.Join(indexDir, sub)
		if err := os.MkdirAll(archDir, 0o755); err != nil {
			t.Fatalf("mkdir archive %s: %v", sub, err)
		}
		if err := os.WriteFile(filepath.Join(archDir, "archived.md"), []byte("## Archived\n\n"+archiveUniqueTerm), 0o644); err != nil {
			t.Fatalf("write archive fixture %s: %v", sub, err)
		}
	}

	if err := indexer.Sync(ctx); err != nil {
		t.Fatalf("re-sync after archive fixtures: %v", err)
	}

	archivedResults, err := indexer.SearchFTS(ctx, awareness.SearchOptions{
		Query:  archiveUniqueTerm,
		Source: "all",
	})
	if err != nil {
		t.Fatalf("SearchFTS archived content: %v", err)
	}
	if len(archivedResults) != 0 {
		t.Errorf("expected 0 results for archived content (Indexer should skip archives/), got %d", len(archivedResults))
	}
}

// ---------------------------------------------------------------------------
// AC #4 — Prompt context injection (both streams)
// ---------------------------------------------------------------------------

// epic22PromptEngine captures PromptBuildRequest with a channel for async waiting.
type epic22PromptEngine struct {
	ch chan intentrouter.PromptBuildRequest
}

func newEpic22PromptEngine() *epic22PromptEngine {
	return &epic22PromptEngine{ch: make(chan intentrouter.PromptBuildRequest, 3)}
}

func (m *epic22PromptEngine) Build(req intentrouter.PromptBuildRequest) (*intentrouter.PromptBuildResponse, error) {
	// Non-blocking send to prevent deadlock if channel is unexpectedly full.
	select {
	case m.ch <- req:
	default:
	}
	return &intentrouter.PromptBuildResponse{
		SystemPrompt: "mock system prompt",
		UserPrompt:   "mock user prompt",
	}, nil
}

// epic22MockAdapter satisfies the IntentAdapter interface for Epic 22 tests.
type epic22MockAdapter struct{}

func (m *epic22MockAdapter) ResolveIntent(_ context.Context, req intentrouter.IntentRequest) (*intentrouter.IntentResponse, error) {
	return &intentrouter.IntentResponse{
		PromptID: req.PromptID,
		Actions: []intentrouter.IntentAction{
			{Type: intentrouter.ActionSpeak, Text: "ok"},
		},
	}, nil
}

func (m *epic22MockAdapter) Name() string { return "epic22-mock-adapter" }
func (m *epic22MockAdapter) Ready() bool  { return true }

func TestEpic22_PromptContextInjection(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	engine := newEpic22PromptEngine()
	adapter := &epic22MockAdapter{}

	// Reuse existing mocks from session_intent_routing_integration_test.go.
	journalProvider := &mockJournalProvider{
		summaries: "JOURNAL_SUMMARIES_MARKER",
		raw:       "JOURNAL_RAW_MARKER",
	}
	convLogProvider := &mockConversationLogProvider{
		summaries: "CONV_SUMMARIES_MARKER",
		raw:       "CONV_RAW_MARKER",
	}

	svc := intentrouter.NewService(bus,
		intentrouter.WithAdapter(adapter),
		intentrouter.WithPromptEngine(engine),
		intentrouter.WithJournalProvider(journalProvider),
		intentrouter.WithConversationLogProvider(convLogProvider),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start intent router: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// --- Test A: user_intent trigger — should include conversation context ---
	eventbus.Publish(ctx, bus, eventbus.Conversation.Prompt, eventbus.SourceConversation,
		eventbus.ConversationPromptEvent{
			SessionID: "test-session",
			PromptID:  "prompt-user-intent",
			NewMessage: eventbus.ConversationMessage{
				Origin: eventbus.OriginUser,
				Text:   "hello",
			},
			Metadata: map[string]string{
				constants.MetadataKeyEventType: constants.PromptEventUserIntent,
			},
		})

	select {
	case req := <-engine.ch:
		if req.ConversationSummaries != "CONV_SUMMARIES_MARKER" {
			t.Errorf("user_intent: ConversationSummaries = %q, want CONV_SUMMARIES_MARKER", req.ConversationSummaries)
		}
		if req.ConversationRaw != "CONV_RAW_MARKER" {
			t.Errorf("user_intent: ConversationRaw = %q, want CONV_RAW_MARKER", req.ConversationRaw)
		}
		// user_intent WITH a session ID → both conversation AND journal context are
		// fetched (intent router calls JournalProvider when targetSession != "").
		// Journal would be empty only for events without a session ID — not tested
		// here because ConversationPromptEvent always has a SessionID.
		if req.JournalSummaries != "JOURNAL_SUMMARIES_MARKER" {
			t.Errorf("user_intent: JournalSummaries = %q, want JOURNAL_SUMMARIES_MARKER", req.JournalSummaries)
		}
		if req.JournalRaw != "JOURNAL_RAW_MARKER" {
			t.Errorf("user_intent: JournalRaw = %q, want JOURNAL_RAW_MARKER", req.JournalRaw)
		}
	case <-time.After(testEventTimeout):
		t.Fatal("timeout waiting for user_intent prompt build")
	}

	// --- Test B: session_output trigger with target session — should include BOTH ---
	eventbus.Publish(ctx, bus, eventbus.Conversation.Prompt, eventbus.SourceConversation,
		eventbus.ConversationPromptEvent{
			SessionID: "target-session",
			PromptID:  "prompt-session-output",
			NewMessage: eventbus.ConversationMessage{
				Origin: eventbus.OriginTool,
				Text:   "session output text",
			},
			Metadata: map[string]string{
				constants.MetadataKeyEventType:    constants.PromptEventSessionOutput,
				constants.MetadataKeySessionOutput: "some output",
			},
		})

	select {
	case req := <-engine.ch:
		if req.ConversationSummaries != "CONV_SUMMARIES_MARKER" {
			t.Errorf("session_output: ConversationSummaries = %q, want CONV_SUMMARIES_MARKER", req.ConversationSummaries)
		}
		if req.ConversationRaw != "CONV_RAW_MARKER" {
			t.Errorf("session_output: ConversationRaw = %q, want CONV_RAW_MARKER", req.ConversationRaw)
		}
		if req.JournalSummaries != "JOURNAL_SUMMARIES_MARKER" {
			t.Errorf("session_output: JournalSummaries = %q, want JOURNAL_SUMMARIES_MARKER", req.JournalSummaries)
		}
		if req.JournalRaw != "JOURNAL_RAW_MARKER" {
			t.Errorf("session_output: JournalRaw = %q, want JOURNAL_RAW_MARKER", req.JournalRaw)
		}
	case <-time.After(testEventTimeout):
		t.Fatal("timeout waiting for session_output prompt build")
	}
}

// ---------------------------------------------------------------------------
// AC #5 — Search source scoping
// ---------------------------------------------------------------------------

func TestEpic22_SearchSourceScoping(t *testing.T) {
	t.Parallel()
	dir := createAwarenessTestDirs(t)

	// Create fixture files in each source directory with a shared keyword
	// and unique per-source keywords.
	fixtures := map[string]string{
		"conversations/scope-test.md": "## Conversation\n\nShared scope_keyword. Unique conversation_only_keyword.",
		"journals/scope-test.md":      "## Journal\n\nShared scope_keyword. Unique journal_only_keyword.",
		"topics/scope-test.md":        "## Topic\n\nShared scope_keyword. Unique topic_only_keyword.",
	}
	for relPath, content := range fixtures {
		if err := os.WriteFile(filepath.Join(dir, relPath), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", relPath, err)
		}
	}

	indexer := awareness.NewIndexer(dir)
	ctx := context.Background()
	if err := indexer.Open(ctx); err != nil {
		t.Fatalf("open indexer: %v", err)
	}
	// Use t.Cleanup instead of defer so the indexer stays open until all
	// parallel subtests complete. defer would close it when the parent
	// function returns, before t.Parallel subtests start.
	t.Cleanup(func() { indexer.Close() })

	if err := indexer.Sync(ctx); err != nil {
		t.Fatalf("sync indexer: %v", err)
	}

	// source=conversations → only conversation results.
	t.Run("conversations", func(t *testing.T) {
		t.Parallel()
		results, err := indexer.SearchFTS(ctx, awareness.SearchOptions{
			Query:  "scope_keyword",
			Source: "conversations",
		})
		if err != nil {
			t.Fatalf("SearchFTS: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("expected non-empty results for source=conversations")
		}
		for _, r := range results {
			if r.FileType != "conversations" {
				t.Errorf("got file_type %q, want 'conversations'", r.FileType)
			}
		}
	})

	// source=journals → only journal results.
	t.Run("journals", func(t *testing.T) {
		t.Parallel()
		results, err := indexer.SearchFTS(ctx, awareness.SearchOptions{
			Query:  "scope_keyword",
			Source: "journals",
		})
		if err != nil {
			t.Fatalf("SearchFTS: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("expected non-empty results for source=journals")
		}
		for _, r := range results {
			if r.FileType != "journals" {
				t.Errorf("got file_type %q, want 'journals'", r.FileType)
			}
		}
	})

	// source=topics → only topic results.
	// Note: "topics" (plural) maps to file_type "topic" (singular) in the DB
	// via resolveSourceFilter() in awareness/search.go.
	t.Run("topics", func(t *testing.T) {
		t.Parallel()
		results, err := indexer.SearchFTS(ctx, awareness.SearchOptions{
			Query:  "scope_keyword",
			Source: "topics",
		})
		if err != nil {
			t.Fatalf("SearchFTS: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("expected non-empty results for source=topics")
		}
		for _, r := range results {
			if r.FileType != "topic" {
				t.Errorf("got file_type %q, want 'topic'", r.FileType)
			}
		}
	})

	// source=all → results from all sources.
	t.Run("all", func(t *testing.T) {
		t.Parallel()
		results, err := indexer.SearchFTS(ctx, awareness.SearchOptions{
			Query:  "scope_keyword",
			Source: "all",
		})
		if err != nil {
			t.Fatalf("SearchFTS: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("expected non-empty results for source=all")
		}
		fileTypes := make(map[string]bool)
		for _, r := range results {
			fileTypes[r.FileType] = true
		}
		if !fileTypes["conversations"] {
			t.Error("source=all missing conversations results")
		}
		if !fileTypes["journals"] {
			t.Error("source=all missing journals results")
		}
		// Note: "topics" directory maps to FileType "topic" (singular) in the DB.
		// See awareness.classifyFile() for the mapping.
		if !fileTypes["topic"] {
			t.Error("source=all missing topic results")
		}
	})
}

// ---------------------------------------------------------------------------
// AC #6 — Event contract safety
// ---------------------------------------------------------------------------

func TestEpic22_EventContractSafety(t *testing.T) {
	t.Parallel()
	banned := []string{"history_summary", "memory_flush", "session_slug"}

	// Resolve module root from this test's source file location,
	// scanning the full Go codebase (not just internal/) per AC #6.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile = .../internal/integration/epic22_integration_test.go
	// filepath.Dir three times → module root
	moduleRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))

	if _, err := os.Stat(moduleRoot); err != nil {
		t.Fatalf("module root not found at %s: %v", moduleRoot, err)
	}

	var violations []string
	filesScanned := 0

	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == "vendor" || base == "clients" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".pb.go") {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		filesScanned++

		// Use go/scanner for proper tokenization — correctly handles
		// string literals, block comments, and inline comments without
		// manual parsing that can miss edge cases (e.g., // inside strings).
		fset := token.NewFileSet()
		var s scanner.Scanner
		file := fset.AddFile(path, fset.Base(), len(src))
		s.Init(file, src, nil, scanner.ScanComments)

		for {
			pos, tok, lit := s.Scan()
			if tok == token.EOF {
				break
			}
			if tok == token.COMMENT {
				continue
			}
			for _, term := range banned {
				if strings.Contains(lit, term) {
					position := fset.Position(pos)
					violations = append(violations, fmt.Sprintf("%s:%d contains %q: %s",
						path, position.Line, term, lit))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk error: %v", err)
	}

	if filesScanned == 0 {
		t.Fatal("event contract safety scan found 0 .go files — moduleRoot may be wrong")
	}

	if len(violations) > 0 {
		t.Errorf("found %d legacy event type references in production code (scanned %d files):\n%s",
			len(violations), filesScanned, strings.Join(violations, "\n"))
	}
}

// ---------------------------------------------------------------------------
// AC #7 — API regression: GetConversation
// ---------------------------------------------------------------------------

func TestEpic22_GetConversationAPI(t *testing.T) {
	t.Parallel()
	// This test validates ConversationLogService.Slice() which is the single
	// method backing the GetConversation gRPC endpoint. The gRPC handler
	// (sessionsService.GetConversation) is an unexported thin wrapper that adds
	// proto serialization and pagination defaults — covered by server package
	// unit tests (TestSessionsServiceGetConversation). Testing through the full
	// gRPC handler from integration/ would require starting a real gRPC server.
	bus := eventbus.New()
	dataDir := createAwarenessTestDirs(t)
	conversationsDir := filepath.Join(dataDir, "conversations")

	ctx, cancel := context.WithCancel(context.Background())

	convLogSvc := awareness.NewConversationLogService(bus, conversationsDir)
	if err := convLogSvc.Start(ctx); err != nil {
		t.Fatalf("start convlog: %v", err)
	}
	// Use t.Cleanup instead of defer so the service stays alive for parallel
	// subtests (defer fires when the parent returns, before subtests run).
	t.Cleanup(func() {
		convLogSvc.Shutdown(context.Background())
		cancel()
	})

	// Publish known turns.
	expectedTurnCount := 4
	turns := []struct {
		origin eventbus.ContentOrigin
		text   string
	}{
		{eventbus.OriginUser, "turn A from user"},
		{eventbus.OriginAI, "turn B from AI"},
		{eventbus.OriginUser, "turn C from user"},
		{eventbus.OriginAI, "turn D from AI"},
	}
	for _, turn := range turns {
		publishConversationTurn(bus, "api-test-session", turn.origin, turn.text)
	}

	// Poll until all turns are available instead of time.Sleep.
	waitForCondition(t, testEventTimeout, "Slice did not return expected turn count", func() bool {
		total, _, err := convLogSvc.Slice(0, 10)
		return err == nil && total == expectedTurnCount
	})

	// ConversationLogService implements server.ConversationStore (compile-time
	// assertion in server/conversation_store_assertion_test.go). Test Slice()
	// directly — this is the exact method GetConversation gRPC handler calls.
	var _ server.ConversationStore = convLogSvc // compile-time check
	total, slicedTurns, err := convLogSvc.Slice(0, 10)
	if err != nil {
		t.Fatalf("Slice: %v", err)
	}
	if total == 0 {
		t.Fatal("Slice returned total=0, expected turns")
	}
	if len(slicedTurns) == 0 {
		t.Fatal("Slice returned empty turns")
	}
	if total != len(turns) {
		t.Errorf("Slice total=%d, want %d", total, len(turns))
	}

	// Verify turn content.
	if slicedTurns[0].Text != "turn A from user" {
		t.Errorf("first turn text = %q, want 'turn A from user'", slicedTurns[0].Text)
	}
	if slicedTurns[0].Origin != eventbus.OriginUser {
		t.Errorf("first turn origin = %s, want user", slicedTurns[0].Origin)
	}

	// Verify complete turn ordering (not just first turn).
	expectedTexts := []string{"turn A from user", "turn B from AI", "turn C from user", "turn D from AI"}
	for i, want := range expectedTexts {
		if i >= len(slicedTurns) {
			t.Errorf("missing turn %d: want %q", i, want)
			continue
		}
		if slicedTurns[i].Text != want {
			t.Errorf("turn[%d].Text = %q, want %q", i, slicedTurns[i].Text, want)
		}
	}

	// Test pagination scenarios (table-driven).
	pagTests := []struct {
		name     string
		offset   int
		limit    int
		wantLen  int
		wantMore bool
	}{
		{"mid-page", 1, 2, 2, true},     // 1+2=3 < 4 → has_more
		{"last-page", 2, 2, 2, false},    // 2+2=4 = total → no more
		{"beyond-range", 100, 10, 0, false},
	}
	for _, tt := range pagTests {
		tt := tt // capture range variable for parallel subtests
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotTotal, page, err := convLogSvc.Slice(tt.offset, tt.limit)
			if err != nil {
				t.Fatalf("Slice(%d, %d): %v", tt.offset, tt.limit, err)
			}
			if gotTotal != expectedTurnCount {
				t.Errorf("total=%d, want %d", gotTotal, expectedTurnCount)
			}
			if len(page) != tt.wantLen {
				t.Errorf("len=%d, want %d", len(page), tt.wantLen)
			}
			hasMore := tt.offset+len(page) < gotTotal
			if hasMore != tt.wantMore {
				t.Errorf("has_more=%v, want %v", hasMore, tt.wantMore)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC #8 — Idle actionability bypass
// ---------------------------------------------------------------------------

func TestEpic22_IdleActionabilityBypass(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()

	convSvc := conversation.NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Subscribe to conversation.prompt BEFORE starting the service.
	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt, eventbus.WithSubscriptionName("test_idle_bypass"))
	defer promptSub.Close()

	if err := convSvc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer convSvc.Shutdown(context.Background())

	sessionID := "idle-test-session"

	// Reset rate limit to ensure clean state.
	convSvc.ResetRateLimitForTesting(sessionID, time.Time{})

	// Publish a pipeline message with waiting_for=user_input annotation.
	// This simulates a tool that is idle and waiting for user input.
	publishPipelineCleaned(bus, sessionID, eventbus.OriginTool, "waiting for input", map[string]string{
		constants.MetadataKeyNotable:    constants.MetadataValueTrue,
		constants.MetadataKeyWaitingFor: "user_input",
	})

	// The ConversationPromptEvent should arrive quickly (bypassing 2s rate limit).
	timer := time.NewTimer(1 * time.Second)
	defer timer.Stop()

	select {
	case env, ok := <-promptSub.C():
		if !ok {
			t.Fatal("prompt subscription closed")
		}
		prompt := env.Payload
		if prompt.SessionID != sessionID {
			t.Errorf("prompt SessionID = %q, want %q", prompt.SessionID, sessionID)
		}
		// Verify the waiting_for annotation is present in metadata.
		if wf := prompt.Metadata[constants.MetadataKeyWaitingFor]; wf != "user_input" {
			t.Errorf("prompt Metadata[waiting_for] = %q, want 'user_input'", wf)
		}
	case <-timer.C:
		t.Fatal("timeout waiting for ConversationPromptEvent — idle bypass did not work")
	}

	// Verify 500ms throttle with drop semantics: shouldBypassForWaitingFor()
	// returns false for events within the throttle window, and the event is
	// silently discarded (NOT queued for later delivery). We wait 600ms —
	// longer than the 500ms throttle window — to confirm the event is
	// permanently dropped, not just delayed.
	publishPipelineCleaned(bus, sessionID, eventbus.OriginTool, "second idle event", map[string]string{
		constants.MetadataKeyNotable:    constants.MetadataValueTrue,
		constants.MetadataKeyWaitingFor: "user_input",
	})

	throttleTimer := time.NewTimer(600 * time.Millisecond)
	defer throttleTimer.Stop()

	select {
	case <-promptSub.C():
		t.Error("expected second prompt to be dropped by throttle, but received it")
	case <-throttleTimer.C:
		// Expected: no prompt even after throttle window passes (drop semantics).
	}
}
