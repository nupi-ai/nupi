package awareness

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

// maxExportContentBytes is the maximum size of a session export markdown file.
// Responses exceeding this limit are truncated with a warning log.
const maxExportContentBytes = 20 * 1024 // 20 KB

// defaultSlugTimeout is the default timeout for AI slug generation.
// Shorter than flush timeout (30s) because slug generation is a simple task.
const defaultSlugTimeout = constants.Duration10Seconds

type exportState struct {
	sessionID string
	promptID  string
	turns     []eventbus.ConversationTurn
}

// slugSanitizeRe matches characters that are NOT lowercase alphanumeric or hyphens.
var slugSanitizeRe = regexp.MustCompile(`[^a-z0-9-]`)

// multiHyphenRe collapses consecutive hyphens.
var multiHyphenRe = regexp.MustCompile(`-{2,}`)

func (s *Service) consumeExportRequests(ctx context.Context) {
	eventbus.Consume(ctx, s.exportSub, &s.wg, func(event eventbus.SessionExportRequestEvent) {
		s.handleExportRequest(ctx, event)
	})
}

func (s *Service) handleExportRequest(ctx context.Context, event eventbus.SessionExportRequestEvent) {
	if s.bus == nil {
		return
	}

	if event.SessionID == "" {
		log.Printf("[Awareness] WARNING: ignoring export request with empty SessionID")
		return
	}

	if len(event.Turns) == 0 {
		log.Printf("[Awareness] WARNING: ignoring export request with empty turns for session %s", event.SessionID)
		return
	}

	serialized := eventbus.SerializeTurns(event.Turns)
	promptID := uuid.NewString()

	timeout := s.slugTimeout
	if timeout == 0 {
		timeout = defaultSlugTimeout
	}

	state := &exportState{
		sessionID: event.SessionID,
		promptID:  promptID,
		turns:     event.Turns,
	}

	// Store before starting the timer so the timeout callback always finds
	// the entry via LoadAndDelete. Previously the timer was started first
	// which created a race window where the callback could fire before Store,
	// silently losing the export (observed under stress with 1ns timeout).
	// The timer field was removed from exportState to eliminate data races:
	// the reply handler no longer needs to stop the timer — if a reply
	// arrives first, LoadAndDelete succeeds there and the timer callback
	// harmlessly gets loaded=false.
	s.pendingExport.Store(promptID, state)

	time.AfterFunc(timeout, func() {
		if s.shuttingDown.Load() {
			s.pendingExport.Delete(promptID)
			return
		}
		if _, loaded := s.pendingExport.LoadAndDelete(promptID); loaded {
			log.Printf("[Awareness] WARNING: slug generation timed out for session %s (promptID=%s), using fallback",
				event.SessionID, promptID)
			fallbackSlug := time.Now().UTC().Format("20060102-150405")
			if err := s.writeSessionExport(context.Background(), event.SessionID, fallbackSlug, "", event.Turns); err != nil {
				log.Printf("[Awareness] ERROR: writing fallback session export for session %s: %v", event.SessionID, err)
			}
		}
	})

	now := time.Now().UTC()
	prompt := eventbus.ConversationPromptEvent{
		SessionID: event.SessionID,
		PromptID:  promptID,
		Context:   event.Turns,
		NewMessage: eventbus.ConversationMessage{
			Origin: eventbus.OriginSystem,
			Text:   serialized,
			At:     now,
			Meta: map[string]string{
				"event_type": "session_slug",
			},
		},
		Metadata: map[string]string{
			"event_type": "session_slug",
		},
	}

	eventbus.Publish(ctx, s.bus, eventbus.Conversation.Prompt, eventbus.SourceAwareness, prompt)
}

func (s *Service) consumeExportReplies(ctx context.Context) {
	eventbus.Consume(ctx, s.exportReplySub, &s.wg, func(reply eventbus.ConversationReplyEvent) {
		// Only process session_slug replies.
		if reply.Metadata["event_type"] != "session_slug" {
			return
		}
		s.handleExportReply(ctx, reply)
	})
}

func (s *Service) handleExportReply(ctx context.Context, reply eventbus.ConversationReplyEvent) {
	val, ok := s.pendingExport.LoadAndDelete(reply.PromptID)
	if !ok {
		return // stale or duplicate
	}

	state, ok := val.(*exportState)
	if !ok {
		return
	}

	// No need to stop the timer — if it fires after this point,
	// LoadAndDelete will return loaded=false (harmless no-op).

	if reply.SessionID == "" {
		log.Printf("[Awareness] WARNING: export reply has empty SessionID (promptID=%s, expected=%s)",
			reply.PromptID, state.sessionID)
	} else if reply.SessionID != state.sessionID {
		log.Printf("[Awareness] WARNING: export reply sessionID mismatch: reply=%s state=%s (promptID=%s)",
			reply.SessionID, state.sessionID, reply.PromptID)
	}

	text := strings.TrimSpace(reply.Text)
	if text == "" || strings.EqualFold(text, "NO_REPLY") {
		log.Printf("[Awareness] Session %s deemed trivial, no export created", state.sessionID)
		return
	}

	slug, summary := parseSlugResponse(text)
	slug = sanitizeSlug(slug)

	if slug == "" {
		slug = time.Now().UTC().Format("20060102-150405")
		log.Printf("[Awareness] WARNING: empty slug after sanitization for session %s, using fallback", state.sessionID)
	}

	if err := s.writeSessionExport(ctx, state.sessionID, slug, summary, state.turns); err != nil {
		log.Printf("[Awareness] ERROR: writing session export for session %s: %v", state.sessionID, err)
	}
}

