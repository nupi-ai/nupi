package awareness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/runtime"
)

// Compile-time interface assertions.
// NOTE: intentrouter.ConversationLogProvider assertion lives in
// internal/intentrouter/conversation_log_provider_test.go to avoid import cycle.
var _ runtime.Service = (*ConversationLogService)(nil)

// newTestConvLog creates a ConversationLogService wired to a real event bus and temp dir.
func newTestConvLog(t *testing.T) (*ConversationLogService, *eventbus.Bus, string) {
	t.Helper()
	bus := eventbus.New()
	base := filepath.Join(t.TempDir(), "conversations")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("create test conversations dir: %v", err)
	}
	svc := NewConversationLogService(bus, base)
	return svc, bus, base
}

// startConvLog starts the service with a background context and registers cleanup.
func startConvLog(t *testing.T, svc *ConversationLogService) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = svc.Shutdown(context.Background())
	})
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation log service: %v", err)
	}
	return ctx
}

// waitForConversationFile waits until conversation.md exists on disk.
func waitForConversationFile(t *testing.T, base string, timeout time.Duration) {
	t.Helper()
	path := filepath.Join(base, "conversation.md")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for conversation.md to be created")
}

// waitForRawContent waits until GetContext raw section contains the expected string.
func waitForRawContent(t *testing.T, svc *ConversationLogService, expected string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, raw, err := svc.GetContext()
		if err == nil && strings.Contains(raw, expected) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, raw, _ := svc.GetContext()
	t.Fatalf("timeout waiting for raw content containing %q, got: %q", expected, raw)
}

// waitForLogContent waits until a log file contains the expected string.
func waitForLogContent(t *testing.T, path, expected string, timeout time.Duration) {
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

// publishTurn publishes a conversation turn event.
func publishTurn(ctx context.Context, bus *eventbus.Bus, origin eventbus.ContentOrigin, text string, at time.Time) {
	eventbus.Publish(ctx, bus, eventbus.Conversation.Turn, eventbus.SourceConversation,
		eventbus.ConversationTurnEvent{
			SessionID: "test-session",
			Turn: eventbus.ConversationTurn{
				Origin: origin,
				Text:   text,
				At:     at,
			},
		})
}

func TestTurnAppendAndFormat(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 10, 30, 0, 0, time.UTC)
	ctx := startConvLog(t, svc)

	// Test all four role types.
	tests := []struct {
		origin   eventbus.ContentOrigin
		text     string
		expected string
	}{
		{eventbus.OriginUser, "Can you also add rate limiting?", "10:30:00 [user] Can you also add rate limiting?"},
		{eventbus.OriginAI, "I'll add rate limiting.", "10:30:00 [ai] I'll add rate limiting."},
		{eventbus.OriginTool, `{"result": "ok"}`, `10:30:00 [tool] {"result": "ok"}`},
		{eventbus.OriginSystem, "Session reconnected", "10:30:00 [system] Session reconnected"},
	}

	for _, tt := range tests {
		publishTurn(ctx, bus, tt.origin, tt.text, fixedTime)
		waitForRawContent(t, svc, tt.expected, 2*time.Second)
	}

	// Verify all entries are in the raw context.
	_, raw, err := svc.GetContext()
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	for _, tt := range tests {
		if !strings.Contains(raw, tt.expected) {
			t.Errorf("raw context missing %q", tt.expected)
		}
	}
}

func TestDailyLogFileWriting(t *testing.T) {
	t.Parallel()
	svc, bus, base := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 14, 0, 0, 0, time.UTC)
	ctx := startConvLog(t, svc)

	publishTurn(ctx, bus, eventbus.OriginUser, "Hello world", fixedTime)
	waitForRawContent(t, svc, "[user] Hello world", 2*time.Second)

	logFile := filepath.Join(base, "logs", "2026-02-28.md")
	waitForLogContent(t, logFile, "14:00:00 [user] Hello world", 2*time.Second)
}

