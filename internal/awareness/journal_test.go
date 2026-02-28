package awareness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/runtime"
)

// Compile-time interface assertions.
// NOTE: intentrouter.JournalProvider assertion lives in
// internal/intentrouter/journal_provider_test.go to avoid import cycle
// (awareness -> intentrouter -> awareness).
var _ runtime.Service = (*JournalService)(nil)

// newTestJournal creates a JournalService wired to a real event bus and temp dir.
// Returns the service, bus, base path, and a cleanup function.
func newTestJournal(t *testing.T) (*JournalService, *eventbus.Bus, string) {
	t.Helper()
	bus := eventbus.New()
	base := filepath.Join(t.TempDir(), "journals")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("create test journals dir: %v", err)
	}
	svc := NewJournalService(bus, base)
	return svc, bus, base
}

// startJournal starts the service with a background context and registers cleanup.
func startJournal(t *testing.T, svc *JournalService) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = svc.Shutdown(context.Background())
	})
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start journal service: %v", err)
	}
	return ctx
}

// waitForJournal waits until a journal entry appears in the active map.
// This ensures both the file is created AND the RollingLog is stored.
// All waitFor* helpers use 5ms polling which is effective across OS schedulers.
func waitForJournal(t *testing.T, svc *JournalService, sessionID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, ok := svc.activeLogs.Load(sessionID); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for journal active log: %s", sessionID)
}

// waitForRawEntry waits until the rolling log raw section contains the expected string.
func waitForRawEntry(t *testing.T, svc *JournalService, sessionID, expected string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, raw, err := svc.GetContext(sessionID)
		if err == nil && strings.Contains(raw, expected) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, raw, _ := svc.GetContext(sessionID)
	t.Fatalf("timeout waiting for raw entry containing %q, got: %q", expected, raw)
}

// waitForNoJournal waits until the session is removed from active map.
func waitForNoJournal(t *testing.T, svc *JournalService, sessionID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, ok := svc.activeLogs.Load(sessionID)
		if !ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for session %s to be removed from active map", sessionID)
}

// waitForLogFileHandle waits until the service has a cached log file handle for the session.
// This is needed because writeLogEntry runs after AppendRaw in the pipeline goroutine,
// so there's a brief window where the raw entry is visible but the fd isn't cached yet.
func waitForLogFileHandle(t *testing.T, svc *JournalService, sessionID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, ok := svc.logFiles.Load(sessionID); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for log file handle cache: %s", sessionID)
}

// waitForLogFileContent waits until a log file contains the expected string.
func waitForLogFileContent(t *testing.T, path, expected string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), expected) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("timeout waiting for log file %s to contain %q, got: %q", filepath.Base(path), expected, string(data))
}

func TestJournalCreation(t *testing.T) {
	t.Parallel()
	svc, bus, base := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 14, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-session-1"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "claude",
		State:     eventbus.SessionStateCreated,
	})

	// Wait for session_start entry which confirms both journal creation and file write.
	waitForRawEntry(t, svc, sessionID, "[session_start]", 2*time.Second)

	// Verify journal file was created on disk.
	journalPath := filepath.Join(base, sessionID+".md")
	if _, err := os.Stat(journalPath); os.IsNotExist(err) {
		t.Fatal("journal file not created")
	}

	// Verify persistent [session_start] entry content.
	_, raw, err := svc.GetContext(sessionID)
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	if !strings.Contains(raw, "Session: "+sessionID) {
		t.Errorf("session start entry missing session ID, got: %s", raw)
	}
	if !strings.Contains(raw, "Command: claude") {
		t.Errorf("session start entry missing command, got: %s", raw)
	}
	if !strings.Contains(raw, "14:00:00 [session_start]") {
		t.Errorf("session start entry missing timestamp prefix, got: %s", raw)
	}
	if !strings.Contains(raw, "Started: 2026-02-27T14:00:00Z") {
		t.Errorf("session start entry missing RFC3339 start time, got: %s", raw)
	}

	// Verify rolling log is in active map.
	_, ok := svc.activeLogs.Load(sessionID)
	if !ok {
		t.Error("session not found in active logs map")
	}

	// Verify [session_start] entry also appears in daily log file.
	logFile := filepath.Join(base, "logs", sessionID, fixedTime.Format("2006-01-02")+".md")
	waitForLogFileContent(t, logFile, "[session_start]", 2*time.Second)
}

