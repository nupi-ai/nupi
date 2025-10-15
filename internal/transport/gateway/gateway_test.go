package gateway

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/server"
	"github.com/nupi-ai/nupi/internal/session"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

type runtimeStub struct{}

func (runtimeStub) Port() int            { return 0 }
func (runtimeStub) GRPCPort() int        { return 0 }
func (runtimeStub) StartTime() time.Time { return time.Unix(0, 0) }

func newGatewayTestAPIServer(t *testing.T) (*server.APIServer, *configstore.Store) {
	t.Helper()

	tmpHome := t.TempDir()
	oldHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", tmpHome); err != nil {
		t.Fatalf("failed to set HOME: %v", err)
	}
	t.Cleanup(func() {
		os.Setenv("HOME", oldHome)
	})

	if _, err := config.EnsureInstanceDirs(config.DefaultInstance); err != nil {
		t.Fatalf("failed to ensure instance dirs: %v", err)
	}
	if _, err := config.EnsureProfileDirs(config.DefaultInstance, config.DefaultProfile); err != nil {
		t.Fatalf("failed to ensure profile dirs: %v", err)
	}

	store, err := configstore.Open(configstore.Options{InstanceName: config.DefaultInstance, ProfileName: config.DefaultProfile})
	if err != nil {
		t.Fatalf("failed to open config store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
	})

	sessionManager := session.NewManager()
	apiServer, err := server.NewAPIServer(sessionManager, store, runtimeStub{}, 0)
	if err != nil {
		t.Fatalf("failed to create api server: %v", err)
	}

	return apiServer, store
}

func TestGatewayStartLoopback(t *testing.T) {
	apiServer, store := newGatewayTestAPIServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := New(apiServer)

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background())

	if info.HTTP.Port <= 0 {
		t.Fatalf("expected HTTP port to be assigned, got %d", info.HTTP.Port)
	}
	if info.GRPC.Port <= 0 {
		t.Fatalf("expected gRPC port to be assigned, got %d", info.GRPC.Port)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + info.HTTP.Address + "/daemon/status")
	if err != nil {
		t.Fatalf("http status request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	conn, err := grpc.DialContext(dialCtx, info.GRPC.Address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("failed to dial grpc: %v", err)
	}
	defer conn.Close()

	healthClient := healthpb.NewHealthClient(conn)
	if _, err := healthClient.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatalf("gRPC health check failed: %v", err)
	}

	// ensure gRPC port persisted back to config store
	cfg, err := store.GetTransportConfig(context.Background())
	if err != nil {
		t.Fatalf("failed to load transport config: %v", err)
	}
	if cfg.GRPCPort != info.GRPC.Port {
		t.Fatalf("expected stored grpc port %d, got %d", info.GRPC.Port, cfg.GRPCPort)
	}
}