func TestDailyLogDateRollover(t *testing.T) {
	t.Parallel()
	svc, bus, base := newTestConvLog(t)
	ctx := startConvLog(t, svc)

	// Write on day 1. Turn.At drives the timestamp and daily log date.
	day1Time := time.Date(2026, 2, 28, 23, 59, 0, 0, time.UTC)
	publishTurn(ctx, bus, eventbus.OriginUser, "Day 1 message", day1Time)
	waitForRawContent(t, svc, "[user] Day 1 message", 2*time.Second)

	logFile1 := filepath.Join(base, "logs", "2026-02-28.md")
	waitForLogContent(t, logFile1, "Day 1 message", 2*time.Second)

	// Roll to next day by publishing a turn with a new date via Turn.At.
	day2Time := time.Date(2026, 3, 1, 0, 1, 0, 0, time.UTC)
	publishTurn(ctx, bus, eventbus.OriginUser, "Day 2 message", day2Time)
	waitForRawContent(t, svc, "[user] Day 2 message", 2*time.Second)

	logFile2 := filepath.Join(base, "logs", "2026-03-01.md")
	waitForLogContent(t, logFile2, "Day 2 message", 2*time.Second)

	// Verify stale day 1 handle was closed by getLogFile during date rollover.
	hasDay1Handle := false
	svc.logFiles.Range(func(key, _ any) bool {
		if key.(string) == "2026-02-28" {
			hasDay1Handle = true
		}
		return true
	})
	if hasDay1Handle {
		t.Error("day 1 log file handle should be closed after date rollover")
	}
}

func TestGetContextDelegation(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)
	ctx := startConvLog(t, svc)

	publishTurn(ctx, bus, eventbus.OriginUser, "test message", fixedTime)
	waitForRawContent(t, svc, "[user] test message", 2*time.Second)

	summaries, raw, err := svc.GetContext()
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	if summaries != "" {
		t.Errorf("expected empty summaries, got: %q", summaries)
	}
	if !strings.Contains(raw, "12:00:00 [user] test message") {
		t.Errorf("raw missing expected entry, got: %q", raw)
	}
}

func TestFirstStartupFileCreation(t *testing.T) {
	t.Parallel()
	svc, bus, base := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 8, 0, 0, 0, time.UTC)
	ctx := startConvLog(t, svc)

	// conversation.md should not exist yet — NewRollingLog does NOT create
	// the file; it is created on first AppendRaw via persistToFile.
	convPath := filepath.Join(base, "conversation.md")
	if _, err := os.Stat(convPath); err == nil {
		t.Log("conversation.md already exists before first turn (unexpected but non-fatal)")
	}

	// Publish first turn — file should be created with content.
	publishTurn(ctx, bus, eventbus.OriginUser, "First message", fixedTime)
	waitForRawContent(t, svc, "[user] First message", 2*time.Second)
	waitForConversationFile(t, base, 2*time.Second)

	// Verify file has content with correct RollingLog structure.
	data, err := os.ReadFile(convPath)
	if err != nil {
		t.Fatalf("read conversation.md: %v", err)
	}
	content := string(data)
	if len(content) == 0 {
		t.Fatal("conversation.md is empty after first turn")
	}
	// The RollingLog engine uses XML-like delimiter tags (not markdown headers
	// like the spec's conceptual format). Verify the structural tags are present.
	if !strings.Contains(content, "<nupi:rolling-log:raw>") {
		t.Errorf("conversation.md missing raw section tag, got:\n%s", content)
	}
	if !strings.Contains(content, "08:00:00 [user] First message") {
		t.Errorf("conversation.md missing formatted turn entry, got:\n%s", content)
	}
}

