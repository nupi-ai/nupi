package grpcclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/bootstrap"
	"github.com/nupi-ai/nupi/internal/config"
	"github.com/nupi-ai/nupi/internal/constants"
	nupiversion "github.com/nupi-ai/nupi/internal/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
)

// passthroughPrefix bypasses gRPC DNS resolution, matching deprecated DialContext behaviour.
const passthroughPrefix = "passthrough:///"

type Client struct {
	conn            *grpc.ClientConn
	daemon          apiv1.DaemonServiceClient
	sessions        apiv1.SessionsServiceClient
	config          apiv1.ConfigServiceClient
	adapters        apiv1.AdaptersServiceClient
	quickstart      apiv1.QuickstartServiceClient
	adapterRuntime  apiv1.AdapterRuntimeServiceClient
	audio           apiv1.AudioServiceClient
	auth            apiv1.AuthServiceClient
	recordings      apiv1.RecordingsServiceClient
	token           string
	versionMu       sync.Mutex
	versionChecked  bool
	versionChecking bool // guards against duplicate concurrent RPCs
	// skipVersionCheck disables the automatic daemon version check.
	// Set via DisableVersionCheck() when the caller handles version
	// reporting itself (e.g. `nupi version`, `nupi daemon status`).
	skipVersionCheck bool
	warningWriter    io.Writer
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

	c, err := newFromStore()
	if err != nil {
		// Fall back to Unix socket if TCP config is unavailable.
		sockClient, sockErr := newFromUnixSocket()
		if sockErr == nil {
			return sockClient, nil
		}
		return nil, err
	}
	return c, nil
}

// NewUnixSocket creates a client connected via Unix socket at the default path.
func NewUnixSocket() (*Client, error) {
	return newFromUnixSocket()
}

func newFromUnixSocket() (*Client, error) {
	paths := config.GetInstancePaths("")
	sockPath := paths.Socket

	token := strings.TrimSpace(os.Getenv("NUPI_API_TOKEN"))
	if token == "" {
		// Try loading token from config store.
		_, tokens, err := LoadTransportSettings()
		if err == nil && len(tokens) > 0 {
			token = tokens[0]
		}
	}

	return dialUnixSocket(sockPath, token)
}

func dialUnixSocket(sockPath string, token string) (*Client, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return net.DialTimeout("unix", sockPath, constants.GRPCClientUnixDialTimeout)
		}),
	}

	conn, err := grpc.NewClient("unix:"+sockPath, opts...)
	if err != nil {
		return nil, fmt.Errorf("grpc: connect unix %s: %w", sockPath, err)
	}

	return newClientFromConn(conn, token), nil
}

func newFromStore() (*Client, error) {
	cfg, tokens, err := LoadTransportSettings()
	if err != nil {
		return nil, err
	}

	tlsEnabled := strings.TrimSpace(cfg.TLSCertPath) != "" && strings.TrimSpace(cfg.TLSKeyPath) != ""

	grpcBinding := strings.TrimSpace(cfg.GRPCBinding)
	if grpcBinding == "" {
		grpcBinding = cfg.Binding
	}
	host := DetermineHost(tlsEnabled)
	if override := strings.TrimSpace(os.Getenv("NUPI_DAEMON_HOST")); override != "" {
		host = override
	}

	grpcPort := cfg.GRPCPort
	if grpcPort <= 0 {
		return nil, fmt.Errorf("grpc: daemon gRPC port not available; is nupid running?")
	}

	address := net.JoinHostPort(host, fmt.Sprintf("%d", grpcPort))

	tlsConfig, err := PrepareTLSConfig(cfg, host, tlsEnabled)
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

	token, tlsOpts := ResolveExplicitOptions(boot)

	var tlsConfig *tls.Config
	if strings.EqualFold(u.Scheme, "https") {
		tlsConfig, err = TLSConfigForExplicit(u, &tlsOpts)
		if err != nil {
			return nil, err
		}
	}

	return dial(address, tlsConfig, token)
}

