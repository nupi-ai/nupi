package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// PluginChecksum represents a single per-file checksum for a plugin.
type PluginChecksum struct {
	PluginID  int64
	FilePath  string
	SHA256    string
	CreatedAt string
}

// SetPluginChecksums replaces all checksums for a plugin in a single transaction.
func (s *Store) SetPluginChecksums(ctx context.Context, pluginID int64, checksums map[string]string) error {
	if s.readOnly {
		return fmt.Errorf("config: set plugin checksums: store opened read-only")
	}
	if pluginID <= 0 {
		return fmt.Errorf("config: set plugin checksums: invalid plugin ID %d", pluginID)
	}
	// Sort keys for deterministic error reporting and deterministic INSERT order.
	paths := make([]string, 0, len(checksums))
	for path := range checksums {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		hash := checksums[path]
		if strings.TrimSpace(path) == "" || hash == "" {
			return fmt.Errorf("config: set plugin checksums: empty file path or hash for entry %q->%q", path, hash)
		}
		if len(hash) != 64 {
			return fmt.Errorf("config: set plugin checksums: invalid SHA-256 hash for %q: length %d, want 64", path, len(hash))
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return fmt.Errorf("config: set plugin checksums: invalid SHA-256 hash for %q: not valid hexadecimal", path)
		}
	}
	// All database operations (existence check, DELETE, INSERTs) run inside a
	// single transaction to avoid TOCTOU races — a concurrent DELETE of the
	// plugin between a pre-check and the transaction would cause an opaque FK
	// error instead of the clean "plugin ID X not found" message.
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var pluginExists int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM installed_plugins WHERE id = ?`, pluginID).Scan(&pluginExists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("config: set plugin checksums: plugin ID %d not found", pluginID)
			}
			return fmt.Errorf("config: set plugin checksums: verify plugin %d: %w", pluginID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_checksums WHERE plugin_id = ?`, pluginID); err != nil {
			return fmt.Errorf("config: set plugin checksums: delete old for plugin %d: %w", pluginID, err)
		}
		// Row-by-row INSERT using sorted paths for deterministic write order.
		// SQLite does not benefit from multi-row VALUES syntax (each row still
		// triggers a separate B-tree operation), and plugin file counts are
		// small enough that the overhead is negligible.
		for _, path := range paths {
			// Normalize to lowercase at insert time for consistent comparison
			// with hex.EncodeToString output. Done here (not in pre-validation)
			// to avoid mutating the caller's map.
			hash := strings.ToLower(checksums[path])
			if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_checksums (plugin_id, file_path, sha256) VALUES (?, ?, ?)`, pluginID, path, hash); err != nil {
				return fmt.Errorf("config: set plugin checksums: insert %q for plugin %d: %w", path, pluginID, err)
			}
		}
		return nil
	})
}

// GetPluginChecksumsByPlugin retrieves checksums by marketplace namespace and plugin slug.
func (s *Store) GetPluginChecksumsByPlugin(ctx context.Context, namespace, slug string) ([]PluginChecksum, error) {
	namespace = strings.TrimSpace(namespace)
	slug = strings.TrimSpace(slug)
	if namespace == "" || slug == "" {
		return nil, fmt.Errorf("config: get plugin checksums by plugin: namespace and slug required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT pc.plugin_id, pc.file_path, pc.sha256, pc.created_at
		FROM plugin_checksums pc
		JOIN installed_plugins ip ON pc.plugin_id = ip.id
		JOIN marketplaces m ON ip.marketplace_id = m.id
		WHERE m.instance_name = ? AND m.namespace = ? AND ip.slug = ?
		ORDER BY pc.file_path
	`, s.instanceName, namespace, slug)
	if err != nil {
		return nil, fmt.Errorf("config: get plugin checksums by plugin: %s/%s: %w", namespace, slug, err)
	}
	defer rows.Close()

	var result []PluginChecksum
	for rows.Next() {
		var pc PluginChecksum
		if err := rows.Scan(&pc.PluginID, &pc.FilePath, &pc.SHA256, &pc.CreatedAt); err != nil {
			return nil, fmt.Errorf("config: get plugin checksums by plugin: scan: %w", err)
		}
		result = append(result, pc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("config: get plugin checksums by plugin: iterate: %w", err)
	}
	if result == nil {
		result = []PluginChecksum{}
	}
	return result, nil
}

// GetPluginChecksumsByID retrieves checksums by plugin database ID.
func (s *Store) GetPluginChecksumsByID(ctx context.Context, pluginID int64) ([]PluginChecksum, error) {
	if pluginID <= 0 {
		return nil, fmt.Errorf("config: get plugin checksums by id: invalid plugin ID %d", pluginID)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT plugin_id, file_path, sha256, created_at
		FROM plugin_checksums
		WHERE plugin_id = ?
		ORDER BY file_path
	`, pluginID)
	if err != nil {
		return nil, fmt.Errorf("config: get plugin checksums by id: plugin %d: %w", pluginID, err)
	}
	defer rows.Close()

	var result []PluginChecksum
	for rows.Next() {
		var pc PluginChecksum
		if err := rows.Scan(&pc.PluginID, &pc.FilePath, &pc.SHA256, &pc.CreatedAt); err != nil {
			return nil, fmt.Errorf("config: get plugin checksums by id: scan: %w", err)
		}
		result = append(result, pc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("config: get plugin checksums by id: iterate: %w", err)
	}
	if result == nil {
		result = []PluginChecksum{}
	}
	return result, nil
}

// DeletePluginChecksumsByID removes all checksums for a plugin by its database ID.
func (s *Store) DeletePluginChecksumsByID(ctx context.Context, pluginID int64) error {
	if s.readOnly {
		return fmt.Errorf("config: delete plugin checksums: store opened read-only")
	}
	if pluginID <= 0 {
		return fmt.Errorf("config: delete plugin checksums: invalid plugin ID %d", pluginID)
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM plugin_checksums WHERE plugin_id = ?`, pluginID)
	if err != nil {
		return fmt.Errorf("config: delete plugin checksums: plugin %d: %w", pluginID, err)
	}
	n, raErr := result.RowsAffected()
	if raErr != nil {
		return fmt.Errorf("config: delete plugin checksums: rows affected: %w", raErr)
	}
	if n == 0 {
		return NotFoundError{Entity: "plugin checksums", Key: fmt.Sprintf("plugin_id=%d", pluginID)}
	}
	return nil
}