func TestConcurrentTurnProcessing(t *testing.T) {
	t.Parallel()
	svc, bus, base := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 15, 0, 0, 0, time.UTC)
	ctx := startConvLog(t, svc)

	// Publish many turns concurrently.
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			publishTurn(ctx, bus, eventbus.OriginUser, fmt.Sprintf("msg-%d", i), fixedTime)
		}(i)
	}
	wg.Wait()

	// Wait for all messages to appear in conversation.md.
	for i := 0; i < n; i++ {
		waitForRawContent(t, svc, fmt.Sprintf("msg-%d", i), 5*time.Second)
	}

	// Verify no race detector issues (go test -race).
	_, raw, err := svc.GetContext()
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}

	count := 0
	for i := 0; i < n; i++ {
		if strings.Contains(raw, fmt.Sprintf("msg-%d", i)) {
			count++
		}
	}
	if count != n {
		t.Errorf("expected %d messages in conversation.md, found %d", n, count)
	}

	// Also verify all entries appear in the daily log file (R5/L3).
	logFile := filepath.Join(base, "logs", "2026-02-28.md")
	for i := 0; i < n; i++ {
		waitForLogContent(t, logFile, fmt.Sprintf("msg-%d", i), 5*time.Second)
	}
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read daily log: %v", err)
	}
	logCount := 0
	for i := 0; i < n; i++ {
		if strings.Contains(string(logData), fmt.Sprintf("msg-%d", i)) {
			logCount++
		}
	}
	if logCount != n {
		t.Errorf("expected %d messages in daily log, found %d", n, logCount)
	}
}

func TestShutdownClosesFileHandles(t *testing.T) {
	t.Parallel()
	svc, bus, base := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 16, 0, 0, 0, time.UTC)

	ctx, cancel := context.WithCancel(context.Background())
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	publishTurn(ctx, bus, eventbus.OriginUser, "before shutdown", fixedTime)
	waitForRawContent(t, svc, "[user] before shutdown", 2*time.Second)

	// Verify log file handle is cached.
	logFile := filepath.Join(base, "logs", "2026-02-28.md")
	waitForLogContent(t, logFile, "before shutdown", 2*time.Second)

	// Check file handle exists.
	hasHandle := false
	svc.logFiles.Range(func(_, _ any) bool {
		hasHandle = true
		return false
	})
	if !hasHandle {
		t.Error("expected cached log file handle before shutdown")
	}

	cancel()
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Verify handles cleared.
	hasHandle = false
	svc.logFiles.Range(func(_, _ any) bool {
		hasHandle = true
		return false
	})
	if hasHandle {
		t.Error("log file handles not cleared after shutdown")
	}

	if svc.started.Load() {
		t.Error("started flag not cleared after shutdown")
	}
}

func TestStartShutdownRestartCycle(t *testing.T) {
	// Tests same-instance Shutdown→Start restart. Verifies that Start()
	// re-creates the RollingLog, re-subscribes, and processes new events
	// while preserving data from the previous cycle.
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 17, 0, 0, 0, time.UTC)

	// Cycle 1.
	ctx1, cancel1 := context.WithCancel(context.Background())
	if err := svc.Start(ctx1); err != nil {
		t.Fatalf("start cycle 1: %v", err)
	}
	publishTurn(ctx1, bus, eventbus.OriginUser, "cycle 1", fixedTime)
	waitForRawContent(t, svc, "[user] cycle 1", 2*time.Second)
	cancel1()
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown cycle 1: %v", err)
	}

	// Cycle 2 — same instance. Start() re-creates the RollingLog (which
	// was set to nil in Shutdown) and re-subscribes to the event bus.
	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel2()
		_ = svc.Shutdown(context.Background())
	})
	if err := svc.Start(ctx2); err != nil {
		t.Fatalf("start cycle 2 (same instance): %v", err)
	}

	// Verify cycle 1 data persisted and is loaded on restart.
	_, raw, err := svc.GetContext()
	if err != nil {
		t.Fatalf("GetContext cycle 2: %v", err)
	}
	if !strings.Contains(raw, "[user] cycle 1") {
		t.Errorf("cycle 1 data lost after restart, got: %q", raw)
	}

	// Write more data in cycle 2.
	publishTurn(ctx2, bus, eventbus.OriginAI, "cycle 2 response", fixedTime)
	waitForRawContent(t, svc, "[ai] cycle 2 response", 2*time.Second)
}

