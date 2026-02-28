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

// conversationCompactionThreshold is the raw tail byte threshold for
// conversation.md compaction. Matches spec section 4.6 default.
const conversationCompactionThreshold = 16000

// conversationSummaryBudget is the summary byte budget for conversation.md.
const conversationSummaryBudget = 10000

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

	lifecycle eventbus.ServiceLifecycle
	started   atomic.Bool

	turnSub *eventbus.TypedSubscription[eventbus.ConversationTurnEvent]

	// nowFunc provides fallback time when Turn.At is zero (defensive).
	// In production, handleTurn uses the event-authoritative Turn.At timestamp;
	// nowFunc is only called if Turn.At is zero. Overridden in tests.
	nowFunc func() time.Time
	// logFunc logs errors. Defaults to log.Printf. Overridden in tests.
	logFunc func(format string, args ...any)
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
		bus:      bus,
		basePath: basePath,
		nowFunc:  func() time.Time { return time.Now().UTC() },
		logFunc:  log.Printf,
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
	s.lifecycle.AddSubscriptions(s.turnSub)
	s.lifecycle.Go(s.consumeTurns)

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
		// shutdownGraceTimeout (5s) defined in journal.go — shared across awareness services.
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

	s.logDirCreated.Store(false)
	s.rl.Store(nil)
	return waitErr
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

	rl := s.rl.Load()
	if rl == nil {
		return
	}
	// NOTE: No ShouldCompact()/triggerCompaction here — compaction and archival
	// for the conversation log are implemented in Story 21.2, not this service.
	if err := rl.AppendRaw(entry); err != nil {
		s.logFunc("[ConversationLog] ERROR: append raw: %v", err)
	}

	if err := s.writeLogEntry(entry, now); err != nil {
		s.logFunc("[ConversationLog] ERROR: write log: %v", err)
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

// getLogFile returns a cached file handle for the daily log. If the date has
// rolled over, the old handle is closed and a new one opened.
func (s *ConversationLogService) getLogFile(logsPath, date string) (*os.File, error) {
	if v, ok := s.logFiles.Load(date); ok {
		return v.(*logFileEntry).file, nil
	}

	// Close any stale handles from previous dates.
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