func dial(address string, tlsConfig *tls.Config, token string) (*Client, error) {
	opts := []grpc.DialOption{
		grpc.WithConnectParams(grpc.ConnectParams{
			MinConnectTimeout: constants.GRPCClientMinConnectTimeout,
		}),
	}
	if tlsConfig != nil {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.NewClient(passthroughPrefix+address, opts...)
	if err != nil {
		return nil, fmt.Errorf("grpc: connect %s: %w", address, err)
	}

	return newClientFromConn(conn, token), nil
}

func newClientFromConn(conn *grpc.ClientConn, token string) *Client {
	return &Client{
		conn:           conn,
		daemon:         apiv1.NewDaemonServiceClient(conn),
		sessions:       apiv1.NewSessionsServiceClient(conn),
		config:         apiv1.NewConfigServiceClient(conn),
		adapters:       apiv1.NewAdaptersServiceClient(conn),
		quickstart:     apiv1.NewQuickstartServiceClient(conn),
		adapterRuntime: apiv1.NewAdapterRuntimeServiceClient(conn),
		audio:          apiv1.NewAudioServiceClient(conn),
		auth:           apiv1.NewAuthServiceClient(conn),
		recordings:     apiv1.NewRecordingsServiceClient(conn),
		token:          strings.TrimSpace(token),
		warningWriter:  os.Stderr,
	}
}

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// ─── DaemonService ───

func (c *Client) DaemonStatus(ctx context.Context) (*apiv1.DaemonStatusResponse, error) {
	ctx = c.attachToken(ctx)
	return c.daemon.Status(ctx, &apiv1.DaemonStatusRequest{})
}

// ensureVersionChecked fetches the daemon version and prints a mismatch warning
// at most once per Client lifetime. Called from attachToken so that any CLI
// command that communicates with the daemon triggers the check (AC #2).
// If the check fails (daemon unreachable), it will retry on the next call
// instead of permanently giving up.
// DisableVersionCheck prevents the automatic daemon version check on this
// client. Use when the caller handles version comparison itself to avoid a
// redundant DaemonStatus RPC.
func (c *Client) DisableVersionCheck() {
	c.versionMu.Lock()
	c.skipVersionCheck = true
	c.versionMu.Unlock()
}

func (c *Client) ensureVersionChecked() {
	c.versionMu.Lock()
	if c.skipVersionCheck || c.versionChecked || c.versionChecking {
		c.versionMu.Unlock()
		return
	}
	c.versionChecking = true
	c.versionMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), constants.GRPCClientVersionCheckTimeout)
	defer cancel()
	ctx = c.withToken(ctx)
	resp, err := c.daemon.Status(ctx, &apiv1.DaemonStatusRequest{})

	c.versionMu.Lock()
	c.versionChecking = false
	if err != nil {
		c.versionMu.Unlock()
		return // daemon unreachable — will retry on next call
	}
	c.versionChecked = true
	c.versionMu.Unlock()

	if w := nupiversion.CheckVersionMismatch(resp.GetVersion()); w != "" {
		fmt.Fprintln(c.warningWriter, w)
	}
}

func (c *Client) Shutdown(ctx context.Context) (*apiv1.ShutdownResponse, error) {
	ctx = c.attachToken(ctx)
	return c.daemon.Shutdown(ctx, &apiv1.ShutdownRequest{})
}

func (c *Client) ReloadPlugins(ctx context.Context) (*apiv1.ReloadPluginsResponse, error) {
	ctx = c.attachToken(ctx)
	return c.daemon.ReloadPlugins(ctx, &apiv1.ReloadPluginsRequest{})
}

// ─── SessionsService ───

func (c *Client) ListSessions(ctx context.Context) (*apiv1.ListSessionsResponse, error) {
	ctx = c.attachToken(ctx)
	return c.sessions.ListSessions(ctx, &apiv1.ListSessionsRequest{})
}

func (c *Client) CreateSession(ctx context.Context, req *apiv1.CreateSessionRequest) (*apiv1.CreateSessionResponse, error) {
	ctx = c.attachToken(ctx)
	return c.sessions.CreateSession(ctx, req)
}

func (c *Client) GetSession(ctx context.Context, sessionID string) (*apiv1.GetSessionResponse, error) {
	ctx = c.attachToken(ctx)
	return c.sessions.GetSession(ctx, &apiv1.GetSessionRequest{SessionId: sessionID})
}

