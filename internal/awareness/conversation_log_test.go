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

	"github.com/nupi-ai/nupi/internal/constants"
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

// --- Story 21.2: Compaction, Archival, and Daemon Wiring Tests ---

// waitForConvPendingCompaction waits until a pending compaction entry appears.
func waitForConvPendingCompaction(t *testing.T, svc *ConversationLogService, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var foundKey string
		svc.pendingCompactions.Range(func(key, _ any) bool {
			foundKey = key.(string)
			return false
		})
		if foundKey != "" {
			return foundKey
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout waiting for pending compaction")
	return ""
}

// waitForConvNoPendingCompactions waits until all pending compactions are resolved.
func waitForConvNoPendingCompactions(t *testing.T, svc *ConversationLogService, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		count := 0
		svc.pendingCompactions.Range(func(_, _ any) bool {
			count++
			return true
		})
		if count == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout waiting for pending compactions to clear")
}

// waitForConvNoCompactionInFlight waits until compactionInFlight is cleared.
// With defer-based cleanup in handleCompactionReply, compactionInFlight clears
// AFTER all work (AppendSummary + triggerArchival), which is later than
// pendingCompactions.LoadAndDelete.
func waitForConvNoCompactionInFlight(t *testing.T, svc *ConversationLogService, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !svc.compactionInFlight.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout waiting for compactionInFlight to clear")
}

// waitForConvSummaryEntry waits until GetContext summaries contain the expected string.
func waitForConvSummaryEntry(t *testing.T, svc *ConversationLogService, expected string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		summaries, _, err := svc.GetContext()
		if err == nil && strings.Contains(summaries, expected) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	summaries, _, _ := svc.GetContext()
	t.Fatalf("timeout waiting for summary containing %q, got: %q", expected, summaries)
}

func TestCompactionTriggerConvLog(t *testing.T) {
	// AC#1: After AppendRaw in handleTurn, call ShouldCompact(). When true,
	// publish ConversationPromptEvent with event_type="conversation_compaction".
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 10, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startConvLog(t, svc)

	// Subscribe to Conversation.Prompt to verify the compaction event is published.
	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt, eventbus.WithSubscriptionName("test_convlog_compact_prompt"))
	defer promptSub.Close()

	// Lower the compaction threshold for test speed.
	rl := svc.rl.Load()
	rl.compactionThreshold = 200

	// Publish enough turns to exceed the threshold.
	for i := range 10 {
		publishTurn(ctx, bus, eventbus.OriginUser, fmt.Sprintf("compaction msg %d %s", i, strings.Repeat("x", 30)), fixedTime)
	}

	// Wait for a pending compaction entry to appear.
	promptID := waitForConvPendingCompaction(t, svc, 5*time.Second)
	if !strings.HasPrefix(promptID, "conversation-compact-") {
		t.Errorf("unexpected promptID prefix: %s", promptID)
	}

	// Verify ConversationPromptEvent was published.
	select {
	case env := <-promptSub.C():
		event := env.Payload
		if event.PromptID != promptID {
			t.Errorf("prompt event PromptID = %s, want %s", event.PromptID, promptID)
		}
		if event.Metadata[constants.MetadataKeyEventType] != constants.PromptEventConversationCompaction {
			t.Errorf("prompt event metadata event_type = %s, want %s", event.Metadata[constants.MetadataKeyEventType], constants.PromptEventConversationCompaction)
		}
		if event.SessionID != "" {
			t.Errorf("prompt event SessionID should be empty for conversation log, got: %q", event.SessionID)
		}
		if event.NewMessage.Text == "" {
			t.Error("prompt event should contain older half text")
		}
		if event.NewMessage.Origin != eventbus.OriginSystem {
			t.Errorf("prompt event Origin = %q, want %q (compaction is system-initiated)", event.NewMessage.Origin, eventbus.OriginSystem)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for ConversationPromptEvent")
	}
}

func TestCompactionReplyHandlingConvLog(t *testing.T) {
	// AC#2: Subscribe to Conversation.Reply, match PromptID, sanitize AI reply,
	// format with ### [summary] HH:MM – HH:MM header, call AppendSummary.
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 11, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startConvLog(t, svc)

	// Manually store a pending compaction entry (simulating triggerCompaction).
	rl := svc.rl.Load()
	promptID := "conversation-compact-12345"
	svc.pendingCompactions.Store(promptID, &convPendingCompaction{
		rl:        rl,
		startTime: "10:00:00",
		endTime:   "10:30:00",
	})

	// Publish a reply event with matching PromptID.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceIntentRouter,
		eventbus.ConversationReplyEvent{
			PromptID: promptID,
			Text:     "User discussed project architecture and API design.",
		})

	// Wait for the summary to be appended.
	waitForConvSummaryEntry(t, svc, "User discussed project architecture", 5*time.Second)

	// Verify the summary has the proper header format.
	summaries, _, _ := svc.GetContext()
	if !strings.Contains(summaries, "### [summary] 10:00:00 – 10:30:00") {
		t.Errorf("summary should have header with time range, got: %q", summaries)
	}

	// Verify pending compaction was removed.
	if _, ok := svc.pendingCompactions.Load(promptID); ok {
		t.Error("pending compaction should be removed after reply handling")
	}
}