// parseSlugResponse extracts the slug and summary from the AI response.
// Expected format:
//
//	SLUG: <slug>
//
//	SUMMARY:
//	<markdown summary>
//
// If the format doesn't match, the first 3 words are used as slug with no summary.
func parseSlugResponse(text string) (slug, summary string) {
	lines := strings.Split(text, "\n")

	// Look for SLUG: line
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(trimmed), "SLUG:") {
			slug = strings.TrimSpace(trimmed[len("SLUG:"):])

			// Look for SUMMARY: in remaining lines
			for j := i + 1; j < len(lines); j++ {
				trimmedJ := strings.TrimSpace(lines[j])
				if strings.HasPrefix(strings.ToUpper(trimmedJ), "SUMMARY:") {
					// Everything after SUMMARY: line is the summary
					rest := strings.TrimSpace(trimmedJ[len("SUMMARY:"):])
					if j+1 < len(lines) {
						remaining := strings.Join(lines[j+1:], "\n")
						if rest != "" {
							summary = rest + "\n" + strings.TrimSpace(remaining)
						} else {
							summary = strings.TrimSpace(remaining)
						}
					} else {
						summary = rest
					}
					return slug, summary
				}
			}
			return slug, ""
		}
	}

	// Fallback: use first 3 words as slug, no summary
	words := strings.Fields(text)
	if len(words) > 3 {
		words = words[:3]
	}
	return strings.Join(words, "-"), ""
}

// sanitizeSlug normalizes a raw slug string to a URL/filename-safe format.
func sanitizeSlug(raw string) string {
	s := strings.TrimSpace(strings.ToLower(raw))
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")
	s = slugSanitizeRe.ReplaceAllString(s, "")
	s = multiHyphenRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 50 {
		s = s[:50]
		s = strings.TrimRight(s, "-")
	}
	return s
}

func (s *Service) writeSessionExport(ctx context.Context, sessionID, slug, aiSummary string, turns []eventbus.ConversationTurn) error {
	s.exportWriteMu.Lock()
	defer s.exportWriteMu.Unlock()

	sessionsDir := filepath.Join(s.awarenessDir, "memory", "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		return fmt.Errorf("create sessions dir: %w", err)
	}

	now := time.Now().UTC()
	date := now.Format("2006-01-02")
	baseName := date + "-" + slug
	filename := filepath.Join(sessionsDir, baseName+".md")

	// Handle filename collision: append numeric suffix if file exists.
	// Cap iterations to prevent infinite loop if the directory is saturated.
	for i := 2; fileExists(filename); i++ {
		if i > 999 {
			return fmt.Errorf("filename collision limit exceeded for %s (999 files with same slug on same day)", baseName)
		}
		filename = filepath.Join(sessionsDir, fmt.Sprintf("%s-%d.md", baseName, i))
	}

	// Build markdown content.
	var b strings.Builder
	fmt.Fprintf(&b, "# Session: %s\n\n", slug)
	fmt.Fprintf(&b, "**Date:** %s\n", now.Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&b, "**Session ID:** %s\n\n", sessionID)

	b.WriteString("## Summary\n\n")
	if aiSummary != "" {
		b.WriteString(aiSummary)
	} else {
		b.WriteString("No AI summary available.")
	}
	b.WriteString("\n\n## Conversation Log\n\n")

	// Turn text is written as-is (markdown passthrough). This is intentional:
	// session exports are private files consumed by the awareness indexer,
	// not rendered in untrusted contexts like a web browser.
	for _, turn := range turns {
		fmt.Fprintf(&b, "**[%s]** %s\n\n", turn.Origin.Label(), turn.Text)
	}

	content := b.String()

	// UTF-8-safe truncation if content exceeds limit.
	// Reserve space for the truncation notice so the final content stays within the limit.
	const truncationNotice = "\n\n[truncated — exceeded session export limit]"
	if len(content) > maxExportContentBytes {
		log.Printf("[Awareness] WARNING: session export for %s exceeds %d bytes (%d), truncating",
			sessionID, maxExportContentBytes, len(content))
		cutAt := maxExportContentBytes - len(truncationNotice)
		if cutAt < 0 {
			cutAt = 0
		}
		truncated := content[:cutAt]
		for len(truncated) > 0 && !utf8.ValidString(truncated) {
			truncated = truncated[:len(truncated)-1]
		}
		content = truncated + truncationNotice
	}

	// Atomic write via temp file + rename.
	tmpPath := filename + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0o644); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmpPath, filename); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp to session export file: %w", err)
	}

	log.Printf("[Awareness] Session export written: %s (%d bytes, session=%s)", filename, len(content), sessionID)

	// Sync the new file to the index.
	if s.indexer != nil {
		if err := s.indexer.Sync(ctx); err != nil {
			log.Printf("[Awareness] WARNING: index sync after session export: %v", err)
		}
	}

	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