func TestEmptyWhitespaceTurnHandling(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 18, 0, 0, 0, time.UTC)
	ctx := startConvLog(t, svc)

	// Publish empty and whitespace-only turns.
	publishTurn(ctx, bus, eventbus.OriginUser, "", fixedTime)
	publishTurn(ctx, bus, eventbus.OriginUser, "   ", fixedTime)
	publishTurn(ctx, bus, eventbus.OriginUser, "\n\n", fixedTime)

	// Publish a real turn.
	publishTurn(ctx, bus, eventbus.OriginUser, "real message", fixedTime)
	waitForRawContent(t, svc, "[user] real message", 2*time.Second)

	// Verify only the real message is in the raw context.
	_, raw, err := svc.GetContext()
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	// Count entries by matching the timestamp pattern (format-independent).
	entryPattern := regexp.MustCompile(`\d{2}:\d{2}:\d{2} \[`)
	matches := entryPattern.FindAllString(raw, -1)
	if len(matches) != 1 {
		t.Errorf("expected 1 entry, found %d in: %q", len(matches), raw)
	}
}

func TestSanitization(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 19, 0, 0, 0, time.UTC)
	ctx := startConvLog(t, svc)

	// Test XML tag stripping.
	publishTurn(ctx, bus, eventbus.OriginAI, "before<nupi:rolling-log:raw>injected</nupi:rolling-log:raw>after", fixedTime)
	waitForRawContent(t, svc, "beforeinjectedafter", 2*time.Second)

	// Test triple newline collapsing.
	publishTurn(ctx, bus, eventbus.OriginUser, "line1\n\n\nline2", fixedTime)
	waitForRawContent(t, svc, "line1\n\nline2", 2*time.Second)

	// Test CR normalization.
	publishTurn(ctx, bus, eventbus.OriginUser, "crlf\r\ntext", fixedTime)
	waitForRawContent(t, svc, "crlf\ntext", 2*time.Second)

	publishTurn(ctx, bus, eventbus.OriginUser, "cr\rtext", fixedTime)
	waitForRawContent(t, svc, "cr\ntext", 2*time.Second)
}

func TestSanitizationTruncation(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 20, 0, 0, 0, time.UTC)
	ctx := startConvLog(t, svc)

	// Create a message larger than 64KB.
	bigText := strings.Repeat("x", 70*1024)
	publishTurn(ctx, bus, eventbus.OriginUser, bigText, fixedTime)
	waitForRawContent(t, svc, "[truncated:", 2*time.Second)
}

func TestGetContextBeforeAnyTurns(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestConvLog(t)
	_ = startConvLog(t, svc)

	summaries, raw, err := svc.GetContext()
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	if summaries != "" {
		t.Errorf("expected empty summaries, got: %q", summaries)
	}
	if raw != "" {
		t.Errorf("expected empty raw, got: %q", raw)
	}
}

func TestConvLogDoubleStartReturnsError(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestConvLog(t)
	_ = startConvLog(t, svc)

	err := svc.Start(context.Background())
	if err == nil {
		t.Fatal("expected error on double start")
	}
	if !strings.Contains(err.Error(), "already started") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOriginToRoleUnknownValue(t *testing.T) {
	t.Parallel()
	// Verify that an unrecognized ContentOrigin maps to "unknown", not "user".
	unknownOrigin := eventbus.ContentOrigin("unrecognized")
	got := originToRole(unknownOrigin)
	if got != "unknown" {
		t.Errorf("originToRole(%q) = %q, want %q", unknownOrigin, got, "unknown")
	}
}

func TestNewConversationLogServicePanicsOnRelativePath(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on relative basePath")
		}
	}()
	NewConversationLogService(eventbus.New(), "relative/path")
}

