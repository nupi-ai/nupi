package awareness

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

// maxFlushContentBytes is the maximum size of AI-generated content that
// writeFlushContent will accept. Responses exceeding this limit are truncated
// with a warning log. Prevents unbounded daily file growth from misconfigured
// or misbehaving models.
const maxFlushContentBytes = 10 * 1024 // 10 KB

type flushState struct {
	sessionID string
	promptID  string
}

func (s *Service) consumeFlushReplies(ctx context.Context) {
	eventbus.Consume(ctx, s.flushReplySub, nil, func(reply eventbus.ConversationReplyEvent) {
		// Only process memory_flush replies.
		if reply.Metadata[constants.MetadataKeyEventType] != "memory_flush" {
			return
		}
		s.handleFlushReply(ctx, reply)
	})
}

func (s *Service) handleFlushReply(ctx context.Context, reply eventbus.ConversationReplyEvent) {
	val, ok := s.pendingFlush.LoadAndDelete(reply.PromptID)
	if !ok {
		return // stale or duplicate
	}

	state, ok := val.(*flushState)
	if !ok {
		return
	}

	// Defense-in-depth: log if the adapter returned a different or empty
	// SessionID compared to what the original flush request carried.
	if reply.SessionID == "" {
		log.Printf("[Awareness] WARNING: flush reply has empty SessionID (promptID=%s, expected=%s)",
			reply.PromptID, state.sessionID)
	} else if reply.SessionID != state.sessionID {
		log.Printf("[Awareness] WARNING: flush reply sessionID mismatch: reply=%s state=%s (promptID=%s)",
			reply.SessionID, state.sessionID, reply.PromptID)
	}

	text := strings.TrimSpace(reply.Text)
	if text == "" || strings.EqualFold(text, "NO_REPLY") {
		return
	}

	if err := s.writeFlushContent(ctx, state.sessionID, text); err != nil {
		log.Printf("[Awareness] ERROR: writing flush content for session %s: %v", state.sessionID, err)
	}
}

func (s *Service) writeFlushContent(ctx context.Context, sessionID, content string) error {
	// Truncate oversized AI responses to prevent unbounded daily file growth.
	// Use UTF-8-safe truncation to avoid splitting multi-byte characters at the boundary.
	if len(content) > maxFlushContentBytes {
		log.Printf("[Awareness] WARNING: flush content for session %s exceeds %d bytes (%d), truncating",
			sessionID, maxFlushContentBytes, len(content))
		truncated := content[:maxFlushContentBytes]
		for len(truncated) > 0 && !utf8.ValidString(truncated) {
			truncated = truncated[:len(truncated)-1]
		}
		content = truncated + "\n\n[truncated — exceeded flush content limit]"
	}

	// Serialize daily file writes to prevent data loss from concurrent flushes
	// (e.g., multiple sessions flushing to the same date file simultaneously).
	s.memoryWriteMu.Lock()
	defer s.memoryWriteMu.Unlock()

	// For now, always write to global daily/ directory.
	// Future stories will add project-scoped writing based on session metadata.
	dailyDir := filepath.Join(s.awarenessDir, "memory", "daily")

	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		return fmt.Errorf("create daily dir: %w", err)
	}

	date := time.Now().UTC().Format("2006-01-02")
	filename := filepath.Join(dailyDir, date+".md")

	var data []byte
	existing, err := os.ReadFile(filename)
	if err == nil {
		// Append with separator.
		data = append(existing, []byte("\n\n---\n\n"+content)...)
	} else if errors.Is(err, os.ErrNotExist) {
		// New file with header.
		data = []byte(fmt.Sprintf("# Daily Log %s\n\n%s", date, content))
	} else {
		return fmt.Errorf("read existing daily file: %w", err)
	}

	// Atomic write via temp file + rename to prevent data loss if the
	// process crashes mid-write (os.WriteFile uses O_TRUNC which would
	// destroy previous content on partial write).
	tmpPath := filename + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		os.Remove(tmpPath) // clean up partial file on write failure
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmpPath, filename); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp to daily file: %w", err)
	}

	log.Printf("[Awareness] Flush content written to %s (%d bytes, session=%s)", filename, len(data), sessionID)

	// Sync the updated file to the index.
	if s.indexer != nil {
		if err := s.indexer.Sync(ctx); err != nil {
			log.Printf("[Awareness] WARNING: index sync after flush write: %v", err)
		}
	}

	return nil
}
