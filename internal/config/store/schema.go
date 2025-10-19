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
	`CREATE TABLE IF NOT EXISTS module_endpoints (
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
}

var requiredAdapterSlots = []string{
	"stt.primary",
	"stt.secondary",
	"ai.primary",
	"tts.primary",
	"vad.primary",
	"tunnel.primary",
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
