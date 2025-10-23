package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/nupi-ai/nupi/internal/api"
	apihttp "github.com/nupi-ai/nupi/internal/api/http"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/modules"
	"github.com/nupi-ai/nupi/internal/protocol"
	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/session"
	"github.com/nupi-ai/nupi/internal/termresize"
	"github.com/nupi-ai/nupi/internal/voice/slots"
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

var allowedRoles = map[string]struct{}{
	string(roleAdmin):    {},
	string(roleReadOnly): {},
}

type streamLogTestHooksKey struct{}

type streamLogTestHooks struct {
	ready   chan struct{}
	emitted chan struct{}
}

const conversationMaxPageLimit = 500
const defaultTTSStreamID = slots.TTSPrimary
const voiceIssueCodeServiceUnavailable = "service_unavailable"

const (
	maxMetadataEntries      = 32
	maxMetadataKeyRunes     = 64
	maxMetadataValueRunes   = 512
	maxMetadataTotalPayload = 4096
)

const (
	defaultIngressBuffer       = 32 * 1024
	maxAudioUploadDuration     = 5 * time.Minute
	maxWebSocketMessageBytes   = 4 * defaultIngressBuffer
	websocketWriteTimeout      = 10 * time.Second
	websocketHeartbeatInterval = 30 * time.Second
	websocketHeartbeatTimeout  = 60 * time.Second
)

const (
	maxAdapterIDLength      = 128
	maxAdapterNameLength    = 256
	maxAdapterVersionLength = 64
	maxAdapterManifestBytes = 64 * 1024
)

var allowedAdapterTypes = map[string]struct{}{
	"stt":          {},
	"tts":          {},
	"ai":           {},
	"vad":          {},
	"tunnel":       {},
	"detector":     {},
	"tool-cleaner": {},
}

var allowedModuleTransports = map[string]struct{}{
	"grpc":    {},
	"http":    {},
	"process": {},
}

var audioWSUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		if origin == "http://localhost" || origin == "http://127.0.0.1" ||
			strings.HasPrefix(origin, "http://localhost:") ||
			strings.HasPrefix(origin, "http://127.0.0.1:") ||
			origin == "tauri://localhost" ||
			origin == "https://tauri.localhost" ||
			origin == "https://tauri.local" ||
			strings.HasPrefix(origin, "https://tauri.local:") {
			return true
		}
		return false
	},
}

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

type APIServer struct {
	sessionManager *session.Manager
	configStore    *configstore.Store
	runtime        RuntimeInfoProvider
	wsServer       *Server
	conversation   ConversationStore
	modules        ModulesController
	audioIngress   AudioCaptureProvider
	audioEgress    AudioPlaybackController
	eventBus       *eventbus.Bus
	resizeManager  *termresize.Manager
	port           int
	httpServer     *http.Server
	listenerOnce   sync.Once
	wsRunOnce      sync.Once
	hookMu         sync.Mutex
	transportHooks []func()

	transportMu    sync.RWMutex
	binding        string
	tlsCertPath    string
	tlsKeyPath     string
	allowedOrigins []string
	grpcBinding    string
	grpcPort       int

	authMu       sync.RWMutex
	authTokens   map[string]storedToken
	authRequired bool

	shutdownMu sync.RWMutex
	shutdownFn func(context.Context) error

	metricsExporter PrometheusExporter
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

type transportResponse struct {
	Port           int      `json:"port"`
	Binding        string   `json:"binding"`
	TLSCertPath    string   `json:"tls_cert_path,omitempty"`
	TLSKeyPath     string   `json:"tls_key_path,omitempty"`
	AllowedOrigins []string `json:"allowed_origins"`
	GRPCPort       int      `json:"grpc_port"`
	GRPCBinding    string   `json:"grpc_binding"`
	AuthRequired   bool     `json:"auth_required"`
}

type transportRequest struct {
	Port           *int      `json:"port,omitempty"`
	Binding        *string   `json:"binding,omitempty"`
	TLSCertPath    *string   `json:"tls_cert_path,omitempty"`
	TLSKeyPath     *string   `json:"tls_key_path,omitempty"`
	AllowedOrigins *[]string `json:"allowed_origins,omitempty"`
	GRPCPort       *int      `json:"grpc_port,omitempty"`
	GRPCBinding    *string   `json:"grpc_binding,omitempty"`
}

type quickstartStatusResponse struct {
	Completed                bool                  `json:"completed"`
	CompletedAt              string                `json:"completed_at,omitempty"`
	PendingSlots             []string              `json:"pending_slots"`
	Modules                  []apihttp.ModuleEntry `json:"modules,omitempty"`
	MissingReferenceAdapters []string              `json:"missing_reference_adapters,omitempty"`
}

type quickstartBinding struct {
	Slot      string `json:"slot"`
	AdapterID string `json:"adapter_id"`
}

type audioInterruptRequest struct {
	SessionID string            `json:"session_id"`
	StreamID  string            `json:"stream_id"`
	Reason    string            `json:"reason"`
	Metadata  map[string]string `json:"metadata"`
}

type quickstartRequest struct {
	Complete *bool               `json:"complete,omitempty"`
	Bindings []quickstartBinding `json:"bindings,omitempty"`
}

type daemonStatusSnapshot struct {
	Version       string
	SessionsCount int
	Port          int
	GRPCPort      int
	Binding       string
	GRPCBinding   string
	AuthRequired  bool
	TLSEnabled    bool
	UptimeSeconds float64
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
		port:           port,
	}

	apiServer.registerSessionListener()

	return apiServer, nil
}

// SetShutdownFunc registers a handler invoked when /daemon/shutdown is called.
func (s *APIServer) SetShutdownFunc(fn func(context.Context) error) {
	s.shutdownMu.Lock()
	s.shutdownFn = fn
	s.shutdownMu.Unlock()
}

// SetConversationStore wires the conversation state provider used by HTTP handlers.
func (s *APIServer) SetConversationStore(store ConversationStore) {
	s.conversation = store
}

