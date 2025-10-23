package grpcclient

import (
	"context"
	"net"
	"strconv"
	"sync"
	"testing"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
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
	defer shutdown()

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
	defer shutdown()

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

func extractPort(addr string) int {
	_, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	return port
}
