package gateway

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// generateTestCert creates a self-signed TLS cert/key pair in dir and returns paths.
func generateTestCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("encode cert pem: %v", err)
	}
	if err := certOut.Close(); err != nil {
		t.Fatalf("close cert file: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal ec key: %v", err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("encode key pem: %v", err)
	}
	if err := keyOut.Close(); err != nil {
		t.Fatalf("close key file: %v", err)
	}
	return
}

func registerTestServices(apiServer *server.APIServer, srv *grpc.Server) {
	server.RegisterGRPCServices(apiServer, srv)
}

// testHTTPClient returns an HTTP client with a reasonable timeout for tests.
var testHTTPClient = &http.Client{Timeout: 10 * time.Second}

// connectPost sends a Connect RPC unary request with the required protocol headers.
func connectPost(ctx context.Context, url string, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	return testHTTPClient.Do(req)
}

func TestGatewayConnectEnabled(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _ := newGatewayTestAPIServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := New(apiServer, Options{ConnectEnabled: true})

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background())

	if info.GRPC.Port <= 0 {
		t.Fatalf("expected gRPC port, got %d", info.GRPC.Port)
	}
	if info.Connect.Port <= 0 {
		t.Fatalf("expected Connect port, got %d", info.Connect.Port)
	}
	if info.Connect.Scheme != "http" {
		t.Fatalf("expected Connect scheme http, got %q", info.Connect.Scheme)
	}
}

func TestGatewayConnectDisabled(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _ := newGatewayTestAPIServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := New(apiServer) // ConnectEnabled defaults to false

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background())

	if info.Connect.Port != 0 {
		t.Fatalf("expected no Connect port when disabled, got %d", info.Connect.Port)
	}
}

func TestConnectDaemonStatus(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _ := newGatewayTestAPIServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := New(apiServer, Options{
		ConnectEnabled: true,
		RegisterGRPC:   func(srv *grpc.Server) { registerTestServices(apiServer, srv) },
	})

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background())

	url := fmt.Sprintf("http://%s/nupi.api.v1.DaemonService/Status", info.Connect.Address)
	resp, err := connectPost(ctx, url, "{}")
	if err != nil {
		t.Fatalf("Connect request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	// DaemonStatusResponse includes version, sessions_count, connect_port, etc.
	if _, ok := result["version"]; !ok {
		t.Errorf("expected version in response, got: %v", result)
	}
	if cp, ok := result["connectPort"]; !ok {
		t.Errorf("expected connectPort in response, got: %v", result)
	} else if _, isNum := cp.(float64); !isNum {
		t.Errorf("expected connectPort to be a number, got %T", cp)
	}
}

func TestConnectListSessions(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _ := newGatewayTestAPIServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := New(apiServer, Options{
		ConnectEnabled: true,
		RegisterGRPC:   func(srv *grpc.Server) { registerTestServices(apiServer, srv) },
	})

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background())

	url := fmt.Sprintf("http://%s/nupi.api.v1.SessionsService/ListSessions", info.Connect.Address)
	resp, err := connectPost(ctx, url, "{}")
	if err != nil {
		t.Fatalf("Connect request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	// sessions field should exist (may be empty array).
	if _, ok := result["sessions"]; !ok {
		t.Errorf("expected sessions in response, got: %v", result)
	}
}

func TestConnectListLanguages(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _ := newGatewayTestAPIServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := New(apiServer, Options{
		ConnectEnabled: true,
		RegisterGRPC:   func(srv *grpc.Server) { registerTestServices(apiServer, srv) },
	})

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background())

	url := fmt.Sprintf("http://%s/nupi.api.v1.DaemonService/ListLanguages", info.Connect.Address)
	resp, err := connectPost(ctx, url, "{}")
	if err != nil {
		t.Fatalf("Connect request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	// languages field should exist.
	if _, ok := result["languages"]; !ok {
		t.Errorf("expected languages in response, got: %v", result)
	}
}

func TestConnectGRPCCoexistence(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _ := newGatewayTestAPIServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := New(apiServer, Options{
		ConnectEnabled: true,
		RegisterGRPC:   func(srv *grpc.Server) { registerTestServices(apiServer, srv) },
	})

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background())

	// gRPC should still work unchanged.
	conn, err := grpc.NewClient(passthroughPrefix+info.GRPC.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial grpc: %v", err)
	}
	defer conn.Close()

	healthClient := healthpb.NewHealthClient(conn)
	if _, err := healthClient.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatalf("gRPC health check failed when Connect is enabled: %v", err)
	}

	// Connect should also work concurrently.
	url := fmt.Sprintf("http://%s/nupi.api.v1.DaemonService/Status", info.Connect.Address)
	resp, err := connectPost(ctx, url, "{}")
	if err != nil {
		t.Fatalf("Connect request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK from Connect, got %d: %s", resp.StatusCode, string(body))
	}
}

func TestConnectLanguageHeaderInvalid(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _ := newGatewayTestAPIServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := New(apiServer, Options{
		ConnectEnabled: true,
		RegisterGRPC:   func(srv *grpc.Server) { registerTestServices(apiServer, srv) },
	})

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background())

	baseURL := fmt.Sprintf("http://%s", info.Connect.Address)

	// Invalid language header should be rejected by the language interceptor.
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/nupi.api.v1.DaemonService/ListLanguages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("nupi-language", "xx-invalid-code")

	resp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("Connect request failed: %v", err)
	}
	defer resp.Body.Close()

	// The gRPC language interceptor should reject this as invalid.
	// Vanguard translates gRPC InvalidArgument to HTTP 400.
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected rejection for invalid language, got 200")
	}
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 400 Bad Request for invalid language, got %d: %s", resp.StatusCode, string(body))
	}
}

