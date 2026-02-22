package adapters

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/napdial"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// checkGRPCHealth is a test convenience function that dials, probes once, and
// closes. The production retry loop uses grpcHealthDial + grpcHealthProbe
// directly for connection and client reuse.
func checkGRPCHealth(ctx context.Context, addr string, tlsCfg *napdial.TLSConfig) error {
	conn, client, err := grpcHealthDial(ctx, addr, tlsCfg)
	if err != nil {
		return err
	}
	defer conn.Close()
	return grpcHealthProbe(ctx, client)
}

// checkHTTPHealth is a test convenience function that sets up a client,
// probes once, and tears down. The production retry loop uses
// httpHealthSetup + httpHealthProbe directly for client reuse.
func checkHTTPHealth(ctx context.Context, addr string, tlsCfg *napdial.TLSConfig) error {
	client, healthURL, cleanup, err := httpHealthSetup(addr, tlsCfg)
	if err != nil {
		return err
	}
	defer cleanup()
	return httpHealthProbe(ctx, client, healthURL)
}

// --- Test assertion helpers ---

// requireTerminal verifies that err is a non-nil *terminalHealthError.
func requireTerminal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var termErr *terminalHealthError
	if !errors.As(err, &termErr) {
		t.Fatalf("expected terminalHealthError, got %T: %v", err, err)
	}
}

// requireFailsFast verifies that an operation completed within 3 seconds,
// as expected for terminal errors that should not burn the readyTimeout.
func requireFailsFast(t *testing.T, elapsed time.Duration) {
	t.Helper()
	if elapsed > 3*time.Second {
		t.Fatalf("expected fail-fast (<3s), but took %v", elapsed)
	}
}

// --- Task 1: gRPC health check tests ---

func startGRPCHealthServer(t *testing.T, servingStatus healthpb.HealthCheckResponse_ServingStatus) (addr string, cleanup func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", servingStatus)
	healthpb.RegisterHealthServer(srv, healthSrv)

	go func() { _ = srv.Serve(lis) }()

	return lis.Addr().String(), func() {
		srv.GracefulStop()
	}
}

func startGRPCServerNoHealth(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer() // no health service registered

	go func() { _ = srv.Serve(lis) }()

	return lis.Addr().String(), func() {
		srv.GracefulStop()
	}
}

func TestCheckGRPCHealth_Serving(t *testing.T) {
	t.Parallel()
	addr, cleanup := startGRPCHealthServer(t, healthpb.HealthCheckResponse_SERVING)
	defer cleanup()

	err := checkGRPCHealth(context.Background(), addr, nil)
	if err != nil {
		t.Fatalf("expected nil error for SERVING, got: %v", err)
	}
}

func TestCheckGRPCHealth_NotServing(t *testing.T) {
	t.Parallel()
	addr, cleanup := startGRPCHealthServer(t, healthpb.HealthCheckResponse_NOT_SERVING)
	defer cleanup()

	err := checkGRPCHealth(context.Background(), addr, nil)
	if err == nil {
		t.Fatal("expected error for NOT_SERVING, got nil")
	}
	if !strings.Contains(err.Error(), "NOT_SERVING") {
		t.Fatalf("expected error to mention NOT_SERVING, got: %v", err)
	}
}

func TestCheckGRPCHealth_Unknown(t *testing.T) {
	t.Parallel()
	// UNKNOWN is a valid gRPC health status that adapters may return during
	// startup before transitioning to SERVING. It must be treated as a
	// retryable (non-terminal) error — the retry loop should keep probing.
	addr, cleanup := startGRPCHealthServer(t, healthpb.HealthCheckResponse_UNKNOWN)
	defer cleanup()

	err := checkGRPCHealth(context.Background(), addr, nil)
	if err == nil {
		t.Fatal("expected error for UNKNOWN, got nil")
	}
	if !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatalf("expected error to mention UNKNOWN, got: %v", err)
	}
	// UNKNOWN must NOT be terminal — adapter may transition to SERVING.
	var termErr *terminalHealthError
	if errors.As(err, &termErr) {
		t.Fatalf("UNKNOWN should be retryable (non-terminal), got terminalHealthError: %v", err)
	}
}

func TestCheckGRPCHealth_Unimplemented(t *testing.T) {
	t.Parallel()
	addr, cleanup := startGRPCServerNoHealth(t)
	defer cleanup()

	err := checkGRPCHealth(context.Background(), addr, nil)
	if err == nil {
		t.Fatal("expected error for Unimplemented, got nil")
	}
	if !strings.Contains(err.Error(), "does not implement grpc.health.v1") {
		t.Fatalf("expected Unimplemented error message, got: %v", err)
	}
}

