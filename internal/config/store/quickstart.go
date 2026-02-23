package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const sqliteTimestampLayout = "2006-01-02 15:04:05"

// QuickstartStatus reports whether the initial setup has been completed.
func (s *Store) QuickstartStatus(ctx context.Context) (bool, *time.Time, error) {
	var (
		completed   int
		completedAt sql.NullString
	)

	err := s.db.QueryRowContext(ctx, `
        SELECT completed, completed_at
        FROM quickstart_status
        WHERE instance_name = ? AND profile_name = ?
    `, s.instanceName, s.profileName).Scan(&completed, &completedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("config: query quickstart status: %w", err)
	}

	var ts *time.Time
	if completedAt.Valid && completedAt.String != "" {
		parsed, parseErr := time.ParseInLocation(sqliteTimestampLayout, completedAt.String, time.Local)
		if parseErr != nil {
			return false, nil, fmt.Errorf("config: parse quickstart timestamp: %w", parseErr)
		}
		ts = &parsed
	}

	return completed == 1, ts, nil
}

// MarkQuickstartCompleted flips the quickstart completion flag.
func (s *Store) MarkQuickstartCompleted(ctx context.Context, complete bool) error {
	if s.readOnly {
		return fmt.Errorf("config: mark quickstart: store opened read-only")
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		var query string
		if complete {
			query = `
                UPDATE quickstart_status
                SET completed = 1,
                    completed_at = CURRENT_TIMESTAMP,
                    updated_at = CURRENT_TIMESTAMP
                WHERE instance_name = ? AND profile_name = ?
            `
		} else {
			query = `
                UPDATE quickstart_status
                SET completed = 0,
                    completed_at = NULL,
                    updated_at = CURRENT_TIMESTAMP
                WHERE instance_name = ? AND profile_name = ?
            `
		}

		res, err := tx.ExecContext(ctx, query, s.instanceName, s.profileName)
		if err != nil {
			return fmt.Errorf("config: update quickstart status: %w", err)
		}
		if rows, _ := res.RowsAffected(); rows == 0 {
			return NotFoundError{Entity: "quickstart_status"}
		}
		return nil
	})
}

// PendingQuickstartSlots lists adapter slots that still require attention.
func (s *Store) PendingQuickstartSlots(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT slot
        FROM adapter_bindings
        WHERE instance_name = ? AND profile_name = ?
          AND (status = 'required' OR adapter_id IS NULL)
        ORDER BY slot
    `, s.instanceName, s.profileName)
	if err != nil {
		return nil, fmt.Errorf("config: quickstart pending slots: %w", err)
	}
	return scanList(rows, scanString, "config: scan quickstart slot", "config: iterate quickstart slots")
}
