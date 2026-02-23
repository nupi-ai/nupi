package adapters

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/napdial"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// healthCheckTimeout is the timeout for a single health check probe (AC#3).
// This is NOT the overall readiness timeout — it applies to one gRPC/HTTP call.
// When the parent context has a shorter deadline, the probe respects the
// shorter deadline (context.WithTimeout inherits the earlier expiry).
const healthCheckTimeout = constants.Duration5Seconds

// healthCheckPollInterval is the delay between retry attempts when a remote
// adapter health check fails but the overall readiness timeout has not expired.
// This is 5x longer than the 100ms TCP dial poll interval in waitForAdapterReady
// because gRPC/HTTP health check RPCs are heavier than bare TCP dials and
// failing adapters benefit from a longer back-off to avoid hammering.
// Intentionally constant (not configurable) — adaptive backoff is unnecessary
// given the short overall readiness timeout (typically 30s).
const healthCheckPollInterval = constants.Duration500Milliseconds

// maxHealthResponseDrain is the maximum number of bytes drained from HTTP
// health check response bodies before closing. Draining allows the underlying
// TCP connection to be reused by the transport's connection pool across retry
// probes. Health endpoint responses are typically tiny; the limit prevents
// unbounded reads from a misbehaving server.
const maxHealthResponseDrain = 4096 // 4KB

// terminalHealthError marks health check failures that are permanent and should
// not be retried. Examples: gRPC Unimplemented (adapter lacks grpc.health.v1),
// HTTP 404 (adapter lacks /health endpoint). These indicate a code/deployment
// issue that won't resolve by waiting.
type terminalHealthError struct {
	msg   string
	cause error
}

func (e *terminalHealthError) Error() string { return e.msg }
func (e *terminalHealthError) Unwrap() error { return e.cause }

// grpcHealthDial creates a lazy gRPC connection and health client configured
// for health checking. The actual TCP dial happens on the first RPC call,
// not here. The caller must close the returned connection.
//
// The ctx parameter is passed to napdial.DialOptions (which may extract a
// custom dialer from context); it does NOT control the TCP dial lifetime.
// gRPC's lazy connection model defers the actual dial to grpcHealthProbe's
// context.WithTimeout(ctx, healthCheckTimeout), which inherently bounds the
// dial time. The HTTP path adds a DialContext timeout as defense-in-depth
// because net/http.Transport's default net.Dialer has no timeout and relies
// solely on the request context deadline.
func grpcHealthDial(ctx context.Context, addr string, tlsCfg *napdial.TLSConfig) (*grpc.ClientConn, healthpb.HealthClient, error) {
	// Pre-validate address format (same check as HTTP path) so malformed
	// endpoints fail fast with an actionable message instead of surfacing
	// as repeated Unavailable/timeout errors in the retry loop.
	if _, _, splitErr := net.SplitHostPort(addr); splitErr != nil {
		return nil, nil, &terminalHealthError{msg: fmt.Sprintf("grpc health check: invalid address %q: %v", addr, splitErr), cause: splitErr}
	}
	dialOpts, err := napdial.DialOptions(ctx, tlsCfg, nil)
	if err != nil {
		return nil, nil, &terminalHealthError{msg: fmt.Sprintf("grpc health check: dial options: %v", err), cause: err}
	}
	// Override gRPC's default exponential backoff (1s base, 2x multiplier,
	// 120s max) for health check connections. The default can cause probes
	// to stall when the server is temporarily unreachable — gRPC waits for
	// its internal backoff before attempting a new connection, even though
	// our healthCheckPollInterval fires every 500ms. A flat 500ms backoff
	// aligns gRPC's reconnection with our polling rhythm.
	dialOpts = append(dialOpts, grpc.WithConnectParams(grpc.ConnectParams{
		Backoff: backoff.Config{
			BaseDelay:  healthCheckPollInterval,
			Multiplier: 1.0,
			Jitter:     0.2,
			MaxDelay:   healthCheckPollInterval,
		},
		MinConnectTimeout: healthCheckTimeout,
	}))
	conn, err := grpc.NewClient(napdial.PassthroughPrefix+addr, dialOpts...)
	if err != nil {
		return nil, nil, &terminalHealthError{msg: fmt.Sprintf("grpc health check: connect: %v", err), cause: err}
	}
	return conn, healthpb.NewHealthClient(conn), nil
}

