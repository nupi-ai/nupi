package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var allowedAdapterTransports = map[string]struct{}{
	"process": {},
	"grpc":    {},
	"http":    {},
}

// NormalizeAdapterTransport trims whitespace, applies the default ("process" when empty)
// and verifies the transport is one of the allowed values.
func NormalizeAdapterTransport(value string) (string, error) {
	return sanitizeTransport(value)
}

func sanitizeTransport(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "process"
	}
	if _, ok := allowedAdapterTransports[value]; !ok {
		return "", fmt.Errorf("config: invalid adapter transport %q", value)
	}
	return value, nil
}

// ListAdapterEndpoints returns stored adapter endpoint definitions.
func (s *Store) ListAdapterEndpoints(ctx context.Context) ([]AdapterEndpoint, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT adapter_id, transport, address, command, args, env,
               tls_cert_path, tls_key_path, tls_ca_cert_path, tls_insecure,
               created_at, updated_at
        FROM adapter_endpoints
        ORDER BY adapter_id
    `)
	if err != nil {
		return nil, fmt.Errorf("config: list adapter endpoints: %w", err)
	}
	defer rows.Close()

	var endpoints []AdapterEndpoint
	for rows.Next() {
		var (
			argsRaw     sql.NullString
			envRaw      sql.NullString
			tlsInsecure int
			endp        AdapterEndpoint
		)
		if err := rows.Scan(
			&endp.AdapterID,
			&endp.Transport,
			&endp.Address,
			&endp.Command,
			&argsRaw,
			&envRaw,
			&endp.TLSCertPath,
			&endp.TLSKeyPath,
			&endp.TLSCACertPath,
			&tlsInsecure,
			&endp.CreatedAt,
			&endp.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("config: scan adapter endpoint: %w", err)
		}
		endp.TLSInsecure = tlsInsecure != 0

		args, err := decodeStringSlice(argsRaw)
		if err != nil {
			return nil, fmt.Errorf("config: decode adapter endpoint args for %s: %w", endp.AdapterID, err)
		}
		endp.Args = args
		env, err := decodeStringMap(envRaw)
		if err != nil {
			return nil, fmt.Errorf("config: decode adapter endpoint env for %s: %w", endp.AdapterID, err)
		}
		endp.Env = env

		endpoints = append(endpoints, endp)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("config: iterate adapter endpoints: %w", err)
	}

	return endpoints, nil
}

// GetAdapterEndpoint retrieves the endpoint definition for a given adapter.
func (s *Store) GetAdapterEndpoint(ctx context.Context, adapterID string) (AdapterEndpoint, error) {
	adapterID = strings.TrimSpace(adapterID)
	row := s.db.QueryRowContext(ctx, `
        SELECT adapter_id, transport, address, command, args, env,
               tls_cert_path, tls_key_path, tls_ca_cert_path, tls_insecure,
               created_at, updated_at
        FROM adapter_endpoints
        WHERE adapter_id = ?
    `, adapterID)

	var (
		argsRaw     sql.NullString
		envRaw      sql.NullString
		tlsInsecure int
		endp        AdapterEndpoint
	)

	if err := row.Scan(
		&endp.AdapterID,
		&endp.Transport,
		&endp.Address,
		&endp.Command,
		&argsRaw,
		&envRaw,
		&endp.TLSCertPath,
		&endp.TLSKeyPath,
		&endp.TLSCACertPath,
		&tlsInsecure,
		&endp.CreatedAt,
		&endp.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AdapterEndpoint{}, NotFoundError{Entity: "adapter_endpoint", Key: adapterID}
		}
		return AdapterEndpoint{}, fmt.Errorf("config: get adapter endpoint %q: %w", adapterID, err)
	}
	endp.TLSInsecure = tlsInsecure != 0

	args, err := decodeStringSlice(argsRaw)
	if err != nil {
		return AdapterEndpoint{}, fmt.Errorf("config: decode adapter endpoint args for %s: %w", adapterID, err)
	}
	endp.Args = args
	env, err := decodeStringMap(envRaw)
	if err != nil {
		return AdapterEndpoint{}, fmt.Errorf("config: decode adapter endpoint env for %s: %w", adapterID, err)
	}
	endp.Env = env
	return endp, nil
}

// UpsertAdapterEndpoint inserts or updates an endpoint definition for the adapter.
func (s *Store) UpsertAdapterEndpoint(ctx context.Context, endpoint AdapterEndpoint) error {
	if s.readOnly {
		return fmt.Errorf("config: upsert adapter endpoint: store opened read-only")
	}

	adapterID := strings.TrimSpace(endpoint.AdapterID)
	if adapterID == "" {
		return fmt.Errorf("config: upsert adapter endpoint: adapter id required")
	}

	trans, err := NormalizeAdapterTransport(endpoint.Transport)
	if err != nil {
		return err
	}

	argsPayload, err := encodeStringSlice(endpoint.Args)
	if err != nil {
		return fmt.Errorf("config: marshal adapter args: %w", err)
	}

	envPayload, err := encodeStringMap(endpoint.Env)
	if err != nil {
		return fmt.Errorf("config: marshal adapter env: %w", err)
	}

	tlsInsecure := 0
	if endpoint.TLSInsecure {
		tlsInsecure = 1
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
            INSERT INTO adapter_endpoints (adapter_id, transport, address, command, args, env,
                tls_cert_path, tls_key_path, tls_ca_cert_path, tls_insecure,
                created_at, updated_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
            ON CONFLICT(adapter_id) DO UPDATE SET
                transport = excluded.transport,
                address = excluded.address,
                command = excluded.command,
                args = excluded.args,
                env = excluded.env,
                tls_cert_path = excluded.tls_cert_path,
                tls_key_path = excluded.tls_key_path,
                tls_ca_cert_path = excluded.tls_ca_cert_path,
                tls_insecure = excluded.tls_insecure,
                updated_at = CURRENT_TIMESTAMP
        `,
			adapterID,
			trans,
			strings.TrimSpace(endpoint.Address),
			strings.TrimSpace(endpoint.Command),
			argsPayload,
			envPayload,
			strings.TrimSpace(endpoint.TLSCertPath),
			strings.TrimSpace(endpoint.TLSKeyPath),
			strings.TrimSpace(endpoint.TLSCACertPath),
			tlsInsecure,
		)
		if err != nil {
			return fmt.Errorf("config: upsert adapter endpoint %q: %w", adapterID, err)
		}
		return nil
	})
}

// RemoveAdapterEndpoint deletes the endpoint definition for the adapter.
func (s *Store) RemoveAdapterEndpoint(ctx context.Context, adapterID string) error {
	if s.readOnly {
		return fmt.Errorf("config: remove adapter endpoint: store opened read-only")
	}

	adapterID = strings.TrimSpace(adapterID)
	if adapterID == "" {
		return fmt.Errorf("config: remove adapter endpoint: adapter id required")
	}

	_, err := s.db.ExecContext(ctx, `
        DELETE FROM adapter_endpoints
        WHERE adapter_id = ?
    `, adapterID)
	if err != nil {
		return fmt.Errorf("config: delete adapter endpoint %q: %w", adapterID, err)
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
