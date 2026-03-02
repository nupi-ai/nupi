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

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

// conversationCompactionThreshold is the raw tail byte threshold for
// conversation.md compaction. Matches spec section 4.6 default.
const conversationCompactionThreshold = 16000

// conversationSummaryBudget is the summary byte budget for conversation.md.
const conversationSummaryBudget = 10000

// defaultStaleCompactionTimeout is the default duration after which an
// in-flight compaction is considered stale. If no AI reply arrives within
// this window, the compaction guard is force-cleared to unblock future
// compactions. Services use the staleTimeout field, which defaults to this
// value and can be overridden in tests.
const defaultStaleCompactionTimeout = 5 * time.Minute

// ConversationLogService captures all conversation turns into a global rolling
// log file (conversations/conversation.md) and daily append-only log files
// (conversations/logs/YYYY-MM-DD.md). It subscribes to TopicConversationTurn
// events and implements runtime.Service and intentrouter.ConversationLogProvider.
type ConversationLogService struct {
	bus      *eventbus.Bus
	basePath string // absolute path to conversations/ directory

	rl atomic.Pointer[RollingLog] // single global rolling log at basePath/conversation.md

	logFiles      sync.Map // date string -> *logFileEntry (logFileEntry defined in journal.go)
	logFileMu     sync.Mutex
	logDirCreated atomic.Bool

	// Compaction state — single track (one global rolling log, not per-session).
	pendingCompactions   sync.Map    // promptID -> *convPendingCompaction
	compactionInFlight   atomic.Bool // dedup guard: at most one in-flight compaction
	compactionStartedAt  atomic.Int64 // UnixNano timestamp of when compaction was triggered (for stale detection)

	lifecycle eventbus.ServiceLifecycle
	started   atomic.Bool

	turnSub  *eventbus.TypedSubscription[eventbus.ConversationTurnEvent]
	replySub *eventbus.TypedSubscription[eventbus.ConversationReplyEvent]

	// nowFunc provides fallback time when Turn.At is zero (defensive).
	// In production, handleTurn uses the event-authoritative Turn.At timestamp;
	// nowFunc is only called if Turn.At is zero. Overridden in tests.
	nowFunc func() time.Time
	// logFunc logs errors. Defaults to log.Printf. Overridden in tests.
	logFunc func(format string, args ...any)
	// staleTimeout is the duration after which an in-flight compaction is
	// considered stale. Defaults to defaultStaleCompactionTimeout. Overridden
	// in tests for faster stale-detection verification.
	staleTimeout time.Duration
}

// convPendingCompaction tracks an in-flight compaction request awaiting AI reply.
// The "conv" prefix disambiguates from journalPendingCompaction in journal.go
// (both types live in the awareness package but track different pipelines).
// The rl field retains a reference to the RollingLog so that the reply handler
// can write the summary even after Shutdown() has set s.rl to nil (prevents
// nil pointer race between reply delivery and shutdown).
type convPendingCompaction struct {
	rl        *RollingLog
	startTime string // first timestamp from older half
	endTime   string // last timestamp from older half
}

// NewConversationLogService creates a ConversationLogService writing under basePath.
// basePath must be an absolute path to the conversations directory
// (e.g., "{instanceDir}/awareness/memory/conversations").
// Panics if basePath is not absolute.
func NewConversationLogService(bus *eventbus.Bus, basePath string) *ConversationLogService {
	if !filepath.IsAbs(basePath) {
		panic(fmt.Sprintf("awareness: NewConversationLogService requires absolute basePath, got: %q", basePath))
	}
	return &ConversationLogService{
		bus:          bus,
		basePath:     basePath,
		nowFunc:      func() time.Time { return time.Now().UTC() },
		logFunc:      log.Printf,
		staleTimeout: defaultStaleCompactionTimeout,
	}
}