// grpcHealthProbe performs a single gRPC health check RPC using a pre-built client.
// Returns nil only when the server responds with SERVING status.
func grpcHealthProbe(ctx context.Context, client healthpb.HealthClient) error {
	checkCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	resp, err := client.Check(checkCtx, &healthpb.HealthCheckRequest{Service: ""})
	if err != nil {
		st, ok := status.FromError(err)
		if ok {
			switch st.Code() {
			case codes.Unimplemented:
				return &terminalHealthError{msg: "grpc health check: adapter does not implement grpc.health.v1 — remote gRPC adapters must register the Health service", cause: err}
			case codes.NotFound:
				// Health service registered but no status set for service="".
				// This is a deployment/code issue — retrying won't help.
				return &terminalHealthError{msg: "grpc health check: health service has no status for empty service name — adapter must call SetServingStatus(\"\", ...)", cause: err}
			case codes.Unavailable:
				// gRPC wraps TLS handshake failures as Unavailable with
				// "authentication handshake failed" in the description.
				// These are deterministic config errors — retrying won't help.
				// Symmetric with isTerminalTLSError in the HTTP path.
				//
				// FRAGILE: depends on google.golang.org/grpc internal error
				// message format. Backed up by isTerminalTLSError below which
				// catches typed TLS errors when the error chain preserves them.
				// Covered by TestCheckGRPCHealth_TLSToPlainGRPCFailsFast.
				// Regression: TestFragileStringDetection_GRPCAuthHandshakeFailed (CI-fatal on string change).
				if strings.Contains(st.Message(), "authentication handshake failed") {
					return &terminalHealthError{msg: fmt.Sprintf("grpc health check: %v", err), cause: err}
				}
			}
		}
		// Also try direct TLS error type detection — works when the gRPC
		// error chain preserves the original x509/tls error types.
		if isTerminalTLSError(err) {
			return &terminalHealthError{msg: fmt.Sprintf("grpc health check: %v", err), cause: err}
		}
		return fmt.Errorf("grpc health check: %w", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("grpc health check: status %s (expected SERVING)", resp.Status)
	}
	return nil
}

// httpHealthSetup validates the address, builds TLS config, and returns a
// reusable http.Client and health URL for repeated probes. This is the HTTP
// counterpart to grpcHealthDial — expensive setup is done once, then
// httpHealthProbe is called per retry iteration.
// The caller must call cleanup() when done to release transport resources.
func httpHealthSetup(addr string, tlsCfg *napdial.TLSConfig) (client *http.Client, healthURL string, cleanup func(), err error) {
	// Validate and normalize address first — fail fast before allocating
	// transport resources. Use url.URL builder instead of fmt.Sprintf so
	// scoped IPv6 zone IDs (e.g. "fe80::1%en0") are properly
	// percent-encoded in the Host field.
	host, port, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		return nil, "", nil, &terminalHealthError{msg: fmt.Sprintf("http health check: invalid address %q: %v", addr, splitErr), cause: splitErr}
	}

	scheme := "http"
	transport := &http.Transport{
		// Explicit dial timeout as defense-in-depth — limits TCP connection
		// establishment time per probe, complementing the per-probe context
		// timeout (healthCheckTimeout). Without this, the default net.Dialer
		// has no timeout and relies solely on the context deadline.
		DialContext: (&net.Dialer{Timeout: healthCheckTimeout}).DialContext,
	}
	if tlsCfg != nil {
		scheme = "https"
		stdTLS, tlsErr := tlsCfg.BuildStdTLSConfig()
		if tlsErr != nil {
			return nil, "", nil, &terminalHealthError{msg: fmt.Sprintf("http health check: tls config: %v", tlsErr), cause: tlsErr}
		}
		transport.TLSClientConfig = stdTLS
		// Setting a custom TLSClientConfig disables Go's automatic HTTP/2.
		// Re-enable it so health checks work against HTTP/2-only endpoints.
		transport.ForceAttemptHTTP2 = true
	}

	healthURL = (&url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(host, port),
		Path:   "/health",
	}).String()

	client = &http.Client{
		Transport: transport,
		// Do not follow redirects — AC#2 requires /health itself to return 200.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return client, healthURL, func() { transport.CloseIdleConnections() }, nil
}

