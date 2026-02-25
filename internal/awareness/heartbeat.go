package awareness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adhocore/gronx"
	"github.com/google/uuid"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

// HeartbeatStore abstracts heartbeat persistence for the awareness service.
// This interface maintains Boundary 4: awareness does not import config/store.
type HeartbeatStore interface {
	ListHeartbeats(ctx context.Context) ([]Heartbeat, error)
	UpdateHeartbeatLastRun(ctx context.Context, id int64, lastRunAt time.Time) error
}

// InsertHeartbeatStore extends HeartbeatStore with insert/delete for tool handlers.
type InsertHeartbeatStore interface {
	HeartbeatStore
	InsertHeartbeat(ctx context.Context, name, cronExpr, prompt string, maxCount int) error
	DeleteHeartbeat(ctx context.Context, name string) error
}

// Heartbeat is the awareness-package-local heartbeat type.
type Heartbeat struct {
	ID        int64
	Name      string
	CronExpr  string
	Prompt    string
	LastRunAt *time.Time
	CreatedAt time.Time
}

// SetHeartbeatStore wires the heartbeat store.
// Must be called before Start.
func (s *Service) SetHeartbeatStore(store InsertHeartbeatStore) {
	if s.started {
		panic("awareness: SetHeartbeatStore called after Start")
	}
	s.heartbeatStore = store
}

// runHeartbeatLoop runs the periodic heartbeat check goroutine.
// It performs an immediate check on startup (catching heartbeats due from downtime),
// then checks every 60 seconds, aligned to the next minute boundary.
//
// NOTE: If the daemon starts near a minute boundary (e.g., 10:00:58), a heartbeat
// with "* * * * *" may fire twice in rapid succession — once for minute 10:00
// (immediate check) and again for 10:01 (aligned check). This is correct: each
// minute is a distinct cron window, and the dedup guard only prevents re-firing
// within the same minute.
func (s *Service) runHeartbeatLoop(ctx context.Context) {
	gron := gronx.New()

	// Immediate check on startup — catch heartbeats due while daemon was offline.
	if !s.shuttingDown.Load() {
		s.checkHeartbeats(ctx, gron)
	}

	// Align first tick to the next minute boundary for precise cron evaluation.
	nextMinute := time.Now().Truncate(time.Minute).Add(time.Minute)
	alignTimer := time.NewTimer(time.Until(nextMinute))
	defer alignTimer.Stop()

	select {
	case <-ctx.Done():
		return
	case <-alignTimer.C:
		if s.shuttingDown.Load() {
			return
		}
		s.checkHeartbeats(ctx, gron)
	}

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.shuttingDown.Load() {
				return
			}
			s.checkHeartbeats(ctx, gron)
		}
	}
}