// Start subscribes to conversation turn events and launches the consumer goroutine.
func (s *ConversationLogService) Start(ctx context.Context) error {
	if s.bus == nil {
		return nil
	}
	if s.started.Load() {
		return fmt.Errorf("conversation_log: service already started")
	}

	// Create RollingLog at basePath/conversation.md.
	convPath := filepath.Join(s.basePath, "conversation.md")
	rl, err := NewRollingLog(convPath,
		WithCompactionThreshold(conversationCompactionThreshold),
		WithSummaryBudget(conversationSummaryBudget),
	)
	if err != nil {
		return fmt.Errorf("conversation_log: create rolling log: %w", err)
	}
	s.rl.Store(rl)

	s.lifecycle.Start(ctx)

	s.turnSub = eventbus.SubscribeTo(s.bus, eventbus.Conversation.Turn, eventbus.WithSubscriptionName("conversation_log_turn"))
	// NOTE: Both ConversationLogService and JournalService subscribe to
	// Conversation.Reply. Each filters by PromptID against its own
	// pendingCompactions map — non-matching replies are discarded in O(1).
	// A separate compaction-reply topic was considered but rejected: it would
	// require the intent router to know which topic to publish on, coupling
	// the router to awareness internals.
	s.replySub = eventbus.SubscribeTo(s.bus, eventbus.Conversation.Reply, eventbus.WithSubscriptionName("conversation_log_compaction_reply"))
	s.lifecycle.AddSubscriptions(s.turnSub, s.replySub)
	s.lifecycle.Go(s.consumeTurns)
	s.lifecycle.Go(s.consumeCompactionReply)

	s.started.Store(true)
	return nil
}

// Shutdown stops the consumer goroutine and closes all cached log file handles.
// Safe to call without a preceding Start() — returns nil immediately.
func (s *ConversationLogService) Shutdown(ctx context.Context) error {
	if s.bus == nil {
		return nil
	}
	if !s.started.Load() {
		return nil
	}
	s.lifecycle.Stop()
	waitErr := s.lifecycle.Wait(ctx)
	if waitErr != nil && ctx.Err() != nil {
		// shutdownGraceTimeout (5s) is defined in journal.go and shared by all
		// awareness services (JournalService, ConversationLogService).
		graceCtx, cancel := context.WithTimeout(context.Background(), shutdownGraceTimeout)
		if s.lifecycle.Wait(graceCtx) == nil {
			waitErr = nil
		}
		cancel()
	}

	// Mark as stopped FIRST — prevents writeLogEntry from creating new file
	// handles after the cleanup loop below (defense-in-depth; the consumer
	// goroutine should already be drained by lifecycle.Wait above).
	s.started.Store(false)

	// Close cached log file handles.
	s.logFileMu.Lock()
	s.logFiles.Range(func(key, value any) bool {
		if err := value.(*logFileEntry).file.Close(); err != nil {
			s.logFunc("[ConversationLog] WARN: close log file %s during shutdown: %v", key, err)
		}
		s.logFiles.Delete(key)
		return true
	})
	s.logFileMu.Unlock()

	// Clear compaction tracking state. Stale entries from the previous
	// lifecycle would permanently block compaction if the service is restarted.
	// WARNING: Each dropped entry represents data loss — OlderHalf() already
	// deleted the raw entries, but the AI summary was never written back.
	droppedCompactions := 0
	s.pendingCompactions.Range(func(key, value any) bool {
		pc := value.(*convPendingCompaction)
		s.logFunc("[ConversationLog] WARN: dropping unresolved compaction (promptID=%s, range=%s–%s) during shutdown — raw entries were compacted but summary was never received", key, pc.startTime, pc.endTime)
		s.pendingCompactions.Delete(key)
		droppedCompactions++
		return true
	})
	if droppedCompactions > 0 {
		s.logFunc("[ConversationLog] WARN: dropped %d unresolved compaction(s) during shutdown — potential data loss", droppedCompactions)
	}
	s.compactionInFlight.Store(false)
	s.compactionStartedAt.Store(0)

	s.logDirCreated.Store(false)
	s.rl.Store(nil)
	return waitErr
}