func TestCheckGRPCHealth_InvalidAddress(t *testing.T) {
	t.Parallel()
	err := checkGRPCHealth(context.Background(), "not-a-host-port", nil)
	if err == nil {
		t.Fatal("expected error for invalid address, got nil")
	}
	if !strings.Contains(err.Error(), "invalid address") {
		t.Fatalf("expected invalid address error, got: %v", err)
	}
	requireTerminal(t, err)
}

func TestCheckGRPCHealth_InvalidAddressFailsFast(t *testing.T) {
	t.Parallel()
	// Invalid address should be terminal — fail fast, not burn readyTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := waitForAdapterReadyTransportAware(ctx, "not-a-host-port", "grpc", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for invalid gRPC address, got nil")
	}
	requireTerminal(t, err)
	requireFailsFast(t, elapsed)
}

// notFoundHealthServer implements grpc.health.v1 but returns NotFound
// for all services. This simulates an adapter that registered the health
// service but never called SetServingStatus("", ...).
type notFoundHealthServer struct {
	healthpb.UnimplementedHealthServer
}

func (s *notFoundHealthServer) Check(context.Context, *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	return nil, status.Error(codes.NotFound, "unknown service")
}

func startNotFoundGRPCServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, &notFoundHealthServer{})

	go func() { _ = srv.Serve(lis) }()

	return lis.Addr().String(), func() {
		srv.GracefulStop()
	}
}

func TestCheckGRPCHealth_NotFoundTerminal(t *testing.T) {
	t.Parallel()
	// Health service registered but no status for service="" → NotFound.
	// This is a deployment/code issue and should fail fast (terminal).
	addr, cleanup := startNotFoundGRPCServer(t)
	defer cleanup()

	err := checkGRPCHealth(context.Background(), addr, nil)
	if err == nil {
		t.Fatal("expected error for NotFound, got nil")
	}
	if !strings.Contains(err.Error(), "no status for empty service name") {
		t.Fatalf("expected terminal NotFound error, got: %v", err)
	}
}

func TestTransportAware_GRPCNotFoundFailsFast(t *testing.T) {
	t.Parallel()
	// NotFound should be terminal — fail fast, not burn the full readyTimeout.
	addr, cleanup := startNotFoundGRPCServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := waitForAdapterReadyTransportAware(ctx, addr, "grpc", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for NotFound, got nil")
	}
	if !strings.Contains(err.Error(), "no status for empty service name") {
		t.Fatalf("expected terminal NotFound error, got: %v", err)
	}
	requireFailsFast(t, elapsed)
}

func TestCheckGRPCHealth_Timeout(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping timing-sensitive test in short mode")
	}
	// Use a listener that accepts but never responds
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	// Track accepted connections for cleanup
	var connsMu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		connsMu.Lock()
		defer connsMu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})

	// Accept connections but don't serve - simulates slow server
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			connsMu.Lock()
			conns = append(conns, conn)
			connsMu.Unlock()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = checkGRPCHealth(ctx, lis.Addr().String(), nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// Verify the error is timeout/deadline-related, not some other failure.
	errStr := strings.ToLower(err.Error())
	if !strings.Contains(errStr, "deadline") && !strings.Contains(errStr, "timeout") {
		t.Fatalf("expected timeout/deadline error, got: %v", err)
	}
}

// --- Task 2: HTTP health check tests ---

func TestCheckHTTPHealth_OK(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	// Extract host:port from server URL (strip http:// prefix)
	addr := strings.TrimPrefix(srv.URL, "http://")

	err := checkHTTPHealth(context.Background(), addr, nil)
	if err != nil {
		t.Fatalf("expected nil error for HTTP 200, got: %v", err)
	}
}

func TestCheckHTTPHealth_ServiceUnavailable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")

	err := checkHTTPHealth(context.Background(), addr, nil)
	if err == nil {
		t.Fatal("expected error for HTTP 503, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected status 503") {
		t.Fatalf("expected status 503 error, got: %v", err)
	}
}

func TestCheckHTTPHealth_InternalServerError(t *testing.T) {
	t.Parallel()
	// HTTP 500 must be treated as retryable (non-terminal) — the adapter
	// may recover. Symmetric with 503 but covers a different server error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")

	err := checkHTTPHealth(context.Background(), addr, nil)
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected status 500") {
		t.Fatalf("expected status 500 error, got: %v", err)
	}
	// 500 must NOT be terminal — server may recover.
	var termErr *terminalHealthError
	if errors.As(err, &termErr) {
		t.Fatalf("HTTP 500 should be retryable (non-terminal), got terminalHealthError: %v", err)
	}
}

