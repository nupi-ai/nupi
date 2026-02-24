package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// GetTransportConfig loads transport-related settings for the active profile.
func (s *Store) GetTransportConfig(ctx context.Context) (TransportConfig, error) {
	settings, err := s.LoadSettings(ctx,
		"transport.http_port",
		"transport.binding",
		"transport.tls_cert_path",
		"transport.tls_key_path",
		"transport.allowed_origins",
		"transport.grpc_port",
		"transport.grpc_binding",
	)
	if err != nil {
		return TransportConfig{}, err
	}

	cfg := TransportConfig{
		Binding:        "loopback",
		AllowedOrigins: []string{},
		GRPCBinding:    "",
	}

	if portStr := settings["transport.http_port"]; portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return TransportConfig{}, fmt.Errorf("config: parse transport.http_port: %w", err)
		}
		cfg.Port = port
	}

	if binding := settings["transport.binding"]; binding != "" {
		cfg.Binding = binding
	}
	cfg.TLSCertPath = settings["transport.tls_cert_path"]
	cfg.TLSKeyPath = settings["transport.tls_key_path"]

	if originsJSON, ok := settings["transport.allowed_origins"]; ok && originsJSON != "" {
		origins, err := DecodeJSON[[]string](sql.NullString{String: originsJSON, Valid: true})
		if err != nil {
			return TransportConfig{}, fmt.Errorf("config: parse transport.allowed_origins: %w", err)
		}
		cfg.AllowedOrigins = origins
	}

	if grpcPortStr := settings["transport.grpc_port"]; grpcPortStr != "" {
		port, err := strconv.Atoi(grpcPortStr)
		if err != nil {
			return TransportConfig{}, fmt.Errorf("config: parse transport.grpc_port: %w", err)
		}
		cfg.GRPCPort = port
	}

	if grpcBinding := settings["transport.grpc_binding"]; grpcBinding != "" {
		cfg.GRPCBinding = grpcBinding
	}
	if strings.TrimSpace(cfg.GRPCBinding) == "" {
		cfg.GRPCBinding = cfg.Binding
	}

	return cfg, nil
}

// SaveTransportConfig persists the provided transport configuration.
func (s *Store) SaveTransportConfig(ctx context.Context, cfg TransportConfig) error {
	originsJSON, _, err := encodeJSONString(cfg.AllowedOrigins, nil)
	if err != nil {
		return fmt.Errorf("config: marshal transport.allowed_origins: %w", err)
	}

	values := map[string]string{
		"transport.http_port":       strconv.Itoa(cfg.Port),
		"transport.binding":         cfg.Binding,
		"transport.tls_cert_path":   cfg.TLSCertPath,
		"transport.tls_key_path":    cfg.TLSKeyPath,
		"transport.allowed_origins": originsJSON,
		"transport.grpc_port":       strconv.Itoa(cfg.GRPCPort),
		"transport.grpc_binding":    cfg.GRPCBinding,
	}

	return s.SaveSettings(ctx, values)
}