// SetModulesController wires the modules controller used by HTTP handlers.
func (s *APIServer) SetModulesController(controller ModulesController) {
	s.modules = controller
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

// CurrentTransportSnapshot returns the currently applied transport configuration.
func (s *APIServer) CurrentTransportSnapshot() TransportSnapshot {
	s.transportMu.RLock()
	defer s.transportMu.RUnlock()

	origins := make([]string, len(s.allowedOrigins))
	copy(origins, s.allowedOrigins)

	return TransportSnapshot{
		Binding:        s.binding,
		Port:           s.port,
		TLSCertPath:    s.tlsCertPath,
		TLSKeyPath:     s.tlsKeyPath,
		AllowedOrigins: origins,
		GRPCBinding:    s.grpcBinding,
		GRPCPort:       s.grpcPort,
		TLSCertModTime: modTimeOrZero(s.tlsCertPath),
		TLSKeyModTime:  modTimeOrZero(s.tlsKeyPath),
	}
}

// EqualConfig compares the snapshot with a transport configuration fetched from the store.
func (snap TransportSnapshot) EqualConfig(cfg configstore.TransportConfig) bool {
	if snap.Binding != normalizeBinding(cfg.Binding) {
		return false
	}
	if snap.Port != cfg.Port {
		return false
	}
	if strings.TrimSpace(snap.TLSCertPath) != strings.TrimSpace(cfg.TLSCertPath) {
		return false
	}
	if strings.TrimSpace(snap.TLSKeyPath) != strings.TrimSpace(cfg.TLSKeyPath) {
		return false
	}
	if snap.GRPCBinding != normalizeBinding(cfg.GRPCBinding) {
		return false
	}
	if snap.GRPCPort != cfg.GRPCPort {
		return false
	}

	normSnap := make([]string, len(snap.AllowedOrigins))
	copy(normSnap, snap.AllowedOrigins)
	for i := range normSnap {
		normSnap[i] = strings.TrimSpace(normSnap[i])
	}
	sort.Strings(normSnap)

	normCfg := make([]string, len(cfg.AllowedOrigins))
	copy(normCfg, cfg.AllowedOrigins)
	for i := range normCfg {
		normCfg[i] = strings.TrimSpace(normCfg[i])
	}
	sort.Strings(normCfg)

	if len(normSnap) != len(normCfg) {
		return false
	}
	for i := range normSnap {
		if normSnap[i] != normCfg[i] {
			return false
		}
	}

	return true
}

func modTimeOrZero(path string) time.Time {
	path = strings.TrimSpace(path)
	if path == "" {
		return time.Time{}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
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

func (s *APIServer) originAllowed(origin string) bool {
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

	s.transportMu.RLock()
	defer s.transportMu.RUnlock()
	for _, allowed := range s.allowedOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
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

func (s *APIServer) lookupToken(token string) (storedToken, bool) {
	if token == "" {
		return storedToken{}, false
	}
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	entry, ok := s.authTokens[token]
	return entry, ok
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

func normalizeBinding(binding string) string {
	b := strings.TrimSpace(strings.ToLower(binding))
	if b == "" {
		return "loopback"
	}
	return b
}

func resolveBindingHost(binding string) (string, error) {
	switch binding {
	case "loopback":
		return "127.0.0.1", nil
	case "lan", "public":
		return "0.0.0.0", nil
	default:
		return "", fmt.Errorf("unknown binding %q", binding)
	}
}

func sanitizeOrigins(origins []string) []string {
	if len(origins) == 0 {
		return nil
	}

	result := make([]string, 0, len(origins))
	seen := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		trimmed := strings.TrimSpace(origin)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

func sanitizeTokens(tokens []string) []string {
	if len(tokens) == 0 {
		return nil
	}

	unique := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		trimmed := strings.TrimSpace(token)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		unique = append(unique, trimmed)
	}
	return unique
}

func sanitizeStoredTokens(tokens []storedToken) []storedToken {
	if len(tokens) == 0 {
		return nil
	}

	seenTokens := make(map[string]struct{}, len(tokens))
	seenIDs := make(map[string]struct{}, len(tokens))
	result := make([]storedToken, 0, len(tokens))

	for _, token := range tokens {
		token.Token = strings.TrimSpace(token.Token)
		if token.Token == "" {
			continue
		}
		if _, exists := seenTokens[token.Token]; exists {
			continue
		}
		token.Role = normalizeRole(token.Role)
		token.Name = strings.TrimSpace(token.Name)
		if token.CreatedAt.IsZero() {
			token.CreatedAt = time.Now().UTC()
		}
		id := strings.TrimSpace(token.ID)
		if id == "" {
			id = defaultTokenID(token.Token)
		}
		for {
			if _, exists := seenIDs[id]; !exists {
				break
			}
			id = defaultTokenID(token.Token) + "-" + generateRandomID()
		}
		token.ID = id
		seenIDs[id] = struct{}{}
		seenTokens[token.Token] = struct{}{}
		result = append(result, token)
	}

	return result
}

func newStoredToken(token, name, role string) storedToken {
	entry := storedToken{
		Token:     strings.TrimSpace(token),
		Name:      strings.TrimSpace(name),
		Role:      normalizeRole(role),
		CreatedAt: time.Now().UTC(),
	}
	processed := sanitizeStoredTokens([]storedToken{entry})
	if len(processed) == 0 {
		return storedToken{}
	}
	return processed[0]
}

func normalizeRole(role string) string {
	trimmed := strings.TrimSpace(strings.ToLower(role))
	if trimmed == "" {
		return string(roleAdmin)
	}
	if _, ok := allowedRoles[trimmed]; ok {
		return trimmed
	}
	return string(roleAdmin)
}

func defaultTokenID(token string) string {
	trimmed := strings.TrimSpace(token)
	if len(trimmed) >= 12 {
		return trimmed[:12]
	}
	if len(trimmed) >= 4 {
		return trimmed
	}
	return generateRandomID()
}

func generateRandomID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err == nil {
		return hex.EncodeToString(buf)
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func validateTransportConfig(cfg configstore.TransportConfig) error {
	binding := normalizeBinding(cfg.Binding)
	if _, err := resolveBindingHost(binding); err != nil {
		return err
	}

	grpcBinding := normalizeBinding(cfg.GRPCBinding)
	if grpcBinding == "" {
		grpcBinding = binding
	}
	if _, err := resolveBindingHost(grpcBinding); err != nil {
		return err
	}

	if cfg.Port < 0 || cfg.Port > 65535 {
		return fmt.Errorf("port must be between 0 and 65535")
	}
	if cfg.GRPCPort < 0 || cfg.GRPCPort > 65535 {
		return fmt.Errorf("grpc_port must be between 0 and 65535")
	}

	certPath := strings.TrimSpace(cfg.TLSCertPath)
	keyPath := strings.TrimSpace(cfg.TLSKeyPath)

	if (certPath == "") != (keyPath == "") {
		return fmt.Errorf("TLS configuration requires both certificate and key paths")
	}

	requireTLS := binding != "loopback" || grpcBinding != "loopback"
	if requireTLS && (certPath == "" || keyPath == "") {
		return fmt.Errorf("bindings (%s/%s) require TLS certificate and key to be configured", binding, grpcBinding)
	}

	if certPath != "" && keyPath != "" {
		if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
			return fmt.Errorf("failed to load TLS certificate/key pair: %w", err)
		}
	}

	return nil
}

func generateAPIToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate api token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func (s *APIServer) isAuthRequired() bool {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return s.authRequired
}

// AuthRequired reports whether transport-level authentication is enforced.
func (s *APIServer) AuthRequired() bool {
	return s.isAuthRequired()
}

func (s *APIServer) setAuthTokens(tokens []storedToken, authRequired bool) {
	tokenMap := make(map[string]storedToken, len(tokens))
	for _, token := range sanitizeStoredTokens(tokens) {
		tokenMap[token.Token] = token
	}

	s.authMu.Lock()
	s.authTokens = tokenMap
	s.authRequired = authRequired
	s.authMu.Unlock()
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

	cfg := configstore.TransportConfig{
		Binding:     "loopback",
		Port:        s.port,
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
	mux.HandleFunc("/config/adapters", s.handleAdapters)
	mux.HandleFunc("/config/adapter-bindings", s.handleAdapterBindings)
	mux.HandleFunc("/modules", s.handleModules)
	mux.HandleFunc("/modules/logs", s.handleModulesLogs)
	mux.HandleFunc("/modules/register", s.handleModulesRegister)
	mux.HandleFunc("/modules/bind", s.handleModulesBind)
	mux.HandleFunc("/modules/start", s.handleModulesStart)
	mux.HandleFunc("/modules/stop", s.handleModulesStop)
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

func (s *APIServer) handleSessionsRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleSessionsList(w, r)
	case http.MethodPost:
		s.handleSessionCreate(w, r)
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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

func (s *APIServer) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	sessions := s.sessionManager.ListSessions()
	dto := api.ToDTOList(sessions)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(dto); err != nil {
		http.Error(w, fmt.Sprintf("failed to encode sessions: %v", err), http.StatusInternalServerError)
	}
}

func (s *APIServer) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	var payload protocol.CreateSessionData
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(payload.Command) == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}

	sess, err := s.createSessionFromPayload(payload)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create session: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(api.ToDTO(sess)); err != nil {
		http.Error(w, fmt.Sprintf("failed to encode session: %v", err), http.StatusInternalServerError)
	}
}

func (s *APIServer) createSessionFromPayload(payload protocol.CreateSessionData) (*session.Session, error) {
	opts := pty.StartOptions{
		Command:    payload.Command,
		Args:       payload.Args,
		WorkingDir: payload.WorkingDir,
		Env:        payload.Env,
		Rows:       payload.Rows,
		Cols:       payload.Cols,
	}
	if opts.Rows == 0 {
		opts.Rows = 24
	}
	if opts.Cols == 0 {
		opts.Cols = 80
	}

	sess, err := s.sessionManager.CreateSession(opts, payload.Inspect)
	if err != nil {
		return nil, err
	}

	if payload.Detached {
		sess.SetStatus(session.StatusDetached)
	} else {
		sess.SetStatus(session.StatusRunning)
	}

	return sess, nil
}

func (s *APIServer) handleSessionSubroutes(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/sessions/")
	if trimmed == "" || trimmed == "/" {
		s.handleSessionsRoot(w, r)
		return
	}

	if strings.HasSuffix(trimmed, "/mode") {
		s.handleSessionMode(w, r)
		return
	}
	if strings.HasSuffix(trimmed, "/attach") {
		s.handleSessionAttach(w, r)
		return
	}
	if strings.HasSuffix(trimmed, "/input") {
		s.handleSessionInput(w, r)
		return
	}
	if strings.HasSuffix(trimmed, "/detach") {
		s.handleSessionDetach(w, r)
		return
	}
	if strings.HasSuffix(trimmed, "/conversation") {
		s.handleSessionConversation(w, r)
		return
	}

	parts := strings.Split(trimmed, "/")
	sessionID := strings.TrimSpace(parts[0])
	if sessionID == "" {
		http.NotFound(w, r)
		return
	}

	if len(parts) > 1 {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleSessionGet(w, r, sessionID)
	case http.MethodDelete:
		s.handleSessionDelete(w, r, sessionID)
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleSessionGet(w http.ResponseWriter, r *http.Request, sessionID string) {
	session, err := s.sessionManager.GetSession(sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("session %s not found", sessionID), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(api.ToDTO(session)); err != nil {
		http.Error(w, fmt.Sprintf("failed to encode session: %v", err), http.StatusInternalServerError)
	}
}

func (s *APIServer) handleSessionDelete(w http.ResponseWriter, r *http.Request, sessionID string) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if err := s.sessionManager.KillSession(sessionID); err != nil {
		http.Error(w, fmt.Sprintf("session %s not found", sessionID), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *APIServer) handleSessionAttach(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[1]) != "attach" {
		http.NotFound(w, r)
		return
	}
	sessionID := strings.TrimSpace(parts[0])

	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		IncludeHistory bool `json:"include_history"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil && err != io.EOF {
		http.Error(w, fmt.Sprintf("invalid payload: %v", err), http.StatusBadRequest)
		return
	}

	sess, err := s.sessionManager.GetSession(sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("session %s not found", sessionID), http.StatusNotFound)
		return
	}

	sessionDTO := api.ToDTO(sess)
	sessionDTO.Mode = s.resizeManager.GetSessionMode(sessionID)
	resp := map[string]any{
		"session":             sessionDTO,
		"stream_url":          fmt.Sprintf("%s://%s/ws?s=%s", websocketScheme(r), r.Host, sessionID),
		"recording_available": s.sessionManager.GetRecordingStore() != nil,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, fmt.Sprintf("failed to encode session: %v", err), http.StatusInternalServerError)
	}
}

type sessionInputRequest struct {
	Input string `json:"input"`
	EOF   bool   `json:"eof"`
}

func (s *APIServer) handleSessionInput(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[1]) != "input" {
		http.NotFound(w, r)
		return
	}
	sessionID := strings.TrimSpace(parts[0])

	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload sessionInputRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid payload: %v", err), http.StatusBadRequest)
		return
	}

	if _, err := s.sessionManager.GetSession(sessionID); err != nil {
		http.Error(w, fmt.Sprintf("session %s not found", sessionID), http.StatusNotFound)
		return
	}

	if payload.Input != "" {
		if err := s.sessionManager.WriteToSession(sessionID, []byte(payload.Input)); err != nil {
			http.Error(w, fmt.Sprintf("failed to send input: %v", err), http.StatusInternalServerError)
			return
		}
	}

	if payload.EOF {
		// Send Ctrl-D (EOT) to signal EOF
		if err := s.sessionManager.WriteToSession(sessionID, []byte{4}); err != nil {
			http.Error(w, fmt.Sprintf("failed to send EOF: %v", err), http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *APIServer) handleSessionDetach(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[1]) != "detach" {
		http.NotFound(w, r)
		return
	}
	sessionID := strings.TrimSpace(parts[0])

	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sess, err := s.sessionManager.GetSession(sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("session %s not found", sessionID), http.StatusNotFound)
		return
	}

	sess.SetStatus(session.StatusDetached)
	w.WriteHeader(http.StatusNoContent)
}

func (s *APIServer) handleSessionConversation(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[1]) != "conversation" {
		http.NotFound(w, r)
		return
	}

	sessionID := strings.TrimSpace(parts[0])
	if sessionID == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
		return
	}

	if s.conversation == nil {
		http.Error(w, "conversation service unavailable", http.StatusServiceUnavailable)
		return
	}

	if _, err := s.sessionManager.GetSession(sessionID); err != nil {
		http.Error(w, fmt.Sprintf("session %s not found", sessionID), http.StatusNotFound)
		return
	}

	query := r.URL.Query()

	parseParam := func(name string) (int, bool, error) {
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

	offset, _, err := parseParam("offset")
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid offset: %v", err), http.StatusBadRequest)
		return
	}

	limit, providedLimit, err := parseParam("limit")
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid limit: %v", err), http.StatusBadRequest)
		return
	}
	if providedLimit && limit > conversationMaxPageLimit {
		limit = conversationMaxPageLimit
	}

	total, turns := s.conversation.Slice(sessionID, offset, limit)
	state := api.ToConversationState(sessionID, total, offset, limit, turns)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(state); err != nil {
		http.Error(w, fmt.Sprintf("failed to encode conversation: %v", err), http.StatusInternalServerError)
	}
}

func (s *APIServer) onSessionEvent(event string, sess *session.Session) {
	log.Printf("[APIServer] Session event: %s for session %s", event, sess.ID)

	switch event {
	case "session_created":
		info := api.ToDTO(sess)
		if s.resizeManager != nil {
			info.Mode = s.resizeManager.GetSessionMode(sess.ID)
		}
		s.wsServer.BroadcastSessionEvent("session_created", sess.ID, info)
		s.broadcastSessionMode(sess.ID)
	case "session_killed":
		s.wsServer.BroadcastSessionEvent("session_killed", sess.ID, nil)
		if s.resizeManager != nil {
			s.resizeManager.ForgetSession(sess.ID)
		}
	case "session_status_changed":
		s.wsServer.BroadcastSessionEvent("session_status_changed", sess.ID, string(sess.CurrentStatus()))
	case "tool_detected":
		info := api.ToDTO(sess)
		s.wsServer.BroadcastSessionEvent("tool_detected", sess.ID, info)
	}
}

func (s *APIServer) registerSessionListener() {
	s.listenerOnce.Do(func() {
		s.sessionManager.AddEventListener(func(event string, sess *session.Session) {
			s.onSessionEvent(event, sess)
		})
	})
}

func (s *APIServer) broadcastSessionMode(sessionID string) {
	if s.resizeManager == nil {
		return
	}

	mode := s.resizeManager.GetSessionMode(sessionID)
	payload := map[string]string{"mode": mode}
	s.wsServer.BroadcastSessionEvent("session_mode_changed", sessionID, payload)
}

func (s *APIServer) handleSessionMode(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
			return
		}
	} else {
		if _, ok := s.requireRole(w, r, roleAdmin); !ok {
			return
		}
	}
	if s.resizeManager == nil {
		http.Error(w, "resize manager not available", http.StatusInternalServerError)
		return
	}

	trimmed := strings.TrimPrefix(r.URL.Path, "/sessions/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || parts[1] != "mode" || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	sessionID := parts[0]

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if _, err := s.sessionManager.GetSession(sessionID); err != nil {
		http.Error(w, fmt.Sprintf("session %s not found", sessionID), http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.respondWithMode(w, sessionID)
	case http.MethodPost, http.MethodPut:
		var payload struct {
			Mode string `json:"mode"`
		}

		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, fmt.Sprintf("invalid payload: %v", err), http.StatusBadRequest)
			return
		}
		if payload.Mode == "" {
			http.Error(w, "mode field is required", http.StatusBadRequest)
			return
		}

		if err := s.resizeManager.SetSessionMode(sessionID, payload.Mode); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, termresize.ErrUnknownMode) {
				http.Error(w, fmt.Sprintf("unknown mode: %s", payload.Mode), status)
			} else {
				http.Error(w, fmt.Sprintf("failed to set mode: %v", err), http.StatusInternalServerError)
			}
			return
		}

		s.broadcastSessionMode(sessionID)
		s.respondWithMode(w, sessionID)
	default:
		w.Header().Set("Allow", "GET,POST,PUT,OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) respondWithMode(w http.ResponseWriter, sessionID string) {
	mode := ""
	if s.resizeManager != nil {
		mode = s.resizeManager.GetSessionMode(sessionID)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"sessionId": sessionID,
		"mode":      mode,
	})
}

func (s *APIServer) handleTransportConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		s.handleTransportGet(w, r)
	case http.MethodPut, http.MethodPost:
		s.handleTransportUpdate(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleTransportGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.configStore == nil {
		http.Error(w, "configuration store not available", http.StatusServiceUnavailable)
		return
	}

	cfg, err := s.configStore.GetTransportConfig(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	binding := normalizeBinding(cfg.Binding)
	origins := sanitizeOrigins(cfg.AllowedOrigins)
	grpcBinding := normalizeBinding(cfg.GRPCBinding)

	resp := transportResponse{
		Port:           cfg.Port,
		Binding:        binding,
		TLSCertPath:    strings.TrimSpace(cfg.TLSCertPath),
		TLSKeyPath:     strings.TrimSpace(cfg.TLSKeyPath),
		AllowedOrigins: origins,
		GRPCPort:       cfg.GRPCPort,
		GRPCBinding:    grpcBinding,
		AuthRequired:   s.isAuthRequired(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *APIServer) handleTransportUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.configStore == nil {
		http.Error(w, "configuration store not available", http.StatusServiceUnavailable)
		return
	}

	var payload transportRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	current, err := s.configStore.GetTransportConfig(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	current.Binding = normalizeBinding(current.Binding)
	current.TLSCertPath = strings.TrimSpace(current.TLSCertPath)
	current.TLSKeyPath = strings.TrimSpace(current.TLSKeyPath)
	current.AllowedOrigins = sanitizeOrigins(current.AllowedOrigins)

	if payload.Port != nil {
		if *payload.Port < 0 || *payload.Port > 65535 {
			http.Error(w, "port must be between 0 and 65535", http.StatusBadRequest)
			return
		}
		current.Port = *payload.Port
	}
	if payload.Binding != nil {
		current.Binding = normalizeBinding(*payload.Binding)
	}
	if payload.TLSCertPath != nil {
		current.TLSCertPath = strings.TrimSpace(*payload.TLSCertPath)
	}
	if payload.TLSKeyPath != nil {
		current.TLSKeyPath = strings.TrimSpace(*payload.TLSKeyPath)
	}
	if payload.AllowedOrigins != nil {
		current.AllowedOrigins = sanitizeOrigins(*payload.AllowedOrigins)
	}
	if payload.GRPCBinding != nil {
		current.GRPCBinding = normalizeBinding(*payload.GRPCBinding)
	}
	if payload.GRPCPort != nil {
		if *payload.GRPCPort < 0 || *payload.GRPCPort > 65535 {
			http.Error(w, "grpc_port must be between 0 and 65535", http.StatusBadRequest)
			return
		}
		current.GRPCPort = *payload.GRPCPort
	}

	grpcBindingSupplied := payload.GRPCBinding != nil

	current.Binding = normalizeBinding(current.Binding)
	current.GRPCBinding = normalizeBinding(current.GRPCBinding)
	if !grpcBindingSupplied {
		current.GRPCBinding = current.Binding
	}
	if current.GRPCBinding == "" {
		current.GRPCBinding = current.Binding
	}

	if err := validateTransportConfig(current); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	binding := current.Binding
	var newToken string

	authRequired := binding != "loopback" || current.GRPCBinding != "loopback"

	if authRequired {
		_, nt, err := s.ensureAuthTokens(r.Context(), true)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to ensure auth token: %v", err), http.StatusInternalServerError)
			return
		}
		newToken = nt
	} else {
		if _, _, err := s.ensureAuthTokens(r.Context(), false); err != nil {
			http.Error(w, fmt.Sprintf("failed to update auth tokens: %v", err), http.StatusInternalServerError)
			return
		}
	}

	if err := s.configStore.SaveTransportConfig(r.Context(), current); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.transportMu.Lock()
	s.binding = binding
	s.port = current.Port
	s.tlsCertPath = current.TLSCertPath
	s.tlsKeyPath = current.TLSKeyPath
	s.allowedOrigins = current.AllowedOrigins
	s.grpcBinding = current.GRPCBinding
	s.grpcPort = current.GRPCPort
	s.transportMu.Unlock()

	s.notifyTransportChanged()

	if newToken != "" {
		response := map[string]interface{}{
			"status":        "ok",
			"binding":       binding,
			"auth_token":    newToken,
			"grpc_binding":  current.GRPCBinding,
			"grpc_port":     current.GRPCPort,
			"auth_required": s.AuthRequired(),
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Nupi-API-Token", newToken)
		json.NewEncoder(w).Encode(response)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *APIServer) handleAdapters(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		s.handleAdaptersGet(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleAdaptersGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
		return
	}
	if s.configStore == nil {
		http.Error(w, "configuration store not available", http.StatusServiceUnavailable)
		return
	}

	adapters, err := s.configStore.ListAdapters(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"adapters": adapters})
}

func (s *APIServer) handleAdapterBindings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		s.handleAdapterBindingsGet(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleAdapterBindingsGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
		return
	}
	if s.configStore == nil {
		http.Error(w, "configuration store not available", http.StatusServiceUnavailable)
		return
	}

	bindings, err := s.configStore.ListAdapterBindings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type bindingResponse struct {
		Slot      string          `json:"slot"`
		AdapterID *string         `json:"adapter_id,omitempty"`
		Status    string          `json:"status"`
		Config    json.RawMessage `json:"config,omitempty"`
		UpdatedAt string          `json:"updated_at"`
	}

	out := make([]bindingResponse, 0, len(bindings))
	for _, binding := range bindings {
		var cfg json.RawMessage
		if binding.Config != "" {
			cfg = json.RawMessage(binding.Config)
		}
		out = append(out, bindingResponse{
			Slot:      binding.Slot,
			AdapterID: binding.AdapterID,
			Status:    binding.Status,
			Config:    cfg,
			UpdatedAt: binding.UpdatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"bindings": out})
}

func (s *APIServer) handleConfigMigrate(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.configStore == nil {
		http.Error(w, "configuration store not available", http.StatusServiceUnavailable)
		return
	}

	result, err := s.configStore.EnsureRequiredAdapterSlots(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("migration failed: %v", err), http.StatusInternalServerError)
		return
	}

	audioUpdated, err := s.configStore.EnsureAudioSettings(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("audio settings migration failed: %v", err), http.StatusInternalServerError)
		return
	}

	response := configMigrationResponse{
		UpdatedSlots:         append([]string{}, result.UpdatedSlots...),
		PendingSlots:         append([]string{}, result.PendingSlots...),
		AudioSettingsUpdated: audioUpdated,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, fmt.Sprintf("encode response: %v", err), http.StatusInternalServerError)
	}
}

func (s *APIServer) handleModules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		s.handleModulesGet(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleModulesLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
		return
	}
	if s.eventBus == nil {
		http.Error(w, "event bus unavailable", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	slotFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("slot")))
	adapterFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("adapter")))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	ctx := r.Context()

	subLogs := s.eventBus.Subscribe(eventbus.TopicModulesLog)
	defer subLogs.Close()
	subPartial := s.eventBus.Subscribe(eventbus.TopicSpeechTranscriptPartial)
	defer subPartial.Close()
	subFinal := s.eventBus.Subscribe(eventbus.TopicSpeechTranscriptFinal)
	defer subFinal.Close()

	logCh := subLogs.C()
	partialCh := subPartial.C()
	finalCh := subFinal.C()
	encoder := json.NewEncoder(w)
	var hooks *streamLogTestHooks
	if v, ok := ctx.Value(streamLogTestHooksKey{}).(*streamLogTestHooks); ok && v != nil {
		hooks = v
		select {
		case hooks.ready <- struct{}{}:
		default:
		}
	}

	for logCh != nil || partialCh != nil || finalCh != nil {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-logCh:
			if !ok {
				logCh = nil
				continue
			}
			entry, emit := filterModuleLogEvent(env, slotFilter, adapterFilter)
			if !emit {
				continue
			}
			if err := encoder.Encode(entry); err != nil {
				return
			}
			flusher.Flush()
			if hooks != nil {
				select {
				case hooks.emitted <- struct{}{}:
				default:
				}
			}
		case env, ok := <-partialCh:
			if !ok {
				partialCh = nil
				continue
			}
			entry, emit := makeTranscriptEntry(env)
			if !emit || (slotFilter != "" && adapterFilter != "") {
				// transcripts cannot currently be mapped to slot/adapter; when both filters
				// are provided, skip to avoid confusion.
				continue
			}
			if err := encoder.Encode(entry); err != nil {
				return
			}
			flusher.Flush()
			if hooks != nil {
				select {
				case hooks.emitted <- struct{}{}:
				default:
				}
			}
		case env, ok := <-finalCh:
			if !ok {
				finalCh = nil
				continue
			}
			entry, emit := makeTranscriptEntry(env)
			if !emit || (slotFilter != "" && adapterFilter != "") {
				continue
			}
			if err := encoder.Encode(entry); err != nil {
				return
			}
			flusher.Flush()
			if hooks != nil {
				select {
				case hooks.emitted <- struct{}{}:
				default:
				}
			}
		}
	}
}

func (s *APIServer) handleModulesRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.configStore == nil {
		http.Error(w, "configuration store unavailable", http.StatusServiceUnavailable)
		return
	}

	var payload apihttp.ModuleRegistrationRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	adapterID := strings.TrimSpace(payload.AdapterID)
	if adapterID == "" {
		http.Error(w, "adapter_id is required", http.StatusBadRequest)
		return
	}
	if len(adapterID) > maxAdapterIDLength {
		http.Error(w, fmt.Sprintf("adapter_id too long (max %d chars)", maxAdapterIDLength), http.StatusBadRequest)
		return
	}

	adapterType := strings.TrimSpace(payload.Type)
	if adapterType != "" {
		if _, ok := allowedAdapterTypes[adapterType]; !ok {
			http.Error(w, fmt.Sprintf("invalid adapter type: %s (expected: stt, tts, ai, vad, tunnel, detector, tool-cleaner)", adapterType), http.StatusBadRequest)
			return
		}
	}

	adapterName := strings.TrimSpace(payload.Name)
	if len(adapterName) > maxAdapterNameLength {
		http.Error(w, fmt.Sprintf("name too long (max %d chars)", maxAdapterNameLength), http.StatusBadRequest)
		return
	}

	adapterVersion := strings.TrimSpace(payload.Version)
	if len(adapterVersion) > maxAdapterVersionLength {
		http.Error(w, fmt.Sprintf("version too long (max %d chars)", maxAdapterVersionLength), http.StatusBadRequest)
		return
	}

	if len(payload.Manifest) > maxAdapterManifestBytes {
		http.Error(w, fmt.Sprintf("manifest too large (max %d bytes)", maxAdapterManifestBytes), http.StatusBadRequest)
		return
	}

	adapter := configstore.Adapter{
		ID:      adapterID,
		Source:  strings.TrimSpace(payload.Source),
		Version: adapterVersion,
		Type:    adapterType,
		Name:    adapterName,
	}
	if len(payload.Manifest) > 0 {
		adapter.Manifest = strings.TrimSpace(string(payload.Manifest))
	}

	if err := s.configStore.UpsertAdapter(r.Context(), adapter); err != nil {
		http.Error(w, fmt.Sprintf("register adapter failed: %v", err), http.StatusInternalServerError)
		return
	}

	if payload.Endpoint != nil {
		transport := strings.TrimSpace(payload.Endpoint.Transport)
		address := strings.TrimSpace(payload.Endpoint.Address)
		command := strings.TrimSpace(payload.Endpoint.Command)

		if transport == "" {
			if address != "" || command != "" || len(payload.Endpoint.Args) > 0 || len(payload.Endpoint.Env) > 0 {
				http.Error(w, "endpoint transport required when endpoint fields are provided", http.StatusBadRequest)
				return
			}
		} else {
			if _, ok := allowedModuleTransports[transport]; !ok {
				http.Error(w, fmt.Sprintf("invalid transport: %s (expected: grpc, http, process)", transport), http.StatusBadRequest)
				return
			}
			switch transport {
			case "grpc":
				if address == "" {
					http.Error(w, "endpoint address required for grpc transport", http.StatusBadRequest)
					return
				}
			case "http":
				if address == "" {
					http.Error(w, "endpoint address required for http transport", http.StatusBadRequest)
					return
				}
				if command != "" || len(payload.Endpoint.Args) > 0 {
					http.Error(w, "endpoint command/args not allowed for http transport", http.StatusBadRequest)
					return
				}
			case "process":
				if command == "" {
					http.Error(w, "endpoint command required for process transport", http.StatusBadRequest)
					return
				}
				if address != "" {
					http.Error(w, "endpoint address not used for process transport", http.StatusBadRequest)
					return
				}
			}

			endpoint := configstore.ModuleEndpoint{
				AdapterID: adapterID,
				Transport: transport,
				Address:   address,
				Command:   command,
			}
			if len(payload.Endpoint.Args) > 0 {
				endpoint.Args = append([]string(nil), payload.Endpoint.Args...)
			}
			if len(payload.Endpoint.Env) > 0 {
				endpoint.Env = cloneStringMap(payload.Endpoint.Env)
			}
			if err := s.configStore.UpsertModuleEndpoint(r.Context(), endpoint); err != nil {
				http.Error(w, fmt.Sprintf("register module endpoint failed: %v", err), http.StatusInternalServerError)
				return
			}
		}
	}

	result := apihttp.ModuleRegistrationResult{
		Adapter: apihttp.ModuleAdapter{
			ID:       adapter.ID,
			Source:   adapter.Source,
			Version:  adapter.Version,
			Type:     adapter.Type,
			Name:     adapter.Name,
			Manifest: payload.Manifest,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("[APIServer] failed to encode module registration result: %v", err)
	}
}

func filterModuleLogEvent(env eventbus.Envelope, slotFilter, adapterFilter string) (apihttp.ModuleLogStreamEntry, bool) {
	event, ok := env.Payload.(eventbus.ModuleLogEvent)
	if !ok {
		return apihttp.ModuleLogStreamEntry{}, false
	}

	slotValue := strings.TrimSpace(event.Fields["slot"])
	if slotFilter != "" && strings.ToLower(slotValue) != slotFilter {
		return apihttp.ModuleLogStreamEntry{}, false
	}
	if adapterFilter != "" && strings.ToLower(strings.TrimSpace(event.ModuleID)) != adapterFilter {
		return apihttp.ModuleLogStreamEntry{}, false
	}

	timestamp := env.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}

	return apihttp.ModuleLogStreamEntry{
		Type:      "log",
		Timestamp: timestamp,
		ModuleID:  event.ModuleID,
		Slot:      slotValue,
		Level:     string(event.Level),
		Message:   event.Message,
	}, true
}

func makeTranscriptEntry(env eventbus.Envelope) (apihttp.ModuleLogStreamEntry, bool) {
	event, ok := env.Payload.(eventbus.SpeechTranscriptEvent)
	if !ok {
		return apihttp.ModuleLogStreamEntry{}, false
	}

	timestamp := env.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}

	return apihttp.ModuleLogStreamEntry{
		Type:       "transcript",
		Timestamp:  timestamp,
		SessionID:  event.SessionID,
		StreamID:   event.StreamID,
		Text:       event.Text,
		Confidence: float64(event.Confidence),
		Final:      event.Final,
	}, true
}

type moduleActionRequest struct {
	Slot      string          `json:"slot"`
	AdapterID string          `json:"adapter_id,omitempty"`
	Config    json.RawMessage `json:"config,omitempty"`
}

type configMigrationResponse struct {
	UpdatedSlots         []string `json:"updated_slots"`
	PendingSlots         []string `json:"pending_slots"`
	AudioSettingsUpdated bool     `json:"audio_settings_updated"`
}

func (s *APIServer) handleModulesGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
		return
	}
	if s.modules == nil {
		http.Error(w, "modules service unavailable", http.StatusServiceUnavailable)
		return
	}

	overview, err := s.modules.Overview(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	modulesOut := make([]apihttp.ModuleEntry, 0, len(overview))
	for _, status := range overview {
		modulesOut = append(modulesOut, bindingStatusToResponse(status))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(apihttp.ModulesOverview{Modules: modulesOut})
}

func (s *APIServer) handleModulesBind(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.configStore == nil {
		http.Error(w, "configuration store not available", http.StatusServiceUnavailable)
		return
	}
	if s.modules == nil {
		http.Error(w, "modules service unavailable", http.StatusServiceUnavailable)
		return
	}

	var payload moduleActionRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	slot := strings.TrimSpace(payload.Slot)
	adapterID := strings.TrimSpace(payload.AdapterID)
	if slot == "" || adapterID == "" {
		http.Error(w, "slot and adapter_id are required", http.StatusBadRequest)
		return
	}

	var cfg map[string]any
	if len(payload.Config) > 0 {
		if err := json.Unmarshal(payload.Config, &cfg); err != nil {
			http.Error(w, fmt.Sprintf("invalid config payload: %v", err), http.StatusBadRequest)
			return
		}
	}

	if err := s.configStore.SetActiveAdapter(r.Context(), slot, adapterID, cfg); err != nil {
		code := http.StatusInternalServerError
		if configstore.IsNotFound(err) {
			code = http.StatusNotFound
		} else if strings.Contains(strings.ToLower(err.Error()), "invalid") {
			code = http.StatusBadRequest
		}
		http.Error(w, fmt.Sprintf("set adapter binding failed: %v", err), code)
		return
	}

	status, err := s.modules.StartSlot(r.Context(), modules.Slot(slot))
	if err != nil {
		http.Error(w, fmt.Sprintf("start module failed: %v", err), http.StatusInternalServerError)
		return
	}

	response := apihttp.ModuleActionResult{Module: bindingStatusToResponse(*status)}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *APIServer) handleModulesStart(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.modules == nil {
		http.Error(w, "modules service unavailable", http.StatusServiceUnavailable)
		return
	}

	var payload moduleActionRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(payload.Slot) == "" {
		http.Error(w, "slot is required", http.StatusBadRequest)
		return
	}

	status, err := s.modules.StartSlot(r.Context(), modules.Slot(payload.Slot))
	if err != nil {
		http.Error(w, fmt.Sprintf("start module failed: %v", err), http.StatusInternalServerError)
		return
	}

	response := apihttp.ModuleActionResult{Module: bindingStatusToResponse(*status)}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *APIServer) handleModulesStop(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.modules == nil {
		http.Error(w, "modules service unavailable", http.StatusServiceUnavailable)
		return
	}

	var payload moduleActionRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(payload.Slot) == "" {
		http.Error(w, "slot is required", http.StatusBadRequest)
		return
	}

	status, err := s.modules.StopSlot(r.Context(), modules.Slot(payload.Slot))
	if err != nil {
		http.Error(w, fmt.Sprintf("stop module failed: %v", err), http.StatusInternalServerError)
		return
	}

	response := apihttp.ModuleActionResult{Module: bindingStatusToResponse(*status)}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *APIServer) publishAudioInterrupt(sessionID, streamID, reason string, metadata map[string]string) {
	if s.eventBus == nil {
		return
	}

	event := eventbus.AudioInterruptEvent{
		SessionID: sessionID,
		StreamID:  streamID,
		Reason:    strings.TrimSpace(reason),
		Timestamp: time.Now().UTC(),
		Metadata:  cloneStringMap(metadata),
	}

	s.eventBus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicAudioInterrupt,
		Source:  eventbus.SourceClient,
		Payload: event,
	})
}

func (s *APIServer) handleAudioIngress(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST,OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.audioIngress == nil {
		http.Error(w, "audio ingress unavailable", http.StatusServiceUnavailable)
		return
	}

	query := r.URL.Query()
	sessionID := strings.TrimSpace(query.Get("session_id"))
	if sessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}
	streamID := strings.TrimSpace(query.Get("stream_id"))
	if streamID == "" {
		streamID = defaultCaptureStreamID
	}

	if s.sessionManager != nil {
		if _, err := s.sessionManager.GetSession(sessionID); err != nil {
			http.Error(w, fmt.Sprintf("session %s not found", sessionID), http.StatusNotFound)
			return
		}
	}

	readiness, err := s.voiceReadiness(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("voice readiness check failed: %v", err), http.StatusInternalServerError)
		return
	}
	if !readiness.CaptureEnabled {
		diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.STTPrimary)
		message := voiceIssueSummary(diags, "voice capture unavailable")
		writeVoiceError(w, http.StatusPreconditionFailed, message, diags, readiness)
		return
	}

	format, err := parseAudioFormatQuery(query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	metadata, err := metadataFromQuery(query)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid metadata: %v", err), http.StatusBadRequest)
		return
	}

	uploadCtx, cancel := context.WithTimeout(r.Context(), maxAudioUploadDuration)
	defer cancel()

	stream, err := s.audioIngress.OpenStream(sessionID, streamID, format, metadata)
	if err != nil {
		switch {
		case errors.Is(err, ErrAudioStreamExists):
			http.Error(w, fmt.Sprintf("audio stream %s/%s already exists", sessionID, streamID), http.StatusConflict)
		default:
			http.Error(w, fmt.Sprintf("open stream: %v", err), http.StatusInternalServerError)
		}
		return
	}
	defer func() {
		if stream != nil {
			_ = stream.Close()
		}
	}()

	buffer := make([]byte, defaultIngressBuffer)
	for {
		if err := uploadCtx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				http.Error(w, "upload timeout", http.StatusRequestTimeout)
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
		n, readErr := r.Body.Read(buffer)
		if n > 0 {
			if err := stream.Write(buffer[:n]); err != nil {
				http.Error(w, fmt.Sprintf("write stream: %v", err), http.StatusInternalServerError)
				return
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if errors.Is(readErr, context.Canceled) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Error(w, fmt.Sprintf("read body: %v", readErr), http.StatusInternalServerError)
			return
		}
	}

	if err := stream.Close(); err != nil {
		http.Error(w, fmt.Sprintf("close stream: %v", err), http.StatusInternalServerError)
		return
	}
	stream = nil

	w.WriteHeader(http.StatusNoContent)
}

func (s *APIServer) handleAudioEgress(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET,OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
		return
	}
	if s.eventBus == nil {
		http.Error(w, "event bus unavailable", http.StatusServiceUnavailable)
		return
	}

	query := r.URL.Query()
	sessionID := strings.TrimSpace(query.Get("session_id"))
	if sessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}
	streamID := strings.TrimSpace(query.Get("stream_id"))
	if streamID == "" {
		if s.audioEgress != nil {
			streamID = s.audioEgress.DefaultStreamID()
		} else {
			streamID = defaultTTSStreamID
		}
	}

	if s.sessionManager != nil {
		if _, err := s.sessionManager.GetSession(sessionID); err != nil {
			http.Error(w, fmt.Sprintf("session %s not found", sessionID), http.StatusNotFound)
			return
		}
	}

	readiness, err := s.voiceReadiness(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("voice readiness check failed: %v", err), http.StatusInternalServerError)
		return
	}
	if !readiness.PlaybackEnabled {
		diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.TTSPrimary)
		message := voiceIssueSummary(diags, "voice playback unavailable")
		writeVoiceError(w, http.StatusPreconditionFailed, message, diags, readiness)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	sub := s.eventBus.Subscribe(
		eventbus.TopicAudioEgressPlayback,
		eventbus.WithSubscriptionName(fmt.Sprintf("http_audio_out_%s_%s_%d", sessionID, streamID, time.Now().UnixNano())),
		eventbus.WithSubscriptionBuffer(64),
	)
	defer sub.Close()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	encoder := json.NewEncoder(w)
	ctx := r.Context()
	formatSent := false

	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				errorChunk := audioEgressHTTPChunk{
					Error: "playback subscription closed",
					Final: true,
				}
				_ = encoder.Encode(errorChunk)
				flusher.Flush()
				return
			}
			evt, ok := env.Payload.(eventbus.AudioEgressPlaybackEvent)
			if !ok || evt.SessionID != sessionID || evt.StreamID != streamID {
				continue
			}

			payload := audioEgressHTTPChunk{
				Sequence:   evt.Sequence,
				DurationMs: durationToMillis(playbackDuration(evt)),
				Final:      evt.Final,
				Metadata:   cloneStringMap(evt.Metadata),
				Data:       base64.StdEncoding.EncodeToString(evt.Data),
			}
			if !formatSent {
				payload.Format = audioFormatPayload(evt.Format)
				formatSent = true
			}

			if err := encoder.Encode(payload); err != nil {
				errorChunk := audioEgressHTTPChunk{
					Error: "encoding error",
					Final: true,
				}
				_ = encoder.Encode(errorChunk)
				flusher.Flush()
				return
			}
			flusher.Flush()

			if evt.Final {
				return
			}
		}
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

func writeAudioWebSocketError(conn *websocket.Conn, message string) {
	if conn == nil {
		return
	}
	bytes, err := json.Marshal(audioEgressHTTPChunk{Error: message, Final: true})
	if err != nil {
		return
	}
	conn.SetWriteDeadline(time.Now().Add(websocketWriteTimeout))
	_ = conn.WriteMessage(websocket.TextMessage, bytes)
}

func parseAudioFormatQuery(values url.Values) (eventbus.AudioFormat, error) {
	format := defaultCaptureFormat

	if v := strings.TrimSpace(values.Get("encoding")); v != "" {
		if !strings.EqualFold(v, string(eventbus.AudioEncodingPCM16)) {
			return eventbus.AudioFormat{}, fmt.Errorf("unsupported encoding %q", v)
		}
	}

	sampleRate, err := intQueryParam(values, "sample_rate", format.SampleRate)
	if err != nil {
		return eventbus.AudioFormat{}, err
	}
	channels, err := intQueryParam(values, "channels", format.Channels)
	if err != nil {
		return eventbus.AudioFormat{}, err
	}
	bitDepth, err := intQueryParam(values, "bit_depth", format.BitDepth)
	if err != nil {
		return eventbus.AudioFormat{}, err
	}
	frameMs, err := intQueryParam(values, "frame_ms", int(format.FrameDuration/time.Millisecond))
	if err != nil {
		return eventbus.AudioFormat{}, err
	}

	format.SampleRate = sampleRate
	format.Channels = channels
	format.BitDepth = bitDepth
	format.FrameDuration = time.Duration(frameMs) * time.Millisecond
	return format, nil
}

func intQueryParam(values url.Values, key string, def int) (int, error) {
	val := strings.TrimSpace(values.Get(key))
	if val == "" {
		return def, nil
	}
	n, err := strconv.Atoi(val)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return n, nil
}

func (s *APIServer) handleAudioIngressWS(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET,OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.audioIngress == nil {
		http.Error(w, "audio ingress unavailable", http.StatusServiceUnavailable)
		return
	}

	query := r.URL.Query()
	sessionID := strings.TrimSpace(query.Get("session_id"))
	if sessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}
	streamID := strings.TrimSpace(query.Get("stream_id"))
	if streamID == "" {
		streamID = defaultCaptureStreamID
	}

	if s.sessionManager != nil {
		if _, err := s.sessionManager.GetSession(sessionID); err != nil {
			http.Error(w, fmt.Sprintf("session %s not found", sessionID), http.StatusNotFound)
			return
		}
	}

	readiness, err := s.voiceReadiness(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("voice readiness check failed: %v", err), http.StatusInternalServerError)
		return
	}
	if !readiness.CaptureEnabled {
		diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.STTPrimary)
		message := voiceIssueSummary(diags, "voice capture unavailable")
		writeVoiceError(w, http.StatusPreconditionFailed, message, diags, readiness)
		return
	}

	format, err := parseAudioFormatQuery(query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	metadata, err := metadataFromQuery(query)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid metadata: %v", err), http.StatusBadRequest)
		return
	}

	conn, err := audioWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxWebSocketMessageBytes)
	conn.SetReadDeadline(time.Now().Add(maxAudioUploadDuration))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(websocketHeartbeatTimeout))
	})
	pingTicker := time.NewTicker(websocketHeartbeatInterval)
	defer pingTicker.Stop()

	uploadCtx, cancel := context.WithTimeout(r.Context(), maxAudioUploadDuration)
	defer cancel()

	stream, err := s.audioIngress.OpenStream(sessionID, streamID, format, metadata)
	if err != nil {
		writeAudioWebSocketError(conn, err.Error())
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "open stream failed"))
		return
	}
	defer func() {
		if stream != nil {
			_ = stream.Close()
		}
	}()

	for {
		if err := uploadCtx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				writeAudioWebSocketError(conn, "upload timeout")
				_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "timeout"))
			}
			return
		}
		select {
		case <-pingTicker.C:
			conn.SetWriteDeadline(time.Now().Add(websocketWriteTimeout))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
			continue
		default:
		}

		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) || errors.Is(err, context.Canceled) {
				return
			}
			writeAudioWebSocketError(conn, fmt.Sprintf("read error: %v", err))
			_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "read error"))
			return
		}
		if messageType == websocket.CloseMessage {
			return
		}
		if messageType != websocket.BinaryMessage {
			writeAudioWebSocketError(conn, "binary frames required")
			_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseUnsupportedData, "binary frames required"))
			return
		}
		if len(payload) == 0 {
			continue
		}
		if err := stream.Write(payload); err != nil {
			writeAudioWebSocketError(conn, fmt.Sprintf("write stream: %v", err))
			_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "write error"))
			return
		}
	}
}

func (s *APIServer) handleAudioEgressWS(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET,OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
		return
	}
	if s.eventBus == nil {
		http.Error(w, "event bus unavailable", http.StatusServiceUnavailable)
		return
	}

	query := r.URL.Query()
	sessionID := strings.TrimSpace(query.Get("session_id"))
	if sessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}
	streamID := strings.TrimSpace(query.Get("stream_id"))
	if streamID == "" {
		if s.audioEgress != nil {
			streamID = s.audioEgress.DefaultStreamID()
		} else {
			streamID = defaultTTSStreamID
		}
	}

	if s.sessionManager != nil {
		if _, err := s.sessionManager.GetSession(sessionID); err != nil {
			http.Error(w, fmt.Sprintf("session %s not found", sessionID), http.StatusNotFound)
			return
		}
	}

	readiness, err := s.voiceReadiness(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("voice readiness check failed: %v", err), http.StatusInternalServerError)
		return
	}
	if !readiness.PlaybackEnabled {
		diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.TTSPrimary)
		message := voiceIssueSummary(diags, "voice playback unavailable")
		writeVoiceError(w, http.StatusPreconditionFailed, message, diags, readiness)
		return
	}

	conn, err := audioWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxWebSocketMessageBytes)
	conn.SetReadDeadline(time.Now().Add(websocketHeartbeatTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(websocketHeartbeatTimeout))
	})

	sub := s.eventBus.Subscribe(
		eventbus.TopicAudioEgressPlayback,
		eventbus.WithSubscriptionName(fmt.Sprintf("ws_audio_out_%s_%s_%d", sessionID, streamID, time.Now().UnixNano())),
		eventbus.WithSubscriptionBuffer(64),
	)
	defer sub.Close()

	ctx := r.Context()
	formatSent := false
	pingTicker := time.NewTicker(websocketHeartbeatInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-pingTicker.C:
			conn.SetWriteDeadline(time.Now().Add(websocketWriteTimeout))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
			continue
		case <-ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				writeAudioWebSocketError(conn, "playback subscription closed")
				_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "playback subscription closed"))
				return
			}
			evt, ok := env.Payload.(eventbus.AudioEgressPlaybackEvent)
			if !ok || evt.SessionID != sessionID || evt.StreamID != streamID {
				continue
			}

			payload := audioEgressHTTPChunk{
				Sequence:   evt.Sequence,
				DurationMs: durationToMillis(playbackDuration(evt)),
				Final:      evt.Final,
				Metadata:   cloneStringMap(evt.Metadata),
				Data:       base64.StdEncoding.EncodeToString(evt.Data),
			}
			if !formatSent {
				payload.Format = audioFormatPayload(evt.Format)
				formatSent = true
			}

			bytes, err := json.Marshal(payload)
			if err != nil {
				writeAudioWebSocketError(conn, "encoding error")
				return
			}
			conn.SetWriteDeadline(time.Now().Add(websocketWriteTimeout))
			if err := conn.WriteMessage(websocket.TextMessage, bytes); err != nil {
				return
			}
			if evt.Final {
				return
			}
		}
	}
}

func metadataFromQuery(values url.Values) (map[string]string, error) {
	var metadata map[string]string
	if raw := strings.TrimSpace(values.Get("metadata")); raw != "" {
		if len(raw) > maxMetadataTotalPayload {
			return nil, fmt.Errorf("metadata payload too large")
		}
		var parsed map[string]string
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return nil, err
		}
		metadata = make(map[string]string, len(parsed))
		if err := mergeMetadataEntries(metadata, parsed); err != nil {
			return nil, err
		}
	}
	for key, vals := range values {
		if !strings.HasPrefix(key, "meta_") {
			continue
		}
		if len(vals) == 0 {
			continue
		}
		entryKey := strings.TrimPrefix(key, "meta_")
		if metadata == nil {
			metadata = make(map[string]string)
		}
		if err := addMetadataEntry(metadata, entryKey, vals[len(vals)-1]); err != nil {
			return nil, fmt.Errorf("metadata %s: %w", entryKey, err)
		}
	}
	return metadata, nil
}

type audioEgressHTTPChunk struct {
	Format     *audioFormatJSON  `json:"format,omitempty"`
	Sequence   uint64            `json:"sequence,omitempty"`
	DurationMs uint32            `json:"duration_ms,omitempty"`
	Final      bool              `json:"final,omitempty"`
	Error      string            `json:"error,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Data       string            `json:"data,omitempty"`
}

type audioFormatJSON struct {
	Encoding        string `json:"encoding"`
	SampleRate      int    `json:"sample_rate"`
	Channels        int    `json:"channels"`
	BitDepth        int    `json:"bit_depth"`
	FrameDurationMs uint32 `json:"frame_duration_ms"`
}

type audioCapabilityJSON struct {
	StreamID string            `json:"stream_id"`
	Format   audioFormatJSON   `json:"format"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type audioCapabilitiesResponse struct {
	Capture         []audioCapabilityJSON `json:"capture"`
	Playback        []audioCapabilityJSON `json:"playback"`
	CaptureEnabled  bool                  `json:"capture_enabled"`
	PlaybackEnabled bool                  `json:"playback_enabled"`
	Diagnostics     []voiceDiagnostic     `json:"diagnostics,omitempty"`
}

type voiceDiagnostic struct {
	Slot    string `json:"slot"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type voiceErrorResponse struct {
	Error         string            `json:"error"`
	Diagnostics   []voiceDiagnostic `json:"diagnostics,omitempty"`
	CaptureReady  *bool             `json:"capture_ready,omitempty"`
	PlaybackReady *bool             `json:"playback_ready,omitempty"`
}

func (s *APIServer) voiceReadiness(ctx context.Context) (configstore.VoiceReadiness, error) {
	if s.configStore == nil {
		readiness := configstore.VoiceReadiness{
			CaptureEnabled:  s.audioIngress != nil,
			PlaybackEnabled: s.audioEgress != nil,
			Issues:          nil,
		}
		if s.audioIngress == nil {
			readiness.Issues = appendVoiceIssue(readiness.Issues, slots.STTPrimary, voiceIssueCodeServiceUnavailable, "audio ingress service unavailable")
		}
		if s.audioEgress == nil {
			readiness.Issues = appendVoiceIssue(readiness.Issues, slots.TTSPrimary, voiceIssueCodeServiceUnavailable, "audio egress service unavailable")
		}
		return readiness, nil
	}
	readiness, err := s.configStore.VoiceReadiness(ctx)
	if err != nil {
		return configstore.VoiceReadiness{}, err
	}
	if s.audioIngress == nil {
		readiness.CaptureEnabled = false
		readiness.Issues = appendVoiceIssue(readiness.Issues, slots.STTPrimary, voiceIssueCodeServiceUnavailable, "audio ingress service unavailable")
	}
	if s.audioEgress == nil {
		readiness.PlaybackEnabled = false
		readiness.Issues = appendVoiceIssue(readiness.Issues, slots.TTSPrimary, voiceIssueCodeServiceUnavailable, "audio egress service unavailable")
	}
	return readiness, nil
}

func appendVoiceIssue(issues []configstore.VoiceIssue, slot, code, message string) []configstore.VoiceIssue {
	for _, issue := range issues {
		if issue.Slot == slot && issue.Code == code {
			return issues
		}
	}
	return append(issues, configstore.VoiceIssue{Slot: slot, Code: code, Message: message})
}

func mapVoiceDiagnostics(issues []configstore.VoiceIssue) []voiceDiagnostic {
	if len(issues) == 0 {
		return nil
	}
	out := make([]voiceDiagnostic, 0, len(issues))
	for _, issue := range issues {
		out = append(out, voiceDiagnostic{
			Slot:    issue.Slot,
			Code:    issue.Code,
			Message: issue.Message,
		})
	}
	return out
}

// filterDiagnostics narrows diagnostics to the exact slot name expected by the daemon.
// Comparison is case-sensitive because slot identifiers are canonicalised upstream.
func filterDiagnostics(diags []voiceDiagnostic, slot string) []voiceDiagnostic {
	if len(diags) == 0 {
		return nil
	}
	filtered := make([]voiceDiagnostic, 0, len(diags))
	for _, diag := range diags {
		if diag.Slot == slot {
			filtered = append(filtered, diag)
		}
	}
	return filtered
}

func voiceIssueSummary(diags []voiceDiagnostic, fallback string) string {
	for _, diag := range diags {
		if msg := strings.TrimSpace(diag.Message); msg != "" {
			return msg
		}
	}
	return fallback
}

func writeVoiceError(w http.ResponseWriter, status int, message string, diags []voiceDiagnostic, readiness configstore.VoiceReadiness) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	payload := voiceErrorResponse{
		Error:       message,
		Diagnostics: diags,
	}
	capture := readiness.CaptureEnabled
	playback := readiness.PlaybackEnabled
	payload.CaptureReady = &capture
	payload.PlaybackReady = &playback

	_ = json.NewEncoder(w).Encode(payload)
}

func audioFormatPayload(format eventbus.AudioFormat) *audioFormatJSON {
	return &audioFormatJSON{
		Encoding:        string(format.Encoding),
		SampleRate:      format.SampleRate,
		Channels:        format.Channels,
		BitDepth:        format.BitDepth,
		FrameDurationMs: uint32((format.FrameDuration + time.Millisecond/2) / time.Millisecond),
	}
}

func mergeMetadataEntries(dst map[string]string, src map[string]string) error {
	for k, v := range src {
		if err := addMetadataEntry(dst, k, v); err != nil {
			return err
		}
	}
	return nil
}

func addMetadataEntry(dst map[string]string, key, value string) error {
	if dst == nil {
		return fmt.Errorf("metadata map nil")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("empty key")
	}
	if utf8.RuneCountInString(key) > maxMetadataKeyRunes {
		return fmt.Errorf("key too long")
	}
	trimmedValue := strings.TrimSpace(value)
	if utf8.RuneCountInString(trimmedValue) > maxMetadataValueRunes {
		return fmt.Errorf("value too long")
	}
	if len(dst) >= maxMetadataEntries {
		if _, exists := dst[key]; !exists {
			return fmt.Errorf("too many metadata entries")
		}
	}
	currentTotal := 0
	for k, v := range dst {
		currentTotal += len(k) + len(v)
	}
	entrySize := len(key) + len(trimmedValue)
	if currentTotal+entrySize > maxMetadataTotalPayload {
		return fmt.Errorf("metadata payload too large")
	}
	dst[key] = trimmedValue
	return nil
}

func (s *APIServer) handleAudioCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET,OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
		return
	}

	query := r.URL.Query()
	sessionID := strings.TrimSpace(query.Get("session_id"))
	if sessionID != "" && s.sessionManager != nil {
		if _, err := s.sessionManager.GetSession(sessionID); err != nil {
			http.Error(w, fmt.Sprintf("session %s not found", sessionID), http.StatusNotFound)
			return
		}
	}

	readiness, err := s.voiceReadiness(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("voice readiness check failed: %v", err), http.StatusInternalServerError)
		return
	}

	resp := audioCapabilitiesResponse{
		Capture:         make([]audioCapabilityJSON, 0, 1),
		Playback:        make([]audioCapabilityJSON, 0, 1),
		CaptureEnabled:  readiness.CaptureEnabled,
		PlaybackEnabled: readiness.PlaybackEnabled,
		Diagnostics:     mapVoiceDiagnostics(readiness.Issues),
	}

	if s.audioIngress != nil {
		if format := audioFormatPayload(defaultCaptureFormat); format != nil {
			metadata := map[string]string{
				"recommended": "true",
				"ready":       strconv.FormatBool(readiness.CaptureEnabled),
			}
			if !readiness.CaptureEnabled {
				diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.STTPrimary)
				metadata["diagnostics"] = voiceIssueSummary(diags, "voice capture unavailable")
			}
			resp.Capture = append(resp.Capture, audioCapabilityJSON{
				StreamID: defaultCaptureStreamID,
				Format:   *format,
				Metadata: metadata,
			})
		}
	}

	if s.audioEgress != nil {
		if format := audioFormatPayload(s.audioEgress.PlaybackFormat()); format != nil {
			metadata := map[string]string{
				"recommended": "true",
				"ready":       strconv.FormatBool(readiness.PlaybackEnabled),
			}
			if !readiness.PlaybackEnabled {
				diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.TTSPrimary)
				metadata["diagnostics"] = voiceIssueSummary(diags, "voice playback unavailable")
			}
			resp.Playback = append(resp.Playback, audioCapabilityJSON{
				StreamID: s.audioEgress.DefaultStreamID(),
				Format:   *format,
				Metadata: metadata,
			})
		}
	}

	payload, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, fmt.Sprintf("encode capabilities: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(payload); err != nil {
		log.Printf("[APIServer] failed to write audio capabilities response: %v", err)
	}
}

func (s *APIServer) handleAudioInterrupt(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST,OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}

	var payload audioInterruptRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	sessionID := strings.TrimSpace(payload.SessionID)
	if sessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}

	streamID := strings.TrimSpace(payload.StreamID)
	if streamID == "" {
		if s.audioEgress != nil {
			streamID = s.audioEgress.DefaultStreamID()
		} else {
			streamID = defaultTTSStreamID
		}
	}

	reason := strings.TrimSpace(payload.Reason)
	metadata := cloneStringMap(payload.Metadata)

	if s.sessionManager != nil {
		if _, err := s.sessionManager.GetSession(sessionID); err != nil {
			http.Error(w, fmt.Sprintf("session %s not found", sessionID), http.StatusNotFound)
			return
		}
	}

	readiness, err := s.voiceReadiness(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("voice readiness check failed: %v", err), http.StatusInternalServerError)
		return
	}
	if !readiness.PlaybackEnabled {
		diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), defaultTTSStreamID)
		message := voiceIssueSummary(diags, "voice playback unavailable")
		writeVoiceError(w, http.StatusPreconditionFailed, message, diags, readiness)
		return
	}

	s.publishAudioInterrupt(sessionID, streamID, reason, metadata)
	if s.audioEgress != nil {
		s.audioEgress.Interrupt(sessionID, streamID, reason, metadata)
	}

	w.WriteHeader(http.StatusNoContent)
}

