package store

import (
	"context"
	"database/sql"
	"fmt"
)

const profileColumns = "name, is_default, created_at, updated_at"
const profileExistsSelect = "SELECT 1 FROM profiles WHERE instance_name = ? AND name = ?"

// Profiles returns all profiles configured for the current instance.
func (s *Store) Profiles(ctx context.Context) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT `+profileColumns+`
        FROM profiles
        WHERE instance_name = ?
        ORDER BY name
    `, s.instanceName)
	if err != nil {
		return nil, fmt.Errorf("config: list profiles: %w", err)
	}
	return scanList(rows, scanProfile, "config: scan profile", "config: iterate profiles")
}

// ActivateProfile marks the provided profile as the default one for the instance.
func (s *Store) ActivateProfile(ctx context.Context, profileName string) error {
	return s.withWriteTx(ctx, "activate profile", func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(`+profileExistsSelect+`)
		`, s.instanceName, profileName).Scan(&exists); err != nil {
			return fmt.Errorf("config: check profile %q: %w", profileName, err)
		}
		if !exists {
			return NotFoundError{Entity: "profile", Key: profileName}
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE profiles
			SET is_default = 0,
			    updated_at = CURRENT_TIMESTAMP
			WHERE instance_name = ?
		`, s.instanceName); err != nil {
			return fmt.Errorf("config: clear default profile: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE profiles
			SET is_default = 1,
			    updated_at = CURRENT_TIMESTAMP
			WHERE instance_name = ? AND name = ?
		`, s.instanceName, profileName)
		if err != nil {
			return fmt.Errorf("config: update default profile: %w", err)
		}

		rows, _ := res.RowsAffected()
		if rows == 0 {
			return NotFoundError{Entity: "profile", Key: profileName}
		}

		return nil
	})
}