func TestCheckHTTPHealth_NotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")

	err := checkHTTPHealth(context.Background(), addr, nil)
	if err == nil {
		t.Fatal("expected error for HTTP 404, got nil")
	}
	if !strings.Contains(err.Error(), "does not expose /health endpoint") {
		t.Fatalf("expected /health not found error, got: %v", err)
	}
}

func TestCheckHTTPHealth_Timeout(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping timing-sensitive test in short mode")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never respond - simulate timeout
		<-r.Context().Done()
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := checkHTTPHealth(ctx, addr, nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestCheckHTTPHealth_InvalidAddress(t *testing.T) {
	t.Parallel()
	// Bare IPv6 without brackets or missing port should produce a clear error
	// instead of silently constructing a malformed URL.
	err := checkHTTPHealth(context.Background(), "::1:8080", nil)
	if err == nil {
		t.Fatal("expected error for bare IPv6 address, got nil")
	}
	if !strings.Contains(err.Error(), "invalid address") {
		t.Fatalf("expected invalid address error, got: %v", err)
	}
}

func TestCheckHTTPHealth_ScopedIPv6(t *testing.T) {
	t.Parallel()
	// Scoped IPv6 with zone ID (e.g. fe80::1%en0) must be percent-encoded
	// in the URL. Verify url.URL builder handles it without panicking.
	// The request will fail (no server), but the URL must be well-formed.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := checkHTTPHealth(ctx, "[fe80::1%25en0]:8080", nil)
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
	// The error should be a connection failure, NOT an "invalid address" error.
	// This proves the URL was constructed correctly.
	if strings.Contains(err.Error(), "invalid address") {
		t.Fatalf("scoped IPv6 should be a valid address, got: %v", err)
	}
}

func TestCheckHTTPHealth_ScopedIPv6RawZoneID(t *testing.T) {
	t.Parallel()
	// Same as above but with a raw zone ID (literal %) as it would appear
	// in config rather than the URL-encoded %25 form. net.SplitHostPort
	// handles both forms identically.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := checkHTTPHealth(ctx, "[fe80::1%en0]:8080", nil)
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
	if strings.Contains(err.Error(), "invalid address") {
		t.Fatalf("raw zone ID should be a valid address, got: %v", err)
	}
}

func TestCheckHTTPHealth_DrainLargeBody(t *testing.T) {
	t.Parallel()
	// Verify that health check succeeds when /health returns a large body.
	// The response drain (maxHealthResponseDrain=4KB via io.LimitReader)
	// should handle the body without blocking, and the TCP connection
	// should be reusable by the transport's connection pool.
	body := strings.Repeat("x", 5000) // 5KB — exceeds the 4KB drain limit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")

	err := checkHTTPHealth(context.Background(), addr, nil)
	if err != nil {
		t.Fatalf("expected nil error for HTTP 200 with large body, got: %v", err)
	}
}

func TestCheckHTTPHealth_InvalidAddressFailsFast(t *testing.T) {
	t.Parallel()
	// An invalid address format is a deterministic config error — terminal,
	// not retried for the full readyTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := waitForAdapterReadyTransportAware(ctx, "not-a-host-port", "http", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for invalid address, got nil")
	}
	if !strings.Contains(err.Error(), "invalid address") {
		t.Fatalf("expected invalid address error, got: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("invalid address should fail fast (terminal), but took %v", elapsed)
	}
}

func TestCheckHTTPHealth_TLSCertErrorFailsFast(t *testing.T) {
	t.Parallel()
	// TLS certificate errors (wrong CA, expired cert) are deterministic
	// config issues — should fail fast instead of retrying until timeout.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "https://")
	// Use TLS config that does NOT trust the test server's CA.
	// This causes x509.UnknownAuthorityError on every attempt.
	tlsCfg := &napdial.TLSConfig{
		InsecureSkipVerify: false,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := waitForAdapterReadyTransportAware(ctx, addr, "http", tlsCfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected TLS error, got nil")
	}
	// Should fail fast — well under the 30s timeout.
	if elapsed > 3*time.Second {
		t.Fatalf("TLS cert error should fail fast (terminal), but took %v", elapsed)
	}
}

// --- Per-probe timeout contract test ---

func TestCheckHTTPHealth_ProbeTimesOutAt5s(t *testing.T) {
	t.Parallel()
	// Verify that a single HTTP probe times out after ~5s (healthCheckTimeout),
	// even when the parent context has a longer deadline. The slow server
	// responds after 7s, so the probe should fail at ~5s — well before both
	// the server response and the parent context's 10s deadline.
	if testing.Short() {
		t.Skip("skipping slow test in short mode")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Respond after 7s — longer than the 5s probe timeout.
		select {
		case <-time.After(7 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := checkHTTPHealth(ctx, addr, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error from probe, got nil")
	}
	// Probe should timeout around 5s (±2s tolerance for CI).
	if elapsed < 4*time.Second {
		t.Fatalf("probe returned too early (%v) — expected ~5s timeout", elapsed)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("probe took too long (%v) — should have timed out at ~5s, not waited for server", elapsed)
	}
}

func TestCheckGRPCHealth_ProbeTimesOutAt5s(t *testing.T) {
	t.Parallel()
	// Verify that a single gRPC probe times out after ~5s (healthCheckTimeout),
	// even when the parent context has a longer deadline. The server accepts
	// connections but never responds to the Check RPC.
	if testing.Short() {
		t.Skip("skipping slow test in short mode")
	}

	// Listener that accepts connections but never serves RPCs.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	var connsMu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		connsMu.Lock()
		defer connsMu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			connsMu.Lock()
			conns = append(conns, conn)
			connsMu.Unlock()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err = checkGRPCHealth(ctx, lis.Addr().String(), nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error from probe, got nil")
	}
	// Probe should timeout around 5s (±2s tolerance for CI).
	if elapsed < 4*time.Second {
		t.Fatalf("probe returned too early (%v) — expected ~5s timeout", elapsed)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("probe took too long (%v) — should have timed out at ~5s, not waited for parent ctx", elapsed)
	}
}

// --- Task 4: Integration tests for transport-aware dispatch ---

func TestTransportAware_GRPCServing(t *testing.T) {
	t.Parallel()
	addr, cleanup := startGRPCHealthServer(t, healthpb.HealthCheckResponse_SERVING)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := waitForAdapterReadyTransportAware(ctx, addr, "grpc", nil)
	if err != nil {
		t.Fatalf("expected nil error for gRPC SERVING, got: %v", err)
	}
}

func TestTransportAware_GRPCNotServing(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping timing-sensitive test in short mode")
	}
	addr, cleanup := startGRPCHealthServer(t, healthpb.HealthCheckResponse_NOT_SERVING)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := waitForAdapterReadyTransportAware(ctx, addr, "grpc", nil)
	if err == nil {
		t.Fatal("expected error for gRPC NOT_SERVING, got nil")
	}
	if !strings.Contains(err.Error(), "NOT_SERVING") {
		t.Fatalf("expected last probe error to mention NOT_SERVING, got: %v", err)
	}
}

func TestTransportAware_GRPCUnimplemented(t *testing.T) {
	t.Parallel()
	addr, cleanup := startGRPCServerNoHealth(t)
	defer cleanup()

	// Use a long timeout to prove the terminal error fails fast
	// (does not wait for the full readyTimeout).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := waitForAdapterReadyTransportAware(ctx, addr, "grpc", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for gRPC Unimplemented, got nil")
	}
	if !strings.Contains(err.Error(), "does not implement grpc.health.v1") {
		t.Fatalf("expected Unimplemented error, got: %v", err)
	}
	requireFailsFast(t, elapsed)
}

func TestTransportAware_HTTPOK(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := waitForAdapterReadyTransportAware(ctx, addr, "http", nil)
	if err != nil {
		t.Fatalf("expected nil error for HTTP 200, got: %v", err)
	}
}

func TestTransportAware_HTTP503(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := waitForAdapterReadyTransportAware(ctx, addr, "http", nil)
	if err == nil {
		t.Fatal("expected error for HTTP 503, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected status 503") {
		t.Fatalf("expected last probe error to mention 503, got: %v", err)
	}
}

func TestTransportAware_HTTP404NoHealth(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	// Use a long timeout to prove the terminal error fails fast.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := waitForAdapterReadyTransportAware(ctx, addr, "http", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for HTTP 404, got nil")
	}
	if !strings.Contains(err.Error(), "does not expose /health endpoint") {
		t.Fatalf("expected /health not found error, got: %v", err)
	}
	requireFailsFast(t, elapsed)
}

func TestTransportAware_Timeout(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping timing-sensitive test in short mode")
	}
	// Test that health check respects timeout (>5s per AC#3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := waitForAdapterReadyTransportAware(ctx, addr, "http", nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestTransportAware_ProcessUsesTCPDial(t *testing.T) {
	t.Parallel()
	// Start a TCP listener (simulates a process adapter that is ready)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	// Process transport should use TCP dial, not health check
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = waitForAdapterReadyTransportAware(ctx, lis.Addr().String(), "process", nil)
	if err != nil {
		t.Fatalf("expected nil error for process TCP dial, got: %v", err)
	}
}

func TestTransportAware_ProcessTCPDialFailure(t *testing.T) {
	t.Parallel()
	// Use an address that nothing is listening on
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := waitForAdapterReadyTransportAware(ctx, "127.0.0.1:1", "process", nil)
	if err == nil {
		t.Fatal("expected error for unreachable process, got nil")
	}
}

func TestWaitForAdapterReady_TimeoutErrorFormat(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping timing-sensitive test in short mode")
	}
	// Verify the TCP dial timeout error message includes "process readiness"
	// prefix and the dial count (parity with health check retry error format).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := waitForAdapterReady(ctx, "127.0.0.1:1")
	if err == nil {
		t.Fatal("expected error for unreachable address, got nil")
	}
	if !strings.Contains(err.Error(), "process readiness for") {
		t.Fatalf("expected error to contain 'process readiness for', got: %v", err)
	}
	// Should include the dial count (plural "dials" since multiple attempts).
	if !strings.Contains(err.Error(), "dial") {
		t.Fatalf("expected error to contain dial count, got: %v", err)
	}
}

func TestWaitForAdapterReady_InvalidAddressFailsFast(t *testing.T) {
	t.Parallel()
	// Malformed address should fail immediately (pre-validation via
	// net.SplitHostPort) instead of burning the full readyTimeout
	// with repeated dial parse errors. Parity with grpcHealthDial
	// and httpHealthSetup which both pre-validate addresses.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := waitForAdapterReady(ctx, "not-a-host-port")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for invalid address, got nil")
	}
	if !strings.Contains(err.Error(), "invalid address") {
		t.Fatalf("expected invalid address error, got: %v", err)
	}
	requireFailsFast(t, elapsed)
}

func TestTransportAware_UnknownTransportReturnsError(t *testing.T) {
	t.Parallel()
	// Unknown transport types should return a terminal error, not silently fall back to TCP dial.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	err = waitForAdapterReadyTransportAware(context.Background(), lis.Addr().String(), "websocket", nil)
	if err == nil {
		t.Fatal("expected error for unknown transport, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported transport") {
		t.Fatalf("expected unsupported transport error, got: %v", err)
	}
	requireTerminal(t, err)
}

// --- Retry loop tests ---

func TestTransportAware_ContextCanceledMidRetry(t *testing.T) {
	t.Parallel()
	// Verify the retry loop exits cleanly on context.Canceled (not just
	// context.DeadlineExceeded). The server always returns 503 so the
	// loop retries until the context is canceled.
	probes := make(chan struct{}, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case probes <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after at least two probes (first probe + one retry) to ensure
	// the retry loop has executed. Channel-based sync avoids timing assumptions.
	go func() {
		for i := 0; i < 2; i++ {
			select {
			case <-probes:
			case <-time.After(5 * time.Second):
				cancel()
				return
			}
		}
		cancel()
	}()

	err := waitForAdapterReadyTransportAware(ctx, addr, "http", nil)
	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled in error chain, got: %v", err)
	}
	// The error message should mention the last meaningful health check
	// failure (503), not redundantly say "context canceled" as the last error.
	if strings.Contains(err.Error(), "last: context canceled") {
		t.Fatalf("expected last error to be the health check failure, not context.Canceled: %v", err)
	}
	if !strings.Contains(err.Error(), "unexpected status 503") {
		t.Fatalf("expected last probe error (503) in the message, got: %v", err)
	}
}

func TestTransportAware_HTTPRetryUntilHealthy(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping timing-sensitive retry test in short mode")
	}
	// Adapter starts returning 503, then becomes healthy after 600ms.
	// The retry loop should poll and eventually succeed within the 3s timeout.
	var mu sync.Mutex
	healthy := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := healthy
		mu.Unlock()
		if r.URL.Path == "/health" && h {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	// Flip to healthy after 600ms
	go func() {
		time.Sleep(600 * time.Millisecond)
		mu.Lock()
		healthy = true
		mu.Unlock()
	}()

	addr := strings.TrimPrefix(srv.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := waitForAdapterReadyTransportAware(ctx, addr, "http", nil)
	if err != nil {
		t.Fatalf("expected nil after adapter becomes healthy, got: %v", err)
	}
}

func TestTransportAware_GRPCRetryUntilServing(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping timing-sensitive retry test in short mode")
	}
	// Start gRPC health server as NOT_SERVING, flip to SERVING after 600ms.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	healthpb.RegisterHealthServer(srv, healthSrv)

	go func() { _ = srv.Serve(lis) }()
	defer srv.GracefulStop()

	// Flip to SERVING after 600ms
	go func() {
		time.Sleep(600 * time.Millisecond)
		healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = waitForAdapterReadyTransportAware(ctx, lis.Addr().String(), "grpc", nil)
	if err != nil {
		t.Fatalf("expected nil after adapter becomes SERVING, got: %v", err)
	}
}

func TestCheckHTTPHealth_RedirectNotFollowed(t *testing.T) {
	t.Parallel()
	// Health endpoint that redirects should NOT be treated as healthy.
	// AC#2 requires /health itself to return 200, not a redirect target.
	// All redirects (3xx) from /health are terminal errors — a properly
	// configured health endpoint never redirects.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			http.Redirect(w, r, "/other", http.StatusFound)
			return
		}
		if r.URL.Path == "/other" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	err := checkHTTPHealth(context.Background(), addr, nil)
	requireTerminal(t, err)
	if !strings.Contains(err.Error(), "redirect 302") {
		t.Fatalf("expected redirect 302 error, got: %v", err)
	}
}

func TestCheckHTTPHealth_AllRedirectsTerminal(t *testing.T) {
	t.Parallel()
	// All 3xx redirects from /health are terminal — a properly configured
	// health endpoint never redirects, whether permanently or temporarily.
	for _, code := range []int{
		http.StatusMovedPermanently,  // 301
		http.StatusFound,             // 302
		http.StatusSeeOther,          // 303
		http.StatusTemporaryRedirect, // 307
		http.StatusPermanentRedirect, // 308
	} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/other", code)
			}))
			defer srv.Close()

			addr := strings.TrimPrefix(srv.URL, "http://")

			// Use a long timeout to prove the terminal error fails fast.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			start := time.Now()
			err := waitForAdapterReadyTransportAware(ctx, addr, "http", nil)
			elapsed := time.Since(start)

			requireTerminal(t, err)
			if !strings.Contains(err.Error(), fmt.Sprintf("redirect %d", code)) {
				t.Fatalf("expected redirect %d error, got: %v", code, err)
			}
			requireFailsFast(t, elapsed)
		})
	}
}

