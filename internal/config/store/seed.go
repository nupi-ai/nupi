package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"maps"

	"github.com/nupi-ai/nupi/internal/constants"
)

//go:embed prompts/*.txt
var promptTemplatesFS embed.FS

var defaultPromptTemplates map[string]string

func init() {
	defaultPromptTemplates = map[string]string{
		constants.PromptEventUserIntent:     mustReadPrompt("prompts/user_intent.txt"),
		constants.PromptEventSessionOutput:  mustReadPrompt("prompts/session_output.txt"),
		constants.PromptEventHistorySummary: mustReadPrompt("prompts/history_summary.txt"),
		constants.PromptEventClarification:  mustReadPrompt("prompts/clarification.txt"),
		constants.PromptEventMemoryFlush:    mustReadPrompt("prompts/memory_flush.txt"),
		constants.PromptEventSessionSlug:    mustReadPrompt("prompts/session_slug.txt"),
		constants.PromptEventOnboarding:     mustReadPrompt("prompts/onboarding.txt"),
	}
}

func mustReadPrompt(name string) string {
	data, err := promptTemplatesFS.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("config/store: embedded prompt template %s: %v", name, err))
	}
	return string(data)
}

var requiredAdapterSlots = append([]string(nil), constants.RequiredAdapterSlots...)

// DefaultPromptTemplates returns a copy of the default prompt templates.
// Used by prompts.Engine for reset operations and CLI.
func DefaultPromptTemplates() map[string]string {
	return maps.Clone(defaultPromptTemplates)
}

// promptEventDescriptions maps event types to human-readable descriptions.
// This is the single source of truth for event type descriptions.
var promptEventDescriptions = map[string]string{
	constants.PromptEventUserIntent:     "Interprets user voice/text commands",
	constants.PromptEventSessionOutput:  "Analyzes terminal output for notifications",
	constants.PromptEventHistorySummary: "Summarizes conversation history",
	constants.PromptEventClarification:  "Handles follow-up responses",
	constants.PromptEventMemoryFlush:    "Saves important context before conversation compaction",
	constants.PromptEventSessionSlug:    "Generates a session slug and summary on session close",
	constants.PromptEventOnboarding:     "First-time setup conversation with a new user",
}

// PromptEventDescriptions returns a copy of the event type descriptions.
// Used by CLI for validation and display.
func PromptEventDescriptions() map[string]string {
	return maps.Clone(promptEventDescriptions)
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
		`+buildInsertDoNothingSQL(
		"quickstart_status",
		[]string{"instance_name", "profile_name", "completed", "completed_at"},
		[]string{"instance_name", "profile_name"},
		insertOptions{
			InsertUpdatedAt: true,
		},
	),
		instanceName, profileName, 0, nil); err != nil {
		tx.Rollback()
		return fmt.Errorf("config: seed quickstart status: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		`+buildInsertDoNothingSQL(
		"audio_settings",
		[]string{"instance_name", "profile_name", "capture_device", "playback_device", "preferred_format", "vad_threshold", "metadata"},
		[]string{"instance_name", "profile_name"},
		insertOptions{
			InsertUpdatedAt: true,
			UpdatedAtExpr:   "STRFTIME('%Y-%m-%dT%H:%M:%fZ', 'now')",
		},
	),
		instanceName, profileName, "", "", "pcm_s16le", 0.5, nil); err != nil {
		tx.Rollback()
		return fmt.Errorf("config: seed audio settings: %w", err)
	}

	for _, slot := range requiredAdapterSlots {
		if _, err := tx.ExecContext(ctx, `
			`+buildInsertDoNothingSQL(
			"adapter_bindings",
			[]string{"instance_name", "profile_name", "slot", "adapter_id", "config", "status"},
			[]string{"instance_name", "profile_name", "slot"},
			insertOptions{
				InsertUpdatedAt: true,
			},
		),
			instanceName, profileName, slot, nil, requiredSlotConfigJSON, BindingStatusRequired); err != nil {
			tx.Rollback()
			return fmt.Errorf("config: seed adapter slot %s: %w", slot, err)
		}
	}

	// Seed default prompt templates
	for eventType, content := range defaultPromptTemplates {
		if _, err := tx.ExecContext(ctx, `
			`+buildInsertDoNothingSQL(
			"prompt_templates",
			[]string{"instance_name", "profile_name", "event_type", "content", "is_custom"},
			[]string{"instance_name", "profile_name", "event_type"},
			insertOptions{
				InsertUpdatedAt: true,
			},
		),
			instanceName, profileName, eventType, content, 0); err != nil {
			tx.Rollback()
			return fmt.Errorf("config: seed prompt template %s: %w", eventType, err)
		}
	}

	// Seed builtin marketplaces: official Nupi marketplace + "others" for local/URL installs
	for _, mp := range builtinMarketplaces {
		if _, err := tx.ExecContext(ctx, `
			`+buildInsertDoNothingSQL(
			"marketplaces",
			[]string{"instance_name", "namespace", "url", "is_builtin"},
			[]string{"instance_name", "namespace"},
			insertOptions{
				InsertCreatedAt: true,
			},
		),
			instanceName, mp.Namespace, mp.URL, 1); err != nil {
			tx.Rollback()
			return fmt.Errorf("config: seed marketplace %s: %w", mp.Namespace, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("config: commit seed transaction: %w", err)
	}

	return nil
}

// BuiltinMarketplaceURL is the official Nupi marketplace index URL.
const BuiltinMarketplaceURL = "https://raw.githubusercontent.com/nupi-ai/marketplace/main/index.yaml"

type builtinMarketplace struct {
	Namespace string
	URL       string
}

var builtinMarketplaces = []builtinMarketplace{
	{Namespace: "ai.nupi", URL: BuiltinMarketplaceURL},
	{Namespace: "others", URL: ""},
}