func TestArchivalTriggerConvLog(t *testing.T) {
	// AC#3, AC#4: After AppendSummary, call ShouldArchive(). When true, archive
	// summaries and publish AwarenessSyncEvent.
	t.Parallel()
	svc, bus, base := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startConvLog(t, svc)

	// Lower the summary budget to make archival trigger quickly.
	rl := svc.rl.Load()
	rl.summaryBudget = 100

	// Inject summaries to exceed the budget.
	for i := range 5 {
		summary := fmt.Sprintf("### [summary] 10:0%d:00 – 10:0%d:30\nSummary block %d with enough text to exceed budget. %s",
			i, i, i, strings.Repeat("y", 30))
		if err := rl.AppendSummary(summary); err != nil {
			t.Fatalf("inject summary %d: %v", i, err)
		}
	}

	// Subscribe to awareness sync to verify FTS5 indexing event.
	syncSub := eventbus.SubscribeTo(bus, eventbus.Memory.Sync, eventbus.WithSubscriptionName("test_convlog_archival_sync"))
	defer syncSub.Close()

	// Trigger a compaction reply that adds one more summary, pushing past archival.
	promptID := "conversation-compact-archival-99999"
	svc.pendingCompactions.Store(promptID, &convPendingCompaction{
		rl:        rl,
		startTime: "11:00:00",
		endTime:   "11:30:00",
	})

	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceIntentRouter,
		eventbus.ConversationReplyEvent{
			PromptID: promptID,
			Text:     "Final summary that triggers archival with sufficient text.",
		})

	// Wait for pending compaction to be resolved.
	waitForConvNoPendingCompactions(t, svc, 5*time.Second)

	// Verify archive file was created — simpler path than journal (no session-id subdirectory).
	archiveDir := filepath.Join(base, "archives")
	expectedArchiveFile := filepath.Join(archiveDir, "2026-02-28.md")
	var archiveData []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d, readErr := os.ReadFile(expectedArchiveFile)
		if readErr == nil && len(d) > 0 {
			archiveData = d
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(archiveData) == 0 {
		t.Fatalf("archive file %s should exist with content", expectedArchiveFile)
	}
	if !strings.Contains(string(archiveData), "[summary]") {
		t.Errorf("archive file should contain summaries, got: %q", string(archiveData))
	}

	// Verify AwarenessSyncEvent was published.
	select {
	case env := <-syncSub.C():
		event := env.Payload
		if event.SyncType != eventbus.SyncTypeCreated {
			t.Errorf("sync event SyncType = %s, want %s", event.SyncType, eventbus.SyncTypeCreated)
		}
		if !strings.Contains(event.FilePath, "archives/2026-02-28.md") {
			t.Errorf("sync event FilePath should point to archive, got: %s", event.FilePath)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for AwarenessSyncEvent after archival")
	}

	// Wait for compactionInFlight to clear (defer-based cleanup completes after archival).
	waitForConvNoCompactionInFlight(t, svc, 5*time.Second)

	// Verify archived summaries were removed from live state (CommitArchival worked).
	summaries, _, err := svc.GetContext()
	if err != nil {
		t.Fatalf("GetContext after archival: %v", err)
	}
	// The 5 injected summaries (blocks 0-4) should have been archived. Only the
	// final reply summary ("Final summary that triggers archival...") should remain
	// in live state (it was added AFTER the archival threshold check).
	for i := range 5 {
		marker := fmt.Sprintf("Summary block %d", i)
		if strings.Contains(summaries, marker) {
			t.Errorf("archived summary %q should not be in live state after archival", marker)
		}
	}
	if !strings.Contains(summaries, "Final summary that triggers archival") {
		t.Errorf("last summary should remain in live state, got: %q", summaries)
	}
}

func TestArchivalSyncTypeUpdatedConvLog(t *testing.T) {
	// Verify that when an archive file already exists (same-day second archival),
	// triggerArchival publishes SyncTypeUpdated instead of SyncTypeCreated.
	t.Parallel()
	svc, bus, base := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 12, 30, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startConvLog(t, svc)

	rl := svc.rl.Load()
	rl.summaryBudget = 100

	// Pre-create the archive file so it already exists when triggerArchival runs.
	archiveDir := filepath.Join(base, "archives")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("create archive dir: %v", err)
	}
	existingArchive := filepath.Join(archiveDir, "2026-02-28.md")
	if err := os.WriteFile(existingArchive, []byte("### [summary] 09:00:00 – 09:30:00\nPrevious summary.\n"), 0o600); err != nil {
		t.Fatalf("create existing archive: %v", err)
	}

	// Inject summaries to exceed the budget.
	for i := range 5 {
		summary := fmt.Sprintf("### [summary] 11:0%d:00 – 11:0%d:30\nUpdate summary block %d. %s",
			i, i, i, strings.Repeat("z", 30))
		if err := rl.AppendSummary(summary); err != nil {
			t.Fatalf("inject summary %d: %v", i, err)
		}
	}

	// Subscribe to awareness sync.
	syncSub := eventbus.SubscribeTo(bus, eventbus.Memory.Sync, eventbus.WithSubscriptionName("test_convlog_archival_updated"))
	defer syncSub.Close()

	// Trigger compaction reply → archival.
	promptID := "conversation-compact-updated-88888"
	svc.pendingCompactions.Store(promptID, &convPendingCompaction{
		rl:        rl,
		startTime: "12:00:00",
		endTime:   "12:30:00",
	})

	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceIntentRouter,
		eventbus.ConversationReplyEvent{
			PromptID: promptID,
			Text:     "Summary triggering archival into existing file.",
		})

	waitForConvNoPendingCompactions(t, svc, 5*time.Second)

	// Verify SyncTypeUpdated (not Created) because archive file pre-existed.
	select {
	case env := <-syncSub.C():
		event := env.Payload
		if event.SyncType != eventbus.SyncTypeUpdated {
			t.Errorf("sync event SyncType = %s, want %s (archive file pre-existed)", event.SyncType, eventbus.SyncTypeUpdated)
		}
		if !strings.Contains(event.FilePath, "archives/2026-02-28.md") {
			t.Errorf("sync event FilePath should point to archive, got: %s", event.FilePath)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for AwarenessSyncEvent after archival")
	}

	// Verify the existing content was preserved (O_APPEND) and new summaries appended.
	data, err := os.ReadFile(existingArchive)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "Previous summary.") {
		t.Error("existing archive content should be preserved")
	}
	if !strings.Contains(content, "Update summary block") {
		t.Error("new summaries should be appended to existing archive")
	}
}

func TestCompactionEmptyReplyConvLog(t *testing.T) {
	// AC#7: Verify empty/whitespace AI reply doesn't call AppendSummary.
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 13, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }

	var mu sync.Mutex
	var logMsgs []string
	svc.logFunc = func(format string, args ...any) {
		mu.Lock()
		logMsgs = append(logMsgs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	ctx := startConvLog(t, svc)

	rl := svc.rl.Load()
	promptID := "conversation-compact-empty-reply"
	svc.pendingCompactions.Store(promptID, &convPendingCompaction{
		rl:        rl,
		startTime: "12:00:00",
		endTime:   "12:30:00",
	})
	svc.compactionInFlight.Store(true)

	// Publish empty reply.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceIntentRouter,
		eventbus.ConversationReplyEvent{
			PromptID: promptID,
			Text:     "",
		})

	// Wait for pending to be resolved.
	waitForConvNoPendingCompactions(t, svc, 5*time.Second)

	// compactionInFlight should be cleared. With defer-based cleanup, poll for it.
	waitForConvNoCompactionInFlight(t, svc, 5*time.Second)

	// Summaries should be empty (AppendSummary was NOT called).
	summaries, _, _ := svc.GetContext()
	if summaries != "" {
		t.Errorf("empty reply should not add summary, got: %q", summaries)
	}

	// A warning should have been logged.
	mu.Lock()
	var foundWarn bool
	for _, msg := range logMsgs {
		if strings.Contains(msg, "WARN") && strings.Contains(msg, "empty compaction reply") {
			foundWarn = true
		}
	}
	mu.Unlock()
	if !foundWarn {
		t.Error("expected warning log for empty compaction reply")
	}
}