// Slice returns a paginated view of conversation turns parsed from the raw tail.
// Implements server.ConversationStore. Safe for concurrent use.
//
// Uses SliceRawTail to atomically read total count and entries in a single lock,
// preventing TOCTOU races during compaction. Because AppendRaw and parseFile
// never store empty entries, total == number of parseable turns.
//
// Note: returned turns have nil Meta fields (raw tail stores plain text only)
// and their At dates use today's date + stored HH:MM:SS (see parseOneTurn doc).
func (s *ConversationLogService) Slice(offset, limit int) (int, []eventbus.ConversationTurn, error) {
	rl := s.rl.Load()
	if rl == nil {
		return 0, nil, nil
	}

	total, entries := rl.SliceRawTail(offset, limit)

	today := s.nowFunc().Format("2006-01-02")
	turns := make([]eventbus.ConversationTurn, 0, len(entries))
	for _, entry := range entries {
		turn, ok := parseOneTurn(entry, today)
		if ok {
			turns = append(turns, turn)
		}
	}

	return total, turns, nil
}

// parseOneTurn parses a single raw tail entry "HH:MM:SS [role] text" into a ConversationTurn.
// The datePrefix (format "2006-01-02") is combined with the parsed HH:MM:SS to reconstruct
// the full timestamp. Since raw entries only store time (no date), the caller provides the
// date — typically today's date from nowFunc(). This means entries from previous days that
// haven't been compacted will appear with the query date, not their original date.
// The returned turn has a nil Meta field because raw tail entries store only plain text;
// per-turn metadata annotations are not persisted through the rolling log format.
func parseOneTurn(entry, datePrefix string) (eventbus.ConversationTurn, bool) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return eventbus.ConversationTurn{}, false
	}

	// Expected format: "HH:MM:SS [role] text"
	// Minimum valid: "00:00:00 [ai] x" = 15 bytes (shortest real role is "ai")
	if len(entry) < 15 {
		return eventbus.ConversationTurn{
			Origin: eventbus.OriginSystem,
			Text:   entry,
		}, true
	}

	// Parse time: first 8 chars must be HH:MM:SS
	timePart := entry[:8]
	ts, err := time.Parse("15:04:05", timePart)
	if err != nil {
		return eventbus.ConversationTurn{
			Origin: eventbus.OriginSystem,
			Text:   entry,
		}, true
	}

	rest := entry[8:]
	if len(rest) < 2 || rest[0] != ' ' || rest[1] != '[' {
		return eventbus.ConversationTurn{
			Origin: eventbus.OriginSystem,
			Text:   entry,
		}, true
	}

	closeBracket := strings.Index(rest, "] ")
	if closeBracket < 0 {
		return eventbus.ConversationTurn{
			Origin: eventbus.OriginSystem,
			Text:   entry,
		}, true
	}

	role := rest[2:closeBracket]
	text := rest[closeBracket+2:]

	// Reconstruct timestamp: today's date + parsed time
	fullTS, _ := time.Parse("2006-01-02 15:04:05", datePrefix+" "+timePart)
	if fullTS.IsZero() {
		fullTS = ts
	}

	return eventbus.ConversationTurn{
		Origin: roleToOrigin(role),
		Text:   text,
		At:     fullTS,
	}, true
}

// roleToOrigin maps a role string from conversation log entries back to ContentOrigin.
func roleToOrigin(role string) eventbus.ContentOrigin {
	switch role {
	case "user":
		return eventbus.OriginUser
	case "ai":
		return eventbus.OriginAI
	case "tool":
		return eventbus.OriginTool
	case "system":
		return eventbus.OriginSystem
	default:
		return eventbus.OriginSystem
	}
}

// GetContext returns summaries and raw tail from the conversation rolling log.
// Implements intentrouter.ConversationLogProvider. Safe for concurrent use —
// rl is accessed via atomic.Pointer to avoid data races with Shutdown().
func (s *ConversationLogService) GetContext() (summaries string, raw string, err error) {
	rl := s.rl.Load()
	if rl == nil {
		return "", "", nil
	}
	return rl.GetContext()
}