func TestOutputAppend(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 14, 30, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-output-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Publish a tool output event.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Text:      "Hello, world!",
		Sequence:  1,
	})

	waitForRawEntry(t, svc, sessionID, "14:30:00 [output] Hello, world!", 2*time.Second)
}

func TestOutputAppendIgnoresNonToolOrigin(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	ctx := startJournal(t, svc)

	sessionID := "test-filter-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Publish a USER origin event — should be ignored.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginUser,
		Text:      "should not appear",
		Sequence:  1,
	})

	// Publish a tool-origin marker event and wait for it. Since the pipeline
	// consumer processes events sequentially, the marker arriving guarantees
	// the preceding user-origin event was already processed (and dropped).
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Text:      "nontool-filter-marker",
		Sequence:  2,
	})
	waitForRawEntry(t, svc, sessionID, "nontool-filter-marker", 2*time.Second)

	_, raw, err := svc.GetContext(sessionID)
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	if strings.Contains(raw, "should not appear") {
		t.Error("non-tool origin event was appended to journal")
	}
}

func TestOutputIgnoresEmptyText(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	ctx := startJournal(t, svc)

	sessionID := "test-empty-text"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Publish event with empty text and no idle annotation — should be ignored.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Text:      "",
		Sequence:  1,
	})

	// Publish marker and wait — guarantees the empty-text event was already processed.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Text:      "empty-text-marker",
		Sequence:  2,
	})
	waitForRawEntry(t, svc, sessionID, "empty-text-marker", 2*time.Second)

	_, raw, err := svc.GetContext(sessionID)
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	// Only expect 1 [output] entry (the marker). The empty-text event should not have produced one.
	if strings.Count(raw, "[output]") != 1 {
		t.Errorf("expected exactly 1 [output] entry (marker only), got %d, raw: %q", strings.Count(raw, "[output]"), raw)
	}
}

func TestLogFileWriting(t *testing.T) {
	t.Parallel()
	svc, bus, base := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 10, 15, 30, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-log-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Text:      "log entry data",
		Sequence:  1,
	})

	waitForRawEntry(t, svc, sessionID, "[output] log entry data", 2*time.Second)

	// Verify daily log file contains the output entry.
	logFile := filepath.Join(base, "logs", sessionID, fixedTime.Format("2006-01-02")+".md")
	waitForLogFileContent(t, logFile, "10:15:30 [output] log entry data", 2*time.Second)
}

func TestIdleEntries(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 9, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-idle-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "claude",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Publish event with waiting_for annotation.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID:   sessionID,
		Origin:      eventbus.OriginTool,
		Text:        "some text",
		Annotations: map[string]string{"waiting_for": "user_input"},
		Sequence:    1,
	})

	waitForRawEntry(t, svc, sessionID, "09:00:00 [idle] Waiting for: user_input", 2*time.Second)
}

func TestToolChange(t *testing.T) {
	t.Parallel()
	svc, bus, base := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 16, 45, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-tool-change-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "claude-code",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	eventbus.Publish(ctx, bus, eventbus.Sessions.ToolChanged, eventbus.SourceSessionManager, eventbus.SessionToolChangedEvent{
		SessionID:    sessionID,
		PreviousTool: "claude-code",
		NewTool:      "bash",
		Timestamp:    fixedTime,
	})

	waitForRawEntry(t, svc, sessionID, "16:45:00 [tool_change] claude-code -> bash", 2*time.Second)

	// Verify [tool_change] entry also appears in daily log file.
	logFile := filepath.Join(base, "logs", sessionID, fixedTime.Format("2006-01-02")+".md")
	waitForLogFileContent(t, logFile, "[tool_change] claude-code -> bash", 2*time.Second)
}

func TestSessionFinalization(t *testing.T) {
	t.Parallel()
	svc, bus, base := newTestJournal(t)
	ctx := startJournal(t, svc)

	sessionID := "test-finalize-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Verify session is in active map.
	if _, ok := svc.activeLogs.Load(sessionID); !ok {
		t.Fatal("session should be in active map after creation")
	}

	// Pre-set toolCache to verify cleanup on stop.
	svc.toolCache.Store(sessionID, "bash")

	// Stop the session.
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateStopped,
	})

	waitForNoJournal(t, svc, sessionID, 2*time.Second)

	// Journal file should still exist on disk.
	journalPath := filepath.Join(base, sessionID+".md")
	if _, err := os.Stat(journalPath); os.IsNotExist(err) {
		t.Error("journal file should persist after session stop")
	}

	// Verify toolCache is cleaned up after session stop.
	if _, ok := svc.toolCache.Load(sessionID); ok {
		t.Error("toolCache should be cleaned after session stop")
	}
}

