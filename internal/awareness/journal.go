package awareness

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// maxEntryTextBytes is the maximum size of event.Text accepted before truncation.
// Prevents unbounded memory and disk usage from CLI tools that dump large output.
const maxEntryTextBytes = 64 * 1024

// shutdownGraceTimeout is the maximum time Shutdown waits for consumer
// goroutines to exit when the caller's context has already expired. 5 seconds
// is sufficient for the three consumer goroutines to drain their event channels
// and complete any in-flight file I/O.
const shutdownGraceTimeout = 5 * time.Second

// JournalService captures per-session CLI output into rolling-log journals.
// It subscribes to pipeline cleaned, session lifecycle, and tool changed events
// to maintain a live journal file per active session. Implements runtime.Service.
type JournalService struct {
	bus      *eventbus.Bus
	basePath string // absolute path to journals/ directory

	activeLogs     sync.Map // sessionID -> *RollingLog
	toolCache      sync.Map // sessionID -> string (current tool name)
	logDirsCreated sync.Map // sessionID -> bool (log dir created)
	logFiles       sync.Map // sessionID -> *logFileEntry (cached daily log file handle)
	logFileMus     sync.Map // sessionID -> *sync.Mutex — per-session serialization of getLogFile + WriteString

	cleanedSub     *eventbus.TypedSubscription[eventbus.PipelineMessageEvent]
	lifecycleSub   *eventbus.TypedSubscription[eventbus.SessionLifecycleEvent]
	toolChangedSub *eventbus.TypedSubscription[eventbus.SessionToolChangedEvent]

	lifecycle eventbus.ServiceLifecycle
	started  atomic.Bool

	// nowFunc returns the current time. Overridden in tests.
	nowFunc func() time.Time
	// logFunc logs errors. Defaults to log.Printf. Overridden in tests.
	logFunc func(format string, args ...any)
}

// logFileEntry caches an open file handle for a session's daily log file.
type logFileEntry struct {
	file *os.File
	date string // YYYY-MM-DD, used to detect date rollover
}

// NewJournalService creates a JournalService writing journals under basePath.
// basePath must be an absolute path to the journals directory
// (e.g., "{instanceDir}/awareness/memory/journals").
func NewJournalService(bus *eventbus.Bus, basePath string) *JournalService {
	return &JournalService{
		bus:      bus,
		basePath: basePath,
		nowFunc:  func() time.Time { return time.Now().UTC() },
		logFunc:  log.Printf,
	}
}

// Start subscribes to event bus topics and launches consumer goroutines.
func (j *JournalService) Start(ctx context.Context) error {
	if j.bus == nil {
		return nil
	}
	if j.started.Load() {
		return fmt.Errorf("journal: service already started")
	}

	j.lifecycle.Start(ctx)

	j.cleanedSub = eventbus.SubscribeTo(j.bus, eventbus.Pipeline.Cleaned, eventbus.WithSubscriptionName("journal_pipeline"))
	j.lifecycleSub = eventbus.SubscribeTo(j.bus, eventbus.Sessions.Lifecycle, eventbus.WithSubscriptionName("journal_lifecycle"))
	j.toolChangedSub = eventbus.SubscribeTo(j.bus, eventbus.Sessions.ToolChanged, eventbus.WithSubscriptionName("journal_tool_changed"))

	j.lifecycle.AddSubscriptions(j.cleanedSub, j.lifecycleSub, j.toolChangedSub)

	j.lifecycle.Go(j.consumePipeline)
	j.lifecycle.Go(j.consumeLifecycle)
	j.lifecycle.Go(j.consumeToolChanged)

	j.started.Store(true)
	return nil
}