// checkHeartbeats evaluates all heartbeats and publishes events for due ones.
func (s *Service) checkHeartbeats(ctx context.Context, gron *gronx.Gronx) {
	if gron == nil {
		log.Printf("[Awareness] Heartbeat: gronx instance is nil, skipping check")
		return
	}

	heartbeats, err := s.heartbeatStore.ListHeartbeats(ctx)
	if err != nil {
		log.Printf("[Awareness] Heartbeat: list error: %v", err)
		return
	}
	if len(heartbeats) == 0 {
		return
	}

	wallNow := time.Now()
	now := wallNow.Truncate(time.Minute)
	triggered := 0
	for _, hb := range heartbeats {
		// Skip if already fired this minute (guards against duplicate execution
		// on daemon restart within the same minute window).
		if hb.LastRunAt != nil && hb.LastRunAt.Truncate(time.Minute).Equal(now) {
			continue
		}

		due, err := gron.IsDue(hb.CronExpr, now)
		if err != nil {
			log.Printf("[Awareness] Heartbeat: invalid cron %q for %q: %v", hb.CronExpr, hb.Name, err)
			continue
		}
		if !due {
			continue
		}
		if s.shuttingDown.Load() {
			return
		}

		event := eventbus.ConversationPromptEvent{
			SessionID: "",
			PromptID:  uuid.New().String(),
			Context:   nil,
			NewMessage: eventbus.ConversationMessage{
				Origin: eventbus.OriginSystem,
				Text:   hb.Prompt,
				At:     wallNow,
				Meta:   map[string]string{"event_type": constants.PromptEventHeartbeat},
			},
			// heartbeat_name is referenced by the heartbeat.txt prompt template
			// via {{index .metadata "heartbeat_name"}}.
			Metadata: map[string]string{
				"event_type":     constants.PromptEventHeartbeat,
				"heartbeat_name": hb.Name,
			},
		}
		// NOTE: at-most-once semantics — last_run_at is updated after Publish
		// even though Publish is fire-and-forget. If the bus is draining or the
		// consumer fails, the heartbeat is marked as run but may not execute.
		// This is acceptable: duplicate execution on restart is guarded by the
		// same-minute dedup check above, and missed beats are preferable to
		// duplicate side-effects (e.g., double memory_write).
		eventbus.Publish(ctx, s.bus, eventbus.Conversation.Prompt, eventbus.SourceAwareness, event)

		if err := s.heartbeatStore.UpdateHeartbeatLastRun(ctx, hb.ID, wallNow); err != nil {
			log.Printf("[Awareness] Heartbeat: update last_run_at for %q: %v", hb.Name, err)
		}
		log.Printf("[Awareness] Heartbeat: triggered %q (cron: %s)", hb.Name, hb.CronExpr)
		triggered++
	}
	log.Printf("[Awareness] Heartbeat: checked %d, triggered %d", len(heartbeats), triggered)
}

// duplicateError is a cross-boundary interface satisfied by config/store.DuplicateError's
// Duplicate() marker method, allowing detection without importing config/store (Boundary 4).
type duplicateError interface {
	Duplicate() bool
}

// isDuplicate checks whether the error is a duplicate key violation.
func isDuplicate(err error) bool {
	var de duplicateError
	return errors.As(err, &de) && de.Duplicate()
}

// notFoundError is a cross-boundary interface satisfied by config/store.NotFoundError's
// NotFound() marker method, allowing detection without importing config/store (Boundary 4).
type notFoundError interface {
	NotFound() bool
}

// isNotFound checks whether the error is a not-found error.
func isNotFound(err error) bool {
	var nfe notFoundError
	return errors.As(err, &nfe) && nfe.NotFound()
}

// validateHeartbeatName delegates to the shared constants.ValidateHeartbeatName
// so that awareness tool handlers and CLI use identical validation logic.
func validateHeartbeatName(name string) error {
	return constants.ValidateHeartbeatName(name)
}

// --- heartbeat_add tool ---

type heartbeatAddArgs struct {
	Name     string `json:"name"`
	CronExpr string `json:"cron_expr"`
	Prompt   string `json:"prompt"`
}

