package store

import (
	"context"
	"database/sql"
	"fmt"
)

const promptTemplateColumns = "event_type, content, is_custom, updated_at"

// GetPromptTemplate returns a prompt template for the given event type.
func (s *Store) GetPromptTemplate(ctx context.Context, eventType string) (*PromptTemplate, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+promptTemplateColumns+`
		FROM prompt_templates
		WHERE instance_name = ? AND profile_name = ? AND event_type = ?
	`, s.instanceName, s.profileName, eventType)

	pt, err := scanPromptTemplate(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, NotFoundError{Entity: "prompt_template", Key: eventType}
		}
		return nil, fmt.Errorf("config: get prompt template %s: %w", eventType, err)
	}

	return &pt, nil
}

// SetPromptTemplate creates or updates a prompt template (sets is_custom=1).
func (s *Store) SetPromptTemplate(ctx context.Context, eventType, content string) error {
	if err := s.ensureWritable("set prompt template"); err != nil {
		return err
	}

	_, err := s.db.ExecContext(ctx, `
		`+buildUpsertSQL(
		"prompt_templates",
		[]string{"instance_name", "profile_name", "event_type", "content", "is_custom"},
		[]string{"instance_name", "profile_name", "event_type"},
		[]string{"content", "is_custom"},
		upsertOptions{
			InsertUpdatedAt: true,
			UpdateUpdatedAt: true,
		},
	),
		s.instanceName, s.profileName, eventType, content, 1)
	if err != nil {
		return fmt.Errorf("config: set prompt template %s: %w", eventType, err)
	}

	return nil
}

// ListPromptTemplates returns all prompt templates for the profile.
func (s *Store) ListPromptTemplates(ctx context.Context) ([]PromptTemplate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+promptTemplateColumns+`
		FROM prompt_templates
		WHERE instance_name = ? AND profile_name = ?
		ORDER BY event_type
	`, s.instanceName, s.profileName)
	if err != nil {
		return nil, fmt.Errorf("config: list prompt templates: %w", err)
	}
	return scanList(rows, scanPromptTemplate, "config: scan prompt template row", "config: iterate prompt template rows")
}

// DeletePromptTemplate deletes a prompt template (used before re-seeding default).
func (s *Store) DeletePromptTemplate(ctx context.Context, eventType string) error {
	if err := s.ensureWritable("delete prompt template"); err != nil {
		return err
	}

	_, err := s.db.ExecContext(ctx, `
		DELETE FROM prompt_templates
		WHERE instance_name = ? AND profile_name = ? AND event_type = ?
	`, s.instanceName, s.profileName, eventType)
	if err != nil {
		return fmt.Errorf("config: delete prompt template %s: %w", eventType, err)
	}

	return nil
}

// SeedPromptTemplate inserts a default prompt template (is_custom=0) if it doesn't exist.
func (s *Store) SeedPromptTemplate(ctx context.Context, eventType, content string) error {
	if err := s.ensureWritable("seed prompt template"); err != nil {
		return err
	}

	_, err := s.db.ExecContext(ctx, `
		`+buildInsertDoNothingSQL(
		"prompt_templates",
		[]string{"instance_name", "profile_name", "event_type", "content", "is_custom"},
		[]string{"instance_name", "profile_name", "event_type"},
		insertOptions{
			InsertUpdatedAt: true,
		},
	),
		s.instanceName, s.profileName, eventType, content, 0)
	if err != nil {
		return fmt.Errorf("config: seed prompt template %s: %w", eventType, err)
	}

	return nil
}

// ResetPromptTemplate resets a template to default by deleting and re-seeding.
func (s *Store) ResetPromptTemplate(ctx context.Context, eventType, defaultContent string) error {
	return s.withWriteTx(ctx, "reset prompt template", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM prompt_templates
			WHERE instance_name = ? AND profile_name = ? AND event_type = ?
		`, s.instanceName, s.profileName, eventType); err != nil {
			return fmt.Errorf("config: delete for reset prompt template %s: %w", eventType, err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO prompt_templates (instance_name, profile_name, event_type, content, is_custom, updated_at)
			VALUES (?, ?, ?, ?, 0, CURRENT_TIMESTAMP)
		`, s.instanceName, s.profileName, eventType, defaultContent); err != nil {
			return fmt.Errorf("config: insert for reset prompt template %s: %w", eventType, err)
		}

		return nil
	})
}

// ResetAllPromptTemplates resets all templates to defaults.
func (s *Store) ResetAllPromptTemplates(ctx context.Context, defaults map[string]string) error {
	return s.withWriteTx(ctx, "reset all prompt templates", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM prompt_templates
			WHERE instance_name = ? AND profile_name = ?
		`, s.instanceName, s.profileName); err != nil {
			return fmt.Errorf("config: delete all prompt templates: %w", err)
		}

		for eventType, content := range defaults {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO prompt_templates (instance_name, profile_name, event_type, content, is_custom, updated_at)
				VALUES (?, ?, ?, ?, 0, CURRENT_TIMESTAMP)
			`, s.instanceName, s.profileName, eventType, content); err != nil {
				return fmt.Errorf("config: insert default prompt template %s: %w", eventType, err)
			}
		}

		return nil
	})
}