// Shutdown stops consumer goroutines and closes all cached log file handles.
// Sessions that never sent a stopped event (e.g., daemon shutting down while
// sessions are still active) have their file descriptors cleaned up here.
func (j *JournalService) Shutdown(ctx context.Context) error {
	if j.bus == nil {
		return nil
	}
	j.lifecycle.Stop()
	waitErr := j.lifecycle.Wait(ctx)
	if waitErr != nil && ctx.Err() != nil {
		// Caller's context expired before goroutines stopped. Retry with a
		// background deadline so consumer goroutines exit before we close
		// file handles — prevents a race between late writes and the cleanup
		// Range below.
		graceCtx, cancel := context.WithTimeout(context.Background(), shutdownGraceTimeout)
		if j.lifecycle.Wait(graceCtx) == nil {
			waitErr = nil // Goroutines stopped within grace period — don't mislead caller.
		}
		cancel()
	}
	// Clear activeLogs before closing file handles. If any consumer goroutine
	// survived the grace timeout, its next writeLogEntry call will see the
	// session as gone (activeLogs guard) and skip the write — preventing it
	// from creating a new file handle via getLogFile after the Range loop
	// below has finished, which would leak the fd.
	j.activeLogs.Range(func(key, _ any) bool {
		j.activeLogs.Delete(key)
		return true
	})
	// Close cached log file handles. Goroutines should be stopped at this point
	// (lifecycle.Wait succeeded), but acquire per-session mutexes as a safety
	// belt in case the grace timeout expired before all goroutines returned.
	j.logFiles.Range(func(key, value any) bool {
		sessionID := key.(string)
		mu := j.sessionLogMu(sessionID)
		mu.Lock()
		value.(*logFileEntry).file.Close()
		j.logFiles.Delete(key)
		j.logFileMus.Delete(key)
		mu.Unlock()
		return true
	})
	j.started.Store(false)
	return waitErr
}

// consumeLifecycle dispatches session lifecycle events.
func (j *JournalService) consumeLifecycle(ctx context.Context) {
	eventbus.Consume(ctx, j.lifecycleSub, nil, func(event eventbus.SessionLifecycleEvent) {
		switch event.State {
		case eventbus.SessionStateCreated:
			j.handleSessionCreated(event)
		case eventbus.SessionStateStopped:
			j.handleSessionStopped(event)
		}
	})
}

// handleSessionCreated creates a new RollingLog for the session and writes a
// persistent [session_start] entry. Unlike the old ephemeral header approach,
// the session metadata now survives RollingLog compactions as a regular raw entry.
func (j *JournalService) handleSessionCreated(event eventbus.SessionLifecycleEvent) {
	if event.SessionID == "" {
		return
	}

	journalPath := filepath.Join(j.basePath, event.SessionID+".md")

	rl, err := NewRollingLog(journalPath)
	if err != nil {
		j.logFunc("[Journal] ERROR: create rolling log for session %s: %v", event.SessionID, err)
		return
	}

	// Write session metadata as a regular raw entry so it persists.
	now := j.nowFunc()
	var toolName string
	if v, ok := j.toolCache.Load(event.SessionID); ok {
		toolName = v.(string)
	}

	entry := fmt.Sprintf("%s [session_start] Session: %s, Command: %s, Started: %s",
		now.Format("15:04:05"), event.SessionID, sanitizeForRollingLog(event.Label), now.Format(time.RFC3339))
	if toolName != "" {
		entry += fmt.Sprintf(", Tool: %s", sanitizeForRollingLog(toolName))
	}

	if err := rl.AppendRaw(entry); err != nil {
		j.logFunc("[Journal] ERROR: write session start for %s: %v", event.SessionID, err)
		return // Don't store a broken journal — prevents unbounded error log storms from subsequent events.
	}

	// Store in activeLogs only after successful persist. A journal that can't
	// write its first entry would cause an error on every subsequent event.
	j.activeLogs.Store(event.SessionID, rl)

	if err := j.writeLogEntry(event.SessionID, entry, now); err != nil {
		j.logFunc("[Journal] ERROR: write session start log for %s: %v", event.SessionID, err)
	}
}