func TestJournalGetContext(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-getcontext-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Add some entries.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Text:      "first output",
		Sequence:  1,
	})
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Text:      "second output",
		Sequence:  2,
	})

	waitForRawEntry(t, svc, sessionID, "second output", 2*time.Second)

	summaries, raw, err := svc.GetContext(sessionID)
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	// No summaries expected (no compaction yet).
	if summaries != "" {
		t.Errorf("expected empty summaries, got: %q", summaries)
	}
	if !strings.Contains(raw, "[output] first output") {
		t.Errorf("raw missing first output: %q", raw)
	}
	if !strings.Contains(raw, "[output] second output") {
		t.Errorf("raw missing second output: %q", raw)
	}
}

func TestJournalGetContextUnknownSession(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestJournal(t)
	_ = startJournal(t, svc)

	summaries, raw, err := svc.GetContext("nonexistent-session")
	if err != nil {
		t.Fatalf("GetContext should not error for unknown session: %v", err)
	}
	if summaries != "" || raw != "" {
		t.Errorf("expected empty strings for unknown session, got summaries=%q raw=%q", summaries, raw)
	}
}

func TestConcurrentSessions(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	var mu sync.Mutex
	var counter int
	baseTime := time.Date(2026, 2, 27, 8, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time {
		mu.Lock()
		counter++
		n := counter
		mu.Unlock()
		return baseTime.Add(time.Duration(n) * time.Second)
	}
	ctx := startJournal(t, svc)

	const numSessions = 5
	sessionIDs := make([]string, numSessions)
	for i := range numSessions {
		sessionIDs[i] = fmt.Sprintf("concurrent-%d", i)
	}

	// Create all sessions concurrently.
	var wg sync.WaitGroup
	for _, sid := range sessionIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
				SessionID: id,
				Label:     "bash",
				State:     eventbus.SessionStateCreated,
			})
		}(sid)
	}
	wg.Wait()

	// Wait for all journals to be created.
	for _, sid := range sessionIDs {
		waitForJournal(t, svc, sid, 2*time.Second)
	}

	// Publish events concurrently to different sessions.
	for _, sid := range sessionIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
				SessionID: id,
				Origin:    eventbus.OriginTool,
				Text:      "output for " + id,
				Sequence:  1,
			})
		}(sid)
	}
	wg.Wait()

	// Verify each session has its own entry.
	for _, sid := range sessionIDs {
		waitForRawEntry(t, svc, sid, "output for "+sid, 2*time.Second)
	}

	// Stop all sessions concurrently.
	for _, sid := range sessionIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
				SessionID: id,
				State:     eventbus.SessionStateStopped,
			})
		}(sid)
	}
	wg.Wait()

	// Verify all sessions removed from active map.
	for _, sid := range sessionIDs {
		waitForNoJournal(t, svc, sid, 2*time.Second)
	}
}

func TestJournalIgnoresEmptySessionID(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	ctx := startJournal(t, svc)

	// Create a sentinel session for deterministic barrier synchronization.
	sentinelID := "sentinel-empty-id"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sentinelID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sentinelID, 2*time.Second)

	// Send events with empty session IDs on all 3 topics.
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "",
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "",
		Origin:    eventbus.OriginTool,
		Text:      "should be ignored",
		Sequence:  1,
	})
	eventbus.Publish(ctx, bus, eventbus.Sessions.ToolChanged, eventbus.SourceSessionManager, eventbus.SessionToolChangedEvent{
		SessionID:    "",
		PreviousTool: "a",
		NewTool:      "b",
	})

	// Barrier: pipeline marker to sentinel — proves pipeline consumer processed
	// the empty-ID event sequentially before this one.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sentinelID,
		Origin:    eventbus.OriginTool,
		Text:      "empty-id-pipeline-barrier",
		Sequence:  2,
	})
	waitForRawEntry(t, svc, sentinelID, "empty-id-pipeline-barrier", 2*time.Second)

	// Barrier: tool_changed marker to sentinel.
	eventbus.Publish(ctx, bus, eventbus.Sessions.ToolChanged, eventbus.SourceSessionManager, eventbus.SessionToolChangedEvent{
		SessionID:    sentinelID,
		PreviousTool: "x",
		NewTool:      "y",
	})
	waitForRawEntry(t, svc, sentinelID, "[tool_change] x -> y", 2*time.Second)

	// Barrier: second sentinel creation proves lifecycle consumer processed
	// the empty-ID created event.
	sentinel2 := "sentinel-empty-id-2"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sentinel2,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sentinel2, 2*time.Second)

	// Only sentinel sessions should exist (no journals for empty ID).
	count := 0
	svc.activeLogs.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 2 {
		t.Errorf("expected 2 active journals (sentinels only), got %d", count)
	}
}