func bindingStatusToResponse(status modules.BindingStatus) apihttp.ModuleEntry {
	resp := apihttp.ModuleEntry{
		Slot:      string(status.Slot),
		Status:    status.Status,
		Config:    status.Config,
		UpdatedAt: status.UpdatedAt,
	}
	if status.AdapterID != nil {
		id := strings.TrimSpace(*status.AdapterID)
		if id != "" {
			resp.AdapterID = &id
		}
	}
	if status.Runtime != nil {
		runtime := apihttp.ModuleRuntime{
			ModuleID:  status.Runtime.ModuleID,
			Health:    string(status.Runtime.Health),
			Message:   status.Runtime.Message,
			UpdatedAt: status.Runtime.UpdatedAt.UTC().Format(time.RFC3339),
			Extra:     status.Runtime.Extra,
		}
		if status.Runtime.StartedAt != nil {
			started := status.Runtime.StartedAt.UTC().Format(time.RFC3339)
			runtime.StartedAt = &started
		}
		resp.Runtime = &runtime
	}
	return resp
}

func (s *APIServer) daemonStatusSnapshot(ctx context.Context) (daemonStatusSnapshot, error) {
	snapshot := daemonStatusSnapshot{
		Version:       "0.2.0",
		SessionsCount: len(s.sessionManager.ListSessions()),
		AuthRequired:  s.isAuthRequired(),
	}

	if s.runtime != nil {
		snapshot.Port = s.runtime.Port()
		snapshot.GRPCPort = s.runtime.GRPCPort()
		if start := s.runtime.StartTime(); !start.IsZero() {
			snapshot.UptimeSeconds = time.Since(start).Seconds()
		}
	}

	if s.configStore != nil {
		cfg, err := s.configStore.GetTransportConfig(ctx)
		if err != nil {
			return snapshot, err
		}
		snapshot.Binding = normalizeBinding(cfg.Binding)
		snapshot.GRPCBinding = normalizeBinding(cfg.GRPCBinding)
		if snapshot.GRPCBinding == "" {
			snapshot.GRPCBinding = snapshot.Binding
		}
		cert := strings.TrimSpace(cfg.TLSCertPath)
		key := strings.TrimSpace(cfg.TLSKeyPath)
		if cert != "" && key != "" {
			snapshot.TLSEnabled = true
		}
		if snapshot.Port == 0 && cfg.Port > 0 {
			snapshot.Port = cfg.Port
		}
		if snapshot.GRPCPort == 0 && cfg.GRPCPort > 0 {
			snapshot.GRPCPort = cfg.GRPCPort
		}
	}

	return snapshot, nil
}