// handleSessionStopped writes a [session_stop] entry, finalizes the journal,
// and removes it from active maps.
func (j *JournalService) handleSessionStopped(event eventbus.SessionLifecycleEvent) {
	if event.SessionID == "" {
		return
	}

	// Write a persistent [session_stop] entry before removing the journal.
	// SessionID is an internal UUID, not user input — no sanitization needed
	// (unlike Label, Reason, and tool names which may contain arbitrary text).
	v, ok := j.activeLogs.Load(event.SessionID)
	if !ok {
		j.logFunc("[Journal] WARN: stopped event for session %s but no active journal (likely race with created event)", event.SessionID)
	}
	if ok {
		rl := v.(*RollingLog)
		now := j.nowFunc()
		entry := fmt.Sprintf("%s [session_stop] Session: %s", now.Format("15:04:05"), event.SessionID)
		if event.ExitCode != nil {
			// ExitCode is *int — %d formatting is injection-safe, no sanitization needed.
			entry += fmt.Sprintf(", ExitCode: %d", *event.ExitCode)
		}
		if event.Reason != "" {
			entry += fmt.Sprintf(", Reason: %s", sanitizeForRollingLog(event.Reason))
		}
		if err := rl.AppendRaw(entry); err != nil {
			j.logFunc("[Journal] ERROR: write session stop for %s: %v", event.SessionID, err)
		}
		if err := j.writeLogEntry(event.SessionID, entry, now); err != nil {
			j.logFunc("[Journal] ERROR: write session stop log for %s: %v", event.SessionID, err)
		}
	}

	// TODO(Story 20.2): Before removing, trigger last compaction of remaining
	// raw tail if significant content exists (ShouldCompact). This requires the
	// AI compaction pipeline which is out of scope for Story 20.1.

	// Remove from active map FIRST — prevents late pipeline/tool events from
	// creating orphan file handles in writeLogEntry. The per-session mutex in
	// writeLogEntry checks activeLogs after acquiring the lock, so any late
	// arrival that raced past the activeLogs.Load in handleOutput will see the
	// session is gone and skip the write.
	j.activeLogs.Delete(event.SessionID)
	j.toolCache.Delete(event.SessionID)
	j.logDirsCreated.Delete(event.SessionID)

	// Close cached log file handle AFTER removing from active map.
	j.closeLogFile(event.SessionID)
}

// consumePipeline filters pipeline events to origin==tool and dispatches.
// NOTE: All sessions share this single consumer goroutine. Under heavy output
// from multiple sessions, I/O from one session may delay processing for others.
func (j *JournalService) consumePipeline(ctx context.Context) {
	eventbus.Consume(ctx, j.cleanedSub, nil, func(event eventbus.PipelineMessageEvent) {
		if event.Origin != eventbus.OriginTool {
			return
		}
		j.handleOutput(event)
	})
}

