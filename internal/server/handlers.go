package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/session"
	"github.com/nupi-ai/nupi/internal/termresize"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// APIServer handles WebSocket connections only
// RuntimeInfoProvider defines methods required to expose runtime metadata.
type RuntimeInfoProvider interface {
	Port() int
	GRPCPort() int
	StartTime() time.Time
}

// ConversationStore exposes a readonly view of conversation history.
type ConversationStore interface {
	Context(sessionID string) []eventbus.ConversationTurn
	Slice(sessionID string, offset, limit int) (int, []eventbus.ConversationTurn)
	GlobalContext() []eventbus.ConversationTurn
	GlobalSlice(offset, limit int) (int, []eventbus.ConversationTurn)
}

// PrometheusExporter renders observability metrics in Prometheus exposition format.
type PrometheusExporter interface {
	Export() []byte
}

type tokenRole string

const (
	roleAdmin    tokenRole = "admin"
	roleReadOnly tokenRole = "read-only"
)

type streamLogTestHooksKey struct{}

type streamLogTestHooks struct {
	ready   chan struct{}
	emitted chan struct{}
}

const conversationMaxPageLimit = 500

// parseQueryIntParam extracts a non-negative integer query parameter.
// Returns (value, provided, error).
func parseQueryIntParam(query url.Values, name string) (int, bool, error) {
	raw := strings.TrimSpace(query.Get(name))
	if raw == "" {
		return 0, false, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, true, err
	}
	if value < 0 {
		return 0, true, fmt.Errorf("value must be non-negative")
	}
	return value, true, nil
}

const (
	maxMetadataEntries      = 32
	maxMetadataKeyRunes     = 64
	maxMetadataValueRunes   = 512
	maxMetadataTotalPayload = 4096
)

type storedToken struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	Name      string    `json:"name,omitempty"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used_at,omitempty"`
}