func (c *Client) KillSession(ctx context.Context, sessionID string) (*apiv1.KillSessionResponse, error) {
	ctx = c.attachToken(ctx)
	return c.sessions.KillSession(ctx, &apiv1.KillSessionRequest{SessionId: sessionID})
}

func (c *Client) SendInput(ctx context.Context, req *apiv1.SendInputRequest) (*apiv1.SendInputResponse, error) {
	ctx = c.attachToken(ctx)
	return c.sessions.SendInput(ctx, req)
}

func (c *Client) AttachSession(ctx context.Context) (apiv1.SessionsService_AttachSessionClient, error) {
	ctx = c.attachToken(ctx)
	return c.sessions.AttachSession(ctx)
}

func (c *Client) GetSessionMode(ctx context.Context, sessionID string) (*apiv1.GetSessionModeResponse, error) {
	ctx = c.attachToken(ctx)
	return c.sessions.GetSessionMode(ctx, &apiv1.GetSessionModeRequest{SessionId: sessionID})
}

func (c *Client) SetSessionMode(ctx context.Context, req *apiv1.SetSessionModeRequest) (*apiv1.SetSessionModeResponse, error) {
	ctx = c.attachToken(ctx)
	return c.sessions.SetSessionMode(ctx, req)
}

// ─── ConfigService ───

func (c *Client) TransportConfig(ctx context.Context) (*apiv1.TransportConfig, error) {
	ctx = c.attachToken(ctx)
	return c.config.GetTransportConfig(ctx, &emptypb.Empty{})
}

func (c *Client) UpdateTransportConfig(ctx context.Context, cfg *apiv1.TransportConfig) (*apiv1.TransportConfig, error) {
	ctx = c.attachToken(ctx)
	return c.config.UpdateTransportConfig(ctx, &apiv1.UpdateTransportConfigRequest{Config: cfg})
}

// ─── AdaptersService ───

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

// ─── QuickstartService ───

func (c *Client) QuickstartStatus(ctx context.Context) (*apiv1.QuickstartStatusResponse, error) {
	ctx = c.attachToken(ctx)
	return c.quickstart.GetStatus(ctx, &emptypb.Empty{})
}

func (c *Client) UpdateQuickstart(ctx context.Context, req *apiv1.UpdateQuickstartRequest) (*apiv1.QuickstartStatusResponse, error) {
	ctx = c.attachToken(ctx)
	return c.quickstart.Update(ctx, req)
}

// ─── AdapterRuntimeService ───

func (c *Client) AdaptersOverview(ctx context.Context) (*apiv1.AdaptersOverviewResponse, error) {
	ctx = c.attachToken(ctx)
	return c.adapterRuntime.Overview(ctx, &emptypb.Empty{})
}

func (c *Client) BindAdapter(ctx context.Context, req *apiv1.BindAdapterRequest) (*apiv1.AdapterActionResponse, error) {
	ctx = c.attachToken(ctx)
	return c.adapterRuntime.BindAdapter(ctx, req)
}

func (c *Client) StartAdapter(ctx context.Context, slot string) (*apiv1.AdapterActionResponse, error) {
	ctx = c.attachToken(ctx)
	return c.adapterRuntime.StartAdapter(ctx, &apiv1.AdapterSlotRequest{Slot: slot})
}

func (c *Client) StopAdapter(ctx context.Context, slot string) (*apiv1.AdapterActionResponse, error) {
	ctx = c.attachToken(ctx)
	return c.adapterRuntime.StopAdapter(ctx, &apiv1.AdapterSlotRequest{Slot: slot})
}

// ─── AudioService ───

func (c *Client) StreamAudioIn(ctx context.Context) (apiv1.AudioService_StreamAudioInClient, error) {
	ctx = c.attachToken(ctx)
	return c.audio.StreamAudioIn(ctx)
}

func (c *Client) StreamAudioOut(ctx context.Context, req *apiv1.StreamAudioOutRequest) (apiv1.AudioService_StreamAudioOutClient, error) {
	ctx = c.attachToken(ctx)
	return c.audio.StreamAudioOut(ctx, req)
}