func TestOutputIgnoredWithoutActiveJournal(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	ctx := startJournal(t, svc)

	// Create a sentinel session for barrier synchronization.
	sentinelID := "sentinel-orphan-output"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sentinelID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sentinelID, 2*time.Second)

	// Publish output event for a session that was never created.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "nonexistent-session",
		Origin:    eventbus.OriginTool,
		Text:      "orphan output",
		Sequence:  1,
	})

	// Barrier: pipeline marker to sentinel — proves orphan event was processed.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sentinelID,
		Origin:    eventbus.OriginTool,
		Text:      "orphan-output-barrier",
		Sequence:  2,
	})
	waitForRawEntry(t, svc, sentinelID, "orphan-output-barrier", 2*time.Second)

	// GetContext should return empty for the nonexistent session.
	summaries, raw, err := svc.GetContext("nonexistent-session")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summaries != "" || raw != "" {
		t.Errorf("expected empty context, got summaries=%q raw=%q", summaries, raw)
	}
}

func TestToolChangeIgnoredWithoutActiveJournal(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	ctx := startJournal(t, svc)

	// Create a sentinel session for barrier synchronization.
	sentinelID := "sentinel-orphan-tool"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sentinelID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sentinelID, 2*time.Second)

	// Publish tool change event for a session that was never created.
	eventbus.Publish(ctx, bus, eventbus.Sessions.ToolChanged, eventbus.SourceSessionManager, eventbus.SessionToolChangedEvent{
		SessionID:    "nonexistent-session",
		PreviousTool: "bash",
		NewTool:      "claude-code",
	})

	// Barrier: tool_changed marker to sentinel — proves orphan event was processed.
	eventbus.Publish(ctx, bus, eventbus.Sessions.ToolChanged, eventbus.SourceSessionManager, eventbus.SessionToolChangedEvent{
		SessionID:    sentinelID,
		PreviousTool: "a",
		NewTool:      "b",
	})
	waitForRawEntry(t, svc, sentinelID, "[tool_change] a -> b", 2*time.Second)

	// GetContext should return empty — no journal was created.
	summaries, raw, err := svc.GetContext("nonexistent-session")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summaries != "" || raw != "" {
		t.Errorf("expected empty context, got summaries=%q raw=%q", summaries, raw)
	}
}

func TestStartWithNilBus(t *testing.T) {
	t.Parallel()
	svc := NewJournalService(nil, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start with nil bus should return nil, got: %v", err)
	}
	// Verify started remains false — nil bus short-circuits before setting started,
	// so a subsequent Start with a real bus will not be blocked.
	if svc.started.Load() {
		t.Error("started should remain false when bus is nil")
	}
}

func TestShutdownWithNilBus(t *testing.T) {
	t.Parallel()
	svc := NewJournalService(nil, t.TempDir())
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown with nil bus should return nil, got: %v", err)
	}
}

func TestToolCacheCleanedOnSessionStop(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	ctx := startJournal(t, svc)

	sessionID := "test-toolcache-cleanup"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Set tool cache via tool changed event.
	eventbus.Publish(ctx, bus, eventbus.Sessions.ToolChanged, eventbus.SourceSessionManager, eventbus.SessionToolChangedEvent{
		SessionID:    sessionID,
		PreviousTool: "bash",
		NewTool:      "claude-code",
	})
	waitForRawEntry(t, svc, sessionID, "[tool_change]", 2*time.Second)

	// Verify tool cache is set.
	if _, ok := svc.toolCache.Load(sessionID); !ok {
		t.Fatal("tool cache should be set after tool changed event")
	}

	// Stop session.
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateStopped,
	})
	waitForNoJournal(t, svc, sessionID, 2*time.Second)

	// Verify tool cache is cleaned up.
	if _, ok := svc.toolCache.Load(sessionID); ok {
		t.Error("tool cache should be cleaned up after session stop")
	}
}

