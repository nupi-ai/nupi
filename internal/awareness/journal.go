package awareness

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

// maxEntryTextBytes is the maximum size of event.Text accepted before truncation.
// Prevents unbounded memory and disk usage from CLI tools that dump large output.
const maxEntryTextBytes = 64 * 1024

// shutdownGraceTimeout is the maximum time Shutdown waits for consumer
// goroutines to exit when the caller's context has already expired. 5 seconds
// is sufficient for consumer goroutines to drain their event channels
// and complete any in-flight file I/O.
const shutdownGraceTimeout = 5 * time.Second

// minFinalizationTailSize is the minimum raw tail size (in bytes) to trigger
// a finalization compaction when a session is stopped.
const minFinalizationTailSize = 1000

// timestampRe extracts HH:MM:SS timestamps from rolling log entries.
var timestampRe = regexp.MustCompile(`(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d`)

// journalPendingCompaction tracks an in-flight compaction request awaiting AI
// reply. The "journal" prefix disambiguates from convPendingCompaction in
// conversation_log.go (both types live in the awareness package but track
// different compaction pipelines). The rl field retains a reference to the
// session's RollingLog so that the reply handler can write the summary even
// after the session has been removed from activeLogs (finalization compaction
// on session stop).
type journalPendingCompaction struct {
	sessionID string
	rl        *RollingLog
	startTime string // first timestamp from older half (for summary header)
	endTime   string // last timestamp from older half (for summary header)
}

// JournalService captures per-session CLI output into rolling-log journals.
// It subscribes to pipeline cleaned, session lifecycle, tool changed, and
// conversation reply events to maintain a live journal file per active session,
// with automatic compaction and archival. Implements runtime.Service.
type JournalService struct {
	bus      *eventbus.Bus
	basePath string // absolute path to journals/ directory

	activeLogs          sync.Map // sessionID -> *RollingLog
	toolCache           sync.Map // sessionID -> string (current tool name)
	logDirsCreated      sync.Map // sessionID -> bool (log dir created)
	logFiles            sync.Map // sessionID -> *logFileEntry (cached daily log file handle)
	logFileMus          sync.Map // sessionID -> *sync.Mutex — per-session serialization of getLogFile + WriteString
	pendingCompactions   sync.Map // promptID -> *journalPendingCompaction
	compactionInFlight   sync.Map // sessionID -> bool — dedup guard, at most one in-flight compaction per session
	compactionStartedAt  sync.Map // sessionID -> int64 (UnixNano timestamp of when compaction was triggered)

	cleanedSub     *eventbus.TypedSubscription[eventbus.PipelineMessageEvent]
	lifecycleSub   *eventbus.TypedSubscription[eventbus.SessionLifecycleEvent]
	toolChangedSub *eventbus.TypedSubscription[eventbus.SessionToolChangedEvent]
	replySub       *eventbus.TypedSubscription[eventbus.ConversationReplyEvent]

	lifecycle eventbus.ServiceLifecycle
	started  atomic.Bool

	// nowFunc returns the current time. Overridden in tests.
	nowFunc func() time.Time
	// logFunc logs errors. Defaults to log.Printf. Overridden in tests.
	logFunc func(format string, args ...any)
	// staleTimeout is the duration after which an in-flight compaction is
	// considered stale. Defaults to defaultStaleCompactionTimeout. Overridden
	// in tests for faster stale-detection verification.
	staleTimeout time.Duration
}

// logFileEntry caches an open file handle for a session's daily log file.
type logFileEntry struct {
	file *os.File
	date string // YYYY-MM-DD, used to detect date rollover
}