func TestTurnUsesEventTimestampNotNowFunc(t *testing.T) {
	// Verifies that handleTurn uses Turn.At (event-authoritative timestamp),
	// not nowFunc(). If the service used nowFunc(), the logged timestamp
	// would be 23:59:59 (nowFunc's value), not 14:05:00 (Turn.At's value).
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	// Set nowFunc to a DIFFERENT time than the turn's At field.
	svc.nowFunc = func() time.Time { return time.Date(2026, 2, 28, 23, 59, 59, 0, time.UTC) }
	ctx := startConvLog(t, svc)

	turnTime := time.Date(2026, 2, 28, 14, 5, 0, 0, time.UTC)
	publishTurn(ctx, bus, eventbus.OriginUser, "timestamp test", turnTime)

	// The entry should use Turn.At (14:05:00), NOT nowFunc (23:59:59).
	waitForRawContent(t, svc, "14:05:00 [user] timestamp test", 2*time.Second)

	// Verify nowFunc's time is NOT in the output.
	_, raw, _ := svc.GetContext()
	if strings.Contains(raw, "23:59:59") {
		t.Errorf("entry should use Turn.At, not nowFunc; got: %q", raw)
	}
}

func TestTurnZeroAtFallbackToNowFunc(t *testing.T) {
	// Verifies that when Turn.At is zero (defensive fallback), handleTurn
	// uses nowFunc() instead. This exercises the guard at conversation_log.go:158-159.
	t.Parallel()
	svc, bus, base := newTestConvLog(t)
	// Set nowFunc to a known time so we can assert it's used as fallback.
	svc.nowFunc = func() time.Time { return time.Date(2026, 2, 28, 9, 15, 30, 0, time.UTC) }
	ctx := startConvLog(t, svc)

	// Publish a turn with zero At (time.Time{}).
	eventbus.Publish(ctx, bus, eventbus.Conversation.Turn, eventbus.SourceConversation,
		eventbus.ConversationTurnEvent{
			SessionID: "test-session",
			Turn: eventbus.ConversationTurn{
				Origin: eventbus.OriginUser,
				Text:   "zero timestamp message",
				At:     time.Time{}, // zero value — triggers nowFunc fallback
			},
		})

	// The entry should use nowFunc's time (09:15:30), not a zero-formatted timestamp.
	waitForRawContent(t, svc, "09:15:30 [user] zero timestamp message", 2*time.Second)

	// Also verify the daily log file uses the correct date from nowFunc.
	logFile := filepath.Join(base, "logs", "2026-02-28.md")
	waitForLogContent(t, logFile, "09:15:30 [user] zero timestamp message", 2*time.Second)
}

func TestMultipleTurnsSameDailyLog(t *testing.T) {
	// Verifies that multiple sequential turns are correctly appended (not
	// overwritten) in the same YYYY-MM-DD.md daily log file.
	t.Parallel()
	svc, bus, base := newTestConvLog(t)
	fixedDate := time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)
	ctx := startConvLog(t, svc)

	turns := []struct {
		origin eventbus.ContentOrigin
		text   string
		offset time.Duration
	}{
		{eventbus.OriginUser, "first question", 10 * time.Minute},
		{eventbus.OriginAI, "first answer", 11 * time.Minute},
		{eventbus.OriginUser, "follow-up question", 15 * time.Minute},
		{eventbus.OriginTool, `{"status":"ok"}`, 16 * time.Minute},
		{eventbus.OriginAI, "final response", 17 * time.Minute},
	}

	for _, tt := range turns {
		publishTurn(ctx, bus, tt.origin, tt.text, fixedDate.Add(tt.offset))
	}

	// Wait for the last turn to appear in conversation.md.
	waitForRawContent(t, svc, "[ai] final response", 2*time.Second)

	// Verify ALL turns appear in the same daily log file.
	logFile := filepath.Join(base, "logs", "2026-02-28.md")
	for _, tt := range turns {
		ts := fixedDate.Add(tt.offset).Format("15:04:05")
		role := originToRole(tt.origin)
		expected := fmt.Sprintf("%s [%s] %s", ts, role, tt.text)
		waitForLogContent(t, logFile, expected, 2*time.Second)
	}

	// Read the full file and verify entry count matches.
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read daily log: %v", err)
	}
	entryPattern := regexp.MustCompile(`\d{2}:\d{2}:\d{2} \[`)
	matches := entryPattern.FindAllString(string(data), -1)
	if len(matches) != len(turns) {
		t.Errorf("expected %d entries in daily log, found %d:\n%s", len(turns), len(matches), string(data))
	}
}