func TestSessionStartPersistsAfterReopen(t *testing.T) {
	t.Parallel()
	svc, bus, base := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 14, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-persist-reopen"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "claude",
		State:     eventbus.SessionStateCreated,
	})
	waitForRawEntry(t, svc, sessionID, "[session_start]", 2*time.Second)

	// Re-open the same rolling log file from disk to verify persistence.
	journalPath := filepath.Join(base, sessionID+".md")
	rl2, err := NewRollingLog(journalPath)
	if err != nil {
		t.Fatalf("reopen rolling log: %v", err)
	}
	_, raw, err := rl2.GetContext()
	if err != nil {
		t.Fatalf("GetContext after reopen: %v", err)
	}
	if !strings.Contains(raw, "[session_start]") {
		t.Errorf("session_start entry lost after reopen, got: %q", raw)
	}
	if !strings.Contains(raw, "Session: "+sessionID) {
		t.Errorf("session ID lost after reopen, got: %q", raw)
	}
	if !strings.Contains(raw, "Started: 2026-02-27T14:00:00Z") {
		t.Errorf("RFC3339 start time lost after reopen, got: %q", raw)
	}
}

func TestSessionStartWithToolCache(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 14, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-toolcache-start"

	// Pre-populate tool cache before the created event (e.g., tool change event
	// arrived before lifecycle event due to consumer goroutine ordering).
	svc.toolCache.Store(sessionID, "claude-code")

	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})

	waitForRawEntry(t, svc, sessionID, "[session_start]", 2*time.Second)

	_, raw, err := svc.GetContext(sessionID)
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	if !strings.Contains(raw, "Tool: claude-code") {
		t.Errorf("session start entry missing tool name, got: %s", raw)
	}
}

func TestSessionStopWritesEntry(t *testing.T) {
	t.Parallel()
	svc, bus, base := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 15, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-stop-entry"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Stop the session with exit code and reason.
	exitCode := 0
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateStopped,
		ExitCode:  &exitCode,
		Reason:    "user exit",
	})

	waitForNoJournal(t, svc, sessionID, 2*time.Second)

	// Read the journal file directly — the [session_stop] entry was written
	// before the journal was removed from active map.
	journalPath := filepath.Join(base, sessionID+".md")
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "[session_stop]") {
		t.Errorf("journal missing [session_stop] entry, got: %s", content)
	}
	if !strings.Contains(content, "ExitCode: 0") {
		t.Errorf("session stop entry missing exit code, got: %s", content)
	}
	if !strings.Contains(content, "Reason: user exit") {
		t.Errorf("session stop entry missing reason, got: %s", content)
	}

	// Verify [session_stop] entry also in daily log file.
	logFile := filepath.Join(base, "logs", sessionID, fixedTime.Format("2006-01-02")+".md")
	waitForLogFileContent(t, logFile, "[session_stop]", 2*time.Second)
}

func TestSanitizeForRollingLog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"no change needed", "hello world", "hello world"},
		{"triple newlines collapsed", "a\n\n\nb", "a\n\nb"},
		{"quadruple newlines collapsed", "a\n\n\n\nb", "a\n\nb"},
		{"multiple triple runs", "a\n\n\nb\n\n\nc", "a\n\nb\n\nc"},
		{"summaries tag stripped", "before<nupi:rolling-log:summaries>after", "beforeafter"},
		{"raw tag stripped", "before<nupi:rolling-log:raw>after", "beforeafter"},
		{"closing tags stripped", "x</nupi:rolling-log:summaries>y</nupi:rolling-log:raw>z", "xyz"},
		{"double newlines preserved", "a\n\nb", "a\n\nb"},
		{"crlf normalized", "a\r\nb\r\nc", "a\nb\nc"},
		{"standalone cr normalized", "a\rb\rc", "a\nb\nc"},
		{"crlf triple collapsed", "a\r\n\r\n\r\nb", "a\n\nb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeForRollingLog(tt.input)
			if got != tt.expected {
				t.Errorf("sanitizeForRollingLog(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestOutputWithTripleNewlinesIsSanitized(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 15, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-sanitize-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Publish output containing triple newlines that would normally be rejected.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Text:      "line1\n\n\nline2",
		Sequence:  1,
	})

	// The triple newlines should be collapsed, and the entry should appear.
	waitForRawEntry(t, svc, sessionID, "[output] line1\n\nline2", 2*time.Second)
}

func TestMultiLineOutput(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 11, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-multiline-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "npm",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	multiLine := "added 127 packages\naudited 312 packages\nfound 0 vulnerabilities"
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Text:      multiLine,
		Sequence:  1,
	})

	waitForRawEntry(t, svc, sessionID, "found 0 vulnerabilities", 2*time.Second)

	_, raw, err := svc.GetContext(sessionID)
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	if !strings.Contains(raw, "added 127 packages\naudited 312 packages") {
		t.Errorf("multi-line output not preserved correctly, got: %q", raw)
	}
}

