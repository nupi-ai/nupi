package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Adapter management --------------------------------------------------------

// ListAdapters returns all registered adapters.
func (s *Store) ListAdapters(ctx context.Context) ([]Adapter, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, source, version, type, name, manifest, created_at, updated_at
        FROM adapters
        ORDER BY name
    `)
	if err != nil {
		return nil, fmt.Errorf("config: list adapters: %w", err)
	}
	defer rows.Close()

	var adapters []Adapter
	for rows.Next() {
		var adapter Adapter
		if err := rows.Scan(
			&adapter.ID,
			&adapter.Source,
			&adapter.Version,
			&adapter.Type,
			&adapter.Name,
			&adapter.Manifest,
			&adapter.CreatedAt,
			&adapter.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("config: scan adapter: %w", err)
		}
		adapters = append(adapters, adapter)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("config: iterate adapters: %w", err)
	}

	return adapters, nil
}

// GetAdapter retrieves adapter metadata by identifier.
func (s *Store) GetAdapter(ctx context.Context, adapterID string) (Adapter, error) {
	adapterID = strings.TrimSpace(adapterID)
	if adapterID == "" {
		return Adapter{}, fmt.Errorf("config: get adapter: adapter id required")
	}

	row := s.db.QueryRowContext(ctx, `
        SELECT id, source, version, type, name, manifest, created_at, updated_at
        FROM adapters
        WHERE id = ?
    `, adapterID)

	var adapter Adapter
	if err := row.Scan(
		&adapter.ID,
		&adapter.Source,
		&adapter.Version,
		&adapter.Type,
		&adapter.Name,
		&adapter.Manifest,
		&adapter.CreatedAt,
		&adapter.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return Adapter{}, NotFoundError{Entity: "adapter", Key: adapterID}
		}
		return Adapter{}, fmt.Errorf("config: get adapter %q: %w", adapterID, err)
	}
	return adapter, nil
}

// UpsertAdapter inserts or updates metadata for the given adapter.
func (s *Store) UpsertAdapter(ctx context.Context, adapter Adapter) error {
	return s.withWriteTx(ctx, "upsert adapter", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
            INSERT INTO adapters (id, source, version, type, name, manifest, created_at, updated_at)
            VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
            ON CONFLICT(id) DO UPDATE SET
                source = excluded.source,
                version = excluded.version,
                type = excluded.type,
                name = excluded.name,
                manifest = excluded.manifest,
                updated_at = CURRENT_TIMESTAMP
        `,
			adapter.ID,
			adapter.Source,
			adapter.Version,
			adapter.Type,
			adapter.Name,
			adapter.Manifest,
		)
		if err != nil {
			return fmt.Errorf("config: upsert adapter %q: %w", adapter.ID, err)
		}
		return nil
	})
}

// RemoveAdapter deletes adapter metadata and clears related bindings.
func (s *Store) RemoveAdapter(ctx context.Context, adapterID string) error {
	return s.withWriteTx(ctx, "remove adapter", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
            UPDATE adapter_bindings
            SET adapter_id = NULL,
                status = 'inactive',
                updated_at = CURRENT_TIMESTAMP
            WHERE instance_name = ? AND profile_name = ? AND adapter_id = ?
        `, s.instanceName, s.profileName, adapterID); err != nil {
			return fmt.Errorf("config: clear adapter bindings: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM adapters WHERE id = ?`, adapterID); err != nil {
			return fmt.Errorf("config: delete adapter %q: %w", adapterID, err)
		}
		return nil
	})
}

// AdapterExists reports whether an adapter with the given identifier is registered.
func (s *Store) AdapterExists(ctx context.Context, adapterID string) (bool, error) {
	if strings.TrimSpace(adapterID) == "" {
		return false, fmt.Errorf("config: adapter id is required")
	}

	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM adapters WHERE id = ?`, adapterID).Scan(&count); err != nil {
		return false, fmt.Errorf("config: check adapter exists: %w", err)
	}
	return count > 0, nil
}

// Adapter bindings ----------------------------------------------------------

// ListAdapterBindings returns all slot bindings for the active profile.
func (s *Store) ListAdapterBindings(ctx context.Context) ([]AdapterBinding, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT slot, adapter_id, config, status, updated_at
        FROM adapter_bindings
        WHERE instance_name = ? AND profile_name = ?
        ORDER BY slot
    `, s.instanceName, s.profileName)
	if err != nil {
		return nil, fmt.Errorf("config: list adapter bindings: %w", err)
	}
	defer rows.Close()

	var bindings []AdapterBinding
	for rows.Next() {
		var (
			slot      string
			adapterID sql.NullString
			config    sql.NullString
			status    string
			updatedAt string
		)
		if err := rows.Scan(&slot, &adapterID, &config, &status, &updatedAt); err != nil {
			return nil, fmt.Errorf("config: scan adapter binding: %w", err)
		}

		var parsedConfig string
		if config.Valid {
			parsedConfig = config.String
		}

		binding := AdapterBinding{
			Slot:      slot,
			Status:    status,
			UpdatedAt: updatedAt,
		}
		if adapterID.Valid {
			binding.AdapterID = &adapterID.String
		}
		binding.Config = parsedConfig
		bindings = append(bindings, binding)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("config: iterate adapter bindings: %w", err)
	}

	return bindings, nil
}

