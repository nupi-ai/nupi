package adapters

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/napdial"
)

const (
	defaultHeartbeatInterval = constants.Duration30Seconds // AC#2: configurable, default 30s

	// heartbeatProbeTimeout is the TCP dial timeout for process-transport probes.
	// Aliases healthCheckTimeout (health.go) so all transport probes share a
	// single 5s value — changing it in one place updates all probe paths.
	heartbeatProbeTimeout = healthCheckTimeout
)

// probeAdapter performs a single-shot health probe against a running adapter.
// Returns nil when the adapter responds successfully within the timeout.
func probeAdapter(ctx context.Context, binding Binding) error {
	transport := binding.Runtime[RuntimeExtraTransport]
	addr := binding.Runtime[RuntimeExtraAddress]

	if transport == "" {
		return fmt.Errorf("adapter %s has no transport configured", binding.AdapterID)
	}

	switch transport {
	case "builtin":
		return nil // In-process mock, always healthy.
	case "grpc":
		tlsCfg, err := buildTLSConfig(binding)
		if err != nil {
			return err
		}
		return probeGRPC(ctx, addr, tlsCfg)
	case "http":
		tlsCfg, err := buildTLSConfig(binding)
		if err != nil {
			return err
		}
		return probeHTTP(ctx, addr, tlsCfg)
	case "process":
		return probeTCP(ctx, addr)
	default:
		return fmt.Errorf("unsupported transport %q for adapter %s", transport, binding.AdapterID)
	}
}

// probeGRPC dials and probes a gRPC adapter. The 5s timeout is managed
// internally by grpcHealthDial/grpcHealthProbe (healthCheckTimeout), so no
// additional wrapper timeout is needed here. Error messages from
// grpcHealthDial and grpcHealthProbe already include address context.
//
// NOTE: Creates a fresh connection per call (no pooling). This is intentional —
// heartbeat fires every 30s and a fresh dial detects stale/half-open connections
// that a pooled connection would mask. Acceptable overhead for ≤10 adapters;
// revisit with connection caching if adapter count grows significantly.
func probeGRPC(ctx context.Context, addr string, tlsCfg *napdial.TLSConfig) error {
	conn, client, err := grpcHealthDial(ctx, addr, tlsCfg)
	if err != nil {
		return err
	}
	defer conn.Close()

	return grpcHealthProbe(ctx, client)
}

// probeHTTP sets up and probes an HTTP adapter. The 5s timeout is managed
// internally by httpHealthProbe (healthCheckTimeout), so no additional wrapper
// timeout is needed here. Error messages from httpHealthSetup and
// httpHealthProbe already include address/URL context.
func probeHTTP(ctx context.Context, addr string, tlsCfg *napdial.TLSConfig) error {
	client, healthURL, cleanup, err := httpHealthSetup(addr, tlsCfg)
	if err != nil {
		return err
	}
	defer cleanup()

	return httpHealthProbe(ctx, client, healthURL)
}

func probeTCP(ctx context.Context, addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("tcp dial: invalid address %q: %w", addr, err)
	}
	conn, err := (&net.Dialer{Timeout: heartbeatProbeTimeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("tcp dial %s: %w", addr, err)
	}
	conn.Close()
	return nil
}

func buildTLSConfig(binding Binding) (*napdial.TLSConfig, error) {
	certPath := binding.Runtime[RuntimeExtraTLSCertPath]
	keyPath := binding.Runtime[RuntimeExtraTLSKeyPath]
	caPath := binding.Runtime[RuntimeExtraTLSCACertPath]
	insecure := strings.EqualFold(binding.Runtime[RuntimeExtraTLSInsecure], "true")

	if certPath == "" && keyPath == "" && caPath == "" && !insecure {
		return nil, nil
	}
	// Client cert requires both cert and key — fail fast with a clear message
	// instead of letting the TLS handshake produce a cryptic error.
	if (certPath != "") != (keyPath != "") {
		return nil, fmt.Errorf("incomplete TLS client cert for adapter %s: cert_path=%q key_path=%q (both required)", binding.AdapterID, certPath, keyPath)
	}
	return &napdial.TLSConfig{
		CertPath:           certPath,
		KeyPath:            keyPath,
		CACertPath:         caPath,
		InsecureSkipVerify: insecure,
	}, nil
}

// heartbeatProbeFn is the package-level function variable for testing.
var heartbeatProbeFn = probeAdapter
var heartbeatProbeMu sync.RWMutex

// SetHeartbeatProber allows tests to override the heartbeat probe function.
// Call the returned function to restore the original behavior.
func SetHeartbeatProber(fn func(ctx context.Context, binding Binding) error) func() {
	heartbeatProbeMu.Lock()
	original := heartbeatProbeFn
	heartbeatProbeFn = fn
	heartbeatProbeMu.Unlock()
	return func() {
		heartbeatProbeMu.Lock()
		heartbeatProbeFn = original
		heartbeatProbeMu.Unlock()
	}
}

func getHeartbeatProber() func(ctx context.Context, binding Binding) error {
	heartbeatProbeMu.RLock()
	fn := heartbeatProbeFn
	heartbeatProbeMu.RUnlock()
	return fn
}
