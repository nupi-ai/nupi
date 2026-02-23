package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// LoadSettings returns key/value settings for the active instance/profile.
// Optional keys limit the selection to specific entries.
func (s *Store) LoadSettings(ctx context.Context, keys ...string) (map[string]string, error) {
	query := `SELECT key, value FROM settings WHERE instance_name = ? AND profile_name = ?`
	args := []any{s.instanceName, s.profileName}

	if len(keys) > 0 {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
		query += fmt.Sprintf(" AND key IN (%s)", placeholders)
		for _, key := range keys {
			args = append(args, key)
		}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("config: load settings: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		key, value, err := scanStringPair(rows)
		if err != nil {
			return nil, fmt.Errorf("config: scan settings row: %w", err)
		}
		result[key] = value
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("config: iterate settings rows: %w", err)
	}

	return result, nil
}

// SaveSettings upserts the provided key/value pairs for the active profile.
func (s *Store) SaveSettings(ctx context.Context, values map[string]string) error {
	if s.readOnly {
		return fmt.Errorf("config: save settings: store opened read-only")
	}
	if len(values) == 0 {
		return nil
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
            INSERT INTO settings (instance_name, profile_name, key, value, updated_at)
            VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
            ON CONFLICT(instance_name, profile_name, key) DO UPDATE SET
                value = excluded.value,
                updated_at = CURRENT_TIMESTAMP
        `)
		if err != nil {
			return fmt.Errorf("config: prepare save settings: %w", err)
		}
		defer stmt.Close()

		for key, value := range values {
			if _, err := stmt.ExecContext(ctx, s.instanceName, s.profileName, key, value); err != nil {
				return fmt.Errorf("config: exec save setting %q: %w", key, err)
			}
		}
		return nil
	})
}