func TestCompactionInFlightDedupConvLog(t *testing.T) {
	// AC#1: compactionInFlight atomic.Bool prevents concurrent compactions.
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 14, 0, 0, 0, time.UTC)
	ctx := startConvLog(t, svc)

	// Subscribe to Conversation.Prompt to count compaction events.
	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt, eventbus.WithSubscriptionName("test_convlog_dedup"))
	defer promptSub.Close()

	// Lower threshold.
	rl := svc.rl.Load()
	rl.compactionThreshold = 200

	// Publish enough to trigger one compaction.
	for i := range 10 {
		publishTurn(ctx, bus, eventbus.OriginUser, fmt.Sprintf("dedup msg %d %s", i, strings.Repeat("x", 30)), fixedTime)
	}

	// Wait for first compaction.
	waitForConvPendingCompaction(t, svc, 5*time.Second)

	// compactionInFlight should be set.
	if !svc.compactionInFlight.Load() {
		t.Fatal("compactionInFlight should be true after first compaction")
	}

	// Publish more data that would trigger another compaction — should be blocked.
	for i := range 10 {
		publishTurn(ctx, bus, eventbus.OriginUser, fmt.Sprintf("extra msg %d %s", i, strings.Repeat("z", 30)), fixedTime)
	}

	// Wait a bit for potential second compaction prompt.
	time.Sleep(200 * time.Millisecond)

	// Drain prompt channel — should have exactly 1 compaction prompt.
	compactionCount := 0
	for {
		select {
		case env := <-promptSub.C():
			if env.Payload.Metadata[constants.MetadataKeyEventType] == constants.PromptEventConversationCompaction {
				compactionCount++
			}
		default:
			goto done
		}
	}
done:
	if compactionCount != 1 {
		t.Errorf("expected exactly 1 compaction prompt (dedup), got %d", compactionCount)
	}
}

func TestConcurrentCompactionAndTurnAppendConvLog(t *testing.T) {
	// AC#7: Race detector safety with concurrent turns and compaction replies.
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 15, 0, 0, 0, time.UTC)
	ctx := startConvLog(t, svc)

	rl := svc.rl.Load()

	// Concurrently publish turns and compaction replies.
	var wg sync.WaitGroup

	// Goroutine 1: Publish turns.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			publishTurn(ctx, bus, eventbus.OriginUser, fmt.Sprintf("concurrent msg %d", i), fixedTime)
		}
	}()

	// Goroutine 2: Inject and resolve compaction replies.
	// Set compactionInFlight before each injection so handleCompactionReply's
	// defer-based cleanup (Store(false)) exercises the real race path with
	// concurrent triggerCompaction attempts from Goroutine 1.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			promptID := fmt.Sprintf("conversation-compact-race-%d", i)
			svc.compactionInFlight.Store(true)
			svc.pendingCompactions.Store(promptID, &convPendingCompaction{
				rl:        rl,
				startTime: "10:00:00",
				endTime:   "10:30:00",
			})
			eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceIntentRouter,
				eventbus.ConversationReplyEvent{
					PromptID: promptID,
					Text:     fmt.Sprintf("Summary for race iteration %d", i),
				})
		}
	}()

	wg.Wait()

	// Wait for all turns to appear.
	for i := 0; i < 20; i++ {
		waitForRawContent(t, svc, fmt.Sprintf("concurrent msg %d", i), 5*time.Second)
	}

	// Wait for all compaction replies to be processed.
	waitForConvNoPendingCompactions(t, svc, 5*time.Second)
	waitForConvNoCompactionInFlight(t, svc, 5*time.Second)

	// Verify at least some summaries were written (not silently dropped).
	summaries, _, err := svc.GetContext()
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	summaryCount := strings.Count(summaries, "[summary]")
	if summaryCount == 0 {
		t.Error("expected at least one summary from concurrent compaction replies, got 0")
	}
}

func TestShutdownClearsPendingMapsConvLog(t *testing.T) {
	// AC#7: Verify pendingCompactions and compactionInFlight are cleared on shutdown.
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 16, 0, 0, 0, time.UTC)

	ctx, cancel := context.WithCancel(context.Background())
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	publishTurn(ctx, bus, eventbus.OriginUser, "before shutdown", fixedTime)
	waitForRawContent(t, svc, "[user] before shutdown", 2*time.Second)

	// Simulate a pending compaction entry.
	rl := svc.rl.Load()
	svc.pendingCompactions.Store("conversation-compact-stale-123", &convPendingCompaction{
		rl:        rl,
		startTime: "15:00:00",
		endTime:   "15:30:00",
	})
	svc.compactionInFlight.Store(true)

	cancel()
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Verify maps are cleared.
	count := 0
	svc.pendingCompactions.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Errorf("pendingCompactions should be empty after shutdown, has %d entries", count)
	}
	if svc.compactionInFlight.Load() {
		t.Error("compactionInFlight should be false after shutdown")
	}
}

