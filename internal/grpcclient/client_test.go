package grpcclient

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	nupiversion "github.com/nupi-ai/nupi/internal/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
)

type testDaemonServer struct {
	apiv1.UnimplementedDaemonServiceServer
}

func (s *testDaemonServer) Status(ctx context.Context, _ *apiv1.DaemonStatusRequest) (*apiv1.DaemonStatusResponse, error) {
	return &apiv1.DaemonStatusResponse{Version: "test"}, nil
}

func startGRPCServer(t *testing.T) (addr string, shutdown func(), auth func() string) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network not permitted: %v", err)
	}

	var mu sync.Mutex
	var token string

	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if values := md.Get("authorization"); len(values) > 0 {
				mu.Lock()
				token = values[0]
				mu.Unlock()
			}
		}
		return handler(ctx, req)
	}))

	apiv1.RegisterDaemonServiceServer(server, &testDaemonServer{})

	go func() {
		if err := server.Serve(lis); err != nil {
			t.Logf("grpc serve exited: %v", err)
		}
	}()

	return lis.Addr().String(), func() {
			server.Stop()
		}, func() string {
			mu.Lock()
			defer mu.Unlock()
			return token
		}
}

func TestNewUsesBaseURL(t *testing.T) {
	addr, shutdown, auth := startGRPCServer(t)
	t.Cleanup(shutdown)

	t.Setenv("NUPI_BASE_URL", "http://"+addr)
	t.Setenv("NUPI_API_TOKEN", "env-token")
	t.Setenv("NUPI_TLS_INSECURE", "")

	client, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { client.Close() })

	resp, err := client.DaemonStatus(context.Background())
	if err != nil {
		t.Fatalf("DaemonStatus: %v", err)
	}
	if resp.GetVersion() != "test" {
		t.Fatalf("unexpected version: %s", resp.GetVersion())
	}
	if auth() != "Bearer env-token" {
		t.Fatalf("expected bearer token propagated, got %q", auth())
	}
}

func TestNewFromStore(t *testing.T) {
	addr, shutdown, auth := startGRPCServer(t)
	t.Cleanup(shutdown)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("NUPI_BASE_URL", "")
	t.Setenv("NUPI_API_TOKEN", "")

	store, err := configstore.Open(configstore.Options{
		InstanceName: config.DefaultInstance,
		ProfileName:  config.DefaultProfile,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	ctx := context.Background()
	cfg := configstore.TransportConfig{
		Port:        extractPort(addr),
		Binding:     "loopback",
		GRPCPort:    extractPort(addr),
		GRPCBinding: "loopback",
	}
	if err := store.SaveTransportConfig(ctx, cfg); err != nil {
		t.Fatalf("save transport config: %v", err)
	}
	if err := store.SaveSecuritySettings(ctx, map[string]string{
		"auth.http_tokens": `["store-token"]`,
	}); err != nil {
		t.Fatalf("save security settings: %v", err)
	}
	store.Close()

	client, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { client.Close() })

	if _, err := client.DaemonStatus(context.Background()); err != nil {
		t.Fatalf("DaemonStatus: %v", err)
	}
	if auth() != "Bearer store-token" {
		t.Fatalf("expected store token propagated, got %q", auth())
	}
}

func TestCheckVersionOnce_PrintsWarningOnMismatch(t *testing.T) {
	cleanup := nupiversion.ForTesting("1.0.0")
	t.Cleanup(cleanup)

	addr, shutdown, _ := startGRPCServer(t) // daemon returns version "test"
	t.Cleanup(shutdown)

	t.Setenv("NUPI_BASE_URL", "http://"+addr)
	t.Setenv("NUPI_API_TOKEN", "")

	client, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { client.Close() })

	var buf bytes.Buffer
	client.warningWriter = &buf

	// First DaemonStatus call — should trigger warning (1.0.0 vs test)
	if _, err := client.DaemonStatus(context.Background()); err != nil {
		t.Fatalf("DaemonStatus: %v", err)
	}

	warning := buf.String()
	if !strings.Contains(warning, "WARNING") {
		t.Errorf("expected version mismatch warning, got %q", warning)
	}
	if !strings.Contains(warning, "nupi v1.0.0") || !strings.Contains(warning, "nupid vtest") {
		t.Errorf("warning missing version details: %q", warning)
	}

	// Second call — versionChecked flag should prevent duplicate warning
	buf.Reset()
	if _, err := client.DaemonStatus(context.Background()); err != nil {
		t.Fatalf("DaemonStatus (2nd): %v", err)
	}
	if buf.Len() > 0 {
		t.Errorf("expected no duplicate warning, got %q", buf.String())
	}
}

func TestCheckVersionOnce_NoWarningWhenMatching(t *testing.T) {
	cleanup := nupiversion.ForTesting("test") // matches daemon's version
	t.Cleanup(cleanup)

	addr, shutdown, _ := startGRPCServer(t)
	t.Cleanup(shutdown)

	t.Setenv("NUPI_BASE_URL", "http://"+addr)
	t.Setenv("NUPI_API_TOKEN", "")

	client, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { client.Close() })

	var buf bytes.Buffer
	client.warningWriter = &buf

	if _, err := client.DaemonStatus(context.Background()); err != nil {
		t.Fatalf("DaemonStatus: %v", err)
	}

	if buf.Len() > 0 {
		t.Errorf("expected no warning for matching versions, got %q", buf.String())
	}
}

// failThenSucceedDaemonServer fails the first failCount calls, then succeeds.
type failThenSucceedDaemonServer struct {
	apiv1.UnimplementedDaemonServiceServer
	failCount int32
	calls     atomic.Int32
}

