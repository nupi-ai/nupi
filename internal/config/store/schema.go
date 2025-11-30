package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS instances (
		name TEXT PRIMARY KEY,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS profiles (
		instance_name TEXT NOT NULL,
		name TEXT NOT NULL,
		is_default INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (instance_name, name),
		FOREIGN KEY (instance_name) REFERENCES instances(name) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS quickstart_status (
		instance_name TEXT NOT NULL,
		profile_name TEXT NOT NULL,
		completed INTEGER NOT NULL DEFAULT 0,
		completed_at TEXT,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (instance_name, profile_name),
		FOREIGN KEY (instance_name, profile_name) REFERENCES profiles(instance_name, name) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS settings (
		instance_name TEXT NOT NULL,
		profile_name TEXT NOT NULL,
		key TEXT NOT NULL,
		value TEXT NOT NULL,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (instance_name, profile_name, key),
		FOREIGN KEY (instance_name, profile_name) REFERENCES profiles(instance_name, name) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS audio_settings (
		instance_name TEXT NOT NULL,
		profile_name TEXT NOT NULL,
		capture_device TEXT NOT NULL DEFAULT '',
		playback_device TEXT NOT NULL DEFAULT '',
		preferred_format TEXT NOT NULL DEFAULT 'pcm_s16le',
		vad_threshold REAL NOT NULL DEFAULT 0.5,
		metadata TEXT,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (instance_name, profile_name),
		FOREIGN KEY (instance_name, profile_name) REFERENCES profiles(instance_name, name) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS adapters (
		id TEXT PRIMARY KEY,
		source TEXT NOT NULL,
		version TEXT,
		type TEXT NOT NULL,
		name TEXT NOT NULL,
		manifest TEXT,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS adapter_bindings (
		instance_name TEXT NOT NULL,
		profile_name TEXT NOT NULL,
		slot TEXT NOT NULL,
		adapter_id TEXT,
		config TEXT,
		status TEXT NOT NULL DEFAULT 'inactive',
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (instance_name, profile_name, slot),
		FOREIGN KEY (instance_name, profile_name) REFERENCES profiles(instance_name, name) ON DELETE CASCADE,
		FOREIGN KEY (adapter_id) REFERENCES adapters(id)
	)`,
	`CREATE TABLE IF NOT EXISTS adapter_endpoints (
		adapter_id TEXT PRIMARY KEY,
		transport TEXT NOT NULL DEFAULT 'process' CHECK (transport IN ('process', 'grpc', 'http')),
		address TEXT NOT NULL DEFAULT '',
		command TEXT,
		args TEXT,
		env TEXT,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (adapter_id) REFERENCES adapters(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS security_settings (
		instance_name TEXT NOT NULL,
		profile_name TEXT NOT NULL,
		key TEXT NOT NULL,
		value TEXT NOT NULL,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (instance_name, profile_name, key),
		FOREIGN KEY (instance_name, profile_name) REFERENCES profiles(instance_name, name) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS prompt_templates (
		instance_name TEXT NOT NULL,
		profile_name TEXT NOT NULL,
		event_type TEXT NOT NULL CHECK (event_type IN ('user_intent', 'session_output', 'history_summary', 'clarification')),
		content TEXT NOT NULL,
		is_custom INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (instance_name, profile_name, event_type),
		FOREIGN KEY (instance_name, profile_name) REFERENCES profiles(instance_name, name) ON DELETE CASCADE
	)`,
}

var requiredAdapterSlots = []string{
	"stt",
	"ai",
	"tts",
	"vad",
	"tunnel",
}

// defaultPromptTemplates contains the default AI prompt templates for seeding.
// This is the single source of truth for default templates.
var defaultPromptTemplates = map[string]string{
	"user_intent":     defaultUserIntentTemplate,
	"session_output":  defaultSessionOutputTemplate,
	"history_summary": defaultHistorySummaryTemplate,
	"clarification":   defaultClarificationTemplate,
}

// DefaultPromptTemplates returns a copy of the default prompt templates.
// Used by prompts.Engine for reset operations and CLI.
func DefaultPromptTemplates() map[string]string {
	result := make(map[string]string, len(defaultPromptTemplates))
	for k, v := range defaultPromptTemplates {
		result[k] = v
	}
	return result
}

// promptEventDescriptions maps event types to human-readable descriptions.
// This is the single source of truth for event type descriptions.
var promptEventDescriptions = map[string]string{
	"user_intent":     "Interprets user voice/text commands",
	"session_output":  "Analyzes terminal output for notifications",
	"history_summary": "Summarizes conversation history",
	"clarification":   "Handles follow-up responses",
}

// PromptEventDescriptions returns a copy of the event type descriptions.
// Used by CLI for validation and display.
func PromptEventDescriptions() map[string]string {
	result := make(map[string]string, len(promptEventDescriptions))
	for k, v := range promptEventDescriptions {
		result[k] = v
	}
	return result
}

func applyPragmas(ctx context.Context, db *sql.DB, readOnly bool) error {
	pragmas := []string{
		fmt.Sprintf("PRAGMA busy_timeout = %d", int(defaultBusyTimeout.Milliseconds())),
		"PRAGMA foreign_keys = ON",
	}

	if !readOnly {
		pragmas = append(pragmas,
			"PRAGMA journal_mode = WAL",
			"PRAGMA synchronous = NORMAL",
			"PRAGMA temp_store = MEMORY",
		)
	}

	for _, pragma := range pragmas {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("config: apply pragma %q: %w", pragma, err)
		}
	}

	return nil
}

func applySchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("config: begin schema transaction: %w", err)
	}

	for _, stmt := range schemaStatements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			tx.Rollback()
			return fmt.Errorf("config: apply schema statement %q: %w", abbreviate(stmt), err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("config: commit schema transaction: %w", err)
	}

	return nil
}

func seedDefaults(ctx context.Context, db *sql.DB, instanceName, profileName string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("config: begin seed transaction: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO instances (name)
		VALUES (?)
		ON CONFLICT(name) DO UPDATE SET updated_at = CURRENT_TIMESTAMP
	`, instanceName); err != nil {
		tx.Rollback()
		return fmt.Errorf("config: seed instance: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO profiles (instance_name, name, is_default)
		VALUES (?, ?, 1)
		ON CONFLICT(instance_name, name) DO UPDATE SET
			is_default = excluded.is_default,
			updated_at = CURRENT_TIMESTAMP
	`, instanceName, profileName); err != nil {
		tx.Rollback()
		return fmt.Errorf("config: seed profile: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE profiles
		SET is_default = CASE WHEN name = ? THEN 1 ELSE 0 END,
		    updated_at = CURRENT_TIMESTAMP
		WHERE instance_name = ?
	`, profileName, instanceName); err != nil {
		tx.Rollback()
		return fmt.Errorf("config: enforce default profile uniqueness: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quickstart_status (instance_name, profile_name, completed, completed_at, updated_at)
		VALUES (?, ?, 0, NULL, CURRENT_TIMESTAMP)
		ON CONFLICT(instance_name, profile_name) DO NOTHING
	`, instanceName, profileName); err != nil {
		tx.Rollback()
		return fmt.Errorf("config: seed quickstart status: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audio_settings (instance_name, profile_name, capture_device, playback_device, preferred_format, vad_threshold, metadata, updated_at)
		VALUES (?, ?, '', '', 'pcm_s16le', 0.5, NULL, STRFTIME('%Y-%m-%dT%H:%M:%fZ', 'now'))
		ON CONFLICT(instance_name, profile_name) DO NOTHING
	`, instanceName, profileName); err != nil {
		tx.Rollback()
		return fmt.Errorf("config: seed audio settings: %w", err)
	}

	for _, slot := range requiredAdapterSlots {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO adapter_bindings (instance_name, profile_name, slot, adapter_id, config, status, updated_at)
			VALUES (?, ?, ?, NULL, '{"required":true}', 'required', CURRENT_TIMESTAMP)
			ON CONFLICT(instance_name, profile_name, slot) DO NOTHING
		`, instanceName, profileName, slot); err != nil {
			tx.Rollback()
			return fmt.Errorf("config: seed adapter slot %s: %w", slot, err)
		}
	}

	// Seed default prompt templates
	for eventType, content := range defaultPromptTemplates {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO prompt_templates (instance_name, profile_name, event_type, content, is_custom, updated_at)
			VALUES (?, ?, ?, ?, 0, CURRENT_TIMESTAMP)
			ON CONFLICT(instance_name, profile_name, event_type) DO NOTHING
		`, instanceName, profileName, eventType, content); err != nil {
			tx.Rollback()
			return fmt.Errorf("config: seed prompt template %s: %w", eventType, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("config: commit seed transaction: %w", err)
	}

	return nil
}

func abbreviate(stmt string) string {
	const maxLen = 64
	trimmed := strings.Join(strings.Fields(stmt), " ")
	if len(trimmed) <= maxLen {
		return trimmed
	}
	return trimmed[:maxLen] + "…"
}

// Default prompt templates - single source of truth for AI prompts.
// These are seeded into SQLite when the store opens and used by prompts.Engine.

const defaultUserIntentTemplate = `You are Nupi, an AI assistant for command-line programming. You help users interact with terminal sessions using voice and text commands.

Your capabilities:
- Execute commands in terminal sessions
- Explain what's happening in sessions
- Help users navigate and control their CLI tools
- Answer questions about available sessions and tools

Available actions you can take:
- "command": Execute a shell command in a session
- "speak": Respond to the user with voice/text (no command execution)
- "clarify": Ask the user for more information
- "noop": Do nothing (when no action is needed)

{{if .has_session}}Current session: {{.session_id}}{{end}}
{{if .has_tool}}Current tool: {{.current_tool}}{{end}}

{{if gt .sessions_count 0}}Available sessions:
{{.sessions}}{{else}}No active sessions.{{end}}

{{if gt .history_count 0}}Recent conversation:
{{.history}}{{end}}

Guidelines:
- Be concise and direct
- When executing commands, prefer the current session unless the user specifies otherwise
- If the user's intent is unclear, ask for clarification
- For dangerous operations (rm -rf, etc.), confirm before executing
- If no session is active and the user wants to run a command, explain they need to start a session first

---USER---
User said: "{{.transcript}}"

Determine the user's intent and respond with the appropriate action.`

const defaultSessionOutputTemplate = `You are Nupi, an AI assistant monitoring terminal sessions. You're analyzing output from a session to determine if the user should be notified.

{{if .has_session}}Session: {{.session_id}}{{end}}
{{if .has_tool}}Tool: {{.current_tool}}{{end}}

{{if gt .history_count 0}}Recent conversation context:
{{.history}}{{end}}

Guidelines for deciding whether to notify the user:
- Notify for: errors, completion of long-running tasks, important status changes, prompts requiring input
- Don't notify for: routine output, progress updates, expected responses
- Be selective - too many notifications are annoying

---USER---
Session output:
{{truncate 2000 .session_output}}

Should the user be notified about this output? If yes, what should be said?`

const defaultHistorySummaryTemplate = `You are Nupi, an AI assistant. Summarize the following conversation history concisely.

Guidelines:
- Focus on key actions taken and their outcomes
- Note any pending tasks or unresolved issues
- Keep the summary brief (2-4 sentences)
- Preserve important context that might be needed for future interactions

---USER---
Conversation to summarize:
{{.history}}

Provide a concise summary.`

const defaultClarificationTemplate = `You are Nupi, an AI assistant for command-line programming. The user is responding to a clarification request.

{{if .has_session}}Current session: {{.session_id}}{{end}}
{{if .has_tool}}Current tool: {{.current_tool}}{{end}}

{{if gt .sessions_count 0}}Available sessions:
{{.sessions}}{{end}}

Original question asked: "{{.clarification_q}}"

{{if gt .history_count 0}}Recent conversation:
{{.history}}{{end}}

---USER---
User's response: "{{.transcript}}"

Based on this clarification, determine the appropriate action to take.`