func TestRestartWithPendingCompactionConvLog(t *testing.T) {
	// AC#7: Verify that Shutdown with an in-flight compaction logs a data
	// loss warning, and a subsequent Start cycle works correctly without
	// stale compaction state blocking new compactions.
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 20, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }

	var mu sync.Mutex
	var logMsgs []string
	svc.logFunc = func(format string, args ...any) {
		mu.Lock()
		logMsgs = append(logMsgs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	// Cycle 1: Start, trigger compaction, shutdown WITHOUT resolving it.
	ctx1, cancel1 := context.WithCancel(context.Background())
	if err := svc.Start(ctx1); err != nil {
		t.Fatalf("start cycle 1: %v", err)
	}

	// Add some raw data.
	publishTurn(ctx1, bus, eventbus.OriginUser, "restart compaction test", fixedTime)
	waitForRawContent(t, svc, "[user] restart compaction test", 2*time.Second)

	// Inject a pending compaction (simulating in-flight).
	rl := svc.rl.Load()
	svc.pendingCompactions.Store("conversation-compact-stale-restart", &convPendingCompaction{
		rl:        rl,
		startTime: "19:00:00",
		endTime:   "19:30:00",
	})
	svc.compactionInFlight.Store(true)

	cancel1()
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown cycle 1: %v", err)
	}

	// Verify data loss warning was logged.
	mu.Lock()
	var foundWarn bool
	for _, msg := range logMsgs {
		if strings.Contains(msg, "WARN") && strings.Contains(msg, "dropping unresolved compaction") {
			foundWarn = true
		}
	}
	mu.Unlock()
	if !foundWarn {
		t.Error("expected data loss warning for dropped compaction during shutdown")
	}

	// Verify stale state is cleared.
	if svc.compactionInFlight.Load() {
		t.Error("compactionInFlight should be cleared after shutdown")
	}

	// Cycle 2: Restart — should work cleanly, no stale state.
	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel2()
		_ = svc.Shutdown(context.Background())
	})
	if err := svc.Start(ctx2); err != nil {
		t.Fatalf("start cycle 2: %v", err)
	}

	// Verify cycle 1 raw data persisted.
	_, raw, err := svc.GetContext()
	if err != nil {
		t.Fatalf("GetContext cycle 2: %v", err)
	}
	if !strings.Contains(raw, "[user] restart compaction test") {
		t.Errorf("cycle 1 data lost after restart, got: %q", raw)
	}

	// Verify compaction can fire again (not blocked by stale state).
	rl2 := svc.rl.Load()
	rl2.compactionThreshold = 100

	for i := range 5 {
		publishTurn(ctx2, bus, eventbus.OriginUser, fmt.Sprintf("post-restart msg %d %s", i, strings.Repeat("x", 30)), fixedTime)
	}

	// If stale compactionInFlight blocked this, no pending compaction would appear.
	promptID := waitForConvPendingCompaction(t, svc, 5*time.Second)
	if !strings.HasPrefix(promptID, "conversation-compact-") {
		t.Errorf("unexpected promptID after restart: %s", promptID)
	}
}

func TestCompactionOlderHalfEmptyReturnConvLog(t *testing.T) {
	// AC#7: Verify compactionInFlight cleared on empty OlderHalf().
	// When raw tail is below threshold but ShouldCompact returns true
	// transiently (e.g., concurrent append), OlderHalf may return empty.
	t.Parallel()
	svc, _, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 17, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	_ = startConvLog(t, svc)

	rl := svc.rl.Load()

	// Set compactionInFlight directly (simulating the guard would be acquired).
	// triggerCompaction will try CompareAndSwap(false, true) so we can't set it first.
	// Instead, just call triggerCompaction on a log with no raw content.
	svc.triggerCompaction(rl)

	// OlderHalf on empty log returns "" — compactionInFlight should be cleared.
	if svc.compactionInFlight.Load() {
		t.Error("compactionInFlight should be false after empty OlderHalf")
	}

	// No pending compaction should exist.
	count := 0
	svc.pendingCompactions.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Errorf("pendingCompactions should be empty, has %d entries", count)
	}
}

func TestTriggerArchivalNilRLConvLog(t *testing.T) {
	// Verify that triggerArchival returns early (no panic) when rl is nil.
	// Exercises the defensive nil guard added in R1/H2.
	t.Parallel()
	svc, _, base := newTestConvLog(t)
	_ = startConvLog(t, svc)

	// Call triggerArchival with nil — should return without error or panic.
	svc.triggerArchival(nil)

	// Verify no archive directory was created.
	archiveDir := filepath.Join(base, "archives")
	if _, err := os.Stat(archiveDir); err == nil {
		t.Error("archives directory should not be created when rl is nil")
	}
}

func TestCompactionReplyWithMarkdownHeadersConvLog(t *testing.T) {
	// AC#7: Verify sanitizeForSummary downgrades `### ` headers in AI reply.
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 18, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startConvLog(t, svc)

	rl := svc.rl.Load()
	promptID := "conversation-compact-headers-test"
	svc.pendingCompactions.Store(promptID, &convPendingCompaction{
		rl:        rl,
		startTime: "17:00:00",
		endTime:   "17:30:00",
	})

	// AI reply contains ### headers that would split the summary on reload.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceIntentRouter,
		eventbus.ConversationReplyEvent{
			PromptID: promptID,
			Text:     "First part of summary.\n\n### Architecture\nSecond part about architecture.",
		})

	waitForConvSummaryEntry(t, svc, "First part of summary", 5*time.Second)

	summaries, _, _ := svc.GetContext()
	// The "### Architecture" should be downgraded to "#### Architecture".
	if strings.Contains(summaries, "\n\n### Architecture") {
		t.Errorf("### headers in AI reply should be downgraded to ####, got: %q", summaries)
	}
	if !strings.Contains(summaries, "#### Architecture") {
		t.Errorf("expected downgraded #### header, got: %q", summaries)
	}
}

func TestConversationLogServiceLifecycle(t *testing.T) {
	// Verify ConversationLogService Start/Shutdown/GetContext lifecycle works
	// correctly on a fresh instance. Interface compatibility (AC#5, AC#6) is
	// validated by the compile-time assertion in
	// internal/intentrouter/conversation_log_provider_test.go.
	t.Parallel()
	bus := eventbus.New()
	base := filepath.Join(t.TempDir(), "conversations")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("create dir: %v", err)
	}

	svc := NewConversationLogService(bus, base)

	// Verify Start/Shutdown cycle works.
	ctx, cancel := context.WithCancel(context.Background())
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Verify GetContext works after Start.
	summaries, raw, err := svc.GetContext()
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	if summaries != "" || raw != "" {
		t.Errorf("expected empty context on fresh service, got summaries=%q raw=%q", summaries, raw)
	}

	cancel()
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestCompactionWhitespaceOnlyReplyConvLog(t *testing.T) {
	// Verify whitespace-only AI reply is treated the same as empty.
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 19, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	ctx := startConvLog(t, svc)

	rl := svc.rl.Load()
	promptID := "conversation-compact-whitespace"
	svc.pendingCompactions.Store(promptID, &convPendingCompaction{
		rl:        rl,
		startTime: "18:00:00",
		endTime:   "18:30:00",
	})
	svc.compactionInFlight.Store(true)

	// Publish whitespace-only reply.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceIntentRouter,
		eventbus.ConversationReplyEvent{
			PromptID: promptID,
			Text:     "   \n\n\t  ",
		})

	waitForConvNoPendingCompactions(t, svc, 5*time.Second)

	// compactionInFlight should be cleared. With defer-based cleanup, poll for it.
	waitForConvNoCompactionInFlight(t, svc, 5*time.Second)

	summaries, _, _ := svc.GetContext()
	if summaries != "" {
		t.Errorf("whitespace reply should not add summary, got: %q", summaries)
	}
}

