package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Options configure additional behaviour for the gateway.
type Options struct {
	// RegisterGRPC allows callers to register additional gRPC services on the shared server.
	RegisterGRPC func(*grpc.Server)
}

// ListenerInfo represents a single listener started by the gateway.
type ListenerInfo struct {
	Scheme  string
	Address string
	Port    int
	Binding string
}

// Info summarises the listeners exposed by the gateway.
type Info struct {
	HTTP ListenerInfo
	GRPC ListenerInfo
}

// Gateway orchestrates HTTP and gRPC listeners exposed by the daemon.
type Gateway struct {
	apiServer *server.APIServer
	opts      Options

	mu           sync.RWMutex
	httpPrepared *server.PreparedHTTPServer
	httpListener net.Listener
	grpcServer   *grpc.Server
	grpcListener net.Listener
	errCh        chan error
	wg           sync.WaitGroup
	info         Info
}

// New constructs a Gateway bound to the provided API server.
func New(api *server.APIServer, opts ...Options) *Gateway {
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}
	return &Gateway{
		apiServer: api,
		opts:      opt,
	}
}

// Start launches HTTP and gRPC listeners. It must not be called concurrently with Shutdown.
func (g *Gateway) Start(ctx context.Context) (*Info, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.httpListener != nil || g.grpcListener != nil {
		return nil, fmt.Errorf("gateway: already started")
	}

	prepared, err := g.apiServer.Prepare(ctx)
	if err != nil {
		return nil, fmt.Errorf("gateway: prepare http server: %w", err)
	}

	httpListener, err := net.Listen("tcp", prepared.Server.Addr)
	if err != nil {
		return nil, fmt.Errorf("gateway: listen http: %w", err)
	}

	httpPort := listenerPort(httpListener)

	grpcBinding := prepared.GRPCBinding
	if grpcBinding == "" {
		grpcBinding = prepared.Binding
	}
	grpcHost, err := resolveBindingHost(grpcBinding)
	if err != nil {
		_ = httpListener.Close()
		return nil, err
	}

	grpcAddr := net.JoinHostPort(grpcHost, strconv.Itoa(prepared.GRPCPort))
	if prepared.GRPCPort <= 0 {
		grpcAddr = net.JoinHostPort(grpcHost, "0")
	}

	grpcListener, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		_ = httpListener.Close()
		return nil, fmt.Errorf("gateway: listen grpc: %w", err)
	}

	grpcPort := listenerPort(grpcListener)

	grpcOpts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(g.unaryAuthInterceptor),
		grpc.ChainStreamInterceptor(g.streamAuthInterceptor),
	}
	if prepared.UseTLS {
		creds, err := credentials.NewServerTLSFromFile(prepared.CertPath, prepared.KeyPath)
		if err != nil {
			_ = httpListener.Close()
			_ = grpcListener.Close()
			return nil, fmt.Errorf("gateway: load tls credentials: %w", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
	}

	grpcServer := grpc.NewServer(grpcOpts...)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	if g.opts.RegisterGRPC != nil {
		g.opts.RegisterGRPC(grpcServer)
	}

	g.httpPrepared = prepared
	g.httpListener = httpListener
	g.grpcServer = grpcServer
	g.grpcListener = grpcListener
	g.errCh = make(chan error, 2)
	g.info = Info{
		HTTP: ListenerInfo{
			Scheme:  prepared.Scheme,
			Address: httpListener.Addr().String(),
			Port:    httpPort,
			Binding: prepared.Binding,
		},
		GRPC: ListenerInfo{
			Scheme:  grpcScheme(prepared.UseTLS),
			Address: grpcListener.Addr().String(),
			Port:    grpcPort,
			Binding: grpcBinding,
		},
	}
	errCh := g.errCh

	g.wg.Add(2)
	go g.serveHTTP(ctx, prepared, httpListener)
	go g.serveGRPC(ctx, grpcServer, grpcListener)

	go func(ch chan error) {
		g.wg.Wait()
		if ch != nil {
			close(ch)
		}
	}(errCh)

	g.apiServer.UpdateActualPort(context.Background(), httpPort)
	g.apiServer.UpdateActualGRPCPort(context.Background(), grpcPort)

	infoCopy := g.info
	return &infoCopy, nil
}

func (g *Gateway) serveHTTP(ctx context.Context, prepared *server.PreparedHTTPServer, listener net.Listener) {
	defer g.wg.Done()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := g.apiServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
			g.pushError(err)
		}
	}()

	var err error
	if prepared.UseTLS {
		err = prepared.Server.ServeTLS(listener, prepared.CertPath, prepared.KeyPath)
	} else {
		err = prepared.Server.Serve(listener)
	}

	if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		g.pushError(err)
	}
}