func (s *failThenSucceedDaemonServer) Status(_ context.Context, _ *apiv1.DaemonStatusRequest) (*apiv1.DaemonStatusResponse, error) {
	n := s.calls.Add(1)
	if n <= s.failCount {
		return nil, grpcstatus.Error(codes.Unavailable, "simulated failure")
	}
	return &apiv1.DaemonStatusResponse{Version: "test"}, nil
}

func startFailThenSucceedServer(t *testing.T, failCount int32) (addr string, shutdown func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network not permitted: %v", err)
	}
	srv := grpc.NewServer()
	apiv1.RegisterDaemonServiceServer(srv, &failThenSucceedDaemonServer{failCount: failCount})
	go func() { _ = srv.Serve(lis) }()
	return lis.Addr().String(), srv.Stop
}

func TestVersionCheckRetriesAfterFailure(t *testing.T) {
	cleanup := nupiversion.ForTesting("1.0.0")
	t.Cleanup(cleanup)

	addr, shutdown := startFailThenSucceedServer(t, 1) // first RPC fails
	t.Cleanup(shutdown)

	t.Setenv("NUPI_BASE_URL", "http://"+addr)
	t.Setenv("NUPI_API_TOKEN", "")

	client, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { client.Close() })

	var buf bytes.Buffer
	client.warningWriter = &buf

	// First call — version check RPC fails, versionChecked stays false
	if _, err := client.DaemonStatus(context.Background()); err != nil {
		t.Fatalf("DaemonStatus (1st): %v", err)
	}
	if buf.Len() > 0 {
		t.Errorf("expected no warning on failed version check, got %q", buf.String())
	}

	// Second call — version check RPC succeeds, should now print warning
	if _, err := client.DaemonStatus(context.Background()); err != nil {
		t.Fatalf("DaemonStatus (2nd): %v", err)
	}
	if !strings.Contains(buf.String(), "WARNING") {
		t.Errorf("expected version mismatch warning after retry, got %q", buf.String())
	}

	// Third call — versionChecked is true, no duplicate warning
	buf.Reset()
	if _, err := client.DaemonStatus(context.Background()); err != nil {
		t.Fatalf("DaemonStatus (3rd): %v", err)
	}
	if buf.Len() > 0 {
		t.Errorf("expected no duplicate warning after successful check, got %q", buf.String())
	}
}

// countingDaemonServer counts Status calls for verifying RPC behaviour.
type countingDaemonServer struct {
	apiv1.UnimplementedDaemonServiceServer
	calls atomic.Int32
}

func (s *countingDaemonServer) Status(_ context.Context, _ *apiv1.DaemonStatusRequest) (*apiv1.DaemonStatusResponse, error) {
	s.calls.Add(1)
	return &apiv1.DaemonStatusResponse{Version: "test"}, nil
}

func TestDisableVersionCheck_NoExtraRPC(t *testing.T) {
	cleanup := nupiversion.ForTesting("1.0.0") // differs from daemon "test" → would warn
	t.Cleanup(cleanup)

	counter := &countingDaemonServer{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network not permitted: %v", err)
	}
	srv := grpc.NewServer()
	apiv1.RegisterDaemonServiceServer(srv, counter)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	t.Setenv("NUPI_BASE_URL", "http://"+lis.Addr().String())
	t.Setenv("NUPI_API_TOKEN", "")

	client, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { client.Close() })

	var buf bytes.Buffer
	client.warningWriter = &buf
	client.DisableVersionCheck()

	// DaemonStatus should make exactly 1 RPC (the actual call), not 2.
	if _, err := client.DaemonStatus(context.Background()); err != nil {
		t.Fatalf("DaemonStatus: %v", err)
	}

	if n := counter.calls.Load(); n != 1 {
		t.Errorf("expected exactly 1 Status RPC (the actual call), got %d", n)
	}
	if buf.Len() > 0 {
		t.Errorf("expected no version warning with DisableVersionCheck, got %q", buf.String())
	}
}

func TestConcurrentVersionCheck(t *testing.T) {
	cleanup := nupiversion.ForTesting("1.0.0")
	t.Cleanup(cleanup)

	counter := &countingDaemonServer{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network not permitted: %v", err)
	}
	srv := grpc.NewServer()
	apiv1.RegisterDaemonServiceServer(srv, counter)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	t.Setenv("NUPI_BASE_URL", "http://"+lis.Addr().String())
	t.Setenv("NUPI_API_TOKEN", "")

	client, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { client.Close() })

	var buf bytes.Buffer
	client.warningWriter = &buf

	// Launch 10 concurrent DaemonStatus calls — only 1 version check RPC should fire.
	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, _ = client.DaemonStatus(context.Background())
		}()
	}
	wg.Wait()

	// Version check RPC + N actual DaemonStatus RPCs.
	// The versionChecking guard should ensure at most 1 version-check RPC.
	statusCalls := counter.calls.Load()
	// Expected: 10 (actual calls) + 1 (version check) = 11, or 10 if version check
	// is concurrent with the first actual call. Either way, should not be 20 (no double checks).
	if statusCalls > int32(goroutines)+1 {
		t.Errorf("expected at most %d Status RPCs (version check + actual calls), got %d", goroutines+1, statusCalls)
	}

	// Warning should appear exactly once (not 10 times).
	warningCount := strings.Count(buf.String(), "WARNING")
	if warningCount != 1 {
		t.Errorf("expected exactly 1 version warning, got %d: %q", warningCount, buf.String())
	}
}

func extractPort(addr string) int {
	_, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	return port
}