// handleOutput formats and appends a pipeline message to the session journal.
func (j *JournalService) handleOutput(event eventbus.PipelineMessageEvent) {
	if event.SessionID == "" {
		return
	}
	// Skip empty output unless it carries an idle annotation.
	if event.Text == "" {
		if wf := event.Annotations["waiting_for"]; wf == "" {
			return
		}
	}

	v, ok := j.activeLogs.Load(event.SessionID)
	if !ok {
		// Known race: pipeline events may arrive before consumeLifecycle has
		// processed the session's "created" event, since each topic has its own
		// consumer goroutine. These early events are silently dropped. In practice,
		// the first few output lines may be lost — acceptable because the
		// [session_start] entry captures session metadata, and meaningful output
		// typically begins after the tool has initialized.
		return
	}
	rl := v.(*RollingLog)

	now := j.nowFunc()
	ts := now.Format("15:04:05")

	// Sanitize first (normalize line endings, collapse triple newlines, strip XML
	// tags), then truncate. This order ensures truncation doesn't split a CRLF pair
	// leaving an orphan \r that sanitization would otherwise have cleaned.
	origLen := len(event.Text)
	text := sanitizeForRollingLog(event.Text)
	if len(text) > maxEntryTextBytes {
		text = text[:maxEntryTextBytes] + fmt.Sprintf("\n[truncated: %d bytes original]", origLen)
	}

	// Check for idle annotation.
	var entry string
	if waitingFor, hasWaiting := event.Annotations["waiting_for"]; hasWaiting && waitingFor != "" {
		entry = fmt.Sprintf("%s [idle] Waiting for: %s", ts, sanitizeForRollingLog(waitingFor))
	} else {
		entry = fmt.Sprintf("%s [output] %s", ts, text)
	}

	if err := rl.AppendRaw(entry); err != nil {
		j.logFunc("[Journal] ERROR: append raw for session %s: %v", event.SessionID, err)
	}

	// Also write to the append-only log file using controlled time.
	if err := j.writeLogEntry(event.SessionID, entry, now); err != nil {
		j.logFunc("[Journal] ERROR: write log for session %s: %v", event.SessionID, err)
	}
}

// consumeToolChanged handles tool change events.
func (j *JournalService) consumeToolChanged(ctx context.Context) {
	eventbus.Consume(ctx, j.toolChangedSub, nil, func(event eventbus.SessionToolChangedEvent) {
		j.handleToolChanged(event)
	})
}

// handleToolChanged appends a tool_change entry to the session journal.
func (j *JournalService) handleToolChanged(event eventbus.SessionToolChangedEvent) {
	if event.SessionID == "" {
		return
	}

	// Update tool cache.
	j.toolCache.Store(event.SessionID, event.NewTool)

	v, ok := j.activeLogs.Load(event.SessionID)
	if !ok {
		// Same race as handleOutput — see comment there.
		return
	}
	rl := v.(*RollingLog)

	ts := event.Timestamp
	if ts.IsZero() {
		ts = j.nowFunc()
	}
	entry := fmt.Sprintf("%s [tool_change] %s -> %s", ts.Format("15:04:05"),
		sanitizeForRollingLog(event.PreviousTool), sanitizeForRollingLog(event.NewTool))

	if err := rl.AppendRaw(entry); err != nil {
		j.logFunc("[Journal] ERROR: append tool change for session %s: %v", event.SessionID, err)
	}
	if err := j.writeLogEntry(event.SessionID, entry, ts); err != nil {
		j.logFunc("[Journal] ERROR: write tool change log for session %s: %v", event.SessionID, err)
	}
}

// writeLogEntry appends an entry to the session's append-only daily log file.
// The ts parameter determines the date partition for the log file, ensuring
// consistency with the entry's formatted timestamp. File handles are cached
// per session and reused across entries; they are closed on session stop or
// date rollover.
func (j *JournalService) writeLogEntry(sessionID, entry string, ts time.Time) error {
	logsPath := filepath.Join(j.basePath, "logs", sessionID)
	if _, ok := j.logDirsCreated.Load(sessionID); !ok {
		if err := os.MkdirAll(logsPath, 0o755); err != nil {
			return fmt.Errorf("create logs dir: %w", err)
		}
		j.logDirsCreated.Store(sessionID, true)
	}

	date := ts.Format("2006-01-02")

	// Serialize getLogFile + WriteString per session to prevent fd leak when
	// pipeline and tool_changed goroutines race on the same session during
	// date rollover. Per-session locking avoids cross-session contention.
	mu := j.sessionLogMu(sessionID)
	mu.Lock()
	defer mu.Unlock()

	// Guard: if session was stopped while we waited for the lock, skip to
	// prevent orphan file handles. handleSessionStopped deletes from activeLogs
	// before calling closeLogFile, so this check catches late arrivals.
	if _, ok := j.activeLogs.Load(sessionID); !ok {
		return nil
	}

	f, err := j.getLogFile(sessionID, logsPath, date)
	if err != nil {
		return err
	}

	if _, err := f.WriteString(entry + "\n"); err != nil {
		return fmt.Errorf("write log: %w", err)
	}
	// No fsync: OS page cache is sufficient for an append-only audit log.
	return nil
}