func (g *Gateway) serveGRPC(ctx context.Context, grpcServer *grpc.Server, listener net.Listener) {
	defer g.wg.Done()

	go func() {
		<-ctx.Done()
		done := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			grpcServer.Stop()
		}
	}()

	if err := grpcServer.Serve(listener); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, grpc.ErrServerStopped) && status.Code(err) != codes.Canceled {
		g.pushError(err)
	}
}

func (g *Gateway) pushError(err error) {
	if err == nil {
		return
	}
	g.mu.RLock()
	ch := g.errCh
	g.mu.RUnlock()
	if ch == nil {
		return
	}
	select {
	case ch <- err:
	default:
	}
}

// Shutdown stops all listeners and waits for goroutines to exit.
func (g *Gateway) Shutdown(ctx context.Context) error {
	g.mu.Lock()
	httpListener := g.httpListener
	grpcListener := g.grpcListener
	grpcServer := g.grpcServer
	prepared := g.httpPrepared
	errCh := g.errCh
	g.httpListener = nil
	g.grpcListener = nil
	g.grpcServer = nil
	g.httpPrepared = nil
	g.errCh = nil
	g.mu.Unlock()

	if httpListener == nil && grpcListener == nil && prepared == nil {
		return nil
	}

	if httpListener != nil {
		_ = httpListener.Close()
	}
	if grpcListener != nil {
		_ = grpcListener.Close()
	}

	if prepared != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := g.apiServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
			cancel()
			return err
		}
		cancel()
	}

	if grpcServer != nil {
		done := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			grpcServer.Stop()
		}
	}

	g.wg.Wait()

	if errCh != nil {
		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
		default:
		}
	}

	return nil
}

// Errors exposes the gateway error channel (closed when the gateway stops).
func (g *Gateway) Errors() <-chan error {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if g.errCh == nil {
		ch := make(chan error)
		close(ch)
		return ch
	}
	return g.errCh
}

// Info returns the last known listener info.
func (g *Gateway) Info() Info {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.info
}

func (g *Gateway) unaryAuthInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	if !g.apiServer.AuthRequired() {
		return handler(ctx, req)
	}

	// ClaimPairing is public — new clients use it to obtain their first token.
	if info.FullMethod == "/nupi.api.v1.AuthService/ClaimPairing" {
		return handler(ctx, req)
	}

	token := tokenFromMetadata(ctx)
	if token == "" {
		return nil, status.Error(codes.Unauthenticated, "unauthorized")
	}
	meta, ok := g.apiServer.AuthenticateToken(token)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "unauthorized")
	}
	ctx = g.apiServer.ContextWithToken(ctx, meta)
	return handler(ctx, req)
}

func (g *Gateway) streamAuthInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if !g.apiServer.AuthRequired() {
		return handler(srv, ss)
	}

	token := tokenFromMetadata(ss.Context())
	if token == "" {
		return status.Error(codes.Unauthenticated, "unauthorized")
	}
	meta, ok := g.apiServer.AuthenticateToken(token)
	if !ok {
		return status.Error(codes.Unauthenticated, "unauthorized")
	}
	wrapped := &authenticatedStream{
		ServerStream: ss,
		ctx:          g.apiServer.ContextWithToken(ss.Context(), meta),
	}
	return handler(srv, wrapped)
}

func tokenFromMetadata(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}

	if values := md.Get("authorization"); len(values) > 0 {
		if token := parseBearer(values[0]); token != "" {
			return token
		}
	}

	if values := md.Get("x-nupi-token"); len(values) > 0 {
		token := strings.TrimSpace(values[0])
		if token != "" {
			return token
		}
	}

	if values := md.Get("token"); len(values) > 0 {
		token := strings.TrimSpace(values[0])
		if token != "" {
			return token
		}
	}

	return ""
}

func parseBearer(header string) string {
	header = strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}

func listenerPort(l net.Listener) int {
	if tcp, ok := l.Addr().(*net.TCPAddr); ok {
		return tcp.Port
	}
	return 0
}

func resolveBindingHost(binding string) (string, error) {
	switch binding {
	case "", "loopback":
		return "127.0.0.1", nil
	case "lan", "public":
		return "0.0.0.0", nil
	default:
		return "", fmt.Errorf("unknown binding %q", binding)
	}
}

type authenticatedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authenticatedStream) Context() context.Context {
	return s.ctx
}

func grpcScheme(useTLS bool) string {
	if useTLS {
		return "grpcs"
	}
	return "grpc"
}