// --- TLS config construction error tests ---

func TestCheckHTTPHealth_TLSConfigErrorFailsFast(t *testing.T) {
	t.Parallel()
	// Partial mTLS (CertPath without KeyPath) causes BuildStdTLSConfig to fail.
	// httpHealthSetup must classify this as terminal so the retry loop fails fast.
	tlsCfg := &napdial.TLSConfig{
		CertPath: "/some/cert.pem",
		// KeyPath intentionally omitted — triggers partial mTLS error
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := waitForAdapterReadyTransportAware(ctx, "127.0.0.1:9999", "http", tlsCfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected TLS config error, got nil")
	}
	requireTerminal(t, err)
	if !strings.Contains(err.Error(), "tls config") {
		t.Fatalf("expected error to mention tls config, got: %v", err)
	}
	requireFailsFast(t, elapsed)
}

func TestCheckGRPCHealth_DialOptionsErrorFailsFast(t *testing.T) {
	t.Parallel()
	// Partial mTLS (CertPath without KeyPath) causes napdial.DialOptions to fail.
	// grpcHealthDial must classify this as terminal so the retry loop fails fast.
	tlsCfg := &napdial.TLSConfig{
		CertPath: "/some/cert.pem",
		// KeyPath intentionally omitted — triggers partial mTLS error
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := waitForAdapterReadyTransportAware(ctx, "127.0.0.1:9999", "grpc", tlsCfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected dial options error, got nil")
	}
	requireTerminal(t, err)
	if !strings.Contains(err.Error(), "dial options") {
		t.Fatalf("expected error to mention dial options, got: %v", err)
	}
	requireFailsFast(t, elapsed)
}

// --- TLS AlertError tests (Go 1.21+) ---

func TestIsTerminalTLSAlert_AllCodes(t *testing.T) {
	t.Parallel()
	// Table-driven test covering all 8 terminal alert codes from
	// isTerminalTLSAlert plus representative non-terminal codes.
	tests := []struct {
		code     uint8
		name     string
		terminal bool
	}{
		// Terminal alerts (RFC 5246 Section 7.2.2 certificate/auth failures)
		{42, "bad_certificate", true},
		{43, "unsupported_certificate", true},
		{44, "certificate_revoked", true},
		{45, "certificate_expired", true},
		{46, "certificate_unknown", true},
		{48, "unknown_ca", true},
		{49, "access_denied", true},
		{71, "insufficient_security", true},
		// Non-terminal alerts
		{0, "close_notify", false},
		{10, "unexpected_message", false},
		{20, "bad_record_mac", false},
		{40, "handshake_failure", false},
		{50, "decode_error", false},
		{70, "protocol_version", false},
		{80, "internal_error", false},
		{90, "user_canceled", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := fmt.Errorf("wrapped: %w", tls.AlertError(tt.code))
			got := isTerminalTLSError(err)
			if got != tt.terminal {
				t.Fatalf("isTerminalTLSError(AlertError(%d) %s) = %v, want %v", tt.code, tt.name, got, tt.terminal)
			}
		})
	}
}

// --- TLS health check tests ---

// generateTestServerTLS creates a self-signed TLS certificate for localhost
// and returns a *tls.Config suitable for a test server.
func generateTestServerTLS(t *testing.T) *tls.Config {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{derBytes},
			PrivateKey:  key,
		}},
	}
}