// getLogFile returns a cached file handle for the session's daily log. If the
// date has rolled over, the old handle is closed and a new one opened.
func (j *JournalService) getLogFile(sessionID, logsPath, date string) (*os.File, error) {
	if v, ok := j.logFiles.Load(sessionID); ok {
		entry := v.(*logFileEntry)
		if entry.date == date {
			return entry.file, nil
		}
		// Date rollover — close old file.
		entry.file.Close()
		j.logFiles.Delete(sessionID)
	}

	filename := filepath.Join(logsPath, date+".md")
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("open log file: %w", err)
		}
		// Directory may have been removed externally; recreate and retry.
		if mkErr := os.MkdirAll(logsPath, 0o755); mkErr != nil {
			return nil, fmt.Errorf("open log file (mkdir retry failed): %w", mkErr)
		}
		f, err = os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open log file after mkdir retry: %w", err)
		}
	}
	j.logFiles.Store(sessionID, &logFileEntry{file: f, date: date})
	return f, nil
}

// closeLogFile closes and removes the cached log file handle for a session.
// The mutex entry is deleted while the lock is held, eliminating the micro-window
// where a concurrent writeLogEntry could load the old mutex via LoadOrStore
// after our Unlock but before our Delete. Any goroutine that loaded the old mutex
// reference before the Delete is either already blocked on Lock (and will proceed
// after Unlock to find logFiles empty) or will call sessionLogMu and get a fresh
// mutex via LoadOrStore.
func (j *JournalService) closeLogFile(sessionID string) {
	mu := j.sessionLogMu(sessionID)
	mu.Lock()
	if v, ok := j.logFiles.LoadAndDelete(sessionID); ok {
		v.(*logFileEntry).file.Close()
	}
	j.logFileMus.Delete(sessionID)
	mu.Unlock()
}

// sessionLogMu returns the per-session mutex for serializing log file I/O.
func (j *JournalService) sessionLogMu(sessionID string) *sync.Mutex {
	if v, ok := j.logFileMus.Load(sessionID); ok {
		return v.(*sync.Mutex)
	}
	v, _ := j.logFileMus.LoadOrStore(sessionID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// GetContext returns summaries and raw tail for a session's journal.
// Returns empty strings (no error) if the session is not found.
// Implements intentrouter.JournalProvider.
func (j *JournalService) GetContext(sessionID string) (summaries string, raw string, err error) {
	v, ok := j.activeLogs.Load(sessionID)
	if !ok {
		return "", "", nil
	}
	rl := v.(*RollingLog)
	return rl.GetContext()
}

// sanitizeForRollingLog cleans text so it can safely pass through
// RollingLog.AppendRaw without being rejected. Collapses triple-or-more
// newlines (the entry delimiter) into double newlines, and strips any
// embedded file-format XML delimiter tags.
func sanitizeForRollingLog(text string) string {
	// Normalize CR line endings from CLI tool output.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	// Collapse runs of 3+ newlines into 2 (single pass, O(n)).
	var b strings.Builder
	b.Grow(len(text))
	nlCount := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			nlCount++
			if nlCount <= 2 {
				b.WriteByte('\n')
			}
		} else {
			nlCount = 0
			b.WriteByte(text[i])
		}
	}
	text = b.String()
	// Remove file-format delimiter tags that would corrupt parsing.
	text = strings.ReplaceAll(text, tagSummariesOpen, "")
	text = strings.ReplaceAll(text, tagSummariesClose, "")
	text = strings.ReplaceAll(text, tagRawOpen, "")
	text = strings.ReplaceAll(text, tagRawClose, "")
	return text
}
