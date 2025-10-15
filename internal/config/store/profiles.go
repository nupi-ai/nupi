package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Profiles returns all profiles configured for the current instance.
func (s *Store) Profiles(ctx context.Context) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT name, is_default, created_at, updated_at
        FROM profiles
        WHERE instance_name = ?
        ORDER BY name
    `, s.instanceName)
	if err != nil {
		return nil, fmt.Errorf("config: list profiles: %w", err)
	}
	defer rows.Close()

	var profiles []Profile
	for rows.Next() {
		var (
			name      string
			isDefault int
			createdAt string
			updatedAt string
		)
		if err := rows.Scan(&name, &isDefault, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("config: scan profile: %w", err)
		}
		profiles = append(profiles, Profile{
			Name:      name,
			IsDefault: isDefault == 1,
			CreatedAt: createdAt,
			UpdatedAt: updatedAt,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("config: iterate profiles: %w", err)
	}

	return profiles, nil
}

// ActivateProfile marks the provided profile as the default one for the instance.
func (s *Store) ActivateProfile(ctx context.Context, profileName string) error {
	if s.readOnly {
		return fmt.Errorf("config: activate profile: store opened read-only")
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
            UPDATE profiles
            SET is_default = CASE WHEN name = ? THEN 1 ELSE 0 END,
                updated_at = CURRENT_TIMESTAMP
            WHERE instance_name = ?
        `, profileName, s.instanceName)
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