// generateTestCA creates a self-signed CA and a server certificate signed by
// that CA. Returns the server *tls.Config and the path to the PEM-encoded CA
// cert file (written to t.TempDir). This allows tests to verify health checks
// with InsecureSkipVerify=false and a properly trusted CA chain.
func generateTestCA(t *testing.T) (serverTLS *tls.Config, caCertPath string) {
	t.Helper()

	// Generate CA key pair
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	// Write CA cert PEM to temp file for napdial.TLSConfig.CACertPath
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}

	// Generate server key pair signed by CA
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		BasicConstraintsValid: true,
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{serverDER},
			PrivateKey:  serverKey,
		}},
	}, caPath
}

func TestCheckHTTPHealth_TLSWithCAValidation(t *testing.T) {
	t.Parallel()
	// Positive TLS test with InsecureSkipVerify=false and a trusted CA.
	// Proves the health check works with proper certificate verification,
	// unlike TestCheckHTTPHealth_TLS which uses InsecureSkipVerify=true.
	serverTLS, caPath := generateTestCA(t)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	srv.TLS = serverTLS
	srv.StartTLS()
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "https://")
	tlsCfg := &napdial.TLSConfig{
		CACertPath:         caPath,
		InsecureSkipVerify: false,
	}

	err := checkHTTPHealth(context.Background(), addr, tlsCfg)
	if err != nil {
		t.Fatalf("expected nil error for HTTPS 200 with CA validation, got: %v", err)
	}
}