// httpHealthProbe sends a single GET /health request using a pre-built client.
// Returns nil only when the server responds with HTTP 200.
func httpHealthProbe(ctx context.Context, client *http.Client, healthURL string) error {
	checkCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		return fmt.Errorf("http health check: create request: %w", err)
	}
	req.Header.Set("User-Agent", "nupi-health-check/1.0")

	resp, err := client.Do(req)
	if err != nil {
		// TLS handshake failures (wrong cert, CA mismatch, expired cert) are
		// deterministic config errors — retrying won't help. Classify them as
		// terminal so the retry loop fails fast with an actionable message.
		if isTerminalTLSError(err) {
			return &terminalHealthError{msg: fmt.Sprintf("http health check: %v", err), cause: err}
		}
		// Protocol mismatch: HTTPS client to plain-HTTP server. Go's HTTP
		// transport sometimes surfaces a tls.RecordHeaderError (caught by
		// isTerminalTLSError above), but it can also wrap the error at a
		// higher level with a descriptive string. The string check below is
		// a secondary detection path for when the typed error is not preserved.
		//
		// FRAGILE: depends on Go net/http internal error message. Backed up
		// by isTerminalTLSError above which catches tls.RecordHeaderError
		// when the error chain preserves it. Covered by
		// TestCheckHTTPHealth_TLSToPlainHTTPFailsFast.
		// Regression: TestFragileStringDetection_HTTPServerGaveHTTPResponse (CI-fatal on string change).
		if strings.Contains(err.Error(), "server gave HTTP response to HTTPS client") {
			return &terminalHealthError{msg: fmt.Sprintf("http health check: %v", err), cause: err}
		}
		return fmt.Errorf("http health check: %w", err)
	}
	defer func() {
		// Drain up to 4KB so the underlying TCP connection can be reused by
		// the transport's connection pool across retry probes. The drain
		// respects the probe's context deadline (checkCtx) so it cannot block
		// indefinitely against a misbehaving server — when the context
		// expires, Read returns immediately and we close the body.
		if checkCtx.Err() == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxHealthResponseDrain))
		}
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return &terminalHealthError{msg: "http health check: adapter does not expose /health endpoint — remote HTTP adapters must implement GET /health (got 404)"}
		}
		// Any redirect from /health indicates a configuration issue — health
		// endpoints should return 200 directly. Permanent (301, 308) and
		// temporary (302, 303, 307) redirects are all terminal because a
		// properly configured health endpoint never redirects. Retrying a
		// misdirected /health won't self-resolve.
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return &terminalHealthError{msg: fmt.Sprintf("http health check: /health returned redirect %d — health endpoints should return 200 directly, not redirect (check adapter or proxy configuration)", resp.StatusCode)}
		}
		return fmt.Errorf("http health check: unexpected status %d (expected 200)", resp.StatusCode)
	}
	return nil
}

// Terminal TLS alert codes (RFC 5246 Section 7.2.2) indicating deterministic
// certificate/auth failures sent by the peer. Go's crypto/tls does not export
// these as named constants; we define them here for readability.
const (
	tlsAlertBadCertificate       tls.AlertError = 42
	tlsAlertUnsupportedCert      tls.AlertError = 43
	tlsAlertCertificateRevoked   tls.AlertError = 44
	tlsAlertCertificateExpired   tls.AlertError = 45
	tlsAlertCertificateUnknown   tls.AlertError = 46
	tlsAlertUnknownCA            tls.AlertError = 48
	tlsAlertAccessDenied         tls.AlertError = 49
	tlsAlertInsufficientSecurity tls.AlertError = 71
)

// isTerminalTLSAlert returns true for TLS alert codes (RFC 5246 Section 7.2.2)
// that indicate deterministic certificate/auth failures sent by the peer.
// These won't self-resolve (wrong cert, unknown CA, expired cert, etc.).
//
// Code 47 (illegal_parameter) is intentionally excluded — it indicates a
// protocol-level error (e.g. malformed handshake message) that may be
// transient rather than a deterministic cert/auth configuration issue.
func isTerminalTLSAlert(alert tls.AlertError) bool {
	switch alert {
	case tlsAlertBadCertificate,
		tlsAlertUnsupportedCert,
		tlsAlertCertificateRevoked,
		tlsAlertCertificateExpired,
		tlsAlertCertificateUnknown,
		tlsAlertUnknownCA,
		tlsAlertAccessDenied,
		tlsAlertInsufficientSecurity:
		return true
	default:
		return false
	}
}