func (s *APIServer) handleDaemonStatus(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	snapshot, err := s.daemonStatusSnapshot(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to compute daemon status: %v", err), http.StatusInternalServerError)
		return
	}

	response := map[string]interface{}{
		"version":        snapshot.Version,
		"sessions_count": snapshot.SessionsCount,
		"port":           snapshot.Port,
		"grpc_port":      snapshot.GRPCPort,
		"binding":        snapshot.Binding,
		"grpc_binding":   snapshot.GRPCBinding,
		"auth_required":  snapshot.AuthRequired,
	}
	if snapshot.UptimeSeconds > 0 {
		response["uptime"] = snapshot.UptimeSeconds
	}
	if snapshot.TLSEnabled {
		response["tls_enabled"] = true
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *APIServer) handleDaemonShutdown(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}

	s.shutdownMu.RLock()
	shutdown := s.shutdownFn
	s.shutdownMu.RUnlock()

	if shutdown == nil {
		http.Error(w, "daemon shutdown not available", http.StatusNotImplemented)
		return
	}

	// Trigger shutdown asynchronously so we can return 202 immediately.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			log.Printf("[APIServer] shutdown handler returned error: %v", err)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]any{
		"status":  "shutting_down",
		"message": "daemon shutdown initiated",
	})
}