func TestCheckGRPCHealth_TLSWithCAValidation(t *testing.T) {
	t.Parallel()
	// Positive TLS test with InsecureSkipVerify=false and a trusted CA.
	// Proves gRPC health check works with proper certificate verification,
	// unlike TestCheckGRPCHealth_TLS which uses InsecureSkipVerify=true.
	serverTLS, caPath := generateTestCA(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, healthSrv)

	go func() { _ = srv.Serve(lis) }()
	defer srv.GracefulStop()

	tlsCfg := &napdial.TLSConfig{
		CACertPath:         caPath,
		InsecureSkipVerify: false,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = checkGRPCHealth(ctx, lis.Addr().String(), tlsCfg)
	if err != nil {
		t.Fatalf("expected nil error for gRPC SERVING over TLS with CA validation, got: %v", err)
	}
}

func TestCheckHTTPHealth_TLS(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "https://")
	tlsCfg := &napdial.TLSConfig{InsecureSkipVerify: true}

	err := checkHTTPHealth(context.Background(), addr, tlsCfg)
	if err != nil {
		t.Fatalf("expected nil error for HTTPS 200, got: %v", err)
	}
}

func TestCheckGRPCHealth_TLSCertErrorFailsFast(t *testing.T) {
	t.Parallel()
	// TLS certificate errors (wrong CA, expired cert) are deterministic
	// config issues — should fail fast instead of retrying until timeout.
	// Symmetric with TestCheckHTTPHealth_TLSCertErrorFailsFast.
	serverTLS := generateTestServerTLS(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, healthSrv)

	go func() { _ = srv.Serve(lis) }()
	defer srv.GracefulStop()

	// Use TLS config that does NOT trust the test server's self-signed CA.
	// This causes x509.UnknownAuthorityError on every attempt.
	tlsCfg := &napdial.TLSConfig{
		InsecureSkipVerify: false,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err = waitForAdapterReadyTransportAware(ctx, lis.Addr().String(), "grpc", tlsCfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected TLS error, got nil")
	}
	// Should fail fast — well under the 30s timeout.
	if elapsed > 3*time.Second {
		t.Fatalf("TLS cert error should fail fast (terminal), but took %v", elapsed)
	}
}

func TestCheckGRPCHealth_TLS(t *testing.T) {
	t.Parallel()
	serverTLS := generateTestServerTLS(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, healthSrv)

	go func() { _ = srv.Serve(lis) }()
	defer srv.GracefulStop()

	tlsCfg := &napdial.TLSConfig{InsecureSkipVerify: true}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = checkGRPCHealth(ctx, lis.Addr().String(), tlsCfg)
	if err != nil {
		t.Fatalf("expected nil error for gRPC SERVING over TLS, got: %v", err)
	}
}

// --- TLS protocol mismatch tests (tls.RecordHeaderError) ---

func TestCheckHTTPHealth_TLSToPlainHTTPFailsFast(t *testing.T) {
	t.Parallel()
	// TLS client connecting to a plain-HTTP server triggers tls.RecordHeaderError.
	// This is a deterministic config error — should fail fast (terminal).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	// Enable TLS on client side — server is plain HTTP → protocol mismatch.
	tlsCfg := &napdial.TLSConfig{InsecureSkipVerify: true}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := waitForAdapterReadyTransportAware(ctx, addr, "http", tlsCfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected TLS protocol mismatch error, got nil")
	}
	requireTerminal(t, err)
	requireFailsFast(t, elapsed)
}

func TestCheckGRPCHealth_TLSToPlainGRPCFailsFast(t *testing.T) {
	t.Parallel()
	// TLS client connecting to a plain gRPC server triggers a TLS handshake
	// failure (authentication handshake failed or tls.RecordHeaderError).
	// Should fail fast as terminal.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer() // plain, no TLS
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, healthSrv)

	go func() { _ = srv.Serve(lis) }()
	defer srv.GracefulStop()

	// Enable TLS on client side — server is plain gRPC → mismatch.
	tlsCfg := &napdial.TLSConfig{InsecureSkipVerify: true}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err = waitForAdapterReadyTransportAware(ctx, lis.Addr().String(), "grpc", tlsCfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected TLS protocol mismatch error, got nil")
	}
	requireTerminal(t, err)
	requireFailsFast(t, elapsed)
}

