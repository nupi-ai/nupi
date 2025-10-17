package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var allowedModuleTransports = map[string]struct{}{
	"process": {},
	"grpc":    {},
	"http":    {},
}

func sanitizeTransport(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "process"
	}
	if _, ok := allowedModuleTransports[value]; !ok {
		return "", fmt.Errorf("config: invalid module transport %q", value)
	}
	return value, nil
}

// ListModuleEndpoints returns stored module endpoint definitions.
func (s *Store) ListModuleEndpoints(ctx context.Context) ([]ModuleEndpoint, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT adapter_id, transport, address, command, args, env, created_at, updated_at
        FROM module_endpoints
        ORDER BY adapter_id
    `)
	if err != nil {
		return nil, fmt.Errorf("config: list module endpoints: %w", err)
	}
	defer rows.Close()

	var endpoints []ModuleEndpoint
	for rows.Next() {
		var (
			argsRaw sql.NullString
			envRaw  sql.NullString
			endp    ModuleEndpoint
		)
		if err := rows.Scan(
			&endp.AdapterID,
			&endp.Transport,
			&endp.Address,
			&endp.Command,
			&argsRaw,
			&envRaw,
			&endp.CreatedAt,
			&endp.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("config: scan module endpoint: %w", err)
		}

		args, err := decodeStringSlice(argsRaw)
		if err != nil {
			return nil, fmt.Errorf("config: decode module endpoint args for %s: %w", endp.AdapterID, err)
		}
		endp.Args = args
		env, err := decodeStringMap(envRaw)
		if err != nil {
			return nil, fmt.Errorf("config: decode module endpoint env for %s: %w", endp.AdapterID, err)
		}
		endp.Env = env

		endpoints = append(endpoints, endp)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("config: iterate module endpoints: %w", err)
	}

	return endpoints, nil
}

// GetModuleEndpoint retrieves the endpoint definition for a given adapter.
func (s *Store) GetModuleEndpoint(ctx context.Context, adapterID string) (ModuleEndpoint, error) {
	adapterID = strings.TrimSpace(adapterID)
	row := s.db.QueryRowContext(ctx, `
        SELECT adapter_id, transport, address, command, args, env, created_at, updated_at
        FROM module_endpoints
        WHERE adapter_id = ?
    `, adapterID)

	var (
		argsRaw sql.NullString
		envRaw  sql.NullString
		endp    ModuleEndpoint
	)

	if err := row.Scan(
		&endp.AdapterID,
		&endp.Transport,
		&endp.Address,
		&endp.Command,
		&argsRaw,
		&envRaw,
		&endp.CreatedAt,
		&endp.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ModuleEndpoint{}, NotFoundError{Entity: "module_endpoint", Key: adapterID}
		}
		return ModuleEndpoint{}, fmt.Errorf("config: get module endpoint %q: %w", adapterID, err)
	}

	args, err := decodeStringSlice(argsRaw)
	if err != nil {
		return ModuleEndpoint{}, fmt.Errorf("config: decode module endpoint args for %s: %w", adapterID, err)
	}
	endp.Args = args
	env, err := decodeStringMap(envRaw)
	if err != nil {
		return ModuleEndpoint{}, fmt.Errorf("config: decode module endpoint env for %s: %w", adapterID, err)
	}
	endp.Env = env
	return endp, nil
}

// UpsertModuleEndpoint inserts or updates an endpoint definition for the adapter.
func (s *Store) UpsertModuleEndpoint(ctx context.Context, endpoint ModuleEndpoint) error {
	if s.readOnly {
		return fmt.Errorf("config: upsert module endpoint: store opened read-only")
	}

	adapterID := strings.TrimSpace(endpoint.AdapterID)
	if adapterID == "" {
		return fmt.Errorf("config: upsert module endpoint: adapter id required")
	}

	trans, err := sanitizeTransport(endpoint.Transport)
	if err != nil {
		return err
	}

	argsPayload, err := encodeStringSlice(endpoint.Args)
	if err != nil {
		return fmt.Errorf("config: marshal module args: %w", err)
	}

	envPayload, err := encodeStringMap(endpoint.Env)
	if err != nil {
		return fmt.Errorf("config: marshal module env: %w", err)
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
            INSERT INTO module_endpoints (adapter_id, transport, address, command, args, env, created_at, updated_at)
            VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
            ON CONFLICT(adapter_id) DO UPDATE SET
                transport = excluded.transport,
                address = excluded.address,
                command = excluded.command,
                args = excluded.args,
                env = excluded.env,
                updated_at = CURRENT_TIMESTAMP
        `,
			adapterID,
			trans,
			strings.TrimSpace(endpoint.Address),
			strings.TrimSpace(endpoint.Command),
			argsPayload,
			envPayload,
		)
		if err != nil {
			return fmt.Errorf("config: upsert module endpoint %q: %w", adapterID, err)
		}
		return nil
	})
}

// RemoveModuleEndpoint deletes the endpoint definition for the adapter.
func (s *Store) RemoveModuleEndpoint(ctx context.Context, adapterID string) error {
	if s.readOnly {
		return fmt.Errorf("config: remove module endpoint: store opened read-only")
	}

	adapterID = strings.TrimSpace(adapterID)
	if adapterID == "" {
		return fmt.Errorf("config: remove module endpoint: adapter id required")
	}

	_, err := s.db.ExecContext(ctx, `
        DELETE FROM module_endpoints
        WHERE adapter_id = ?
    `, adapterID)
	if err != nil {
		return fmt.Errorf("config: delete module endpoint %q: %w", adapterID, err)
	}
	return nil
}

func decodeStringSlice(raw sql.NullString) ([]string, error) {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func encodeStringSlice(values []string) (any, error) {
	if len(values) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

func decodeStringMap(raw sql.NullString) (map[string]string, error) {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil, nil
	}
	dst := make(map[string]string)
	if err := json.Unmarshal([]byte(raw.String), &dst); err != nil {
		return nil, err
	}
	return dst, nil
}

func encodeStringMap(values map[string]string) (any, error) {
	if len(values) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}