func TestWriteLogEntryMkdirError(t *testing.T) {
	t.Parallel()
	svc, bus, base := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 21, 0, 0, 0, time.UTC)

	// Block the logs directory by placing a regular file where MkdirAll
	// expects to create a directory. MkdirAll will fail with "not a directory".
	logsPath := filepath.Join(base, "logs")
	if err := os.WriteFile(logsPath, []byte("blocker"), 0o600); err != nil {
		t.Fatalf("create blocker file: %v", err)
	}

	var mu sync.Mutex
	var logMsgs []string
	svc.logFunc = func(format string, args ...any) {
		mu.Lock()
		logMsgs = append(logMsgs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	ctx := startConvLog(t, svc)

	publishTurn(ctx, bus, eventbus.OriginUser, "should fail log write", fixedTime)
	// RollingLog write (conversation.md) should still succeed.
	waitForRawContent(t, svc, "[user] should fail log write", 2*time.Second)

	// Wait for the error to be logged (async processing).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		found := false
		for _, msg := range logMsgs {
			if strings.Contains(msg, "ERROR") && strings.Contains(msg, "write log") {
				found = true
			}
		}
		mu.Unlock()
		if found {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Errorf("expected error log for failed log directory creation, got: %v", logMsgs)
}

func TestGetContextAfterShutdown(t *testing.T) {
	// Verifies that GetContext returns empty strings after Shutdown.
	// Exercises the atomic.Pointer[RollingLog] nil-load path.
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 22, 0, 0, 0, time.UTC)

	ctx, cancel := context.WithCancel(context.Background())
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Write a turn so there's data in the rolling log.
	publishTurn(ctx, bus, eventbus.OriginUser, "before shutdown", fixedTime)
	waitForRawContent(t, svc, "[user] before shutdown", 2*time.Second)

	cancel()
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// After Shutdown, rl is nil — GetContext should return empty strings.
	summaries, raw, err := svc.GetContext()
	if err != nil {
		t.Fatalf("GetContext after shutdown: %v", err)
	}
	if summaries != "" {
		t.Errorf("expected empty summaries after shutdown, got: %q", summaries)
	}
	if raw != "" {
		t.Errorf("expected empty raw after shutdown, got: %q", raw)
	}
}

func TestConvLogShutdownWithoutStart(t *testing.T) {
	// Verifies that calling Shutdown without a preceding Start is a safe no-op.
	// Exercises the started guard added in R6/M2.
	t.Parallel()
	svc, _, _ := newTestConvLog(t)
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown without Start should not error: %v", err)
	}
}

func TestConvLogNilBusStartAndShutdown(t *testing.T) {
	// Verifies that Start and Shutdown are safe no-ops when bus is nil.
	// Exercises the nil bus guards at the top of Start() and Shutdown().
	t.Parallel()
	base := filepath.Join(t.TempDir(), "conversations")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	svc := NewConversationLogService(nil, base)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start with nil bus should return nil: %v", err)
	}
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown with nil bus should return nil: %v", err)
	}
}