func TestOversizedTextTruncated(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 13, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-truncate-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Create text that exceeds maxEntryTextBytes.
	bigText := strings.Repeat("x", maxEntryTextBytes+1000)
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Text:      bigText,
		Sequence:  1,
	})

	waitForRawEntry(t, svc, sessionID, "[truncated:", 2*time.Second)

	_, raw, err := svc.GetContext(sessionID)
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	// Annotation now reports the original event.Text length (pre-sanitization),
	// which matches len(bigText) here because the input is ASCII-only with no
	// CRLF sequences to normalize.
	if !strings.Contains(raw, fmt.Sprintf("[truncated: %d bytes original]", len(bigText))) {
		t.Errorf("truncation annotation missing, got tail: %q", raw[len(raw)-100:])
	}
}

func TestLogFileHandleCacheReuse(t *testing.T) {
	t.Parallel()
	svc, bus, base := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 10, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-logcache-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Send multiple output events — all should use the same cached file handle.
	for i := range 3 {
		eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
			SessionID: sessionID,
			Origin:    eventbus.OriginTool,
			Text:      fmt.Sprintf("entry-%d", i),
			Sequence:  uint64(i + 1),
		})
	}

	waitForRawEntry(t, svc, sessionID, "entry-2", 2*time.Second)

	// Verify cache entry exists.
	if _, ok := svc.logFiles.Load(sessionID); !ok {
		t.Error("log file handle should be cached after writes")
	}

	// Verify log file has all entries.
	logFile := filepath.Join(base, "logs", sessionID, fixedTime.Format("2006-01-02")+".md")
	waitForLogFileContent(t, logFile, "entry-2", 2*time.Second)

	// Stop session — cache should be cleaned up.
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		State:     eventbus.SessionStateStopped,
	})
	waitForNoJournal(t, svc, sessionID, 2*time.Second)

	if _, ok := svc.logFiles.Load(sessionID); ok {
		t.Error("log file handle should be removed after session stop")
	}
}

func TestLogFileHandleDateRollover(t *testing.T) {
	t.Parallel()
	svc, bus, base := newTestJournal(t)
	var mu sync.Mutex
	currentTime := time.Date(2026, 2, 27, 23, 59, 59, 0, time.UTC)
	svc.nowFunc = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return currentTime
	}
	ctx := startJournal(t, svc)

	sessionID := "test-rollover-session"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// First entry at 23:59:59 on day 1.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Text:      "day1-entry",
		Sequence:  1,
	})
	waitForRawEntry(t, svc, sessionID, "day1-entry", 2*time.Second)

	// Advance time to next day.
	mu.Lock()
	currentTime = time.Date(2026, 2, 28, 0, 0, 1, 0, time.UTC)
	mu.Unlock()

	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Text:      "day2-entry",
		Sequence:  2,
	})
	waitForRawEntry(t, svc, sessionID, "day2-entry", 2*time.Second)

	// Verify both daily log files exist.
	day1File := filepath.Join(base, "logs", sessionID, "2026-02-27.md")
	day2File := filepath.Join(base, "logs", sessionID, "2026-02-28.md")
	waitForLogFileContent(t, day1File, "day1-entry", 2*time.Second)
	waitForLogFileContent(t, day2File, "day2-entry", 2*time.Second)

	// Verify correct date partitioning — no cross-contamination between days.
	data1, err := os.ReadFile(day1File)
	if err != nil {
		t.Fatalf("read day1 file: %v", err)
	}
	if strings.Contains(string(data1), "day2-entry") {
		t.Error("day1 log file should NOT contain day2 entry")
	}

	data2, err := os.ReadFile(day2File)
	if err != nil {
		t.Fatalf("read day2 file: %v", err)
	}
	if strings.Contains(string(data2), "day1-entry") {
		t.Error("day2 log file should NOT contain day1 entry")
	}
}