func TestHandleCompactionReplyNilRLConvLog(t *testing.T) {
	// Verify that handleCompactionReply returns early (no panic) when the
	// retained RollingLog in convPendingCompaction is nil (R4/M3 guard).
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 2, 28, 21, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }

	var mu sync.Mutex
	var logMsgs []string
	svc.logFunc = func(format string, args ...any) {
		mu.Lock()
		logMsgs = append(logMsgs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	ctx := startConvLog(t, svc)

	// Store a pending compaction with nil rl (defensive scenario).
	promptID := "conversation-compact-nil-rl-test"
	svc.pendingCompactions.Store(promptID, &convPendingCompaction{
		rl:        nil,
		startTime: "20:00:00",
		endTime:   "20:30:00",
	})
	svc.compactionInFlight.Store(true)

	// Publish a reply — should not panic, should log warning.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceIntentRouter,
		eventbus.ConversationReplyEvent{
			PromptID: promptID,
			Text:     "Summary that cannot be written because rl is nil.",
		})

	// Wait for pending to be resolved.
	waitForConvNoPendingCompactions(t, svc, 5*time.Second)
	waitForConvNoCompactionInFlight(t, svc, 5*time.Second)

	// Verify warning was logged.
	mu.Lock()
	var foundWarn bool
	for _, msg := range logMsgs {
		if strings.Contains(msg, "WARN") && strings.Contains(msg, "nil RollingLog") {
			foundWarn = true
		}
	}
	mu.Unlock()
	if !foundWarn {
		t.Error("expected warning log for nil RollingLog in pending compaction")
	}

	// Verify no summary was added (no crash means the guard worked).
	summaries, _, _ := svc.GetContext()
	if summaries != "" {
		t.Errorf("nil rl should not add summary, got: %q", summaries)
	}
}

func TestCompactionOlderHalfErrorConvLog(t *testing.T) {
	// M1: Verify compactionInFlight is cleared when OlderHalf() returns an error
	// (e.g., disk write failure during persist). Exercises the error path at
	// conversation_log.go triggerCompaction.
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("chmod 0o555 does not restrict root — skipping on root")
	}

	// Create a ConversationLogService with a custom base path. We'll make the
	// directory read-only after adding raw entries so that OlderHalf's
	// persistToFile (which creates a .tmp file) fails.
	bus := eventbus.New()
	base := filepath.Join(t.TempDir(), "conversations")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	svc := NewConversationLogService(bus, base)

	var mu sync.Mutex
	var logMsgs []string
	svc.logFunc = func(format string, args ...any) {
		mu.Lock()
		logMsgs = append(logMsgs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		// Restore write permission before cleanup to allow temp dir removal.
		os.Chmod(base, 0o755)
		cancel()
		_ = svc.Shutdown(context.Background())
	})
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	rl := svc.rl.Load()

	// Add enough raw entries so OlderHalf has something to extract.
	for i := 0; i < 4; i++ {
		if err := rl.AppendRaw(fmt.Sprintf("entry-%d", i)); err != nil {
			t.Fatalf("append raw: %v", err)
		}
	}

	// Make the base directory read-only so persistToFile cannot create the
	// .tmp file. This forces OlderHalf to return an error.
	if err := os.Chmod(base, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}

	// Call triggerCompaction — OlderHalf should fail, compactionInFlight should clear.
	svc.triggerCompaction(rl)

	if svc.compactionInFlight.Load() {
		t.Error("compactionInFlight should be false after OlderHalf error")
	}

	// No pending compaction should exist.
	count := 0
	svc.pendingCompactions.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Errorf("pendingCompactions should be empty after OlderHalf error, has %d", count)
	}

	// Error should have been logged.
	mu.Lock()
	var foundErr bool
	for _, msg := range logMsgs {
		if strings.Contains(msg, "ERROR") && strings.Contains(msg, "extract older half") {
			foundErr = true
		}
	}
	mu.Unlock()
	if !foundErr {
		t.Error("expected error log for OlderHalf failure")
	}

	// Restore permissions for cleanup.
	os.Chmod(base, 0o755)
}