// consumeTurns reads conversation turn events and dispatches them.
func (s *ConversationLogService) consumeTurns(ctx context.Context) {
	eventbus.Consume(ctx, s.turnSub, nil, func(event eventbus.ConversationTurnEvent) {
		s.handleTurn(event)
	})
}

// handleTurn formats and appends a conversation turn to the rolling log
// and daily log file.
func (s *ConversationLogService) handleTurn(event eventbus.ConversationTurnEvent) {
	if !s.started.Load() {
		return
	}
	turn := event.Turn
	if strings.TrimSpace(turn.Text) == "" {
		return
	}

	// Use the event's authoritative timestamp (set by the conversation
	// service when the turn was created). Fall back to nowFunc() only if
	// Turn.At is zero (defensive, should not happen in production).
	now := turn.At
	if now.IsZero() {
		now = s.nowFunc()
	}
	ts := now.Format("15:04:05")
	role := originToRole(turn.Origin)

	// Sanitize and truncate (maxEntryTextBytes = 64KB, defined in journal.go).
	// origLen captures pre-sanitization size for the truncation annotation,
	// giving the user visibility into the original payload size.
	origLen := len(turn.Text)
	text := sanitizeForRollingLog(turn.Text)
	if len(text) > maxEntryTextBytes {
		text = text[:maxEntryTextBytes] + fmt.Sprintf("\n[truncated: %d bytes original]", origLen)
	}

	entry := fmt.Sprintf("%s [%s] %s", ts, role, text)

	// Write to daily audit log unconditionally — the append-only log captures
	// all turns regardless of rolling log state. This runs before AppendRaw
	// and triggerCompaction so that the entry is persisted even if rl is nil
	// (Shutdown race) or AppendRaw fails.
	if err := s.writeLogEntry(entry, now); err != nil {
		s.logFunc("[ConversationLog] ERROR: write log: %v", err)
	}

	rl := s.rl.Load()
	if rl == nil {
		return
	}
	appendErr := rl.AppendRaw(entry)
	if appendErr != nil {
		s.logFunc("[ConversationLog] ERROR: append raw: %v", appendErr)
	}

	// Check if compaction is needed after appending. Skip if AppendRaw failed —
	// the in-memory state may be inconsistent with disk (e.g., RAM grew but
	// persistToFile failed on disk full), so compacting would risk losing data
	// that cannot be recovered after a restart.
	if appendErr == nil && rl.ShouldCompact() {
		s.triggerCompaction(rl)
	}
}

// originToRole maps ContentOrigin to the role string used in conversation log entries.
func originToRole(origin eventbus.ContentOrigin) string {
	switch origin {
	case eventbus.OriginUser:
		return "user"
	case eventbus.OriginAI:
		return "ai"
	case eventbus.OriginTool:
		return "tool"
	case eventbus.OriginSystem:
		return "system"
	default:
		return "unknown"
	}
}

// writeLogEntry appends an entry to the daily log file (conversations/logs/YYYY-MM-DD.md).
func (s *ConversationLogService) writeLogEntry(entry string, ts time.Time) error {
	logsPath := filepath.Join(s.basePath, "logs")
	if !s.logDirCreated.Load() {
		if err := os.MkdirAll(logsPath, 0o755); err != nil {
			return fmt.Errorf("create logs dir: %w", err)
		}
		s.logDirCreated.Store(true)
	}

	date := ts.Format("2006-01-02")

	s.logFileMu.Lock()
	defer s.logFileMu.Unlock()

	// Check if service was stopped while waiting for lock. This is safe because
	// Shutdown() sets started=false BEFORE acquiring logFileMu for cleanup
	// (see Shutdown lines: started.Store(false) → Lock → Range/Delete → Unlock).
	// The mutex creates a happens-before guarantee: if we acquire the lock after
	// Shutdown releases it, our started.Load() will observe false and bail out.
	// If we acquire the lock before Shutdown, handles are still valid.
	if !s.started.Load() {
		return nil
	}

	f, err := s.getLogFile(logsPath, date)
	if err != nil {
		return err
	}

	if _, err := f.WriteString(entry + "\n"); err != nil {
		return fmt.Errorf("write log: %w", err)
	}
	// No fsync: OS page cache is sufficient for an append-only audit log.
	return nil
}