func (s *APIServer) handleAuthTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		s.handleAuthTokensGet(w, r)
	case http.MethodPost:
		s.handleAuthTokensPost(w, r)
	case http.MethodDelete:
		s.handleAuthTokensDelete(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type tokenResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Role        string `json:"role"`
	MaskedToken string `json:"masked_token"`
	CreatedAt   string `json:"created_at"`
}

func maskToken(token string) string {
	if len(token) <= 8 {
		return token
	}
	return token[:4] + strings.Repeat("*", len(token)-8) + token[len(token)-4:]
}

func (s *APIServer) handleAuthTokensGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}

	tokens, err := s.loadAuthTokens(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := struct {
		Tokens []tokenResponse `json:"tokens"`
	}{
		Tokens: make([]tokenResponse, 0, len(tokens)),
	}

	sort.Slice(tokens, func(i, j int) bool {
		return tokens[i].CreatedAt.Before(tokens[j].CreatedAt)
	})

	for _, token := range tokens {
		entry := tokenResponse{
			ID:          token.ID,
			Name:        token.Name,
			Role:        token.Role,
			MaskedToken: maskToken(token.Token),
			CreatedAt:   token.CreatedAt.Format(time.RFC3339),
		}
		resp.Tokens = append(resp.Tokens, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *APIServer) handleAuthTokensPost(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}

	ctx := r.Context()
	tokens, err := s.loadAuthTokens(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var payload struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil && err != io.EOF {
			http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
			return
		}
	}

	token, err := generateAPIToken()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to generate token: %v", err), http.StatusInternalServerError)
		return
	}

	entry := newStoredToken(token, payload.Name, payload.Role)
	if entry.Token == "" {
		http.Error(w, "failed to create token entry", http.StatusInternalServerError)
		return
	}

	tokens = append(tokens, entry)
	if err := s.storeAuthTokens(ctx, tokens); err != nil {
		http.Error(w, fmt.Sprintf("failed to persist token: %v", err), http.StatusInternalServerError)
		return
	}

	s.setAuthTokens(tokens, s.AuthRequired())

	resp := struct {
		Token string `json:"token"`
		ID    string `json:"id"`
		Name  string `json:"name,omitempty"`
		Role  string `json:"role"`
	}{
		Token: token,
		ID:    entry.ID,
		Name:  entry.Name,
		Role:  entry.Role,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *APIServer) handleAuthTokensDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}

	var payload struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	target := strings.TrimSpace(payload.Token)
	id := strings.TrimSpace(payload.ID)
	if target == "" && id == "" {
		http.Error(w, "token or id is required", http.StatusBadRequest)
		return
	}

	tokens, err := s.loadAuthTokens(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	newTokens := make([]storedToken, 0, len(tokens))
	removed := false
	for _, tok := range tokens {
		if (target != "" && tok.Token == target) || (id != "" && tok.ID == id) {
			removed = true
			continue
		}
		newTokens = append(newTokens, tok)
	}

	if !removed {
		http.Error(w, "token not found", http.StatusNotFound)
		return
	}

	if err := s.storeAuthTokens(r.Context(), newTokens); err != nil {
		http.Error(w, fmt.Sprintf("failed to persist tokens: %v", err), http.StatusInternalServerError)
		return
	}

	s.setAuthTokens(newTokens, s.AuthRequired())
	w.WriteHeader(http.StatusNoContent)
}

