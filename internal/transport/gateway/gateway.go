package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/language"
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
	// GRPCUnixSocket is an optional Unix socket path on which the gRPC server
	// will also listen. When set, the same gRPC server serves on both the TCP
	// listener and this Unix socket.
	GRPCUnixSocket string
	// ConnectEnabled activates the Connect RPC (HTTP) transport alongside gRPC.
	// When true, a separate HTTP listener is started that serves Connect
	// protocol via a Vanguard transcoder wrapping the gRPC server.
	ConnectEnabled bool
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
	GRPC    ListenerInfo
	Connect ListenerInfo // Connect RPC (HTTP) transport; zero value if not enabled.
}

// Gateway orchestrates gRPC and Connect RPC listeners exposed by the daemon.
type Gateway struct {
	apiServer *server.APIServer
	opts      Options

	mu               sync.RWMutex
	grpcServer       *grpc.Server
	grpcListener     net.Listener
	grpcUnixListener net.Listener
	connectServer    *http.Server
	connectListener  net.Listener
	cancelServe      context.CancelFunc
	errCh            chan error
	wg               sync.WaitGroup
	info             Info
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

// Start launches gRPC listeners. It must not be called concurrently with Shutdown.
func (g *Gateway) Start(ctx context.Context) (*Info, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.grpcListener != nil {
		return nil, fmt.Errorf("gateway: already started")
	}

	prepared, err := g.apiServer.Prepare(ctx)
	if err != nil {
		return nil, fmt.Errorf("gateway: prepare transport: %w", err)
	}

	grpcBinding := prepared.GRPCBinding
	grpcHost, err := resolveBindingHost(grpcBinding)
	if err != nil {
		return nil, err
	}

	grpcAddr := net.JoinHostPort(grpcHost, strconv.Itoa(prepared.GRPCPort))
	if prepared.GRPCPort <= 0 {
		grpcAddr = net.JoinHostPort(grpcHost, "0")
	}

	grpcListener, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return nil, fmt.Errorf("gateway: listen grpc: %w", err)
	}

	grpcPort := listenerPort(grpcListener)

	grpcOpts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(unaryLanguageInterceptor, g.unaryAuthInterceptor),
		grpc.ChainStreamInterceptor(streamLanguageInterceptor, g.streamAuthInterceptor),
	}
	if prepared.UseTLS {
		creds, err := credentials.NewServerTLSFromFile(prepared.CertPath, prepared.KeyPath)
		if err != nil {
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

	// Optionally create a Unix socket listener for the same gRPC server.
	var grpcUnixListener net.Listener
	if g.opts.GRPCUnixSocket != "" {
		if err := os.MkdirAll(filepath.Dir(g.opts.GRPCUnixSocket), 0o755); err != nil {
			_ = grpcListener.Close()
			return nil, fmt.Errorf("gateway: create grpc socket dir: %w", err)
		}
		if err := os.Remove(g.opts.GRPCUnixSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = grpcListener.Close()
			return nil, fmt.Errorf("gateway: remove existing grpc socket: %w", err)
		}
		grpcUnixListener, err = net.Listen("unix", g.opts.GRPCUnixSocket)
		if err != nil {
			_ = grpcListener.Close()
			return nil, fmt.Errorf("gateway: listen grpc unix: %w", err)
		}
		if err := os.Chmod(g.opts.GRPCUnixSocket, 0o600); err != nil {
			_ = grpcListener.Close()
			_ = grpcUnixListener.Close()
			return nil, fmt.Errorf("gateway: chmod grpc socket: %w", err)
		}
	}

	numListeners := 1
	if grpcUnixListener != nil {
		numListeners = 2
	}

	// Create a child context so Shutdown can cancel serve goroutines and
	// WebSocket connections without relying on the caller's context lifecycle.
	serveCtx, cancelServe := context.WithCancel(ctx)
	g.cancelServe = cancelServe

	// Optionally start a Connect RPC (HTTP) transport using Vanguard transcoder.
	var connectListener net.Listener
	var connectServer *http.Server
	connectUseTLS := false
	if g.opts.ConnectEnabled {
		transcoder, tErr := server.NewConnectTranscoder(grpcServer)
		if tErr != nil {
			cancelServe()
			_ = grpcListener.Close()
			if grpcUnixListener != nil {
				_ = grpcUnixListener.Close()
			}
			return nil, fmt.Errorf("gateway: create connect transcoder: %w", tErr)
		}
		connectAddr := net.JoinHostPort(grpcHost, "0")
		connectListener, err = net.Listen("tcp", connectAddr)
		if err != nil {
			cancelServe()
			_ = grpcListener.Close()
			if grpcUnixListener != nil {
				_ = grpcUnixListener.Close()
			}
			return nil, fmt.Errorf("gateway: listen connect: %w", err)
		}
		if prepared.UseTLS {
			// Cert loaded separately from gRPC's NewServerTLSFromFile — intentional
			// simplicity over sharing a single tls.Certificate across transports.
			cert, certErr := tls.LoadX509KeyPair(prepared.CertPath, prepared.KeyPath)
			if certErr != nil {
				cancelServe()
				_ = grpcListener.Close()
				_ = connectListener.Close()
				if grpcUnixListener != nil {
					_ = grpcUnixListener.Close()
				}
				return nil, fmt.Errorf("gateway: load connect tls: %w", certErr)
			}
			connectListener = tls.NewListener(connectListener, &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			})
			connectUseTLS = true
		}
		// Route WebSocket paths to the session bridge handler, all other
		// paths to the Vanguard transcoder with a body-size limit.
		// WebSocket must NOT be wrapped in MaxBytesHandler (streaming has
		// no fixed body size) and WriteTimeout does not affect hijacked
		// connections.
		mux := http.NewServeMux()
		mux.Handle("/ws/session/", newWSSessionHandler(g.apiServer, serveCtx))
		mux.Handle("/", http.MaxBytesHandler(transcoder, 4<<20)) // 4 MB body limit
		connectServer = &http.Server{
			Handler:        mux,
			ReadTimeout:    30 * time.Second,
			WriteTimeout:   30 * time.Second,
			IdleTimeout:    120 * time.Second,
			MaxHeaderBytes: 1 << 20, // 1 MB
			ErrorLog:       log.New(log.Default().Writer(), "Transport gateway Connect: ", log.LstdFlags), // writer captured at Start time
		}
		numListeners++
	}

	g.grpcServer = grpcServer
	g.grpcListener = grpcListener
	g.grpcUnixListener = grpcUnixListener
	g.connectServer = connectServer
	g.connectListener = connectListener
	g.errCh = make(chan error, numListeners)
	g.info = Info{
		GRPC: ListenerInfo{
			Scheme:  grpcScheme(prepared.UseTLS),
			Address: grpcListener.Addr().String(),
			Port:    grpcPort,
			Binding: grpcBinding,
		},
	}
	if connectListener != nil {
		connectScheme := "http"
		if connectUseTLS {
			connectScheme = "https"
		}
		g.info.Connect = ListenerInfo{
			Scheme:  connectScheme,
			Address: connectListener.Addr().String(),
			Port:    listenerPort(connectListener),
			Binding: grpcBinding,
		}
	}
	errCh := g.errCh

	g.wg.Add(numListeners)
	go g.serveGRPC(serveCtx, grpcServer, grpcListener)
	if grpcUnixListener != nil {
		go g.serveGRPC(serveCtx, grpcServer, grpcUnixListener)
		log.Printf("Transport gateway gRPC Unix socket listening on %s", g.opts.GRPCUnixSocket)
	}
	if connectListener != nil {
		go g.serveConnect(serveCtx, connectServer, connectListener)
	}

	go func(ch chan error) {
		g.wg.Wait()
		if ch != nil {
			close(ch)
		}
	}(errCh)

	g.apiServer.UpdateActualGRPCPort(context.Background(), grpcPort)

	infoCopy := g.info
	return &infoCopy, nil
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

func (g *Gateway) serveConnect(ctx context.Context, srv *http.Server, listener net.Listener) {
	defer g.wg.Done()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			// Forcibly close active connections that didn't drain in time,
			// mirroring grpcServer.Stop() fallback in serveGRPC.
			srv.Close()
		}
	}()

	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
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
// The ctx parameter controls how long this method blocks waiting for the
// Connect HTTP server drain (capped at 5 s). The actual drain runs
// independently in the serveConnect goroutine with its own 5 s timeout,
// so the server always gets a full 5 s to drain regardless of ctx.
// The gRPC GracefulStop uses a separate hardcoded 5 s timeout and does
// not use ctx — grpc.Server does not accept a context for GracefulStop.
func (g *Gateway) Shutdown(ctx context.Context) error {
	g.mu.Lock()
	grpcListener := g.grpcListener
	grpcUnixListener := g.grpcUnixListener
	grpcServer := g.grpcServer
	connectServer := g.connectServer
	connectListener := g.connectListener
	cancelServe := g.cancelServe
	errCh := g.errCh
	g.grpcListener = nil
	g.grpcUnixListener = nil
	g.grpcServer = nil
	g.connectServer = nil
	g.connectListener = nil
	g.cancelServe = nil
	g.errCh = nil
	g.mu.Unlock()

	if grpcListener == nil && grpcServer == nil {
		return nil
	}

	// Cancel the serve context to unblock goroutines waiting on ctx.Done().
	if cancelServe != nil {
		cancelServe()
	}

	if grpcListener != nil {
		_ = grpcListener.Close()
	}
	if grpcUnixListener != nil {
		_ = grpcUnixListener.Close()
	}
	if connectListener != nil {
		_ = connectListener.Close()
	}

	if connectServer != nil {
		shutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = connectServer.Shutdown(shutCtx)
	}

	// GracefulStop is also called in serveGRPC's ctx.Done handler — the
	// duplicate call here is intentional defense-in-depth. Both GracefulStop
	// and http.Server.Shutdown are idempotent and safe to call concurrently.
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

	// Clean up Unix socket file.
	if g.opts.GRPCUnixSocket != "" {
		_ = os.Remove(g.opts.GRPCUnixSocket)
	}

	if errCh != nil {
		var errs []error
		for {
			select {
			case err, ok := <-errCh:
				if !ok {
					goto done
				}
				if err != nil && !errors.Is(err, context.Canceled) {
					errs = append(errs, err)
				}
			default:
				goto done
			}
		}
	done:
		if len(errs) > 0 {
			return errors.Join(errs...)
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

	return ""
}

func parseBearer(header string) string {
	header = strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}

// unaryLanguageInterceptor reads the nupi-language gRPC metadata header,
// validates it against the language registry, and stores the expanded
// Language record in the request context. If no header is present the
// request passes through unchanged. An invalid code is rejected.
func unaryLanguageInterceptor(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	ctx, err := extractLanguageToContext(ctx)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// streamLanguageInterceptor is the streaming counterpart.
func streamLanguageInterceptor(srv interface{}, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx, err := extractLanguageToContext(ss.Context())
	if err != nil {
		return err
	}
	return handler(srv, &languageStream{ServerStream: ss, ctx: ctx})
}

// extractLanguageToContext reads nupi-language from gRPC metadata, validates,
// expands, and returns a context with the language attached. If no header is
// present the original context is returned. An invalid code returns an error.
func extractLanguageToContext(ctx context.Context) (context.Context, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx, nil
	}
	values := md.Get(language.GRPCHeader)
	if len(values) == 0 || strings.TrimSpace(values[0]) == "" {
		return ctx, nil
	}
	code := strings.TrimSpace(values[0])
	lang, ok := language.Lookup(code)
	if !ok {
		return ctx, status.Errorf(codes.InvalidArgument, "invalid language code: %q", code)
	}
	return language.WithLanguage(ctx, lang), nil
}

// languageStream wraps a grpc.ServerStream with an enriched context.
type languageStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *languageStream) Context() context.Context { return s.ctx }

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