func TestConnectLanguageHeaderValid(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _ := newGatewayTestAPIServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := New(apiServer, Options{
		ConnectEnabled: true,
		RegisterGRPC:   func(srv *grpc.Server) { registerTestServices(apiServer, srv) },
	})

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background())

	// Valid language header should be accepted and propagated.
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("http://%s/nupi.api.v1.DaemonService/ListLanguages", info.Connect.Address),
		strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("nupi-language", "en")

	resp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("Connect request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK for valid language header, got %d: %s", resp.StatusCode, string(body))
	}
}

func TestConnectAuthEnforcement(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, store := newGatewayTestAPIServer(t)

	certPath, keyPath := generateTestCert(t, t.TempDir())

	// Set binding to "lan" so auth is required; provide TLS certs.
	if err := store.SaveTransportConfig(context.Background(), configstore.TransportConfig{
		Binding:     "lan",
		TLSCertPath: certPath,
		TLSKeyPath:  keyPath,
	}); err != nil {
		t.Fatalf("failed to set transport config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := New(apiServer, Options{
		ConnectEnabled: true,
		RegisterGRPC:   func(srv *grpc.Server) { registerTestServices(apiServer, srv) },
	})

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background())

	// Connect should use HTTPS when TLS is configured.
	if info.Connect.Scheme != "https" {
		t.Fatalf("expected Connect scheme https when TLS enabled, got %q", info.Connect.Scheme)
	}

	// Use TLS client that skips cert verification (self-signed).
	tlsClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test cert
		},
	}
	t.Cleanup(tlsClient.CloseIdleConnections)

	url := fmt.Sprintf("https://%s/nupi.api.v1.DaemonService/Status", info.Connect.Address)

	// Unauthenticated request should be rejected.
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")

	resp, err := tlsClient.Do(req)
	if err != nil {
		t.Fatalf("Connect request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected auth rejection for unauthenticated request, got 200")
	}
	// gRPC Unauthenticated maps to HTTP 401.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %d", resp.StatusCode)
	}

	// Positive path: load the auto-generated admin token and verify success.
	secVals, loadErr := store.LoadSecuritySettings(context.Background(), "auth.http_tokens")
	if loadErr != nil {
		t.Fatalf("failed to load auth tokens: %v", loadErr)
	}
	raw, ok := secVals["auth.http_tokens"]
	if !ok || raw == "" {
		t.Fatal("no auth tokens found in store after gateway start")
	}
	var tokenEntries []struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(raw), &tokenEntries); err != nil {
		t.Fatalf("failed to parse auth tokens: %v", err)
	}
	if len(tokenEntries) == 0 {
		t.Fatal("auth token list is empty")
	}

	authReq, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	authReq.Header.Set("Content-Type", "application/json")
	authReq.Header.Set("Connect-Protocol-Version", "1")
	authReq.Header.Set("Authorization", "Bearer "+tokenEntries[0].Token)

	authResp, err := tlsClient.Do(authReq)
	if err != nil {
		t.Fatalf("authenticated Connect request failed: %v", err)
	}
	defer authResp.Body.Close()
	if authResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for authenticated Connect request, got %d", authResp.StatusCode)
	}
}