func (s *APIServer) handleAuthPairings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		s.handleAuthPairingsGet(w, r)
	case http.MethodPost:
		s.handleAuthPairingsPost(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleAuthPairingsGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}

	pairings, err := s.loadPairings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := struct {
		Pairings []pairingEntry `json:"pairings"`
	}{
		Pairings: sanitizePairings(pairings, time.Now().UTC()),
	}

	if err := s.storePairings(r.Context(), resp.Pairings); err != nil {
		log.Printf("[AuthPairings] failed to persist pairings cleanup: %v", err)
	}

	sort.Slice(resp.Pairings, func(i, j int) bool {
		return resp.Pairings[i].ExpiresAt.Before(resp.Pairings[j].ExpiresAt)
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *APIServer) handleAuthPairingsPost(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}

	pairings, err := s.loadPairings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var payload struct {
		Name      string `json:"name"`
		Role      string `json:"role"`
		ExpiresIn int    `json:"expires_in_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	duration := time.Duration(payload.ExpiresIn) * time.Second
	if duration <= 0 || duration > 30*time.Minute {
		duration = 5 * time.Minute
	}

	code, err := generatePairingCode()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to generate pairing code: %v", err), http.StatusInternalServerError)
		return
	}

	now := time.Now().UTC()
	entry := pairingEntry{
		Code:      code,
		Name:      strings.TrimSpace(payload.Name),
		Role:      normalizeRole(payload.Role),
		CreatedAt: now,
		ExpiresAt: now.Add(duration),
	}

	pairings = append(pairings, entry)
	pairings = sanitizePairings(pairings, now)
	if err := s.storePairings(r.Context(), pairings); err != nil {
		http.Error(w, fmt.Sprintf("failed to persist pairing: %v", err), http.StatusInternalServerError)
		return
	}

	resp := struct {
		Code      string `json:"pair_code"`
		Name      string `json:"name,omitempty"`
		Role      string `json:"role"`
		ExpiresAt string `json:"expires_at"`
	}{
		Code:      entry.Code,
		Name:      entry.Name,
		Role:      entry.Role,
		ExpiresAt: entry.ExpiresAt.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *APIServer) handleAuthPair(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
		s.handleAuthPairClaim(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleAuthPairClaim(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	code := strings.ToUpper(strings.TrimSpace(payload.Code))
	if code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	pairings, err := s.loadPairings(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	now := time.Now().UTC()
	index := -1
	var entry pairingEntry
	for i, pairing := range pairings {
		if strings.EqualFold(pairing.Code, code) {
			entry = pairing
			index = i
			break
		}
	}

	if index == -1 {
		http.Error(w, "pairing code not found", http.StatusNotFound)
		return
	}

	pairings = append(pairings[:index], pairings[index+1:]...)
	pairings = sanitizePairings(pairings, now)
	if err := s.storePairings(ctx, pairings); err != nil {
		http.Error(w, fmt.Sprintf("failed to persist pairings: %v", err), http.StatusInternalServerError)
		return
	}

	if now.After(entry.ExpiresAt) {
		http.Error(w, "pairing code expired", http.StatusGone)
		return
	}

	tokens, err := s.loadAuthTokens(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	newTokenValue, err := generateAPIToken()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to generate token: %v", err), http.StatusInternalServerError)
		return
	}

	name := strings.TrimSpace(payload.Name)
	if name == "" {
		name = entry.Name
	}

	newEntry := newStoredToken(newTokenValue, name, entry.Role)
	if newEntry.Token == "" {
		http.Error(w, "failed to create token entry", http.StatusInternalServerError)
		return
	}

	tokens = append(tokens, newEntry)
	if err := s.storeAuthTokens(ctx, tokens); err != nil {
		http.Error(w, fmt.Sprintf("failed to persist token: %v", err), http.StatusInternalServerError)
		return
	}

	s.setAuthTokens(tokens, s.AuthRequired())

	resp := struct {
		Token     string `json:"token"`
		Name      string `json:"name,omitempty"`
		Role      string `json:"role"`
		CreatedAt string `json:"created_at"`
	}{
		Token:     newTokenValue,
		Name:      newEntry.Name,
		Role:      newEntry.Role,
		CreatedAt: newEntry.CreatedAt.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *APIServer) loadPairings(ctx context.Context) ([]pairingEntry, error) {
	if s.configStore == nil {
		return nil, nil
	}

	values, err := s.configStore.LoadSecuritySettings(ctx, "auth.pairings")
	if err != nil {
		return nil, err
	}

	raw, ok := values["auth.pairings"]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	var entries []pairingEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("parse auth.pairings: %w", err)
	}

	return sanitizePairings(entries, time.Now().UTC()), nil
}

func (s *APIServer) storePairings(ctx context.Context, entries []pairingEntry) error {
	if s.configStore == nil {
		return nil
	}
	payload, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return s.configStore.SaveSecuritySettings(ctx, map[string]string{
		"auth.pairings": string(payload),
	})
}

func sanitizePairings(entries []pairingEntry, now time.Time) []pairingEntry {
	if len(entries) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(entries))
	result := make([]pairingEntry, 0, len(entries))
	for _, entry := range entries {
		entry.Code = strings.ToUpper(strings.TrimSpace(entry.Code))
		if entry.Code == "" {
			continue
		}
		if _, exists := seen[entry.Code]; exists {
			continue
		}
		if entry.Role == "" {
			entry.Role = string(roleAdmin)
		} else {
			entry.Role = normalizeRole(entry.Role)
		}
		if entry.CreatedAt.IsZero() {
			entry.CreatedAt = now
		}
		if entry.ExpiresAt.IsZero() {
			entry.ExpiresAt = entry.CreatedAt.Add(5 * time.Minute)
		}
		if now.After(entry.ExpiresAt) {
			continue
		}
		seen[entry.Code] = struct{}{}
		result = append(result, entry)
	}
	return result
}

func generatePairingCode() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	code := strings.ToUpper(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
	if len(code) > 10 {
		code = code[:10]
	}
	return code, nil
}

func (s *APIServer) handleQuickstart(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		s.handleQuickstartGet(w, r)
	case http.MethodPost:
		s.handleQuickstartPost(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleQuickstartGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.configStore == nil {
		http.Error(w, "configuration store not available", http.StatusServiceUnavailable)
		return
	}

	completed, completedAt, err := s.configStore.QuickstartStatus(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	pending, err := s.configStore.PendingQuickstartSlots(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	moduleStatuses, err := s.quickstartModuleStatuses(r.Context())
	if err != nil {
		if errors.Is(err, errModulesServiceUnavailable) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		} else {
			http.Error(w, fmt.Sprintf("modules overview failed: %v", err), http.StatusInternalServerError)
		}
		return
	}

	moduleEntries := make([]apihttp.ModuleEntry, 0, len(moduleStatuses))
	for _, status := range moduleStatuses {
		moduleEntries = append(moduleEntries, bindingStatusToResponse(status))
	}

	missingRefs, err := s.missingReferenceAdapters(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("reference adapter check failed: %v", err), http.StatusInternalServerError)
		return
	}

	resp := quickstartStatusResponse{
		Completed:                completed,
		PendingSlots:             pending,
		Modules:                  moduleEntries,
		MissingReferenceAdapters: missingRefs,
	}

	if completedAt != nil {
		resp.CompletedAt = completedAt.UTC().Format(time.RFC3339)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *APIServer) handleQuickstartPost(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.configStore == nil {
		http.Error(w, "configuration store not available", http.StatusServiceUnavailable)
		return
	}

	var payload quickstartRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	for _, binding := range payload.Bindings {
		slot := strings.TrimSpace(binding.Slot)
		if slot == "" {
			http.Error(w, "binding slot is required", http.StatusBadRequest)
			return
		}

		adapterID := strings.TrimSpace(binding.AdapterID)
		var err error
		if adapterID == "" {
			err = s.configStore.ClearAdapterBinding(ctx, slot)
		} else {
			err = s.configStore.SetActiveAdapter(ctx, slot, adapterID, nil)
		}

		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	pending, err := s.configStore.PendingQuickstartSlots(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	missingRefs, err := s.missingReferenceAdapters(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("reference adapter check failed: %v", err), http.StatusInternalServerError)
		return
	}

	if payload.Complete != nil {
		if *payload.Complete {
			if len(pending) > 0 {
				http.Error(w, "quickstart cannot be completed while required slots remain unassigned", http.StatusBadRequest)
				return
			}
			if len(missingRefs) > 0 {
				http.Error(w, fmt.Sprintf("reference adapters missing: %s", strings.Join(missingRefs, ", ")), http.StatusBadRequest)
				return
			}
		}

		if err := s.configStore.MarkQuickstartCompleted(ctx, *payload.Complete); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	completed, completedAt, err := s.configStore.QuickstartStatus(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	moduleStatuses, err := s.quickstartModuleStatuses(ctx)
	if err != nil {
		if errors.Is(err, errModulesServiceUnavailable) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		} else {
			http.Error(w, fmt.Sprintf("modules overview failed: %v", err), http.StatusInternalServerError)
		}
		return
	}

	moduleEntries := make([]apihttp.ModuleEntry, 0, len(moduleStatuses))
	for _, status := range moduleStatuses {
		moduleEntries = append(moduleEntries, bindingStatusToResponse(status))
	}

	resp := quickstartStatusResponse{
		Completed:                completed,
		PendingSlots:             pending,
		Modules:                  moduleEntries,
		MissingReferenceAdapters: missingRefs,
	}

	if completedAt != nil {
		resp.CompletedAt = completedAt.UTC().Format(time.RFC3339)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

var errModulesServiceUnavailable = errors.New("modules service unavailable")

func (s *APIServer) quickstartModuleStatuses(ctx context.Context) ([]modules.BindingStatus, error) {
	if s.modules == nil {
		return nil, errModulesServiceUnavailable
	}
	statuses, err := s.modules.Overview(ctx)
	if err != nil {
		return nil, err
	}
	return statuses, nil
}

func (s *APIServer) missingReferenceAdapters(ctx context.Context) ([]string, error) {
	if s.configStore == nil {
		return nil, nil
	}
	missing := make([]string, 0, len(modules.RequiredReferenceAdapters))
	for _, id := range modules.RequiredReferenceAdapters {
		exists, err := s.configStore.AdapterExists(ctx, id)
		if err != nil {
			return nil, err
		}
		if !exists {
			missing = append(missing, id)
		}
	}
	return missing, nil
}

// handleRecordingsList returns list of all recordings
func (s *APIServer) handleRecordingsList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET,OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get recording store from session manager
	store := s.sessionManager.GetRecordingStore()
	if store == nil {
		http.Error(w, "recording store not available", http.StatusInternalServerError)
		return
	}

	metadata, err := store.LoadAll()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load recordings: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metadata)
}

// handleRecordingFile serves recording .cast files
func (s *APIServer) handleRecordingFile(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET,OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract session ID from path: /recordings/{sessionID}
	sessionID := strings.TrimPrefix(r.URL.Path, "/recordings/")
	if sessionID == "" {
		http.Error(w, "session ID required", http.StatusBadRequest)
		return
	}

	// Get recording store
	store := s.sessionManager.GetRecordingStore()
	if store == nil {
		http.Error(w, "recording store not available", http.StatusInternalServerError)
		return
	}

	// Get metadata for session
	metadata, err := store.GetBySessionID(sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("recording not found: %v", err), http.StatusNotFound)
		return
	}

	// Serve the .cast file
	w.Header().Set("Content-Type", "application/x-asciicast")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%s", metadata.Filename))
	http.ServeFile(w, r, metadata.RecordingPath)
}