// writeFallbackLogEntry writes directly to a daily log file without checking
// s.started. Used by handleCompactionReply's AppendSummary failure path, which
// intentionally runs even during shutdown (replies MUST be processed because
// OlderHalf already deleted the raw entries). The regular writeLogEntry method
// checks s.started and would silently discard the fallback during shutdown.
func (s *ConversationLogService) writeFallbackLogEntry(entry string, ts time.Time) error {
	logsPath := filepath.Join(s.basePath, "logs")
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

// getLogFile returns a cached file handle for the daily log. If the date has
// rolled over, the old handle is closed and a new one opened.
func (s *ConversationLogService) getLogFile(logsPath, date string) (*os.File, error) {
	if v, ok := s.logFiles.Load(date); ok {
		return v.(*logFileEntry).file, nil
	}

	// Close the previous date's handle (at most one exists for the global
	// conversation log — unlike JournalService which has per-session maps).
	// We iterate to find it because sync.Map has no "get any key != X" method,
	// but the loop body executes at most once.
	s.logFiles.Range(func(key, value any) bool {
		if key.(string) != date {
			if err := value.(*logFileEntry).file.Close(); err != nil {
				s.logFunc("[ConversationLog] WARN: close stale log file %s: %v", key, err)
			}
			s.logFiles.Delete(key)
		}
		return true
	})

	filename := filepath.Join(logsPath, date+".md")
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("open log file: %w", err)
		}
		if mkErr := os.MkdirAll(logsPath, 0o755); mkErr != nil {
			return nil, fmt.Errorf("open log file (mkdir retry failed): %w", mkErr)
		}
		f, err = os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open log file after mkdir retry: %w", err)
		}
	}
	s.logFiles.Store(date, &logFileEntry{file: f, date: date})
	return f, nil
}

// triggerCompaction extracts the older half of the conversation rolling log
// and publishes a ConversationPromptEvent for AI summarization.
func (s *ConversationLogService) triggerCompaction(rl *RollingLog) {
	// Skip if a compaction is already in flight. Without this guard, a
	// non-responsive AI adapter would cause unbounded growth of the
	// pendingCompactions map.
	if !s.compactionInFlight.CompareAndSwap(false, true) {
		s.checkStaleCompaction()
		// After clearing stale state, retry the guard. If checkStaleCompaction
		// cleared compactionInFlight, the CAS succeeds and we proceed with
		// the new compaction. If it didn't clear (not stale yet, or another
		// goroutine already re-acquired), we return without wasting a turn.
		if !s.compactionInFlight.CompareAndSwap(false, true) {
			return
		}
	}
	s.compactionStartedAt.Store(time.Now().UnixNano())

	olderHalf, err := rl.OlderHalf()
	if err != nil {
		s.compactionInFlight.Store(false)
		s.logFunc("[ConversationLog] ERROR: extract older half: %v", err)
		return
	}
	if olderHalf == "" {
		s.compactionInFlight.Store(false)
		return
	}

	startTime, endTime := extractTimeRange(olderHalf)

	// Use time.Now() (not nowFunc) for promptID uniqueness. nowFunc is
	// overridden to a fixed time in tests for deterministic archive dates;
	// using it here would produce duplicate promptIDs across multiple
	// compactions within the same test.
	promptID := fmt.Sprintf("conversation-compact-%d", time.Now().UnixNano())

	s.pendingCompactions.Store(promptID, &convPendingCompaction{
		rl:        rl,
		startTime: startTime,
		endTime:   endTime,
	})

	// context.Background() is intentional: the compaction prompt must be
	// delivered even if the service is shutting down.
	eventbus.Publish(context.Background(), s.bus, eventbus.Conversation.Prompt, eventbus.SourceAwareness,
		eventbus.ConversationPromptEvent{
			SessionID: "", // empty — conversation is global, not per-session
			PromptID:  promptID,
			NewMessage: eventbus.ConversationMessage{
				Origin: eventbus.OriginSystem,
				Text:   olderHalf,
			},
			Metadata: map[string]string{
				constants.MetadataKeyEventType: constants.PromptEventConversationCompaction,
			},
		})
	s.logFunc("[ConversationLog] INFO: compaction triggered (promptID=%s, range=%s–%s)", promptID, startTime, endTime)
}