func TestLogFuncCapturesErrors(t *testing.T) {
	t.Parallel()

	// Construct service with an unwritable base path from the start (instead of
	// mutating basePath after construction). AppendRaw succeeds via a valid
	// RollingLog injected into activeLogs; writeLogEntry fails because it cannot
	// create the logs/ subdirectory under the invalid base path.
	bus := eventbus.New()
	svc := NewJournalService(bus, "/dev/null/impossible-journals")

	var mu sync.Mutex
	var captured []string
	svc.logFunc = func(format string, args ...any) {
		mu.Lock()
		captured = append(captured, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	svc.nowFunc = func() time.Time { return time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC) }

	rlPath := filepath.Join(t.TempDir(), "valid.md")
	rl, err := NewRollingLog(rlPath)
	if err != nil {
		t.Fatalf("NewRollingLog: %v", err)
	}
	svc.activeLogs.Store("err-session", rl)

	svc.handleOutput(eventbus.PipelineMessageEvent{
		SessionID: "err-session",
		Origin:    eventbus.OriginTool,
		Text:      "trigger error",
		Sequence:  1,
	})

	mu.Lock()
	defer mu.Unlock()
	foundWriteLog := false
	for _, msg := range captured {
		if strings.Contains(msg, "[Journal] ERROR:") && strings.Contains(msg, "write log") {
			foundWriteLog = true
			break
		}
	}
	if !foundWriteLog {
		t.Errorf("logFunc should have captured a write-log error, got: %v", captured)
	}
}

func TestShutdownClosesLogFileHandles(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 10, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }

	ctx, cancel := context.WithCancel(context.Background())
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	sessionID := "test-shutdown-fds"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Send output to create a cached log file handle.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Text:      "some output",
		Sequence:  1,
	})
	waitForRawEntry(t, svc, sessionID, "some output", 2*time.Second)

	// Wait for log file handle to be cached. writeLogEntry runs sequentially
	// after AppendRaw in the pipeline goroutine, so there's a brief window
	// where the raw entry is visible but the log file handle isn't cached yet.
	waitForLogFileHandle(t, svc, sessionID, 2*time.Second)

	// Shutdown WITHOUT sending a stopped event — simulates daemon shutdown
	// while sessions are still active.
	cancel()
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// After shutdown, all cached log file handles must be closed and removed.
	count := 0
	svc.logFiles.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Errorf("expected 0 cached log file handles after shutdown, got %d", count)
	}
}

func TestShutdownExpiredContextStillClosesHandles(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 10, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }

	ctx, cancel := context.WithCancel(context.Background())
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	sessionID := "test-expired-ctx-fd"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Send output to create a cached log file handle.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID,
		Origin:    eventbus.OriginTool,
		Text:      "deadline output",
		Sequence:  1,
	})
	waitForRawEntry(t, svc, sessionID, "deadline output", 2*time.Second)

	// Wait for log file handle to be cached (same reason as TestShutdownClosesLogFileHandles).
	waitForLogFileHandle(t, svc, sessionID, 2*time.Second)

	// Cancel parent context so consumers begin exiting, then call Shutdown
	// with an already-expired context. Shutdown retries lifecycle.Wait with
	// a background deadline to ensure goroutines exit before file handle cleanup.
	cancel()
	expiredCtx, expiredCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expiredCancel()
	_ = svc.Shutdown(expiredCtx)

	// After Shutdown returns, all cached log file handles must be closed.
	// No polling needed — Shutdown's grace-wait ensures goroutines exited.
	count := 0
	svc.logFiles.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Errorf("expected 0 cached handles after shutdown with expired ctx, got %d", count)
	}
}

func TestDoubleStartReturnsError(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestJournal(t)
	ctx := startJournal(t, svc)
	_ = ctx

	// Second Start should return an error.
	if err := svc.Start(context.Background()); err == nil {
		t.Fatal("expected error on double Start, got nil")
	}
}

func TestIdleAnnotationWithXMLTagSanitized(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 9, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-idle-xml-sanitize"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Publish idle event with a waiting_for annotation containing XML delimiter tag.
	eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID:   sessionID,
		Origin:      eventbus.OriginTool,
		Text:        "some text",
		Annotations: map[string]string{"waiting_for": "input<nupi:rolling-log:raw>injection"},
		Sequence:    1,
	})

	// The XML tag should be stripped from the annotation.
	waitForRawEntry(t, svc, sessionID, "[idle] Waiting for: inputinjection", 2*time.Second)

	_, raw, _ := svc.GetContext(sessionID)
	if strings.Contains(raw, "<nupi:rolling-log:raw>") {
		t.Errorf("XML delimiter tag should be sanitized from idle annotation, got: %q", raw)
	}
}

