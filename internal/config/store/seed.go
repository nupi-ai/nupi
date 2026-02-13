package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
)

//go:embed prompts/*.txt
var promptTemplatesFS embed.FS

var defaultPromptTemplates map[string]string

func init() {
	defaultPromptTemplates = map[string]string{
		"user_intent":     mustReadPrompt("prompts/user_intent.txt"),
		"session_output":  mustReadPrompt("prompts/session_output.txt"),
		"history_summary": mustReadPrompt("prompts/history_summary.txt"),
		"clarification":   mustReadPrompt("prompts/clarification.txt"),
	}
}

func mustReadPrompt(name string) string {
	data, err := promptTemplatesFS.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("config/store: embedded prompt template %s: %v", name, err))
	}
	return string(data)
}

var requiredAdapterSlots = []string{
	"stt",
	"ai",
	"tts",
	"vad",
	"tunnel",
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