// checkStaleCompaction detects and clears compaction state that has been
// in-flight for longer than staleCompactionTimeout. This recovers the service
// from scenarios where the AI adapter never responds (crash, misconfiguration,
// network partition). Without this reaper, compactionInFlight stays true
// forever, the rolling log's raw tail grows unbounded, and the only recovery
// is a service restart.
func (s *ConversationLogService) checkStaleCompaction() {
	startedAt := s.compactionStartedAt.Load()
	if startedAt == 0 {
		return
	}
	age := time.Since(time.Unix(0, startedAt))
	if age < s.staleTimeout {
		return
	}

	s.logFunc("[ConversationLog] WARN: compaction has been in-flight for %v (threshold %v) — clearing stale state to unblock pipeline", age, s.staleTimeout)
	s.pendingCompactions.Range(func(key, value any) bool {
		pc := value.(*convPendingCompaction)
		s.logFunc("[ConversationLog] WARN: dropping stale compaction (promptID=%s, range=%s–%s) — raw entries were compacted but summary was never received", key, pc.startTime, pc.endTime)
		s.pendingCompactions.Delete(key)
		return true
	})
	s.compactionInFlight.Store(false)
	s.compactionStartedAt.Store(0)
}

// consumeCompactionReply handles AI-generated summaries for compaction requests.
func (s *ConversationLogService) consumeCompactionReply(ctx context.Context) {
	eventbus.Consume(ctx, s.replySub, nil, func(event eventbus.ConversationReplyEvent) {
		s.handleCompactionReply(event)
	})
}

