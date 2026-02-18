package gateway

import (
	"context"
	"net"
	"runtime"
	"strings"
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

// passthroughPrefix bypasses gRPC DNS resolution for test connections.
const passthroughPrefix = "passthrough:///"

// skipIfNoNetwork skips the test if network binding is not available
// (e.g., in sandboxed environments without network access).
func skipIfNoNetwork(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// Windows might have different behavior, test separately
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "operation not permitted") ||
			strings.Contains(msg, "permission denied") ||
			strings.Contains(msg, "bind") {
			t.Skipf("Network binding not available: %v", err)
		}
	}
	if ln != nil {
		ln.Close()
	}
}

type runtimeStub struct{}

func (runtimeStub) GRPCPort() int        { return 0 }
func (runtimeStub) StartTime() time.Time { return time.Unix(0, 0) }

func newGatewayTestAPIServer(t *testing.T) (*server.APIServer, *configstore.Store) {
	t.Helper()

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

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
	apiServer, err := server.NewAPIServer(sessionManager, store, runtimeStub{})
	if err != nil {
		t.Fatalf("failed to create api server: %v", err)
	}

	return apiServer, store
}

func TestGatewayStartLoopback(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, store := newGatewayTestAPIServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := New(apiServer)

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background())

	if info.GRPC.Port <= 0 {
		t.Fatalf("expected gRPC port to be assigned, got %d", info.GRPC.Port)
	}

	conn, err := grpc.NewClient(passthroughPrefix+info.GRPC.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
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