// --- FRAGILE string detection regression tests ---
// These tests verify that the specific string-based error detection paths
// used as secondary detection for TLS/protocol errors continue to match
// the strings produced by the Go standard library and gRPC. If a Go or
// gRPC upgrade changes these strings, these tests will fail — alerting us
// to update the string checks (or rely solely on the typed error backup).

func TestFragileStringDetection_GRPCAuthHandshakeFailed(t *testing.T) {
	t.Parallel()
	// Verify that gRPC wraps TLS handshake failures as codes.Unavailable
	// with "authentication handshake failed" in the description. This is
	// the string checked in grpcHealthProbe (FRAGILE comment, health.go).
	//
	// Setup: TLS client → plain gRPC server → handshake mismatch.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer() // plain, no TLS
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, healthSrv)

	go func() { _ = srv.Serve(lis) }()
	defer srv.GracefulStop()

	// Use TLS client against plain server.
	tlsCfg := &napdial.TLSConfig{InsecureSkipVerify: true}
	err = checkGRPCHealth(context.Background(), lis.Addr().String(), tlsCfg)
	if err == nil {
		t.Fatal("expected error for TLS-to-plain gRPC, got nil")
	}

	// The error must be terminal (detected by either string or typed check).
	requireTerminal(t, err)

	// Verify the gRPC error contains the expected status code and message.
	// This is the FRAGILE string. If this assertion fails after a gRPC
	// upgrade, update the string in grpcHealthProbe accordingly.
	st, ok := status.FromError(errors.Unwrap(err))
	if ok && st.Code() == codes.Unavailable {
		if !strings.Contains(st.Message(), "authentication handshake failed") {
			t.Fatalf("FRAGILE string changed: gRPC Unavailable error no longer contains 'authentication handshake failed': %s\n"+
				"Update the string check in grpcHealthProbe. Fail-fast still works via isTerminalTLSError backup.", st.Message())
		}
	}
}