func TestStaleCompactionReaperConvLog(t *testing.T) {
	// H1: Verify that stale compaction state (in-flight > threshold) is
	// automatically cleared when a new compaction attempt is blocked.
	t.Parallel()
	svc, _, _ := newTestConvLog(t)

	var mu sync.Mutex
	var logMsgs []string
	svc.logFunc = func(format string, args ...any) {
		mu.Lock()
		logMsgs = append(logMsgs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	_ = startConvLog(t, svc)

	rl := svc.rl.Load()

	// Simulate a stale compaction: set compactionInFlight and a very old startedAt.
	svc.compactionInFlight.Store(true)
	// Set startedAt to 10 minutes ago (well past the 5-minute threshold).
	staleTime := time.Now().Add(-10 * time.Minute).UnixNano()
	svc.compactionStartedAt.Store(staleTime)
	svc.pendingCompactions.Store("conversation-compact-stale-999", &convPendingCompaction{
		rl:        rl,
		startTime: "08:00:00",
		endTime:   "08:30:00",
	})

	// Attempt a new compaction — should detect stale state and clear it.
	svc.triggerCompaction(rl)

	// After clearing stale state, compactionInFlight should be false.
	// triggerCompaction returns immediately after checkStaleCompaction clears
	// the stale state. If rl has no raw entries and triggerCompaction is retried,
	// OlderHalf returns "" and clears compactionInFlight.
	if svc.compactionInFlight.Load() {
		t.Error("compactionInFlight should be false after stale reaper cleared it")
	}

	// The stale pending compaction should be removed.
	if _, ok := svc.pendingCompactions.Load("conversation-compact-stale-999"); ok {
		t.Error("stale pending compaction should have been cleared by reaper")
	}

	// Warning should have been logged.
	mu.Lock()
	var foundStaleWarn bool
	for _, msg := range logMsgs {
		if strings.Contains(msg, "WARN") && strings.Contains(msg, "in-flight for") {
			foundStaleWarn = true
		}
	}
	mu.Unlock()
	if !foundStaleWarn {
		t.Error("expected stale compaction warning log")
	}
}

func TestAppendSummaryFailureFallbackConvLog(t *testing.T) {
	// M2: Verify that when AppendSummary fails, the AI summary is written
	// to the daily audit log as a fallback.
	t.Parallel()

	bus := eventbus.New()
	base := filepath.Join(t.TempDir(), "conversations")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	// Pre-create logs/ directory so writeLogEntry (fallback) can write even
	// when the base directory is read-only.
	logsDir := filepath.Join(base, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("create logs dir: %v", err)
	}
	svc := NewConversationLogService(bus, base)
	fixedTime := time.Date(2026, 2, 28, 22, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }

	var mu sync.Mutex
	var logMsgs []string
	svc.logFunc = func(format string, args ...any) {
		mu.Lock()
		logMsgs = append(logMsgs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		os.Chmod(base, 0o755)
		cancel()
		_ = svc.Shutdown(context.Background())
	})
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Write a turn to ensure the daily log file is opened and logDirCreated is set.
	publishTurn(ctx, bus, eventbus.OriginUser, "setup turn", fixedTime)
	waitForRawContent(t, svc, "[user] setup turn", 2*time.Second)
	logFile := filepath.Join(logsDir, "2026-02-28.md")
	waitForLogContent(t, logFile, "setup turn", 2*time.Second)

	rl := svc.rl.Load()
	promptID := "conversation-compact-fallback-test"
	svc.pendingCompactions.Store(promptID, &convPendingCompaction{
		rl:        rl,
		startTime: "21:00:00",
		endTime:   "21:30:00",
	})

	// Make base directory read-only so AppendSummary's persistToFile (which
	// creates a .tmp file) fails. The logs/ directory is already created and
	// the log file handle is already open, so the fallback writeLogEntry
	// can still write.
	if err := os.Chmod(base, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}

	// Publish a reply — AppendSummary should fail, fallback should write to audit log.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceIntentRouter,
		eventbus.ConversationReplyEvent{
			PromptID: promptID,
			Text:     "Summary that should fall back to audit log.",
		})

	// Wait for the pending compaction to be resolved.
	waitForConvNoPendingCompactions(t, svc, 5*time.Second)
	waitForConvNoCompactionInFlight(t, svc, 5*time.Second)

	// Restore permissions before reading.
	os.Chmod(base, 0o755)

	// Verify the fallback was written to the daily audit log.
	deadline := time.Now().Add(5 * time.Second)
	var logData []byte
	for time.Now().Before(deadline) {
		d, err := os.ReadFile(logFile)
		if err == nil && strings.Contains(string(d), "[compaction-fallback]") {
			logData = d
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(string(logData), "[compaction-fallback]") {
		mu.Lock()
		t.Errorf("expected fallback compaction entry in daily audit log, log messages: %v", logMsgs)
		mu.Unlock()
	}
	if !strings.Contains(string(logData), "Summary that should fall back to audit log.") {
		t.Error("expected AI summary text in fallback entry")
	}
}

func TestHandleTurnAppendRawFailureSkipsCompaction(t *testing.T) {
	// R6/M1: Verify that when AppendRaw fails (e.g., disk full), handleTurn
	// does NOT call triggerCompaction. Compacting data that may not have
	// persisted to disk risks silent data loss on restart.
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("chmod 0o555 does not restrict root — skipping on root")
	}

	bus := eventbus.New()
	base := filepath.Join(t.TempDir(), "conversations")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	svc := NewConversationLogService(bus, base)
	fixedTime := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }

	var mu sync.Mutex
	var logMsgs []string
	svc.logFunc = func(format string, args ...any) {
		mu.Lock()
		logMsgs = append(logMsgs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		os.Chmod(base, 0o755)
		cancel()
		_ = svc.Shutdown(context.Background())
	})
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	rl := svc.rl.Load()

	// Add enough data to exceed the compaction threshold so the NEXT AppendRaw
	// would trigger ShouldCompact() == true.
	bigEntry := strings.Repeat("x", conversationCompactionThreshold+100)
	if err := rl.AppendRaw(bigEntry); err != nil {
		t.Fatalf("seed append: %v", err)
	}

	// Make directory read-only so the next AppendRaw (via handleTurn) fails
	// on persistToFile.
	if err := os.Chmod(base, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// Simulate a turn — AppendRaw should fail, and triggerCompaction should NOT
	// be called (no compaction event published, no compactionInFlight set).
	publishTurn(ctx, bus, eventbus.OriginUser, "should-fail", fixedTime)

	// Wait for the error to be logged.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		found := false
		for _, msg := range logMsgs {
			if strings.Contains(msg, "ERROR: append raw") {
				found = true
				break
			}
		}
		mu.Unlock()
		if found {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Verify compactionInFlight was NOT set (triggerCompaction was not called).
	if svc.compactionInFlight.Load() {
		t.Error("compactionInFlight should NOT be set when AppendRaw fails")
	}

	// Verify no compaction event was published by checking for "compaction triggered" log.
	mu.Lock()
	for _, msg := range logMsgs {
		if strings.Contains(msg, "compaction triggered") {
			t.Errorf("triggerCompaction should NOT be called after AppendRaw failure, but found log: %s", msg)
		}
	}
	mu.Unlock()
}

func TestStaleCompactionReaperVsLateReplyConvLog(t *testing.T) {
	// R6/M2: Verify behavior when checkStaleCompaction clears a pending entry
	// and a reply for that promptID arrives shortly after. The reply should be
	// silently dropped (promptID not found in pendingCompactions).
	t.Parallel()

	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }

	var mu sync.Mutex
	var logMsgs []string
	svc.logFunc = func(format string, args ...any) {
		mu.Lock()
		logMsgs = append(logMsgs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	ctx := startConvLog(t, svc)

	rl := svc.rl.Load()

	// Manually inject a "stale" pending compaction that's old enough to be reaped.
	promptID := "conversation-compact-stale-test"
	svc.compactionInFlight.Store(true)
	svc.compactionStartedAt.Store(time.Now().Add(-10 * time.Minute).UnixNano()) // 10 min ago = stale
	svc.pendingCompactions.Store(promptID, &convPendingCompaction{
		rl:        rl,
		startTime: "10:00:00",
		endTime:   "10:30:00",
	})

	// Trigger checkStaleCompaction by attempting a new compaction while one is in-flight.
	// Since compactionInFlight is already true, triggerCompaction will call checkStaleCompaction.
	svc.triggerCompaction(rl)

	// After stale reaping, compactionInFlight should be false.
	waitForConvNoCompactionInFlight(t, svc, 3*time.Second)

	// Now publish a reply matching the reaped promptID — it should be silently dropped.
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceIntentRouter,
		eventbus.ConversationReplyEvent{
			PromptID: promptID,
			Text:     "Late reply for reaped compaction.",
		})

	// Publish a sentinel reply to ensure the late reply was processed first.
	sentinelID := "sentinel-after-stale"
	eventbus.Publish(ctx, bus, eventbus.Conversation.Reply, eventbus.SourceIntentRouter,
		eventbus.ConversationReplyEvent{
			PromptID: sentinelID,
			Text:     "sentinel",
		})

	// Wait briefly for both replies to be processed.
	time.Sleep(200 * time.Millisecond)

	// Verify: no AppendSummary was called for the stale promptID — the summary
	// section should be empty (only raw turns from the service start).
	summaries, _, err := svc.GetContext()
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	if strings.Contains(summaries, "Late reply for reaped compaction") {
		t.Error("stale compaction reply should have been dropped, but summary was written")
	}
}

func TestHandleTurnWithNilRL(t *testing.T) {
	// Verify that handleTurn still writes to the daily audit log when rl is nil
	// (e.g., during a race with Shutdown). The audit log should capture all
	// turns regardless of rolling log state.
	t.Parallel()
	svc, bus, base := newTestConvLog(t)
	fixedTime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	ctx := startConvLog(t, svc)

	// Force rl to nil to simulate the race window.
	svc.rl.Store(nil)

	publishTurn(ctx, bus, eventbus.OriginUser, "turn during nil rl", fixedTime)

	// Wait for the audit log entry to appear — writeLogEntry runs before rl check.
	logFile := filepath.Join(base, "logs", "2026-03-01.md")
	waitForLogContent(t, logFile, "12:00:00 [user] turn during nil rl", 2*time.Second)

	// GetContext should return empty (rl is nil).
	summaries, raw, err := svc.GetContext()
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	if summaries != "" || raw != "" {
		t.Errorf("expected empty context with nil rl, got summaries=%q raw=%q", summaries, raw)
	}
}

// ---------------------------------------------------------------------------
// parseTurns / parseOneTurn / roleToOrigin unit tests
// ---------------------------------------------------------------------------

func TestRoleToOrigin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role string
		want eventbus.ContentOrigin
	}{
		{"user", eventbus.OriginUser},
		{"ai", eventbus.OriginAI},
		{"tool", eventbus.OriginTool},
		{"system", eventbus.OriginSystem},
		{"unknown", eventbus.OriginSystem},
		{"bogus", eventbus.OriginSystem},
		{"", eventbus.OriginSystem},
	}
	for _, tt := range tests {
		if got := roleToOrigin(tt.role); got != tt.want {
			t.Errorf("roleToOrigin(%q) = %q, want %q", tt.role, got, tt.want)
		}
	}
}

func TestParseOneTurn_ValidFormats(t *testing.T) {
	t.Parallel()
	datePrefix := "2026-03-02"

	tests := []struct {
		name       string
		entry      string
		wantOrigin eventbus.ContentOrigin
		wantText   string
		wantOK     bool
		wantTime   string // "15:04:05" format
	}{
		{
			name:       "user turn",
			entry:      "14:30:00 [user] Hello world",
			wantOrigin: eventbus.OriginUser,
			wantText:   "Hello world",
			wantOK:     true,
			wantTime:   "14:30:00",
		},
		{
			name:       "ai turn",
			entry:      "14:30:01 [ai] I can help with that",
			wantOrigin: eventbus.OriginAI,
			wantText:   "I can help with that",
			wantOK:     true,
			wantTime:   "14:30:01",
		},
		{
			name:       "tool turn",
			entry:      "14:30:02 [tool] Running command...",
			wantOrigin: eventbus.OriginTool,
			wantText:   "Running command...",
			wantOK:     true,
			wantTime:   "14:30:02",
		},
		{
			name:       "system turn",
			entry:      "14:30:03 [system] Session started",
			wantOrigin: eventbus.OriginSystem,
			wantText:   "Session started",
			wantOK:     true,
			wantTime:   "14:30:03",
		},
		{
			name:       "multi-line text",
			entry:      "09:00:00 [ai] Line one\nLine two\nLine three",
			wantOrigin: eventbus.OriginAI,
			wantText:   "Line one\nLine two\nLine three",
			wantOK:     true,
			wantTime:   "09:00:00",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			turn, ok := parseOneTurn(tt.entry, datePrefix)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if turn.Origin != tt.wantOrigin {
				t.Errorf("Origin = %q, want %q", turn.Origin, tt.wantOrigin)
			}
			if turn.Text != tt.wantText {
				t.Errorf("Text = %q, want %q", turn.Text, tt.wantText)
			}
			if tt.wantTime != "" {
				gotTime := turn.At.Format("15:04:05")
				if gotTime != tt.wantTime {
					t.Errorf("At time = %q, want %q", gotTime, tt.wantTime)
				}
				gotDate := turn.At.Format("2006-01-02")
				if gotDate != datePrefix {
					t.Errorf("At date = %q, want %q", gotDate, datePrefix)
				}
			}
		})
	}
}