// NewJournalService creates a JournalService writing journals under basePath.
// basePath must be an absolute path to the journals directory
// (e.g., "{instanceDir}/awareness/memory/journals").
// Panics if basePath is not absolute — a relative basePath would cause
// RollingLog.Archive() to fail at runtime with a confusing error.
func NewJournalService(bus *eventbus.Bus, basePath string) *JournalService {
	if !filepath.IsAbs(basePath) {
		panic(fmt.Sprintf("awareness: NewJournalService requires absolute basePath, got: %q", basePath))
	}
	return &JournalService{
		bus:          bus,
		basePath:     basePath,
		nowFunc:      func() time.Time { return time.Now().UTC() },
		logFunc:      log.Printf,
		staleTimeout: defaultStaleCompactionTimeout,
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
	j.replySub = eventbus.SubscribeTo(j.bus, eventbus.Conversation.Reply, eventbus.WithSubscriptionName("journal_compaction_reply"))

	j.lifecycle.AddSubscriptions(j.cleanedSub, j.lifecycleSub, j.toolChangedSub, j.replySub)

	j.lifecycle.Go(j.consumePipeline)
	j.lifecycle.Go(j.consumeLifecycle)
	j.lifecycle.Go(j.consumeToolChanged)
	j.lifecycle.Go(j.consumeCompactionReply)

	j.started.Store(true)
	return nil
}

// Shutdown stops consumer goroutines and closes all cached log file handles.
// Sessions that never sent a stopped event (e.g., daemon shutting down while
// sessions are still active) have their file descriptors cleaned up here.
// Safe to call without a preceding Start() — returns nil immediately.
func (j *JournalService) Shutdown(ctx context.Context) error {
	if j.bus == nil {
		return nil
	}
	if !j.started.Load() {
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
	// Mark as stopped FIRST — prevents writeLogEntry / handleOutput from
	// creating new file handles after the cleanup loops below (defense-in-
	// depth; consumer goroutines should already be drained by lifecycle.Wait).
	j.started.Store(false)
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
		if err := value.(*logFileEntry).file.Close(); err != nil {
			j.logFunc("[Journal] WARN: close log file for session %s during shutdown: %v", sessionID, err)
		}
		j.logFiles.Delete(key)
		j.logFileMus.Delete(key)
		mu.Unlock()
		return true
	})
	// Clear compaction tracking maps. Stale entries from the previous
	// lifecycle would permanently block compaction for reused session IDs
	// if the service is restarted via Start().
	// WARNING: Each dropped entry represents data loss — OlderHalf() already
	// deleted the raw entries, but the AI summary was never written back.
	droppedCompactions := 0
	j.pendingCompactions.Range(func(key, v any) bool {
		pc := v.(*journalPendingCompaction)
		j.logFunc("[Journal] WARN: dropping unresolved compaction (promptID=%s, session=%s) during shutdown — raw entries were compacted but summary was never received", key, pc.sessionID)
		j.pendingCompactions.Delete(key)
		droppedCompactions++
		return true
	})
	if droppedCompactions > 0 {
		j.logFunc("[Journal] WARN: dropped %d unresolved compaction(s) during shutdown — potential data loss", droppedCompactions)
	}
	j.compactionInFlight.Range(func(key, _ any) bool {
		j.compactionInFlight.Delete(key)
		return true
	})
	j.compactionStartedAt.Range(func(key, _ any) bool {
		j.compactionStartedAt.Delete(key)
		return true
	})
	// Clear session metadata caches. Stale logDirsCreated entries would skip
	// MkdirAll on restart if the logs directory was removed externally between
	// cycles, causing writeLogEntry to fail with "no such file or directory".
	// Stale toolCache entries would inject wrong tool names into [session_start]
	// entries for reused session IDs.
	j.logDirsCreated.Range(func(key, _ any) bool {
		j.logDirsCreated.Delete(key)
		return true
	})
	j.toolCache.Range(func(key, _ any) bool {
		j.toolCache.Delete(key)
		return true
	})
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

	// TODO(Epic 20): No ShouldCompact() check here. On daemon restart with a
	// pre-existing near-threshold journal, this single [session_start] entry
	// (~100 bytes) won't trigger compaction. The next handleOutput call will.
	// Adding ShouldCompact here causes a data race with tests that modify
	// compactionThreshold directly on the RollingLog after creation. Fix
	// requires exposing a compaction threshold option on NewRollingLog or
	// NewJournalService instead of direct field mutation.

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

	v, ok := j.activeLogs.Load(event.SessionID)
	if !ok {
		// No active journal for this session — nothing to finalize or clean up.
		// This happens when the stopped event arrives before the created event
		// (cross-goroutine race) or for sessions that failed journal creation.
		j.logFunc("[Journal] WARN: stopped event for session %s but no active journal (likely race with created event)", event.SessionID)
		return
	}
	rl := v.(*RollingLog)

	// Finalization compaction: trigger before writing [session_stop] so the
	// stop entry always stays in the raw tail (never gets compacted into a
	// summary). The compaction reply handler resolves by promptID (not
	// activeLogs), so it can handle the reply even after the session is
	// removed from activeLogs below.
	if rl.ShouldCompact() && rl.RawTailSize() > minFinalizationTailSize {
		j.triggerCompaction(event.SessionID, rl)
	}

	// Write a persistent [session_stop] entry after any finalization compaction.
	// SessionID is an internal UUID, not user input — no sanitization needed
	// (unlike Label, Reason, and tool names which may contain arbitrary text).
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

	// Remove from active map FIRST — prevents late pipeline/tool events from
	// creating orphan file handles in writeLogEntry. The per-session mutex in
	// writeLogEntry checks activeLogs after acquiring the lock, so any late
	// arrival that raced past the activeLogs.Load in handleOutput will see the
	// session is gone and skip the write.
	j.activeLogs.Delete(event.SessionID)
	j.toolCache.Delete(event.SessionID)
	j.logDirsCreated.Delete(event.SessionID)
	j.compactionInFlight.Delete(event.SessionID)
	j.compactionStartedAt.Delete(event.SessionID)

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
	// origLen captures pre-sanitization size for the truncation annotation,
	// giving the user visibility into the original payload size.
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

	// Check if compaction is needed after appending.
	if rl.ShouldCompact() {
		j.triggerCompaction(event.SessionID, rl)
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

	// Check if compaction is needed after appending (same as handleOutput).
	if rl.ShouldCompact() {
		j.triggerCompaction(event.SessionID, rl)
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

// writeFallbackLogEntry writes directly to a session's daily log file without
// checking activeLogs. Used by handleCompactionReply's AppendSummary failure
// path, which intentionally runs even after the session has been stopped
// (replies MUST be processed because OlderHalf already deleted the raw entries).
// The regular writeLogEntry checks activeLogs and would silently discard the
// fallback after session stop.
func (j *JournalService) writeFallbackLogEntry(sessionID, entry string, ts time.Time) error {
	logsPath := filepath.Join(j.basePath, "logs", sessionID)
	if err := os.MkdirAll(logsPath, 0o755); err != nil {
		return fmt.Errorf("create logs dir: %w", err)
	}
	date := ts.Format("2006-01-02")
	filename := filepath.Join(logsPath, date+".md")
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open fallback log file: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(entry + "\n"); err != nil {
		return fmt.Errorf("write fallback log: %w", err)
	}
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
		if err := entry.file.Close(); err != nil {
			j.logFunc("[Journal] WARN: close stale log file for session %s (date rollover): %v", sessionID, err)
		}
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
		if err := v.(*logFileEntry).file.Close(); err != nil {
			j.logFunc("[Journal] WARN: close log file for session %s: %v", sessionID, err)
		}
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

// triggerCompaction extracts the older half of a session's raw tail and
// publishes a ConversationPromptEvent for AI summarization. The promptID is
// stored in pendingCompactions so the reply handler can match it.
func (j *JournalService) triggerCompaction(sessionID string, rl *RollingLog) {
	// Skip if a compaction for this session is already in flight. Without this
	// guard, a non-responsive AI adapter would cause unbounded growth of the
	// pendingCompactions map as each ShouldCompact() == true generates a new entry.
	if _, loaded := j.compactionInFlight.LoadOrStore(sessionID, true); loaded {
		j.checkStaleCompaction(sessionID)
		// After clearing stale state, retry the guard. If checkStaleCompaction
		// cleared compactionInFlight, LoadOrStore succeeds and we proceed.
		if _, loaded := j.compactionInFlight.LoadOrStore(sessionID, true); loaded {
			return
		}
	}
	j.compactionStartedAt.Store(sessionID, time.Now().UnixNano())

	olderHalf, err := rl.OlderHalf()
	if err != nil {
		j.compactionInFlight.Delete(sessionID)
		j.logFunc("[Journal] ERROR: extract older half for session %s: %v", sessionID, err)
		return
	}
	if olderHalf == "" {
		j.compactionInFlight.Delete(sessionID)
		return
	}

	// Extract start/end timestamps from the older half text.
	startTime, endTime := extractTimeRange(olderHalf)

	// Use time.Now() (not nowFunc) for promptID uniqueness. nowFunc is
	// overridden to a fixed time in tests for deterministic archive dates;
	// using it here would produce duplicate promptIDs across multiple
	// compactions within the same test.
	promptID := fmt.Sprintf("journal-compact-%s-%d", sessionID, time.Now().UnixNano())

	j.pendingCompactions.Store(promptID, &journalPendingCompaction{
		sessionID: sessionID,
		rl:        rl,
		startTime: startTime,
		endTime:   endTime,
	})

	// context.Background() is intentional: the compaction prompt must be
	// delivered even if the service is shutting down, so we don't propagate
	// the lifecycle context which may already be canceled.
	eventbus.Publish(context.Background(), j.bus, eventbus.Conversation.Prompt, eventbus.SourceAwareness,
		eventbus.ConversationPromptEvent{
			SessionID: sessionID,
			PromptID:  promptID,
			NewMessage: eventbus.ConversationMessage{
				Origin: eventbus.OriginSystem,
				Text:   olderHalf,
			},
			Metadata: map[string]string{
				constants.MetadataKeyEventType: constants.PromptEventJournalCompaction,
			},
		})
	j.logFunc("[Journal] INFO: compaction triggered (session=%s, promptID=%s, range=%s–%s)", sessionID, promptID, startTime, endTime)
}

// checkStaleCompaction detects and clears per-session compaction state that has
// been in-flight for longer than staleCompactionTimeout. See the
// ConversationLogService.checkStaleCompaction doc comment for rationale.
func (j *JournalService) checkStaleCompaction(sessionID string) {
	v, ok := j.compactionStartedAt.Load(sessionID)
	if !ok {
		return
	}
	startedAt := v.(int64)
	if startedAt == 0 {
		return
	}
	age := time.Since(time.Unix(0, startedAt))
	if age < j.staleTimeout {
		return
	}

	j.logFunc("[Journal] WARN: compaction for session %s has been in-flight for %v (threshold %v) — clearing stale state to unblock pipeline", sessionID, age, j.staleTimeout)
	// Find and drop the stale pending compaction entry for this session.
	j.pendingCompactions.Range(func(key, value any) bool {
		pc := value.(*journalPendingCompaction)
		if pc.sessionID == sessionID {
			j.logFunc("[Journal] WARN: dropping stale compaction (promptID=%s, session=%s, range=%s–%s) — raw entries were compacted but summary was never received", key, pc.sessionID, pc.startTime, pc.endTime)
			j.pendingCompactions.Delete(key)
		}
		return true
	})
	j.compactionInFlight.Delete(sessionID)
	j.compactionStartedAt.Delete(sessionID)
}

// consumeCompactionReply handles AI-generated summaries for compaction requests.
func (j *JournalService) consumeCompactionReply(ctx context.Context) {
	eventbus.Consume(ctx, j.replySub, nil, func(event eventbus.ConversationReplyEvent) {
		j.handleCompactionReply(event)
	})
}

// handleCompactionReply processes a ConversationReplyEvent if it matches a
// pending compaction. It formats the summary with a header, calls AppendSummary,
// and triggers archival if needed.
func (j *JournalService) handleCompactionReply(event eventbus.ConversationReplyEvent) {
	if event.PromptID == "" {
		return
	}

	v, ok := j.pendingCompactions.LoadAndDelete(event.PromptID)
	if !ok {
		return // Not a compaction reply — belongs to another consumer.
	}
	pc := v.(*journalPendingCompaction)
	// Defer clearing the guard so new compactions for this session are blocked
	// until ALL work (AppendSummary + triggerArchival) completes. Clearing
	// early would allow a new triggerCompaction from consumePipeline while
	// this reply is still being finalized, wasting AI adapter calls.
	defer func() {
		j.compactionInFlight.Delete(pc.sessionID)
		j.compactionStartedAt.Delete(pc.sessionID)
	}()

	// Use the retained RollingLog reference from journalPendingCompaction. This
	// handles both the normal case (session still active) and the finalization
	// case (session removed from activeLogs during stop — the retained
	// reference allows writing the summary regardless, preventing data loss).
	rl := pc.rl
	if rl == nil {
		j.logFunc("[Journal] WARN: nil RollingLog in pending compaction (promptID=%s, session=%s) — cannot write summary", event.PromptID, pc.sessionID)
		return
	}

	// Guard: if the AI returned an empty reply (error, content filter, empty
	// response), skip AppendSummary. The older half raw entries were already
	// deleted by OlderHalf() in triggerCompaction — writing a header-only
	// summary with no content would be meaningless. Log a warning so the
	// data loss is visible in diagnostics.
	if strings.TrimSpace(event.Text) == "" {
		j.logFunc("[Journal] WARN: empty compaction reply for session %s (promptID=%s) — raw entries already compacted, skipping summary", pc.sessionID, event.PromptID)
		return
	}

	// Sanitize AI reply text before building the summary. AI-generated text
	// may contain "\n\n### " (markdown headers that would split the summary
	// into multiple entries on reload), format delimiter tags, or triple
	// newlines that corrupt round-trip parsing.
	sanitizedText := sanitizeForSummary(event.Text)

	// Format with a proper "### " header for AppendSummary.
	header := fmt.Sprintf("### [summary] %s – %s", pc.startTime, pc.endTime)
	formattedSummary := header + "\n" + sanitizedText

	if err := rl.AppendSummary(formattedSummary); err != nil {
		j.logFunc("[Journal] ERROR: append summary for session %s (promptID=%s, range=%s–%s): %v", pc.sessionID, event.PromptID, pc.startTime, pc.endTime, err)
		// Fallback: write the AI summary to the daily audit log so it is not
		// permanently lost. OlderHalf() already deleted the raw entries; if the
		// rolling log write also fails (e.g., disk full), this is the last
		// chance to preserve the summarized content.
		fallbackEntry := fmt.Sprintf("[compaction-fallback] %s\n%s", header, sanitizedText)
		if fbErr := j.writeFallbackLogEntry(pc.sessionID, fallbackEntry, j.nowFunc()); fbErr != nil {
			j.logFunc("[Journal] ERROR: fallback audit log write also failed (session=%s, promptID=%s): %v", pc.sessionID, event.PromptID, fbErr)
		} else {
			j.logFunc("[Journal] INFO: AI summary saved to audit log as fallback (session=%s, promptID=%s)", pc.sessionID, event.PromptID)
		}
		return
	}
	j.logFunc("[Journal] INFO: compaction reply processed (session=%s, promptID=%s, range=%s–%s)", pc.sessionID, event.PromptID, pc.startTime, pc.endTime)

	// After adding a summary, check if archival is needed.
	if rl.ShouldArchive() {
		j.triggerArchival(pc.sessionID, rl)
	}
}

// triggerArchival moves older summaries to an archive file and triggers FTS5
// re-indexing via AwarenessSyncEvent.
//
// IMPORTANT: This method must only be called from handleCompactionReply (which
// runs in the single consumeCompactionReply goroutine). The OlderSummaries →
// Archive → CommitArchival sequence is NOT thread-safe: OlderSummaries takes
// an RLock and returns a slice that becomes stale if another goroutine calls
// CommitArchival or AppendSummary concurrently (see rolling_log.go TOCTOU
// warning). The single-goroutine guarantee serializes all archival calls.
func (j *JournalService) triggerArchival(sessionID string, rl *RollingLog) {
	summaries := rl.OlderSummaries()
	if len(summaries) == 0 {
		return
	}

	archivePath := filepath.Join(j.basePath, "archives", sessionID)
	archiveDate := j.nowFunc().Format("2006-01-02")
	archiveFile := filepath.Join(archivePath, archiveDate+".md")

	// Check existence BEFORE Archive() writes/appends to determine correct SyncType.
	_, statErr := os.Stat(archiveFile)
	archiveExisted := statErr == nil

	if err := rl.Archive(archivePath, summaries, archiveDate); err != nil {
		j.logFunc("[Journal] ERROR: archive summaries for session %s: %v", sessionID, err)
		return
	}
	// NOTE: If CommitArchival fails (e.g., disk full on atomic rename), the
	// summaries remain in live state while also persisted in the archive file.
	// On the next ShouldArchive() → OlderSummaries() cycle, the same summaries
	// will be re-archived (appended again via O_APPEND), creating duplicates.
	// This is acceptable: the failure scenario is rare (requires a transient
	// disk error between Archive and CommitArchival), and duplicate summaries
	// in FTS5 search results are harmless (same content, slightly redundant).
	if err := rl.CommitArchival(len(summaries)); err != nil {
		j.logFunc("[Journal] ERROR: commit archival for session %s: %v", sessionID, err)
		return
	}

	syncType := eventbus.SyncTypeCreated
	if archiveExisted {
		syncType = eventbus.SyncTypeUpdated
	}
	// context.Background() is intentional: the sync event must be delivered
	// even during shutdown, so we don't propagate the lifecycle context.
	eventbus.Publish(context.Background(), j.bus, eventbus.Memory.Sync, eventbus.SourceAwareness,
		eventbus.AwarenessSyncEvent{
			FilePath: archiveFile,
			SyncType: syncType,
		})

	j.logFunc("[Journal] INFO: archived %d summaries for session %s → %s", len(summaries), sessionID, archiveFile)
}

// extractTimeRange finds the first and last HH:MM:SS timestamps in text.
func extractTimeRange(text string) (start, end string) {
	matches := timestampRe.FindAllString(text, -1)
	if len(matches) == 0 {
		return "??:??:??", "??:??:??"
	}
	return matches[0], matches[len(matches)-1]
}

// sanitizeForSummary cleans AI-generated summary text so it can safely pass
// through RollingLog.AppendSummary. Specifically:
//   - Strips format delimiter tags (would corrupt file parsing on reload)
//   - Strips leading "### " if the AI reply starts with it (would create a
//     confusing double-header when prepended with the [summary] header)
//   - Replaces "\n\n### " with "\n\n#### " (would split into multiple entries)
//   - Collapses 3+ consecutive newlines into double newlines
func sanitizeForSummary(text string) string {
	// Normalize CR line endings from AI adapters (same as sanitizeForRollingLog).
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	// Strip format delimiter tags.
	text = strings.ReplaceAll(text, tagSummariesOpen, "")
	text = strings.ReplaceAll(text, tagSummariesClose, "")
	text = strings.ReplaceAll(text, tagRawOpen, "")
	text = strings.ReplaceAll(text, tagRawClose, "")
	// Strip leading "### " if the AI starts its reply with a markdown H3
	// header. The caller prepends its own "### [summary] ..." header, so a
	// leading "### " in the reply body creates a confusing double-header.
	if strings.HasPrefix(text, "### ") {
		text = "#### " + text[4:]
	}
	// Downgrade "### " headers preceded by a blank line to "#### " so they
	// don't split the summary into multiple entries on file reload.
	text = strings.ReplaceAll(text, "\n\n### ", "\n\n#### ")
	return collapseExcessiveNewlines(text)
}

// sanitizeForRollingLog cleans text so it can safely pass through
// RollingLog.AppendRaw without being rejected. Strips file-format delimiter
// tags and collapses triple-or-more newlines (the entry delimiter) into
// double newlines. Operation order matches sanitizeForSummary:
// 1. Normalize CR line endings
// 2. Strip format delimiter tags (may introduce new newline sequences)
// 3. Collapse 3+ consecutive newlines
func sanitizeForRollingLog(text string) string {
	// Normalize CR line endings from CLI tool output.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	// Remove file-format delimiter tags that would corrupt parsing.
	// Done before newline collapsing so that tag removal doesn't create
	// new triple-newline sequences that go uncollapsed.
	text = strings.ReplaceAll(text, tagSummariesOpen, "")
	text = strings.ReplaceAll(text, tagSummariesClose, "")
	text = strings.ReplaceAll(text, tagRawOpen, "")
	text = strings.ReplaceAll(text, tagRawClose, "")
	return collapseExcessiveNewlines(text)
}

// collapseExcessiveNewlines replaces runs of 3 or more consecutive newlines
// with exactly 2 newlines. Single pass, O(n). Used by both sanitizeForSummary
// and sanitizeForRollingLog to prevent entry delimiter injection (the rolling
// log format uses "\n\n\n" as the entry separator).
func collapseExcessiveNewlines(text string) string {
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
	return b.String()
}