func TestFragileStringDetection_HTTPServerGaveHTTPResponse(t *testing.T) {
	t.Parallel()
	// Verify that Go's HTTP client produces an error containing
	// "server gave HTTP response to HTTPS client" when an HTTPS client
	// connects to a plain HTTP server. This is the string checked in
	// httpHealthProbe (FRAGILE comment, health.go).
	//
	// Setup: HTTPS client → plain HTTP server → protocol mismatch.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	// Enable TLS on client side — server is plain HTTP → mismatch.
	tlsCfg := &napdial.TLSConfig{InsecureSkipVerify: true}

	err := checkHTTPHealth(context.Background(), addr, tlsCfg)
	if err == nil {
		t.Fatal("expected error for HTTPS-to-plain-HTTP, got nil")
	}

	// The error must be terminal (detected by either string or typed check).
	requireTerminal(t, err)

	// Check if the specific FRAGILE string is present. If this assertion
	// fails after a Go upgrade, update the string in httpHealthProbe.
	errMsg := err.Error()
	hasRecordHeaderError := strings.Contains(errMsg, "tls:") // typed tls.RecordHeaderError path
	hasStringDetection := strings.Contains(errMsg, "server gave HTTP response to HTTPS client")
	if !hasRecordHeaderError && !hasStringDetection {
		t.Fatalf("FRAGILE string changed: neither tls.RecordHeaderError nor 'server gave HTTP response to HTTPS client' found in: %s\n"+
			"Update the string check in httpHealthProbe. Terminal detection still works via typed error backup.", errMsg)
	}
}