func TestConnectProtoContentType(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _ := newGatewayTestAPIServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := New(apiServer, Options{
		ConnectEnabled: true,
		RegisterGRPC:   func(srv *grpc.Server) { registerTestServices(apiServer, srv) },
	})

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background())

	// Send a proto-encoded request (empty DaemonStatusRequest = empty bytes).
	url := fmt.Sprintf("http://%s/nupi.api.v1.DaemonService/Status", info.Connect.Address)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")

	resp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("Connect proto request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK for proto request, got %d: %s", resp.StatusCode, string(body))
	}

	// Response should be proto-encoded (not JSON).
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/proto") {
		t.Errorf("expected application/proto response content type, got %q", ct)
	}
}

func TestConnectShutdownClean(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _ := newGatewayTestAPIServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // safety net for early test failure

	gw := New(apiServer, Options{
		ConnectEnabled: true,
		RegisterGRPC:   func(srv *grpc.Server) { registerTestServices(apiServer, srv) },
	})

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background()) // safety net for early test failure

	// Verify Connect is serving.
	url := fmt.Sprintf("http://%s/nupi.api.v1.DaemonService/Status", info.Connect.Address)
	resp, err := connectPost(ctx, url, "{}")
	if err != nil {
		t.Fatalf("Connect request failed: %v", err)
	}
	resp.Body.Close() // explicit close (not defer) so the idle connection is released before shutdown

	// Cancel context and shut down explicitly (the test subject).
	cancel()
	if err := gw.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	// After shutdown, Connect requests should fail (use fresh context since ctx is cancelled).
	_, err = connectPost(context.Background(), url, "{}")
	if err == nil {
		t.Fatalf("expected Connect request to fail after shutdown")
	}
	// Expect a connection-level error (refused/reset), not a timeout or DNS failure.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "refused") && !strings.Contains(errMsg, "reset") && !strings.Contains(errMsg, "closed") {
		t.Logf("post-shutdown error (expected connection refused/reset/closed): %v", err)
	}
}

func TestConnectStreamingRPCError(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _ := newGatewayTestAPIServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := New(apiServer, Options{
		ConnectEnabled: true,
		RegisterGRPC:   func(srv *grpc.Server) { registerTestServices(apiServer, srv) },
	})

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background())

	// AttachSession is a bidi-streaming RPC — Connect protocol does not
	// support bidi streaming. Verify we get an error, not 200 OK.
	url := fmt.Sprintf("http://%s/nupi.api.v1.SessionsService/AttachSession", info.Connect.Address)
	resp, err := connectPost(ctx, url, "{}")
	if err != nil {
		t.Fatalf("Connect request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnsupportedMediaType {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected HTTP 415 for bidi-streaming RPC via Connect, got %d: %s", resp.StatusCode, string(body))
	}
}