type pairingEntry struct {
	Code      string    `json:"code"`
	Name      string    `json:"name,omitempty"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type authContextKey struct{}

// transportConfig groups network transport settings protected by a single
// read-write mutex. Embedded in APIServer so that promoted fields keep existing
// call-sites compiling without changes.
type transportConfig struct {
	transportMu    sync.RWMutex
	binding        string
	port           int
	tlsCertPath    string
	tlsKeyPath     string
	allowedOrigins []string
	grpcBinding    string
	grpcPort       int
}

// originAllowed reports whether the given Origin header is acceptable.
func (tc *transportConfig) originAllowed(origin string) bool {
	if origin == "" {
		return false
	}

	if origin == "tauri://localhost" ||
		origin == "https://tauri.localhost" ||
		origin == "https://tauri.local" ||
		strings.HasPrefix(origin, "https://tauri.local:") ||
		origin == "http://localhost" ||
		origin == "http://127.0.0.1" ||
		strings.HasPrefix(origin, "http://localhost:") ||
		strings.HasPrefix(origin, "http://127.0.0.1:") {
		return true
	}

	tc.transportMu.RLock()
	defer tc.transportMu.RUnlock()
	for _, allowed := range tc.allowedOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
}

// authState groups authentication tokens and settings protected by a single
// read-write mutex.
type authState struct {
	authMu       sync.RWMutex
	authTokens   map[string]storedToken
	authRequired bool
}

// lookupToken retrieves a stored token by its raw value.
func (a *authState) lookupToken(token string) (storedToken, bool) {
	if token == "" {
		return storedToken{}, false
	}
	a.authMu.RLock()
	defer a.authMu.RUnlock()
	entry, ok := a.authTokens[token]
	return entry, ok
}

// isAuthRequired reports whether token-based authentication is enforced.
func (a *authState) isAuthRequired() bool {
	a.authMu.RLock()
	defer a.authMu.RUnlock()
	return a.authRequired
}

// setAuthTokens replaces the active token set atomically.
func (a *authState) setAuthTokens(tokens []storedToken, authRequired bool) {
	tokenMap := make(map[string]storedToken, len(tokens))
	for _, token := range sanitizeStoredTokens(tokens) {
		tokenMap[token.Token] = token
	}

	a.authMu.Lock()
	a.authTokens = tokenMap
	a.authRequired = authRequired
	a.authMu.Unlock()
}

type APIServer struct {
	// Service dependencies (immutable after init)
	sessionManager *session.Manager
	configStore    *configstore.Store
	runtime        RuntimeInfoProvider
	wsServer       *Server
	conversation   ConversationStore
	adapters       AdaptersController
	audioIngress   AudioCaptureProvider
	audioEgress    AudioPlaybackController
	eventBus       *eventbus.Bus
	resizeManager  *termresize.Manager
	httpServer     *http.Server

	// Lifecycle sync
	listenerOnce   sync.Once
	wsRunOnce      sync.Once
	hookMu         sync.Mutex
	transportHooks []func()

	// Grouped state (embedded — fields promoted)
	transportConfig
	authState

	// Shutdown
	shutdownMu sync.RWMutex
	shutdownFn func(context.Context) error

	// Observability
	metricsExporter PrometheusExporter
	pluginWarnings  PluginWarningsProvider
}

// TransportSnapshot captures the runtime server transport settings.
type TransportSnapshot struct {
	Binding        string
	Port           int
	TLSCertPath    string
	TLSKeyPath     string
	AllowedOrigins []string
	GRPCBinding    string
	GRPCPort       int
	TLSCertModTime time.Time
	TLSKeyModTime  time.Time
}

// NewAPIServer creates a new API server
func NewAPIServer(sessionManager *session.Manager, configStore *configstore.Store, runtime RuntimeInfoProvider, port int) (*APIServer, error) {
	resizeManager, err := termresize.NewManagerWithDefaults()
	if err != nil {
		return nil, fmt.Errorf("failed to initialise resize manager: %w", err)
	}

	wsServer := NewServer(sessionManager, resizeManager)

	apiServer := &APIServer{
		sessionManager: sessionManager,
		configStore:    configStore,
		runtime:        runtime,
		wsServer:       wsServer,
		resizeManager:  resizeManager,
	}
	apiServer.port = port

	apiServer.registerSessionListener()

	return apiServer, nil
}

// SetShutdownFunc registers a handler invoked when /daemon/shutdown is called.
func (s *APIServer) SetShutdownFunc(fn func(context.Context) error) {
	s.shutdownMu.Lock()
	s.shutdownFn = fn
	s.shutdownMu.Unlock()
}

// RequestShutdown triggers a graceful daemon shutdown using the registered
// shutdown function. It is safe to call from any goroutine and returns
// immediately — the actual shutdown proceeds asynchronously.
func (s *APIServer) RequestShutdown() {
	s.shutdownMu.RLock()
	fn := s.shutdownFn
	s.shutdownMu.RUnlock()
	if fn != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := fn(ctx); err != nil {
				log.Printf("[APIServer] shutdown error: %v", err)
			}
		}()
	}
}

// SetConversationStore wires the conversation state provider used by HTTP handlers.
func (s *APIServer) SetConversationStore(store ConversationStore) {
	s.conversation = store
}

// SetAdaptersController wires the adapter controller used by HTTP handlers.
func (s *APIServer) SetAdaptersController(controller AdaptersController) {
	s.adapters = controller
}

// SetAudioIngress wires the audio ingress handler used by streaming endpoints.
func (s *APIServer) SetAudioIngress(provider AudioCaptureProvider) {
	s.audioIngress = provider
}

// SetAudioEgress wires the audio egress handler used by playback endpoints.
func (s *APIServer) SetAudioEgress(controller AudioPlaybackController) {
	s.audioEgress = controller
}

// SetEventBus wires the event bus used for publishing audio/control events.
func (s *APIServer) SetEventBus(bus *eventbus.Bus) {
	s.eventBus = bus
}

// SetMetricsExporter wires the metrics exporter used by the /metrics endpoint.
func (s *APIServer) SetMetricsExporter(exporter PrometheusExporter) {
	s.metricsExporter = exporter
}

// SetPluginWarningsProvider wires the plugin warnings provider used by the /plugins/warnings endpoint.
func (s *APIServer) SetPluginWarningsProvider(provider PluginWarningsProvider) {
	s.pluginWarnings = provider
}

func websocketScheme(r *http.Request) string {
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return "wss"
	}
	return "ws"
}

// wrapWithCORS adds CORS headers for WebSocket connections from Tauri app and configured origins.
func (s *APIServer) wrapWithCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		if s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}

		// Handle preflight requests
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}


func (s *APIServer) wrapWithSecurity(next http.Handler) http.Handler {
	corsHandler := s.wrapWithCORS(next)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicAuthEndpoint(r) {
			corsHandler.ServeHTTP(w, r)
			return
		}

		if !s.isAuthRequired() {
			corsHandler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, storedToken{Role: string(roleAdmin)})))
			return
		}

		tokenValue := extractAuthToken(r)
		info, ok := s.lookupToken(tokenValue)
		if tokenValue == "" || !ok {
			writeUnauthorized(w)
			return
		}

		ctx := context.WithValue(r.Context(), authContextKey{}, info)
		corsHandler.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isPublicAuthEndpoint(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "/auth/pair" && (r.Method == http.MethodPost || r.Method == http.MethodOptions) {
		return true
	}
	return false
}


func (s *APIServer) validateToken(token string) bool {
	_, ok := s.lookupToken(token)
	return ok
}

// ValidateAuthToken verifies the supplied API token against the active allowlist.
func (s *APIServer) ValidateAuthToken(token string) bool {
	return s.validateToken(token)
}

func tokenFromContext(ctx context.Context) (storedToken, bool) {
	if ctx == nil {
		return storedToken{}, false
	}
	value := ctx.Value(authContextKey{})
	if token, ok := value.(storedToken); ok && strings.TrimSpace(token.Token) != "" {
		return token, true
	}
	return storedToken{}, false
}

// AuthenticateToken returns token metadata if present in the allowlist.
func (s *APIServer) AuthenticateToken(token string) (storedToken, bool) {
	return s.lookupToken(token)
}

// ContextWithToken attaches token metadata to the provided context.
func (s *APIServer) ContextWithToken(ctx context.Context, token storedToken) context.Context {
	return context.WithValue(ctx, authContextKey{}, token)
}

func (s *APIServer) requireRoleGRPC(ctx context.Context, allowed ...tokenRole) (storedToken, error) {
	if !s.isAuthRequired() {
		return storedToken{Role: string(roleAdmin)}, nil
	}
	token, ok := tokenFromContext(ctx)
	if !ok {
		return storedToken{}, status.Error(codes.Unauthenticated, "unauthorized")
	}
	if len(allowed) == 0 || hasRole(token.Role, allowed...) {
		return token, nil
	}
	return storedToken{}, status.Error(codes.PermissionDenied, "forbidden")
}

func hasRole(actual string, allowed ...tokenRole) bool {
	actual = normalizeRole(actual)
	if actual == string(roleAdmin) {
		return true
	}
	for _, role := range allowed {
		if actual == string(role) {
			return true
		}
	}
	return false
}

func (s *APIServer) requireRole(w http.ResponseWriter, r *http.Request, allowed ...tokenRole) (storedToken, bool) {
	if !s.isAuthRequired() {
		return storedToken{Role: string(roleAdmin)}, true
	}
	token, ok := tokenFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return storedToken{}, false
	}
	if len(allowed) == 0 || hasRole(token.Role, allowed...) {
		return token, true
	}
	http.Error(w, "forbidden", http.StatusForbidden)
	return storedToken{}, false
}

// AuthRequired reports whether transport-level authentication is enforced.
func (s *APIServer) AuthRequired() bool {
	return s.isAuthRequired()
}

func (s *APIServer) loadAuthTokens(ctx context.Context) ([]storedToken, error) {
	if s.configStore == nil {
		return nil, nil
	}

	values, err := s.configStore.LoadSecuritySettings(ctx, "auth.http_tokens")
	if err != nil {
		return nil, err
	}

	raw, ok := values["auth.http_tokens"]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	var structured []storedToken
	if err := json.Unmarshal([]byte(raw), &structured); err == nil {
		return sanitizeStoredTokens(structured), nil
	}

	var legacy []string
	if err := json.Unmarshal([]byte(raw), &legacy); err == nil {
		legacy = sanitizeTokens(legacy)
		tokens := make([]storedToken, 0, len(legacy))
		for _, token := range legacy {
			tokens = append(tokens, newStoredToken(token, "", string(roleAdmin)))
		}
		return tokens, nil
	}

	return nil, fmt.Errorf("parse auth.http_tokens: %w", err)
}

func (s *APIServer) ensureAuthTokens(ctx context.Context, required bool) ([]storedToken, string, error) {
	if !required {
		s.setAuthTokens(nil, false)
		return nil, "", nil
	}

	tokens, err := s.loadAuthTokens(ctx)
	if err != nil {
		return nil, "", err
	}

	if len(tokens) == 0 {
		token, genErr := generateAPIToken()
		if genErr != nil {
			return nil, "", genErr
		}
		tokens = []storedToken{newStoredToken(token, "default", string(roleAdmin))}
		if err := s.storeAuthTokens(ctx, tokens); err != nil {
			return nil, "", err
		}
		s.setAuthTokens(tokens, true)
		return tokens, token, nil
	}

	s.setAuthTokens(tokens, true)
	return tokens, "", nil
}

func (s *APIServer) storeAuthTokens(ctx context.Context, tokens []storedToken) error {
	if s.configStore == nil {
		return nil
	}
	payload, err := json.Marshal(tokens)
	if err != nil {
		return err
	}
	return s.configStore.SaveSecuritySettings(ctx, map[string]string{
		"auth.http_tokens": string(payload),
	})
}

func extractAuthToken(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
			return strings.TrimSpace(authHeader[7:])
		}
	}

	if headerToken := r.Header.Get("X-Nupi-Token"); headerToken != "" {
		return strings.TrimSpace(headerToken)
	}

	if queryToken := r.URL.Query().Get("token"); queryToken != "" {
		return strings.TrimSpace(queryToken)
	}

	return ""
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="nupi"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// Start starts the HTTP/WebSocket server.
func (s *APIServer) Start() error {
	prepared, err := s.Prepare(context.Background())
	if err != nil {
		return err
	}
	if prepared.UseTLS {
		return prepared.Server.ListenAndServeTLS(prepared.CertPath, prepared.KeyPath)
	}
	return prepared.Server.ListenAndServe()
}

// Shutdown gracefully shuts down the HTTP server
func (s *APIServer) Shutdown(ctx context.Context) error {
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

// PreparedHTTPServer holds metadata about a prepared HTTP server instance.
type PreparedHTTPServer struct {
	Server      *http.Server
	UseTLS      bool
	CertPath    string
	KeyPath     string
	Scheme      string
	Binding     string
	GRPCBinding string
	GRPCPort    int
}

// Prepare initialises the HTTP server without starting to serve, allowing the caller to manage the listener lifecycle.
func (s *APIServer) Prepare(ctx context.Context) (*PreparedHTTPServer, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	// Ensure listener registered (idempotent across Start/New)
	s.registerSessionListener()

	// Start WebSocket server goroutine once
	s.wsRunOnce.Do(func() {
		go s.wsServer.Run()
	})

	s.transportMu.RLock()
	fallbackPort := s.port
	s.transportMu.RUnlock()

	cfg := configstore.TransportConfig{
		Binding:     "loopback",
		Port:        fallbackPort,
		GRPCBinding: "",
		GRPCPort:    0,
	}
	if s.configStore != nil {
		storedCfg, err := s.configStore.GetTransportConfig(ctx)
		if err != nil {
			return nil, err
		}
		cfg = storedCfg
	}

	binding := normalizeBinding(cfg.Binding)
	host, err := resolveBindingHost(binding)
	if err != nil {
		return nil, err
	}

	grpcBinding := normalizeBinding(cfg.GRPCBinding)
	if grpcBinding == "" {
		grpcBinding = binding
	}
	if _, err := resolveBindingHost(grpcBinding); err != nil {
		return nil, err
	}

	port := cfg.Port
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("invalid HTTP port %d", port)
	}

	certPath := strings.TrimSpace(cfg.TLSCertPath)
	keyPath := strings.TrimSpace(cfg.TLSKeyPath)
	allowedOrigins := sanitizeOrigins(cfg.AllowedOrigins)

	requireTLS := binding != "loopback" || grpcBinding != "loopback"

	if requireTLS && (certPath == "" || keyPath == "") {
		return nil, fmt.Errorf("bindings (%s/%s) require TLS certificate and key to be configured", binding, grpcBinding)
	}
	if (certPath == "") != (keyPath == "") {
		return nil, fmt.Errorf("TLS configuration requires both certificate and key paths")
	}

	if certPath != "" && keyPath != "" {
		if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
			return nil, fmt.Errorf("failed to load TLS certificate/key pair: %w", err)
		}
	}

	address := net.JoinHostPort(host, strconv.Itoa(port))

	s.transportMu.Lock()
	s.port = port
	s.binding = binding
	s.grpcBinding = grpcBinding
	s.grpcPort = cfg.GRPCPort
	s.tlsCertPath = certPath
	s.tlsKeyPath = keyPath
	s.allowedOrigins = allowedOrigins
	s.transportMu.Unlock()

	if _, _, err := s.ensureAuthTokens(ctx, requireTLS); err != nil {
		return nil, err
	}

	// Setup HTTP routes - only WebSocket now
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.wsServer.HandleWebSocket)
	mux.HandleFunc("/sessions", s.handleSessionsRoot)
	mux.HandleFunc("/sessions/", s.handleSessionSubroutes)
	mux.HandleFunc("/recordings", s.handleRecordingsList)
	mux.HandleFunc("/recordings/", s.handleRecordingFile)
	mux.HandleFunc("/config/transport", s.handleTransportConfig)
	mux.HandleFunc("/config/migrate", s.handleConfigMigrate)
	mux.HandleFunc("/config/adapters", s.handleConfigAdapters)
	mux.HandleFunc("/config/adapter-bindings", s.handleConfigAdapterBindings)
	mux.HandleFunc("/adapters", s.handleAdapters)
	mux.HandleFunc("/adapters/logs", s.handleAdaptersLogs)
	mux.HandleFunc("/adapters/register", s.handleAdaptersRegister)
	mux.HandleFunc("/adapters/bind", s.handleAdaptersBind)
	mux.HandleFunc("/adapters/start", s.handleAdaptersStart)
	mux.HandleFunc("/adapters/stop", s.handleAdaptersStop)
	mux.HandleFunc("/plugins/warnings", s.handlePluginWarnings)
	mux.HandleFunc("/daemon/status", s.handleDaemonStatus)
	mux.HandleFunc("/daemon/shutdown", s.handleDaemonShutdown)
	mux.HandleFunc("/config/quickstart", s.handleQuickstart)
	mux.HandleFunc("/auth/tokens", s.handleAuthTokens)
	mux.HandleFunc("/auth/pairings", s.handleAuthPairings)
	mux.HandleFunc("/auth/pair", s.handleAuthPair)
	mux.HandleFunc("/audio/interrupt", s.handleAudioInterrupt)
	mux.HandleFunc("/audio/ingress", s.handleAudioIngress)
	mux.HandleFunc("/audio/egress", s.handleAudioEgress)
	mux.HandleFunc("/audio/ingress/ws", s.handleAudioIngressWS)
	mux.HandleFunc("/audio/egress/ws", s.handleAudioEgressWS)
	mux.HandleFunc("/audio/capabilities", s.handleAudioCapabilities)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/global/conversation", s.handleGlobalConversation)

	// Create and store the HTTP server with CORS middleware
	server := &http.Server{
		Addr:    address,
		Handler: s.wrapWithSecurity(mux),
	}
	s.httpServer = server

	prepared := &PreparedHTTPServer{
		Server:      server,
		Scheme:      "http",
		Binding:     binding,
		GRPCBinding: grpcBinding,
		GRPCPort:    cfg.GRPCPort,
	}
	if certPath != "" && keyPath != "" {
		prepared.UseTLS = true
		prepared.CertPath = certPath
		prepared.KeyPath = keyPath
		prepared.Scheme = "https"
	}

	return prepared, nil
}

// BroadcastSessionEvent forwards events to WebSocket server
func (s *APIServer) BroadcastSessionEvent(eventType string, sessionID string, data interface{}) {
	s.wsServer.BroadcastSessionEvent(eventType, sessionID, data)
}

// UpdateActualPort persists the effective HTTP port back into the configuration store.
func (s *APIServer) UpdateActualPort(ctx context.Context, port int) {
	if s.configStore == nil || port <= 0 {
		return
	}

	cfg, err := s.configStore.GetTransportConfig(ctx)
	if err != nil {
		log.Printf("[APIServer] Failed to load transport config: %v", err)
		return
	}
	if cfg.Port == port {
		return
	}
	cfg.Port = port
	if saveErr := s.configStore.SaveTransportConfig(ctx, cfg); saveErr != nil {
		log.Printf("[APIServer] Failed to persist transport port: %v", saveErr)
	} else {
		s.transportMu.Lock()
		s.port = port
		s.transportMu.Unlock()
	}
}

// UpdateActualGRPCPort persists the effective gRPC port into the configuration store.
func (s *APIServer) UpdateActualGRPCPort(ctx context.Context, port int) {
	if s.configStore == nil || port <= 0 {
		return
	}

	cfg, err := s.configStore.GetTransportConfig(ctx)
	if err != nil {
		log.Printf("[APIServer] Failed to load transport config: %v", err)
		return
	}
	if cfg.GRPCPort == port {
		return
	}
	cfg.GRPCPort = port
	if saveErr := s.configStore.SaveTransportConfig(ctx, cfg); saveErr != nil {
		log.Printf("[APIServer] Failed to persist transport gRPC port: %v", saveErr)
	} else {
		s.transportMu.Lock()
		s.grpcPort = port
		s.transportMu.Unlock()
	}
}

// AddTransportListener registers a callback invoked after transport config changes.
func (s *APIServer) AddTransportListener(fn func()) {
	if fn == nil {
		return
	}

	s.hookMu.Lock()
	s.transportHooks = append(s.transportHooks, fn)
	s.hookMu.Unlock()
}

func (s *APIServer) notifyTransportChanged() {
	s.hookMu.Lock()
	hooks := append([]func(){}, s.transportHooks...)
	s.hookMu.Unlock()

	for _, hook := range hooks {
		go hook()
	}
}

// ResizeManager exposes the resize manager for components outside the server package.
func (s *APIServer) ResizeManager() *termresize.Manager {
	return s.resizeManager
}

func (s *APIServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.serveMetrics(w, r)
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metricsExporter == nil {
		http.Error(w, "metrics exporter not configured", http.StatusServiceUnavailable)
		return
	}

	payload := s.metricsExporter.Export()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Header().Set("Cache-Control", "no-cache")
	if _, err := w.Write(payload); err != nil {
		log.Printf("[APIServer] failed to write metrics response: %v", err)
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
