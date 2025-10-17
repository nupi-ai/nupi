package grpcclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/bootstrap"
	"github.com/nupi-ai/nupi/internal/client"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
)

type Client struct {
	conn       *grpc.ClientConn
	daemon     apiv1.DaemonServiceClient
	config     apiv1.ConfigServiceClient
	adapters   apiv1.AdaptersServiceClient
	quickstart apiv1.QuickstartServiceClient
	modules    apiv1.ModulesServiceClient
	token      string
}

func New() (*Client, error) {
	if base := strings.TrimSpace(os.Getenv("NUPI_BASE_URL")); base != "" {
		return newExplicit(base, nil)
	}

	boot, err := bootstrap.Load()
	if err != nil {
		return nil, err
	}
	if boot != nil && strings.TrimSpace(boot.BaseURL) != "" {
		return newExplicit(boot.BaseURL, boot)
	}

	return newFromStore()
}

func newFromStore() (*Client, error) {
	cfg, tokens, err := client.LoadTransportSettings()
	if err != nil {
		return nil, err
	}

	tlsEnabled := strings.TrimSpace(cfg.TLSCertPath) != "" && strings.TrimSpace(cfg.TLSKeyPath) != ""

	host := client.DetermineHost(cfg.Binding, tlsEnabled)
	if override := strings.TrimSpace(os.Getenv("NUPI_DAEMON_HOST")); override != "" {
		host = override
	}

	if cfg.Port <= 0 {
		return nil, fmt.Errorf("grpc: daemon HTTP port not available; is nupid running?")
	}

	address := net.JoinHostPort(host, fmt.Sprintf("%d", effectiveGRPCPort(cfg)))

	tlsConfig, err := client.PrepareTLSConfig(cfg, host, tlsEnabled)
	if err != nil {
		return nil, err
	}

	token := strings.TrimSpace(os.Getenv("NUPI_API_TOKEN"))
	if token == "" && len(tokens) > 0 {
		token = tokens[0]
	}

	return dial(address, tlsConfig, token)
}

func newExplicit(raw string, boot *bootstrap.Config) (*Client, error) {
	val := strings.TrimSpace(raw)
	if val == "" {
		return nil, fmt.Errorf("grpc: NUPI_BASE_URL is empty")
	}
	if !strings.Contains(val, "://") {
		val = "https://" + val
	}

	u, err := url.Parse(val)
	if err != nil {
		return nil, fmt.Errorf("grpc: parse NUPI_BASE_URL: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("grpc: NUPI_BASE_URL missing host")
	}

	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = defaultPortForScheme(u.Scheme)
	}

	address := net.JoinHostPort(host, port)

	token, tlsOpts := client.ResolveExplicitOptions(boot)

	var tlsConfig *tls.Config
	if strings.EqualFold(u.Scheme, "https") {
		tlsConfig, err = client.TLSConfigForExplicit(u, &tlsOpts)
		if err != nil {
			return nil, err
		}
	}

	return dial(address, tlsConfig, token)
}

func dial(address string, tlsConfig *tls.Config, token string) (*Client, error) {
	opts := []grpc.DialOption{grpc.WithBlock()}
	if tlsConfig != nil {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, address, opts...)
	if err != nil {
		return nil, fmt.Errorf("grpc: dial %s: %w", address, err)
	}

	return &Client{
		conn:       conn,
		daemon:     apiv1.NewDaemonServiceClient(conn),
		config:     apiv1.NewConfigServiceClient(conn),
		adapters:   apiv1.NewAdaptersServiceClient(conn),
		quickstart: apiv1.NewQuickstartServiceClient(conn),
		modules:    apiv1.NewModulesServiceClient(conn),
		token:      strings.TrimSpace(token),
	}, nil
}

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func (c *Client) DaemonStatus(ctx context.Context) (*apiv1.DaemonStatusResponse, error) {
	ctx = c.attachToken(ctx)
	return c.daemon.Status(ctx, &apiv1.DaemonStatusRequest{})
}

func (c *Client) TransportConfig(ctx context.Context) (*apiv1.TransportConfig, error) {
	ctx = c.attachToken(ctx)
	return c.config.GetTransportConfig(ctx, &emptypb.Empty{})
}

func (c *Client) UpdateTransportConfig(ctx context.Context, cfg *apiv1.TransportConfig) (*apiv1.TransportConfig, error) {
	ctx = c.attachToken(ctx)
	return c.config.UpdateTransportConfig(ctx, &apiv1.UpdateTransportConfigRequest{Config: cfg})
}

func (c *Client) ListAdapters(ctx context.Context) (*apiv1.ListAdaptersResponse, error) {
	ctx = c.attachToken(ctx)
	return c.adapters.ListAdapters(ctx, &emptypb.Empty{})
}

func (c *Client) ListAdapterBindings(ctx context.Context) (*apiv1.ListAdapterBindingsResponse, error) {
	ctx = c.attachToken(ctx)
	return c.adapters.ListAdapterBindings(ctx, &emptypb.Empty{})
}

func (c *Client) SetAdapterBinding(ctx context.Context, req *apiv1.SetAdapterBindingRequest) (*apiv1.AdapterBinding, error) {
	ctx = c.attachToken(ctx)
	return c.adapters.SetAdapterBinding(ctx, req)
}

func (c *Client) ClearAdapterBinding(ctx context.Context, slot string) error {
	ctx = c.attachToken(ctx)
	_, err := c.adapters.ClearAdapterBinding(ctx, &apiv1.ClearAdapterBindingRequest{Slot: slot})
	return err
}

func (c *Client) QuickstartStatus(ctx context.Context) (*apiv1.QuickstartStatusResponse, error) {
	ctx = c.attachToken(ctx)
	return c.quickstart.GetStatus(ctx, &emptypb.Empty{})
}

func (c *Client) UpdateQuickstart(ctx context.Context, req *apiv1.UpdateQuickstartRequest) (*apiv1.QuickstartStatusResponse, error) {
	ctx = c.attachToken(ctx)
	return c.quickstart.Update(ctx, req)
}

func (c *Client) ModulesOverview(ctx context.Context) (*apiv1.ModulesOverviewResponse, error) {
	ctx = c.attachToken(ctx)
	return c.modules.Overview(ctx, &emptypb.Empty{})
}

func (c *Client) BindModule(ctx context.Context, req *apiv1.BindModuleRequest) (*apiv1.ModuleActionResponse, error) {
	ctx = c.attachToken(ctx)
	return c.modules.BindModule(ctx, req)
}

func (c *Client) StartModule(ctx context.Context, slot string) (*apiv1.ModuleActionResponse, error) {
	ctx = c.attachToken(ctx)
	return c.modules.StartModule(ctx, &apiv1.ModuleSlotRequest{Slot: slot})
}

func (c *Client) StopModule(ctx context.Context, slot string) (*apiv1.ModuleActionResponse, error) {
	ctx = c.attachToken(ctx)
	return c.modules.StopModule(ctx, &apiv1.ModuleSlotRequest{Slot: slot})
}

func (c *Client) attachToken(ctx context.Context) context.Context {
	token := strings.TrimSpace(c.token)
	if token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func effectiveGRPCPort(cfg configstore.TransportConfig) int {
	if cfg.GRPCPort > 0 {
		return cfg.GRPCPort
	}
	if cfg.Port > 0 {
		return cfg.Port
	}
	return 0
}

func defaultPortForScheme(scheme string) string {
	if strings.EqualFold(scheme, "https") {
		return "443"
	}
	if strings.EqualFold(scheme, "http") {
		return "80"
	}
	return "80"
}