// AdapterBinding retrieves a single binding entry by slot.
func (s *Store) AdapterBinding(ctx context.Context, slot string) (*AdapterBinding, error) {
	if s == nil || s.db == nil {
		return nil, sql.ErrConnDone
	}
	slot = strings.TrimSpace(slot)
	if slot == "" {
		return nil, fmt.Errorf("config: adapter binding: slot is required")
	}

	var (
		adapterID sql.NullString
		config    sql.NullString
		status    string
		updatedAt string
	)

	err := s.db.QueryRowContext(ctx, `
        SELECT adapter_id, config, status, updated_at
        FROM adapter_bindings
        WHERE instance_name = ? AND profile_name = ? AND slot = ?
    `, s.instanceName, s.profileName, slot).Scan(&adapterID, &config, &status, &updatedAt)
	switch {
	case err == sql.ErrNoRows:
		return nil, NotFoundError{Entity: "adapter_binding", Key: slot}
	case err != nil:
		return nil, fmt.Errorf("config: adapter binding %s: %w", slot, err)
	}

	binding := AdapterBinding{
		Slot:      slot,
		Status:    status,
		UpdatedAt: updatedAt,
	}
	if adapterID.Valid {
		id := strings.TrimSpace(adapterID.String)
		if id != "" {
			binding.AdapterID = &id
		}
	}
	if config.Valid {
		binding.Config = config.String
	}
	return &binding, nil
}

// SetActiveAdapter binds an adapter to a slot with optional JSON configuration.
func (s *Store) SetActiveAdapter(ctx context.Context, slot string, adapterID string, config map[string]any) error {
	if err := s.ensureWritable("set active adapter"); err != nil {
		return err
	}

	payload, err := encodeJSON(config, nullWhenNilMap[string, any])
	if err != nil {
		return fmt.Errorf("config: marshal adapter config: %w", err)
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
            UPDATE adapter_bindings
            SET adapter_id = ?,
                config = ?,
                status = 'active',
                updated_at = CURRENT_TIMESTAMP
            WHERE instance_name = ? AND profile_name = ? AND slot = ?
        `, adapterID, payload, s.instanceName, s.profileName, slot)
		if err != nil {
			return fmt.Errorf("config: update adapter binding %q: %w", slot, err)
		}

		rows, _ := res.RowsAffected()
		if rows == 0 {
			return NotFoundError{Entity: "adapter_binding", Key: slot}
		}
		return nil
	})
}

// ClearAdapterBinding removes the adapter from the slot and marks it inactive.
func (s *Store) ClearAdapterBinding(ctx context.Context, slot string) error {
	return s.withWriteTx(ctx, "clear adapter", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
            UPDATE adapter_bindings
            SET adapter_id = NULL,
                config = NULL,
                status = 'inactive',
                updated_at = CURRENT_TIMESTAMP
            WHERE instance_name = ? AND profile_name = ? AND slot = ?
        `, s.instanceName, s.profileName, slot)
		if err != nil {
			return fmt.Errorf("config: clear adapter binding %q: %w", slot, err)
		}
		if rows, _ := res.RowsAffected(); rows == 0 {
			return NotFoundError{Entity: "adapter_binding", Key: slot}
		}
		return nil
	})
}

// UpdateAdapterBindingStatus updates the status flag for the binding without altering adapter assignment.
func (s *Store) UpdateAdapterBindingStatus(ctx context.Context, slot string, status string) error {
	if err := s.ensureWritable("update binding status"); err != nil {
		return err
	}

	status = strings.TrimSpace(strings.ToLower(status))
	switch status {
	case BindingStatusActive, BindingStatusInactive, BindingStatusRequired:
	default:
		return fmt.Errorf("config: invalid adapter binding status %q", status)
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
            UPDATE adapter_bindings
            SET status = ?,
                updated_at = CURRENT_TIMESTAMP
            WHERE instance_name = ? AND profile_name = ? AND slot = ?
        `, status, s.instanceName, s.profileName, slot)
		if err != nil {
			return fmt.Errorf("config: update binding status %q: %w", slot, err)
		}
		if rows, _ := res.RowsAffected(); rows == 0 {
			return NotFoundError{Entity: "adapter_binding", Key: slot}
		}
		return nil
	})
}