func heartbeatAddSpec(s *Service) ToolSpec {
	return ToolSpec{
		Name:        "heartbeat_add",
		Description: "Add a recurring heartbeat task. The task will run automatically based on the cron expression. Use standard 5-field cron syntax (minute hour day month weekday). Examples: '0 9 * * 1-5' (weekdays 9am), '0 */2 * * *' (every 2 hours), '30 20 * * *' (daily 8:30pm).",
		ParametersJSON: `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Unique name for this heartbeat"},
    "cron_expr": {"type": "string", "description": "Standard 5-field cron expression"},
    "prompt": {"type": "string", "description": "The prompt text to execute on heartbeat"}
  },
  "required": ["name", "cron_expr", "prompt"],
  "additionalProperties": false
}`,
		Handler: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			var a heartbeatAddArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}

			name := strings.TrimSpace(a.Name)
			cronExpr := strings.TrimSpace(a.CronExpr)
			prompt := strings.TrimSpace(a.Prompt)

			if name == "" {
				return nil, fmt.Errorf("name is required")
			}
			if err := validateHeartbeatName(name); err != nil {
				return nil, err
			}
			if utf8.RuneCountInString(name) > constants.MaxHeartbeatNameLen {
				return nil, fmt.Errorf("name exceeds maximum length of %d characters", constants.MaxHeartbeatNameLen)
			}
			if cronExpr == "" {
				return nil, fmt.Errorf("cron_expr is required")
			}
			if utf8.RuneCountInString(cronExpr) > constants.MaxHeartbeatCronExprLen {
				return nil, fmt.Errorf("cron_expr exceeds maximum length of %d characters", constants.MaxHeartbeatCronExprLen)
			}
			if prompt == "" {
				return nil, fmt.Errorf("prompt is required")
			}
			if utf8.RuneCountInString(prompt) > constants.MaxHeartbeatPromptLen {
				return nil, fmt.Errorf("prompt exceeds maximum length of %d characters", constants.MaxHeartbeatPromptLen)
			}

			// Create per-invocation gronx instance to avoid sharing mutable state
			// across concurrent tool invocations from parallel AI sessions.
			if !gronx.New().IsValid(cronExpr) {
				return nil, fmt.Errorf("invalid cron expression: %q", cronExpr)
			}

			if err := s.heartbeatStore.InsertHeartbeat(ctx, name, cronExpr, prompt, constants.MaxHeartbeatCount); err != nil {
				if isDuplicate(err) {
					return nil, fmt.Errorf("heartbeat %q already exists", name)
				}
				return nil, fmt.Errorf("add heartbeat %q: %w", name, err)
			}

			return json.Marshal(map[string]string{
				"status":  "ok",
				"message": fmt.Sprintf("Heartbeat %q added (cron: %s)", name, cronExpr),
			})
		},
	}
}

// --- heartbeat_remove tool ---

type heartbeatRemoveArgs struct {
	Name string `json:"name"`
}

func heartbeatRemoveSpec(s *Service) ToolSpec {
	return ToolSpec{
		Name:        "heartbeat_remove",
		Description: "Remove a heartbeat task by name. The task will no longer run.",
		ParametersJSON: `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Name of the heartbeat to remove"}
  },
  "required": ["name"],
  "additionalProperties": false
}`,
		Handler: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			var a heartbeatRemoveArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}

			name := strings.TrimSpace(a.Name)
			if name == "" {
				return nil, fmt.Errorf("name is required")
			}
			if err := validateHeartbeatName(name); err != nil {
				return nil, err
			}

			if err := s.heartbeatStore.DeleteHeartbeat(ctx, name); err != nil {
				if isNotFound(err) {
					return nil, fmt.Errorf("heartbeat %q not found", name)
				}
				return nil, fmt.Errorf("remove heartbeat %q: %w", name, err)
			}

			return json.Marshal(map[string]string{
				"status":  "ok",
				"message": fmt.Sprintf("Heartbeat %q removed", name),
			})
		},
	}
}

// --- heartbeat_list tool ---

func heartbeatListSpec(s *Service) ToolSpec {
	return ToolSpec{
		Name:           "heartbeat_list",
		Description:    "List all configured heartbeat tasks with their names, cron expressions, prompts, and last run times.",
		ParametersJSON: `{"type":"object","properties":{},"additionalProperties":false}`,
		Handler: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			heartbeats, err := s.heartbeatStore.ListHeartbeats(ctx)
			if err != nil {
				return nil, fmt.Errorf("list heartbeats: %w", err)
			}

			type hbEntry struct {
				ID        int64   `json:"id"`
				Name      string  `json:"name"`
				CronExpr  string  `json:"cron_expr"`
				Prompt    string  `json:"prompt"`
				LastRunAt *string `json:"last_run_at"`
				CreatedAt string  `json:"created_at"`
			}

			entries := make([]hbEntry, 0, len(heartbeats))
			for _, h := range heartbeats {
				entry := hbEntry{
					ID:        h.ID,
					Name:      h.Name,
					CronExpr:  h.CronExpr,
					Prompt:    h.Prompt,
					CreatedAt: h.CreatedAt.UTC().Format(time.RFC3339),
				}
				if h.LastRunAt != nil {
					formatted := h.LastRunAt.UTC().Format(time.RFC3339)
					entry.LastRunAt = &formatted
				}
				entries = append(entries, entry)
			}

			return json.Marshal(map[string]interface{}{
				"heartbeats": entries,
				"count":      len(entries),
			})
		},
	}
}
