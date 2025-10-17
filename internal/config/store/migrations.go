package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

const requiredSlotConfigJSON = `{"required":true}`

// MigrationResult captures changes applied while reconciling configuration defaults.
type MigrationResult struct {
	UpdatedSlots []string
	PendingSlots []string
}

// EnsureRequiredAdapterSlots makes sure required module slots exist and are marked as required when no adapter is bound.
func (s *Store) EnsureRequiredAdapterSlots(ctx context.Context) (MigrationResult, error) {
	var result MigrationResult
	if s.readOnly {
		return result, fmt.Errorf("config: ensure required slots: store opened read-only")
	}

	updated := make(map[string]struct{})

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		for _, slot := range requiredAdapterSlots {
			var (
				adapterID sql.NullString
				status    sql.NullString
				config    sql.NullString
			)
			err := tx.QueryRowContext(ctx, `
				SELECT adapter_id, status, config
				FROM adapter_bindings
				WHERE instance_name = ? AND profile_name = ? AND slot = ?
			`, s.instanceName, s.profileName, slot).Scan(&adapterID, &status, &config)
			switch {
			case err == sql.ErrNoRows:
				if _, execErr := tx.ExecContext(ctx, `
                        INSERT INTO adapter_bindings (instance_name, profile_name, slot, adapter_id, config, status, updated_at)
                        VALUES (?, ?, ?, NULL, ?, ?, CURRENT_TIMESTAMP)
                    `, s.instanceName, s.profileName, slot, requiredSlotConfigJSON, BindingStatusRequired); execErr != nil {
					return fmt.Errorf("config: insert adapter slot %s: %w", slot, execErr)
				}
				updated[slot] = struct{}{}
			case err != nil:
				return fmt.Errorf("config: select adapter slot %s: %w", slot, err)
			default:
				adapterPresent := adapterID.Valid && strings.TrimSpace(adapterID.String) != ""
				currentStatus := strings.TrimSpace(status.String)
				currentConfig := strings.TrimSpace(config.String)
				if adapterPresent {
					continue
				}
				if currentStatus != BindingStatusRequired || currentConfig == "" {
					if _, execErr := tx.ExecContext(ctx, `
                            UPDATE adapter_bindings
                            SET status = ?, config = ?, updated_at = CURRENT_TIMESTAMP
                            WHERE instance_name = ? AND profile_name = ? AND slot = ?
                        `, BindingStatusRequired, requiredSlotConfigJSON, s.instanceName, s.profileName, slot); execErr != nil {
						return fmt.Errorf("config: update adapter slot %s: %w", slot, execErr)
					}
					updated[slot] = struct{}{}
				}
			}
		}
		return nil
	})
	if err != nil {
		return result, err
	}

	if len(updated) > 0 {
		result.UpdatedSlots = make([]string, 0, len(updated))
		for slot := range updated {
			result.UpdatedSlots = append(result.UpdatedSlots, slot)
		}
		sort.Strings(result.UpdatedSlots)
	}

	pending, err := s.PendingQuickstartSlots(ctx)
	if err != nil {
		return result, err
	}
	sort.Strings(pending)
	result.PendingSlots = pending

	return result, nil
}