// handleCompactionReply processes a ConversationReplyEvent if it matches a
// pending compaction. It formats the summary with a header, calls AppendSummary,
// and triggers archival if needed.
//
// NOTE: No s.started guard here — intentional. Compaction replies MUST be
// processed even during shutdown because OlderHalf() already deleted the raw
// entries. Skipping the reply would cause silent data loss. The retained
// pc.rl reference (not s.rl.Load()) ensures the write succeeds even after
// Shutdown() has set s.rl to nil.
func (s *ConversationLogService) handleCompactionReply(event eventbus.ConversationReplyEvent) {
	if event.PromptID == "" {
		return
	}

	v, ok := s.pendingCompactions.LoadAndDelete(event.PromptID)
	if !ok {
		return // Not a compaction reply — belongs to another consumer.
	}
	pc := v.(*convPendingCompaction)
	// Defer clearing the guard so new compactions are blocked until ALL work
	// (AppendSummary + triggerArchival) completes. Clearing early would allow
	// a new triggerCompaction from consumeTurns while this reply is still
	// being finalized, wasting AI adapter calls.
	defer func() {
		s.compactionInFlight.Store(false)
		s.compactionStartedAt.Store(0)
	}()

	rl := pc.rl
	if rl == nil {
		s.logFunc("[ConversationLog] WARN: nil RollingLog in pending compaction (promptID=%s) — cannot write summary", event.PromptID)
		return
	}

	// Guard: empty/whitespace AI reply — skip AppendSummary. Older half raw
	// entries were already deleted by OlderHalf(); writing a header-only
	// summary with no content would be meaningless.
	if strings.TrimSpace(event.Text) == "" {
		s.logFunc("[ConversationLog] WARN: empty compaction reply (promptID=%s) — raw entries already compacted, skipping summary", event.PromptID)
		return
	}

	sanitizedText := sanitizeForSummary(event.Text)

	header := fmt.Sprintf("### [summary] %s – %s", pc.startTime, pc.endTime)
	formattedSummary := header + "\n" + sanitizedText

	if err := rl.AppendSummary(formattedSummary); err != nil {
		s.logFunc("[ConversationLog] ERROR: append summary (promptID=%s, range=%s–%s): %v", event.PromptID, pc.startTime, pc.endTime, err)
		// Fallback: write the AI summary to the daily audit log so it is not
		// permanently lost. OlderHalf() already deleted the raw entries; if the
		// rolling log write also fails (e.g., disk full), this is the last
		// chance to preserve the summarized content.
		// NOTE: Uses writeFallbackLogEntry (not writeLogEntry) because this
		// handler intentionally runs without a started guard — replies MUST be
		// processed even during shutdown. writeLogEntry checks s.started and
		// would silently drop the fallback during the shutdown window.
		fallbackEntry := fmt.Sprintf("[compaction-fallback] %s\n%s", header, sanitizedText)
		// nowFunc() drives the daily log file date partition (YYYY-MM-DD.md).
		// The compaction time range (pc.startTime–pc.endTime) is embedded in
		// the header above — the file date just needs a reasonable wall-clock day.
		if fbErr := s.writeFallbackLogEntry(fallbackEntry, s.nowFunc()); fbErr != nil {
			s.logFunc("[ConversationLog] ERROR: fallback audit log write also failed (promptID=%s): %v", event.PromptID, fbErr)
		} else {
			s.logFunc("[ConversationLog] INFO: AI summary saved to audit log as fallback (promptID=%s)", event.PromptID)
		}
		return
	}
	s.logFunc("[ConversationLog] INFO: compaction reply processed (promptID=%s, range=%s–%s)", event.PromptID, pc.startTime, pc.endTime)

	// After adding a summary, check if archival is needed.
	if rl.ShouldArchive() {
		s.triggerArchival(rl)
	}
}

// triggerArchival moves older summaries to an archive file and triggers FTS5
// re-indexing via AwarenessSyncEvent.
//
// IMPORTANT: Must only be called from handleCompactionReply (which runs in the
// single consumeCompactionReply goroutine). The OlderSummaries → Archive →
// CommitArchival sequence is NOT thread-safe (see rolling_log.go TOCTOU warning).
func (s *ConversationLogService) triggerArchival(rl *RollingLog) {
	if rl == nil {
		return
	}
	summaries := rl.OlderSummaries()
	if len(summaries) == 0 {
		return
	}

	archivePath := filepath.Join(s.basePath, "archives")
	archiveDate := s.nowFunc().Format("2006-01-02")
	archiveFile := filepath.Join(archivePath, archiveDate+".md")

	// Check existence BEFORE Archive() writes/appends to determine correct SyncType.
	_, statErr := os.Stat(archiveFile)
	archiveExisted := statErr == nil

	if err := rl.Archive(archivePath, summaries, archiveDate); err != nil {
		s.logFunc("[ConversationLog] ERROR: archive summaries: %v", err)
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
		s.logFunc("[ConversationLog] ERROR: commit archival: %v", err)
		return
	}

	syncType := eventbus.SyncTypeCreated
	if archiveExisted {
		syncType = eventbus.SyncTypeUpdated
	}
	// context.Background() is intentional: the sync event must be delivered
	// even during shutdown, so we don't propagate the lifecycle context.
	eventbus.Publish(context.Background(), s.bus, eventbus.Memory.Sync, eventbus.SourceAwareness,
		eventbus.AwarenessSyncEvent{
			FilePath: archiveFile,
			SyncType: syncType,
		})

	s.logFunc("[ConversationLog] INFO: archived %d summaries → %s", len(summaries), archiveFile)
}