// isTerminalTLSError returns true for TLS/certificate errors that indicate
// a deterministic configuration problem (wrong cert, CA mismatch, expired cert,
// missing system roots, insecure algorithm, peer-sent TLS alerts). These will
// never self-resolve and should not be retried.
func isTerminalTLSError(err error) bool {
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var unknownAuth x509.UnknownAuthorityError
	if errors.As(err, &unknownAuth) {
		return true
	}
	var certInvalid x509.CertificateInvalidError
	if errors.As(err, &certInvalid) {
		return true
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return true
	}
	var sysRootsErr x509.SystemRootsError
	if errors.As(err, &sysRootsErr) {
		return true
	}
	var insecureAlg x509.InsecureAlgorithmError
	if errors.As(err, &insecureAlg) {
		return true
	}
	// TLS protocol mismatch: TLS client connecting to a plain-text server
	// (or vice versa). The first bytes don't look like a TLS record header.
	var recHdrErr tls.RecordHeaderError
	if errors.As(err, &recHdrErr) {
		return true
	}
	// Peer-sent TLS alerts for certificate/auth failures (Go 1.21+).
	var alertErr tls.AlertError
	if errors.As(err, &alertErr) {
		return isTerminalTLSAlert(alertErr)
	}
	return false
}

// waitForAdapterReadyTransportAware dispatches readiness checks by transport type.
// For gRPC: polls grpc.health.v1 health check until SERVING or ctx expires.
// For HTTP: polls GET /health until 200 or ctx expires.
// For process: uses existing TCP dial polling loop (startup detection, not a health check).
// Unknown transports return an error immediately.
func waitForAdapterReadyTransportAware(ctx context.Context, addr, transport string, tlsCfg *napdial.TLSConfig) error {
	var probe func() error

	switch transport {
	case "grpc":
		conn, client, dialErr := grpcHealthDial(ctx, addr, tlsCfg)
		if dialErr != nil {
			return dialErr
		}
		defer conn.Close()
		// ctx is the caller's readyTimeout context. Each probe creates its own
		// healthCheckTimeout sub-context (see grpcHealthProbe), so the per-probe
		// 5s timeout is an upper bound — if ctx expires sooner, the probe
		// inherits the earlier deadline.
		probe = func() error { return grpcHealthProbe(ctx, client) }
	case "http":
		client, healthURL, cleanup, setupErr := httpHealthSetup(addr, tlsCfg)
		if setupErr != nil {
			return setupErr
		}
		defer cleanup()
		// Same ctx semantics as gRPC: each probe's healthCheckTimeout is bounded
		// by the caller's readyTimeout context (see httpHealthProbe).
		probe = func() error { return httpHealthProbe(ctx, client, healthURL) }
	case "process":
		return waitForAdapterReady(ctx, addr)
	default:
		return &terminalHealthError{msg: fmt.Sprintf("health check: unsupported transport %q for %s", transport, addr)}
	}

	// Retry loop: probe until success or the caller's context (readyTimeout) expires.
	// Each probe has its own internal 5s healthCheckTimeout; the outer ctx controls
	// the overall retry budget (typically 30s readyTimeout from startAdapter).
	ticker := time.NewTicker(healthCheckPollInterval)
	defer ticker.Stop()

	var lastErr error
	probes := 0
	for {
		probes++
		err := probe()
		if err == nil {
			if probes > 1 {
				log.Printf("[Adapters] %s health check for %s succeeded after %d probes", transport, addr, probes)
			}
			return nil
		}

		// Terminal errors (e.g. gRPC Unimplemented, HTTP 404) indicate a
		// permanent contract violation — retrying won't help. Fail fast.
		// Use errors.As so fail-fast still triggers if the error is wrapped.
		var termErr *terminalHealthError
		if errors.As(err, &termErr) {
			return err
		}

		// Update lastErr unless the parent context is already done AND we
		// have a meaningful error from a prior probe. This preserves the
		// actual health check failure (e.g. NOT_SERVING, 503) instead of
		// replacing it with "context deadline exceeded" from a late probe.
		if ctx.Err() == nil || lastErr == nil {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			p := "attempts"
			if probes == 1 {
				p = "attempt"
			}
			// Suppress redundant "(last: context deadline exceeded)" when
			// the only captured error is itself a context error (e.g. first probe
			// exceeded the parent context before any meaningful health error).
			// Use "readiness" (not "health check") to avoid prefix stutter with
			// probe errors that already contain "grpc/http health check:".
			if lastErr != nil && !errors.Is(lastErr, context.DeadlineExceeded) && !errors.Is(lastErr, context.Canceled) {
				return fmt.Errorf("%s readiness for %s: %w after %d %s (last: %v)", transport, addr, ctx.Err(), probes, p, lastErr)
			}
			return fmt.Errorf("%s readiness for %s: %w after %d %s", transport, addr, ctx.Err(), probes, p)
		case <-ticker.C:
		}
	}
}
