// Package napdial provides shared gRPC dialing utilities for NAP adapter clients.
//
// All NAP adapter clients (AI, STT, TTS, VAD) use this package to establish
// gRPC connections. This centralizes transport credential configuration
// including TLS for remote/tunnel adapter deployments.
//
// Process transport (localhost) continues to work without TLS when no
// TLSConfig is provided.
package napdial

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/nupi-ai/nupi/internal/tlswarn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// TLSConfig holds TLS parameters for connecting to a remote NAP adapter.
type TLSConfig struct {
	CertPath           string // client cert path (mTLS)
	KeyPath            string // client key path (mTLS)
	CACertPath         string // CA bundle to verify server
	ServerName         string // SNI override
	InsecureSkipVerify bool   // dev only — skip server cert verification
}

// QoSConfig holds quality-of-service parameters for gRPC connections.
type QoSConfig struct {
	KeepaliveTime    time.Duration // interval between keepalive pings (default 30s)
	KeepaliveTimeout time.Duration // timeout waiting for ping ack (default 10s)
}

// DefaultQoS returns sensible QoS defaults for NAP adapter connections.
func DefaultQoS() *QoSConfig {
	return &QoSConfig{
		KeepaliveTime:    30 * time.Second,
		KeepaliveTimeout: 10 * time.Second,
	}
}

// TLSConfigFromFields constructs a TLSConfig from individual field values.
// Returns nil when all paths are empty and insecure is false (process transport).
func TLSConfigFromFields(certPath, keyPath, caCertPath string, insecureSkipVerify bool) *TLSConfig {
	if certPath == "" && keyPath == "" && caCertPath == "" && !insecureSkipVerify {
		return nil
	}
	return &TLSConfig{
		CertPath:           certPath,
		KeyPath:            keyPath,
		CACertPath:         caCertPath,
		InsecureSkipVerify: insecureSkipVerify,
	}
}

// buildTLSConfig converts TLSConfig into a stdlib *tls.Config.
func (t *TLSConfig) buildTLSConfig() (*tls.Config, error) {
	// Validate partial mTLS: both cert and key must be provided, or neither.
	if (t.CertPath != "") != (t.KeyPath != "") {
		return nil, fmt.Errorf("mTLS requires both CertPath and KeyPath (got cert=%q, key=%q)", t.CertPath, t.KeyPath)
	}

	if t.InsecureSkipVerify {
		tlswarn.LogInsecure()
		cfg := &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ServerName:         t.ServerName,
			InsecureSkipVerify: true, //nolint:gosec // dev-only flag
		}
		// Still load client certificates for mTLS identity even in insecure mode.
		if t.CertPath != "" && t.KeyPath != "" {
			cert, err := tls.LoadX509KeyPair(t.CertPath, t.KeyPath)
			if err != nil {
				return nil, err
			}
			cfg.Certificates = []tls.Certificate{cert}
		}
		return cfg, nil
	}

	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: t.ServerName,
	}

	// Load CA cert pool
	if t.CACertPath != "" {
		caPEM, err := os.ReadFile(t.CACertPath)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, &os.PathError{Op: "parse", Path: t.CACertPath, Err: os.ErrInvalid}
		}
		cfg.RootCAs = pool
	}

	// Load client cert + key for mTLS
	if t.CertPath != "" && t.KeyPath != "" {
		cert, err := tls.LoadX509KeyPair(t.CertPath, t.KeyPath)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

type dialerContextKey struct{}

// ContextWithDialer attaches a custom dialer to the context.
// This is primarily for tests using bufconn without real network sockets.
func ContextWithDialer(ctx context.Context, dialer func(context.Context, string) (net.Conn, error)) context.Context {
	if ctx == nil || dialer == nil {
		return ctx
	}
	return context.WithValue(ctx, dialerContextKey{}, dialer)
}

// DialerFromContext extracts a custom dialer from the context, if present.
func DialerFromContext(ctx context.Context) func(context.Context, string) (net.Conn, error) {
	if ctx == nil {
		return nil
	}
	dialer, _ := ctx.Value(dialerContextKey{}).(func(context.Context, string) (net.Conn, error))
	return dialer
}

// DialOptions returns gRPC dial options for connecting to a NAP adapter.
//
// When tlsCfg is nil the connection uses insecure credentials (suitable for
// process transport over localhost). When tlsCfg is non-nil, TLS credentials
// are built and any configuration error is returned — there is no silent
// fallback to insecure.
//
// When qos is non-nil and KeepaliveTime > 0, keepalive parameters are added.
func DialOptions(ctx context.Context, tlsCfg *TLSConfig, qos *QoSConfig) ([]grpc.DialOption, error) {
	var opts []grpc.DialOption

	if tlsCfg != nil {
		stdTLS, err := tlsCfg.buildTLSConfig()
		if err != nil {
			return nil, fmt.Errorf("napdial: build TLS config: %w", err)
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(stdTLS)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	if qos != nil && qos.KeepaliveTime > 0 {
		opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                qos.KeepaliveTime,
			Timeout:             qos.KeepaliveTimeout,
			PermitWithoutStream: true,
		}))
	}

	if dialer := DialerFromContext(ctx); dialer != nil {
		opts = append(opts, grpc.WithContextDialer(dialer))
	}

	return opts, nil
}
