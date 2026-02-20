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
		event_type TEXT NOT NULL CHECK (event_type IN ('user_intent', 'session_output', 'history_summary', 'clarification', 'memory_flush')),
		content TEXT NOT NULL,
		is_custom INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (instance_name, profile_name, event_type),
		FOREIGN KEY (instance_name, profile_name) REFERENCES profiles(instance_name, name) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS marketplaces (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		instance_name TEXT NOT NULL,
		namespace TEXT NOT NULL,
		url TEXT NOT NULL DEFAULT '',
		is_builtin INTEGER NOT NULL DEFAULT 0,
		cached_index TEXT,
		last_refreshed TEXT,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE (instance_name, namespace),
		FOREIGN KEY (instance_name) REFERENCES instances(name) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS installed_plugins (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		marketplace_id INTEGER NOT NULL,
		slug TEXT NOT NULL,
		source_url TEXT,
		installed_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		enabled INTEGER NOT NULL DEFAULT 0,
		UNIQUE (marketplace_id, slug),
		FOREIGN KEY (marketplace_id) REFERENCES marketplaces(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS plugin_checksums (
		plugin_id INTEGER NOT NULL,
		file_path TEXT NOT NULL,
		sha256 TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (plugin_id, file_path),
		FOREIGN KEY (plugin_id) REFERENCES installed_plugins(id) ON DELETE CASCADE
	)`,
}

// migrationStatements are ALTER TABLE statements applied after the initial schema.
// applyMigrations swallows "duplicate column name" errors so these are idempotent.
var migrationStatements = []string{
	`ALTER TABLE adapter_endpoints ADD COLUMN tls_cert_path TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE adapter_endpoints ADD COLUMN tls_key_path TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE adapter_endpoints ADD COLUMN tls_ca_cert_path TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE adapter_endpoints ADD COLUMN tls_insecure INTEGER NOT NULL DEFAULT 0`,
}

// applyMigrations runs ALTER TABLE migrations idempotently.
// "duplicate column name" errors are expected on subsequent runs and ignored.
func applyMigrations(ctx context.Context, db *sql.DB) error {
	for _, stmt := range migrationStatements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			// SQLite returns "duplicate column name" when column already exists.
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return fmt.Errorf("config: apply migration %q: %w", abbreviate(stmt), err)
		}
	}
	return nil
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

func abbreviate(stmt string) string {
	const maxLen = 64
	trimmed := strings.Join(strings.Fields(stmt), " ")
	if len(trimmed) <= maxLen {
		return trimmed
	}
	return trimmed[:maxLen] + "…"
}