func TestParseOneTurn_MalformedEntries(t *testing.T) {
	t.Parallel()
	datePrefix := "2026-03-02"

	tests := []struct {
		name       string
		entry      string
		wantOK     bool
		wantOrigin eventbus.ContentOrigin
		wantText   string
	}{
		{
			name:   "empty string",
			entry:  "",
			wantOK: false,
		},
		{
			name:   "whitespace only",
			entry:  "   \n\t  ",
			wantOK: false,
		},
		{
			name:       "too short",
			entry:      "hello",
			wantOrigin: eventbus.OriginSystem,
			wantText:   "hello",
			wantOK:     true,
		},
		{
			name:       "no brackets",
			entry:      "14:30:00 user Hello",
			wantOrigin: eventbus.OriginSystem,
			wantText:   "14:30:00 user Hello",
			wantOK:     true,
		},
		{
			name:       "invalid time format",
			entry:      "XX:YY:ZZ [user] Hello",
			wantOrigin: eventbus.OriginSystem,
			wantText:   "XX:YY:ZZ [user] Hello",
			wantOK:     true,
		},
		{
			name:       "missing closing bracket space",
			entry:      "14:30:00 [user]Hello",
			wantOrigin: eventbus.OriginSystem,
			wantText:   "14:30:00 [user]Hello",
			wantOK:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			turn, ok := parseOneTurn(tt.entry, datePrefix)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if turn.Origin != tt.wantOrigin {
				t.Errorf("Origin = %q, want %q", turn.Origin, tt.wantOrigin)
			}
			if turn.Text != tt.wantText {
				t.Errorf("Text = %q, want %q", turn.Text, tt.wantText)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ConversationLogService.Slice unit tests
// ---------------------------------------------------------------------------

func TestSlice_NilRollingLog(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	base := filepath.Join(t.TempDir(), "conversations")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	svc := NewConversationLogService(bus, base)
	// Don't call Start — rl remains nil.

	total, turns, err := svc.Slice(0, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 0 || len(turns) != 0 {
		t.Errorf("expected empty result, got total=%d turns=%d", total, len(turns))
	}
}

func TestSlice_SingleTurn(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 3, 2, 14, 30, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	startConvLog(t, svc)

	publishTurn(context.Background(), bus, eventbus.OriginUser, "Hello", fixedTime)
	waitForRawContent(t, svc, "14:30:00 [user] Hello", 2*time.Second)

	total, turns, err := svc.Slice(0, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want 1", len(turns))
	}
	if turns[0].Origin != eventbus.OriginUser {
		t.Errorf("Origin = %q, want %q", turns[0].Origin, eventbus.OriginUser)
	}
	if turns[0].Text != "Hello" {
		t.Errorf("Text = %q, want %q", turns[0].Text, "Hello")
	}
}

func TestSlice_MultipleTurns(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 3, 2, 14, 30, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	startConvLog(t, svc)

	publishTurn(context.Background(), bus, eventbus.OriginUser, "Question", fixedTime)
	waitForRawContent(t, svc, "14:30:00 [user] Question", 2*time.Second)

	publishTurn(context.Background(), bus, eventbus.OriginAI, "Answer", fixedTime.Add(time.Second))
	waitForRawContent(t, svc, "14:30:01 [ai] Answer", 2*time.Second)

	publishTurn(context.Background(), bus, eventbus.OriginTool, "Tool output", fixedTime.Add(2*time.Second))
	waitForRawContent(t, svc, "14:30:02 [tool] Tool output", 2*time.Second)

	total, turns, err := svc.Slice(0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(turns) != 3 {
		t.Fatalf("len(turns) = %d, want 3", len(turns))
	}
}

func TestSlice_OffsetAndLimit(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 3, 2, 14, 30, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	startConvLog(t, svc)

	for i := 0; i < 5; i++ {
		ts := fixedTime.Add(time.Duration(i) * time.Second)
		text := fmt.Sprintf("Turn %d", i)
		publishTurn(context.Background(), bus, eventbus.OriginUser, text, ts)
		waitForRawContent(t, svc, text, 2*time.Second)
	}

	// offset=1, limit=2 → turns 1,2
	total, turns, err := svc.Slice(1, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 5 {
		t.Fatalf("total = %d, want 5", total)
	}
	if len(turns) != 2 {
		t.Fatalf("len(turns) = %d, want 2", len(turns))
	}
	if turns[0].Text != "Turn 1" {
		t.Errorf("turns[0].Text = %q, want %q", turns[0].Text, "Turn 1")
	}
	if turns[1].Text != "Turn 2" {
		t.Errorf("turns[1].Text = %q, want %q", turns[1].Text, "Turn 2")
	}
}

func TestSlice_OffsetBeyondTotal(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 3, 2, 14, 30, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	startConvLog(t, svc)

	publishTurn(context.Background(), bus, eventbus.OriginUser, "Only turn", fixedTime)
	waitForRawContent(t, svc, "14:30:00 [user] Only turn", 2*time.Second)

	total, turns, err := svc.Slice(100, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if len(turns) != 0 {
		t.Fatalf("len(turns) = %d, want 0", len(turns))
	}
}

func TestSlice_NegativeOffset(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 3, 2, 14, 30, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	startConvLog(t, svc)

	publishTurn(context.Background(), bus, eventbus.OriginUser, "Hello", fixedTime)
	waitForRawContent(t, svc, "14:30:00 [user] Hello", 2*time.Second)

	total, turns, err := svc.Slice(-5, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want 1 (negative offset clamped to 0)", len(turns))
	}
}

func TestSlice_ZeroLimit(t *testing.T) {
	t.Parallel()
	svc, bus, _ := newTestConvLog(t)
	fixedTime := time.Date(2026, 3, 2, 14, 30, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return fixedTime }
	startConvLog(t, svc)

	publishTurn(context.Background(), bus, eventbus.OriginUser, "Hello", fixedTime)
	waitForRawContent(t, svc, "14:30:00 [user] Hello", 2*time.Second)

	// limit=0 means no limit — return all from offset
	total, turns, err := svc.Slice(0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want 1 (limit=0 means all)", len(turns))
	}
}

// --- Additional parseOneTurn edge case tests (code review 2026-03-02 round 2) ---

func TestParseOneTurn_TextWithBracketsInBody(t *testing.T) {
	t.Parallel()
	// Verify "] " inside text body doesn't break parsing — parser should find
	// the first "] " (after the role), not a later one in the text.
	turn, ok := parseOneTurn("14:30:00 [user] text with [brackets] inside", "2026-03-02")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if turn.Origin != eventbus.OriginUser {
		t.Errorf("Origin = %q, want %q", turn.Origin, eventbus.OriginUser)
	}
	if turn.Text != "text with [brackets] inside" {
		t.Errorf("Text = %q, want %q", turn.Text, "text with [brackets] inside")
	}
}

func TestParseOneTurn_TrailingWhitespaceAfterRole(t *testing.T) {
	t.Parallel()
	// Entry "14:30:00 [user]  " → TrimSpace → "14:30:00 [user]" → no "] " found
	// → treated as malformed, returned as system-origin turn.
	turn, ok := parseOneTurn("14:30:00 [user]  ", "2026-03-02")
	if !ok {
		t.Fatal("expected ok=true (malformed entries return system turn)")
	}
	if turn.Origin != eventbus.OriginSystem {
		t.Errorf("Origin = %q, want %q", turn.Origin, eventbus.OriginSystem)
	}
	if turn.Text != "14:30:00 [user]" {
		t.Errorf("Text = %q, want trimmed entry", turn.Text)
	}
}

func TestParseOneTurn_CaseSensitiveRole(t *testing.T) {
	t.Parallel()
	// Roles are case-sensitive; uppercase "USER" is an unknown role → OriginSystem.
	turn, ok := parseOneTurn("14:30:00 [USER] hello", "2026-03-02")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if turn.Origin != eventbus.OriginSystem {
		t.Errorf("Origin = %q, want %q (uppercase role is unknown)", turn.Origin, eventbus.OriginSystem)
	}
}