func TestToolChangeWithXMLTagSanitized(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 16, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startJournal(t, svc)

	sessionID := "test-tool-xml-sanitize"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Publish tool change with tool names containing XML delimiter tags.
	eventbus.Publish(ctx, bus, eventbus.Sessions.ToolChanged, eventbus.SourceSessionManager, eventbus.SessionToolChangedEvent{
		SessionID:    sessionID,
		PreviousTool: "bash<nupi:rolling-log:summaries>",
		NewTool:      "<nupi:rolling-log:raw>claude",
		Timestamp:    fixedTime,
	})

	// Both tool names should have XML tags stripped.
	waitForRawEntry(t, svc, sessionID, "[tool_change] bash -> claude", 2*time.Second)

	_, raw, _ := svc.GetContext(sessionID)
	if strings.Contains(raw, "<nupi:rolling-log") {
		t.Errorf("XML delimiter tags should be sanitized from tool names, got: %q", raw)
	}
}

func TestStoppedWithoutCreatedLogsWarning(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)

	var mu sync.Mutex
	var captured []string
	svc.logFunc = func(format string, args ...any) {
		mu.Lock()
		captured = append(captured, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	ctx := startJournal(t, svc)

	// Create a sentinel session for barrier synchronization.
	sentinelID := "sentinel-orphan-stop"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sentinelID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sentinelID, 2*time.Second)

	// Send stopped event for a session that was never created.
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "never-created-session",
		State:     eventbus.SessionStateStopped,
	})

	// Barrier: create another sentinel to prove the stopped event was processed.
	sentinel2 := "sentinel-orphan-stop-2"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sentinel2,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sentinel2, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, msg := range captured {
		if strings.Contains(msg, "WARN") && strings.Contains(msg, "never-created-session") && strings.Contains(msg, "no active journal") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning for stopped event without active journal, captured: %v", captured)
	}
}

func TestSessionCreatedWithInvalidBasePath(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	svc := NewJournalService(bus, "/dev/null/impossible-journals")

	var mu sync.Mutex
	var captured []string
	svc.logFunc = func(format string, args ...any) {
		mu.Lock()
		captured = append(captured, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	svc.nowFunc = func() time.Time { return time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC) }

	// Call handleSessionCreated directly to test the error path without
	// needing event bus synchronization.
	svc.handleSessionCreated(eventbus.SessionLifecycleEvent{
		SessionID: "invalid-path-session",
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})

	// Session should NOT be in activeLogs when NewRollingLog fails.
	if _, ok := svc.activeLogs.Load("invalid-path-session"); ok {
		t.Error("session should NOT be in active map when NewRollingLog fails")
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, msg := range captured {
		if strings.Contains(msg, "ERROR") && strings.Contains(msg, "create rolling log") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error log for failed NewRollingLog, got: %v", captured)
	}
}

func TestStartAfterShutdown(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 10, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }

	// First Start → Shutdown cycle.
	ctx, cancel := context.WithCancel(context.Background())
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("first start: %v", err)
	}

	sessionID := "restart-session-1"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	cancel()
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}

	// Second Start — must work without errors.
	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel2()
		_ = svc.Shutdown(context.Background())
	})
	if err := svc.Start(ctx2); err != nil {
		t.Fatalf("second start after shutdown: %v", err)
	}

	sessionID2 := "restart-session-2"
	eventbus.Publish(ctx2, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID2,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID2, 2*time.Second)

	eventbus.Publish(ctx2, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: sessionID2,
		Origin:    eventbus.OriginTool,
		Text:      "after restart",
		Sequence:  1,
	})
	waitForRawEntry(t, svc, sessionID2, "after restart", 2*time.Second)
}

func TestConcurrentOutputAndStop(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestJournal(t)
	fixedTime := time.Date(2026, 2, 27, 10, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	svc.logFunc = func(format string, args ...any) {} // Suppress expected warnings
	ctx := startJournal(t, svc)

	sessionID := "test-concurrent-stop-output"
	eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: sessionID,
		Label:     "bash",
		State:     eventbus.SessionStateCreated,
	})
	waitForJournal(t, svc, sessionID, 2*time.Second)

	// Publish output and stop events concurrently. The service must not panic
	// or deadlock regardless of processing order.
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			eventbus.Publish(ctx, bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
				SessionID: sessionID,
				Origin:    eventbus.OriginTool,
				Text:      fmt.Sprintf("race-output-%d", n),
				Sequence:  uint64(n + 1),
			})
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		eventbus.Publish(ctx, bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
			SessionID: sessionID,
			State:     eventbus.SessionStateStopped,
		})
	}()
	wg.Wait()

	waitForNoJournal(t, svc, sessionID, 5*time.Second)
}
