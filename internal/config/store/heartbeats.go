package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// Heartbeat represents a scheduled heartbeat task.
type Heartbeat struct {
	ID        int64
	Name      string
	CronExpr  string
	Prompt    string
	LastRunAt *time.Time
	CreatedAt time.Time
}

// InsertHeartbeat creates a new heartbeat. Returns an error on duplicate name.
// When maxCount > 0, atomically checks that the current count is below the limit
// before inserting (prevents TOCTOU race conditions).
func (s *Store) InsertHeartbeat(ctx context.Context, name, cronExpr, prompt string, maxCount int) error {
	return s.withWriteTx(ctx, "InsertHeartbeat", func(tx *sql.Tx) error {
		if maxCount > 0 {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM heartbeats`).Scan(&count); err != nil {
				return fmt.Errorf("count heartbeats: %w", err)
			}
			if count >= maxCount {
				return fmt.Errorf("maximum number of heartbeats (%d) reached", maxCount)
			}
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err := tx.ExecContext(ctx,
			`INSERT INTO heartbeats (name, cron_expr, prompt, created_at) VALUES (?, ?, ?, ?)`,
			name, cronExpr, prompt, now,
		)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "unique") {
				return DuplicateError{Entity: "heartbeat", Key: name}
			}
			return fmt.Errorf("insert heartbeat %q: %w", name, err)
		}
		return nil
	})
}

// DeleteHeartbeat removes a heartbeat by name. Returns NotFoundError if not found.
func (s *Store) DeleteHeartbeat(ctx context.Context, name string) error {
	return s.withWriteTx(ctx, "DeleteHeartbeat", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM heartbeats WHERE name = ?`, name,
		)
		if err != nil {
			return fmt.Errorf("delete heartbeat %q: %w", name, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete heartbeat %q rows affected: %w", name, err)
		}
		if n == 0 {
			return NotFoundError{Entity: "heartbeat", Key: name}
		}
		return nil
	})
}

// ListHeartbeats returns all heartbeats ordered by id.
func (s *Store) ListHeartbeats(ctx context.Context) ([]Heartbeat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, cron_expr, prompt, last_run_at, created_at FROM heartbeats ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list heartbeats: %w", err)
	}
	defer rows.Close()

	result := make([]Heartbeat, 0)
	for rows.Next() {
		var h Heartbeat
		var lastRun sql.NullString
		var createdAt string
		if err := rows.Scan(&h.ID, &h.Name, &h.CronExpr, &h.Prompt, &lastRun, &createdAt); err != nil {
			return nil, fmt.Errorf("scan heartbeat: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			h.CreatedAt = t
		} else if t, err := time.Parse("2006-01-02 15:04:05", createdAt); err == nil {
			h.CreatedAt = t
		} else {
			log.Printf("[Config] heartbeat %q: invalid created_at %q", h.Name, createdAt)
		}
		if lastRun.Valid {
			t, err := time.Parse(time.RFC3339Nano, lastRun.String)
			if err == nil {
				h.LastRunAt = &t
			} else {
				log.Printf("[Config] heartbeat %q: invalid last_run_at %q: %v", h.Name, lastRun.String, err)
			}
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

// UpdateHeartbeatLastRun sets the last_run_at timestamp for a heartbeat.
func (s *Store) UpdateHeartbeatLastRun(ctx context.Context, id int64, lastRunAt time.Time) error {
	return s.withWriteTx(ctx, "UpdateHeartbeatLastRun", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE heartbeats SET last_run_at = ? WHERE id = ?`,
			lastRunAt.UTC().Format(time.RFC3339Nano), id,
		)
		if err != nil {
			return fmt.Errorf("update heartbeat last_run_at: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("update heartbeat last_run_at rows affected: %w", err)
		}
		if n == 0 {
			return NotFoundError{Entity: "heartbeat", Key: fmt.Sprintf("id=%d", id)}
		}
		return nil
	})
}