func (c *Client) InterruptTTS(ctx context.Context, req *apiv1.InterruptTTSRequest) error {
	ctx = c.attachToken(ctx)
	_, err := c.audio.InterruptTTS(ctx, req)
	return err
}

func (c *Client) AudioCapabilities(ctx context.Context, req *apiv1.GetAudioCapabilitiesRequest) (*apiv1.GetAudioCapabilitiesResponse, error) {
	ctx = c.attachToken(ctx)
	return c.audio.GetAudioCapabilities(ctx, req)
}

// ─── AuthService ───

func (c *Client) ListTokens(ctx context.Context) (*apiv1.ListTokensResponse, error) {
	ctx = c.attachToken(ctx)
	return c.auth.ListTokens(ctx, &apiv1.ListTokensRequest{})
}

func (c *Client) CreateToken(ctx context.Context, req *apiv1.CreateTokenRequest) (*apiv1.CreateTokenResponse, error) {
	ctx = c.attachToken(ctx)
	return c.auth.CreateToken(ctx, req)
}

func (c *Client) DeleteToken(ctx context.Context, req *apiv1.DeleteTokenRequest) error {
	ctx = c.attachToken(ctx)
	_, err := c.auth.DeleteToken(ctx, req)
	return err
}

func (c *Client) ListPairings(ctx context.Context) (*apiv1.ListPairingsResponse, error) {
	ctx = c.attachToken(ctx)
	return c.auth.ListPairings(ctx, &apiv1.ListPairingsRequest{})
}

func (c *Client) CreatePairing(ctx context.Context, req *apiv1.CreatePairingRequest) (*apiv1.CreatePairingResponse, error) {
	ctx = c.attachToken(ctx)
	return c.auth.CreatePairing(ctx, req)
}

func (c *Client) ClaimPairing(ctx context.Context, req *apiv1.ClaimPairingRequest) (*apiv1.ClaimPairingResponse, error) {
	// ClaimPairing is auth-exempt, but include token if available.
	ctx = c.attachToken(ctx)
	return c.auth.ClaimPairing(ctx, req)
}

// ─── AdapterRuntimeService (extended) ───

func (c *Client) RegisterAdapter(ctx context.Context, req *apiv1.RegisterAdapterRequest) (*apiv1.RegisterAdapterResponse, error) {
	ctx = c.attachToken(ctx)
	return c.adapterRuntime.RegisterAdapter(ctx, req)
}

func (c *Client) StreamAdapterLogs(ctx context.Context, req *apiv1.StreamAdapterLogsRequest) (apiv1.AdapterRuntimeService_StreamAdapterLogsClient, error) {
	ctx = c.attachToken(ctx)
	return c.adapterRuntime.StreamAdapterLogs(ctx, req)
}

func (c *Client) GetAdapterLogs(ctx context.Context, req *apiv1.GetAdapterLogsRequest) (*apiv1.GetAdapterLogsResponse, error) {
	ctx = c.attachToken(ctx)
	return c.adapterRuntime.GetAdapterLogs(ctx, req)
}

// ─── ConfigService (extended) ───

func (c *Client) Migrate(ctx context.Context, req *apiv1.ConfigMigrateRequest) (*apiv1.ConfigMigrateResponse, error) {
	ctx = c.attachToken(ctx)
	return c.config.Migrate(ctx, req)
}

// ─── RecordingsService ───

func (c *Client) ListRecordings(ctx context.Context, req *apiv1.ListRecordingsRequest) (*apiv1.ListRecordingsResponse, error) {
	ctx = c.attachToken(ctx)
	return c.recordings.ListRecordings(ctx, req)
}

func (c *Client) GetRecording(ctx context.Context, req *apiv1.GetRecordingRequest) (apiv1.RecordingsService_GetRecordingClient, error) {
	ctx = c.attachToken(ctx)
	return c.recordings.GetRecording(ctx, req)
}

// ─── internal helpers ───

func (c *Client) attachToken(ctx context.Context) context.Context {
	c.ensureVersionChecked()
	return c.withToken(ctx)
}

// withToken appends the bearer token to outgoing gRPC metadata.
func (c *Client) withToken(ctx context.Context) context.Context {
	token := strings.TrimSpace(c.token)
	if token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
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
