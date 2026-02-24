package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/nupi-ai/nupi/internal/constants"
)

var allowedAdapterTransports = constants.StringSet(constants.AllowedAdapterTransports)

const adapterEndpointColumns = "adapter_id, transport, address, command, args, env, tls_cert_path, tls_key_path, tls_ca_cert_path, tls_insecure, created_at, updated_at"

// NormalizeAdapterTransport trims whitespace, applies the default ("process" when empty)
// and verifies the transport is one of the allowed values.
func NormalizeAdapterTransport(value string) (string, error) {
	return sanitizeTransport(value)
}

func sanitizeTransport(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = constants.DefaultAdapterTransport
	}
	if _, ok := allowedAdapterTransports[value]; !ok {
		return "", fmt.Errorf("config: invalid adapter transport %q", value)
	}
	return value, nil
}

// ListAdapterEndpoints returns stored adapter endpoint definitions.
func (s *Store) ListAdapterEndpoints(ctx context.Context) ([]AdapterEndpoint, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT `+adapterEndpointColumns+`
        FROM adapter_endpoints
        ORDER BY adapter_id
    `)
	if err != nil {
		return nil, fmt.Errorf("config: list adapter endpoints: %w", err)
	}
	return scanList(rows, scanAdapterEndpoint, "config: scan adapter endpoint", "config: iterate adapter endpoints")
}

// GetAdapterEndpoint retrieves the endpoint definition for a given adapter.
func (s *Store) GetAdapterEndpoint(ctx context.Context, adapterID string) (AdapterEndpoint, error) {
	adapterID = strings.TrimSpace(adapterID)
	row := s.db.QueryRowContext(ctx, `
        SELECT `+adapterEndpointColumns+`
        FROM adapter_endpoints
        WHERE adapter_id = ?
    `, adapterID)

	endp, err := scanAdapterEndpoint(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AdapterEndpoint{}, NotFoundError{Entity: "adapter_endpoint", Key: adapterID}
		}
		return AdapterEndpoint{}, fmt.Errorf("config: get adapter endpoint %q: %w", adapterID, err)
	}
	return endp, nil
}

// UpsertAdapterEndpoint inserts or updates an endpoint definition for the adapter.
func (s *Store) UpsertAdapterEndpoint(ctx context.Context, endpoint AdapterEndpoint) error {
	if err := s.ensureWritable("upsert adapter endpoint"); err != nil {
		return err
	}

	adapterID := strings.TrimSpace(endpoint.AdapterID)
	if adapterID == "" {
		return fmt.Errorf("config: upsert adapter endpoint: adapter id required")
	}

	trans, err := NormalizeAdapterTransport(endpoint.Transport)
	if err != nil {
		return err
	}

	argsPayload, err := encodeJSON(endpoint.Args, nullWhenEmptySlice[string])
	if err != nil {
		return fmt.Errorf("config: marshal adapter args: %w", err)
	}

	envPayload, err := encodeJSON(endpoint.Env, nullWhenEmptyMap[string, string])
	if err != nil {
		return fmt.Errorf("config: marshal adapter env: %w", err)
	}

	tlsInsecure := 0
	if endpoint.TLSInsecure {
		tlsInsecure = 1
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, buildUpsertSQL(
			"adapter_endpoints",
			[]string{
				"adapter_id",
				"transport",
				"address",
				"command",
				"args",
				"env",
				"tls_cert_path",
				"tls_key_path",
				"tls_ca_cert_path",
				"tls_insecure",
			},
			[]string{"adapter_id"},
			[]string{
				"transport",
				"address",
				"command",
				"args",
				"env",
				"tls_cert_path",
				"tls_key_path",
				"tls_ca_cert_path",
				"tls_insecure",
			},
			upsertOptions{
				InsertCreatedAt: true,
				InsertUpdatedAt: true,
				UpdateUpdatedAt: true,
			},
		),
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
	if err := s.ensureWritable("remove adapter endpoint"); err != nil {
		return err
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
