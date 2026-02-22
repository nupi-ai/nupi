package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	adapters "github.com/nupi-ai/nupi/internal/plugins/adapters"
	"github.com/nupi-ai/nupi/internal/protocol"
	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/session"
	"github.com/nupi-ai/nupi/internal/termresize"
	nupiversion "github.com/nupi-ai/nupi/internal/version"
	"github.com/nupi-ai/nupi/internal/voice/slots"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RuntimeInfoProvider defines methods required to expose runtime metadata.
type RuntimeInfoProvider interface {
	GRPCPort() int
	ConnectPort() int
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

const conversationMaxPageLimit = 500

const defaultTTSStreamID = slots.TTS
const voiceIssueCodeServiceUnavailable = "service_unavailable"

const (
	maxAdapterIDLength      = 128
	maxAdapterNameLength    = 256
	maxAdapterVersionLength = 64
	maxAdapterManifestBytes = 64 * 1024
)

var allowedAdapterTypes = map[string]struct{}{
	"stt":              {},
	"tts":              {},
	"ai":               {},
	"vad":              {},
	"tunnel":           {},
	"tool-handler":     {},
	"pipeline-cleaner": {},
}

var allowedAdapterTransports = map[string]struct{}{
	"grpc":    {},
	"http":    {},
	"process": {},
}

var allowedRoles = map[string]struct{}{
	string(roleAdmin):    {},
	string(roleReadOnly): {},
}

var errAdaptersServiceUnavailable = fmt.Errorf("adapter service unavailable")

// storedToken represents a persisted API authentication token.
type storedToken struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	Name      string    `json:"name,omitempty"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used_at,omitempty"`
}

// pairingEntry represents a temporary pairing code used for device registration.
type pairingEntry struct {
	Code      string    `json:"code"`
	Name      string    `json:"name,omitempty"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type authContextKey struct{}

// transportConfig groups network transport settings protected by a single
// read-write mutex.
type transportConfig struct {
	transportMu sync.RWMutex
	binding     string
	tlsCertPath string
	tlsKeyPath  string
	grpcBinding string
	grpcPort    int
}

// authState groups authentication tokens and settings protected by a single
// read-write mutex.
type authState struct {
	authMu       sync.RWMutex
	authTokens   map[string]storedToken
	authRequired bool
	// tokenOpsMu serialises token & pairing mutate operations (load→modify→store)
	// to prevent TOCTOU races on the config store.
	tokenOpsMu sync.Mutex
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

// lifecycleState groups synchronisation primitives and shutdown coordination.
type lifecycleState struct {
	hookMu         sync.RWMutex
	transportHooks []func()
	shutdownMu     sync.RWMutex
	shutdownFn     func(context.Context) error
}

// observabilityState groups optional diagnostic providers.
type observabilityState struct {
	metricsExporter PrometheusExporter
	pluginWarnings  PluginWarningsProvider
	pluginReloader  PluginReloader
}

// voiceDiagnostic describes a single voice-related issue.
type voiceDiagnostic struct {
	Slot    string `json:"slot"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// daemonStatusSnapshot captures runtime daemon status.
type daemonStatusSnapshot struct {
	Version       string
	SessionsCount int
	GRPCPort      int
	ConnectPort   int
	Binding       string
	GRPCBinding   string
	AuthRequired  bool
	TLSEnabled    bool
	UptimeSeconds float64
}

// TransportSnapshot captures the runtime server transport settings.
type TransportSnapshot struct {
	Binding        string
	TLSCertPath    string
	TLSKeyPath     string
	GRPCBinding    string
	GRPCPort       int
	TLSCertModTime time.Time
	TLSKeyModTime  time.Time
}

// APIServer provides the shared business logic and state for the Nupi daemon API.
// It is used by gRPC services and the transport gateway.
type APIServer struct {
	// Service dependencies (immutable after init)
	sessionManager SessionManager
	configStore    ConfigStore
	runtime        RuntimeInfoProvider
	conversation   ConversationStore
	adapters       AdaptersController
	audioIngress   AudioCaptureProvider
	audioEgress    AudioPlaybackController
	eventBus       *eventbus.Bus
	resizeManager  *termresize.Manager

	// Grouped state (embedded — fields promoted)
	transportConfig
	authState

	// Lifecycle & shutdown coordination
	lifecycle lifecycleState

	// Observability (optional providers, immutable after Start)
	observability observabilityState
}

// NewAPIServer creates a new API server.
func NewAPIServer(sessionManager SessionManager, configStore ConfigStore, runtime RuntimeInfoProvider) (*APIServer, error) {
	if sessionManager == nil {
		return nil, fmt.Errorf("session manager is required")
	}

	resizeManager, err := termresize.NewManagerWithDefaults()
	if err != nil {
		return nil, fmt.Errorf("failed to initialise resize manager: %w", err)
	}

	apiServer := &APIServer{
		sessionManager: sessionManager,
		configStore:    configStore,
		runtime:        runtime,
		resizeManager:  resizeManager,
	}

	return apiServer, nil
}

// SetShutdownFunc registers a handler invoked when a shutdown is requested.
func (s *APIServer) SetShutdownFunc(fn func(context.Context) error) {
	s.lifecycle.shutdownMu.Lock()
	s.lifecycle.shutdownFn = fn
	s.lifecycle.shutdownMu.Unlock()
}

// RequestShutdown triggers a graceful daemon shutdown using the registered
// shutdown function.
func (s *APIServer) RequestShutdown() {
	s.lifecycle.shutdownMu.RLock()
	fn := s.lifecycle.shutdownFn
	s.lifecycle.shutdownMu.RUnlock()
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

// SetConversationStore wires the conversation state provider.
func (s *APIServer) SetConversationStore(store ConversationStore) {
	s.conversation = store
}

// SetAdaptersController wires the adapter controller.
func (s *APIServer) SetAdaptersController(controller AdaptersController) {
	s.adapters = controller
}

// SetAudioIngress wires the audio ingress handler.
func (s *APIServer) SetAudioIngress(provider AudioCaptureProvider) {
	s.audioIngress = provider
}

// SetAudioEgress wires the audio egress handler.
func (s *APIServer) SetAudioEgress(controller AudioPlaybackController) {
	s.audioEgress = controller
}

// SetEventBus wires the event bus.
func (s *APIServer) SetEventBus(bus *eventbus.Bus) {
	s.eventBus = bus
}

// SetMetricsExporter wires the metrics exporter. Must be called before Start.
func (s *APIServer) SetMetricsExporter(exporter PrometheusExporter) {
	s.observability.metricsExporter = exporter
}

// SetPluginWarningsProvider wires the plugin warnings provider. Must be called before Start.
func (s *APIServer) SetPluginWarningsProvider(provider PluginWarningsProvider) {
	s.observability.pluginWarnings = provider
}

// SetPluginReloader wires the plugin reloader. Must be called before Start.
func (s *APIServer) SetPluginReloader(reloader PluginReloader) {
	s.observability.pluginReloader = reloader
}

// ResizeManager exposes the resize manager for components outside the server package.
func (s *APIServer) ResizeManager() *termresize.Manager {
	return s.resizeManager
}

// SessionMgr exposes the session manager for components outside the server package.
func (s *APIServer) SessionMgr() SessionManager {
	return s.sessionManager
}

// EventBus exposes the event bus for components outside the server package.
func (s *APIServer) EventBus() *eventbus.Bus {
	return s.eventBus
}

// AudioIngress exposes the audio ingress provider for components outside the server package.
func (s *APIServer) AudioIngress() AudioCaptureProvider {
	return s.audioIngress
}

// AudioEgress exposes the audio egress controller for components outside the server package.
func (s *APIServer) AudioEgress() AudioPlaybackController {
	return s.audioEgress
}

// PublishAudioInterrupt publishes an audio interrupt event on the event bus.
func (s *APIServer) PublishAudioInterrupt(sessionID, streamID, reason string, metadata map[string]string) {
	s.publishAudioInterrupt(sessionID, streamID, reason, metadata)
}

// ---------------------------------------------------------------------------
// Auth: token management
// ---------------------------------------------------------------------------

// AuthRequired reports whether transport-level authentication is enforced.
func (s *APIServer) AuthRequired() bool {
	return s.isAuthRequired()
}

// ValidateAuthToken verifies the supplied API token against the active allowlist.
func (s *APIServer) ValidateAuthToken(token string) bool {
	_, ok := s.lookupToken(token)
	return ok
}

// AuthenticateToken returns token metadata if present in the allowlist.
func (s *APIServer) AuthenticateToken(token string) (storedToken, bool) {
	return s.lookupToken(token)
}

// ContextWithToken attaches token metadata to the provided context.
func (s *APIServer) ContextWithToken(ctx context.Context, token storedToken) context.Context {
	return context.WithValue(ctx, authContextKey{}, token)
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

// authTokensKey is the legacy storage key for API tokens. The name predates
// the gRPC-only migration but is retained for backward compatibility with
// existing config databases.
const authTokensKey = "auth.http_tokens"

func (s *APIServer) loadAuthTokens(ctx context.Context) ([]storedToken, error) {
	if s.configStore == nil {
		return nil, nil
	}

	values, err := s.configStore.LoadSecuritySettings(ctx, authTokensKey)
	if err != nil {
		return nil, err
	}

	raw, ok := values[authTokensKey]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	var structured []storedToken
	if err := json.Unmarshal([]byte(raw), &structured); err == nil {
		return sanitizeStoredTokens(structured), nil
	}

	var legacy []string
	legacyErr := json.Unmarshal([]byte(raw), &legacy)
	if legacyErr == nil {
		legacy = sanitizeTokens(legacy)
		tokens := make([]storedToken, 0, len(legacy))
		for _, token := range legacy {
			tokens = append(tokens, newStoredToken(token, "", string(roleAdmin)))
		}
		return tokens, nil
	}

	return nil, fmt.Errorf("parse auth.http_tokens: %w", legacyErr)
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
		authTokensKey: string(payload),
	})
}

func (s *APIServer) ensureAuthTokens(ctx context.Context, required bool) ([]storedToken, string, error) {
	if !required {
		s.setAuthTokens(nil, false)
		return nil, "", nil
	}

	s.tokenOpsMu.Lock()
	defer s.tokenOpsMu.Unlock()

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

// ---------------------------------------------------------------------------
// Auth: token helpers (pure functions)
// ---------------------------------------------------------------------------

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

func generateAPIToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate api token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func maskToken(token string) string {
	if len(token) <= 8 {
		return token
	}
	return token[:4] + strings.Repeat("*", len(token)-8) + token[len(token)-4:]
}

// ---------------------------------------------------------------------------
// Auth: pairings
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Transport configuration
// ---------------------------------------------------------------------------

// Prepare resolves transport configuration from the store and returns the
// current transport settings. It validates bindings, TLS, and auth tokens.
func (s *APIServer) Prepare(ctx context.Context) (*PreparedTransport, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	cfg := configstore.TransportConfig{
		Binding:     "loopback",
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
	if _, err := resolveBindingHost(binding); err != nil {
		return nil, err
	}

	rawGRPC := strings.TrimSpace(cfg.GRPCBinding)
	if rawGRPC == "" {
		rawGRPC = cfg.Binding
	}
	grpcBinding := normalizeBinding(rawGRPC)
	if _, err := resolveBindingHost(grpcBinding); err != nil {
		return nil, err
	}

	certPath := strings.TrimSpace(cfg.TLSCertPath)
	keyPath := strings.TrimSpace(cfg.TLSKeyPath)

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

	s.transportMu.Lock()
	s.binding = binding
	s.grpcBinding = grpcBinding
	s.grpcPort = cfg.GRPCPort
	s.tlsCertPath = certPath
	s.tlsKeyPath = keyPath
	s.transportMu.Unlock()

	if _, _, err := s.ensureAuthTokens(ctx, requireTLS); err != nil {
		return nil, err
	}

	prepared := &PreparedTransport{
		Binding:     binding,
		GRPCBinding: grpcBinding,
		GRPCPort:    cfg.GRPCPort,
	}
	if certPath != "" && keyPath != "" {
		prepared.UseTLS = true
		prepared.CertPath = certPath
		prepared.KeyPath = keyPath
	}

	return prepared, nil
}

// PreparedTransport holds transport parameters resolved by Prepare().
type PreparedTransport struct {
	Binding     string
	GRPCBinding string
	GRPCPort    int
	UseTLS      bool
	CertPath    string
	KeyPath     string
}

// CurrentTransportSnapshot returns the currently applied transport configuration.
func (s *APIServer) CurrentTransportSnapshot() TransportSnapshot {
	s.transportMu.RLock()
	snap := TransportSnapshot{
		Binding:     s.binding,
		TLSCertPath: s.tlsCertPath,
		TLSKeyPath:  s.tlsKeyPath,
		GRPCBinding: s.grpcBinding,
		GRPCPort:    s.grpcPort,
	}
	s.transportMu.RUnlock()

	snap.TLSCertModTime = modTimeOrZero(snap.TLSCertPath)
	snap.TLSKeyModTime = modTimeOrZero(snap.TLSKeyPath)
	return snap
}

// applyTransportConfig updates auth tokens and in-memory transport state after
// a configuration has been persisted to the store.
func (s *APIServer) applyTransportConfig(ctx context.Context, cfg configstore.TransportConfig) (string, error) {
	rawGRPC := strings.TrimSpace(cfg.GRPCBinding)
	if rawGRPC == "" {
		rawGRPC = cfg.Binding
	}
	binding := normalizeBinding(cfg.Binding)
	grpcBinding := normalizeBinding(rawGRPC)
	authRequired := binding != "loopback" || grpcBinding != "loopback"

	_, newToken, err := s.ensureAuthTokens(ctx, authRequired)
	if err != nil {
		return "", err
	}

	s.transportMu.Lock()
	s.binding = binding
	s.tlsCertPath = strings.TrimSpace(cfg.TLSCertPath)
	s.tlsKeyPath = strings.TrimSpace(cfg.TLSKeyPath)
	s.grpcBinding = grpcBinding
	s.grpcPort = cfg.GRPCPort
	s.transportMu.Unlock()

	s.notifyTransportChanged()
	return newToken, nil
}

// EqualConfig compares the snapshot with a transport configuration fetched from the store.
func (snap TransportSnapshot) EqualConfig(cfg configstore.TransportConfig) bool {
	if snap.Binding != normalizeBinding(cfg.Binding) {
		return false
	}
	if strings.TrimSpace(snap.TLSCertPath) != strings.TrimSpace(cfg.TLSCertPath) {
		return false
	}
	if strings.TrimSpace(snap.TLSKeyPath) != strings.TrimSpace(cfg.TLSKeyPath) {
		return false
	}
	rawGRPC := strings.TrimSpace(cfg.GRPCBinding)
	if rawGRPC == "" {
		rawGRPC = cfg.Binding
	}
	if snap.GRPCBinding != normalizeBinding(rawGRPC) {
		return false
	}
	if snap.GRPCPort != cfg.GRPCPort {
		return false
	}
	return true
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

	s.lifecycle.hookMu.Lock()
	s.lifecycle.transportHooks = append(s.lifecycle.transportHooks, fn)
	s.lifecycle.hookMu.Unlock()
}

func (s *APIServer) notifyTransportChanged() {
	s.lifecycle.hookMu.RLock()
	hooks := make([]func(), len(s.lifecycle.transportHooks))
	copy(hooks, s.lifecycle.transportHooks)
	s.lifecycle.hookMu.RUnlock()

	for _, hook := range hooks {
		go hook()
	}
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

func validateTransportConfig(cfg configstore.TransportConfig) error {
	binding := normalizeBinding(cfg.Binding)
	if _, err := resolveBindingHost(binding); err != nil {
		return err
	}

	rawGRPC := strings.TrimSpace(cfg.GRPCBinding)
	if rawGRPC == "" {
		rawGRPC = cfg.Binding
	}
	grpcBinding := normalizeBinding(rawGRPC)
	if _, err := resolveBindingHost(grpcBinding); err != nil {
		return err
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

// ---------------------------------------------------------------------------
// Session creation
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Daemon status
// ---------------------------------------------------------------------------

func (s *APIServer) daemonStatus(ctx context.Context) (daemonStatusSnapshot, error) {
	var sessionsCount int
	if s.sessionManager != nil {
		sessionsCount = len(s.sessionManager.ListSessions())
	}

	snapshot := daemonStatusSnapshot{
		Version:       nupiversion.String(),
		SessionsCount: sessionsCount,
		AuthRequired:  s.isAuthRequired(),
	}

	if s.runtime != nil {
		snapshot.GRPCPort = s.runtime.GRPCPort()
		snapshot.ConnectPort = s.runtime.ConnectPort()
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
		rawGRPC := strings.TrimSpace(cfg.GRPCBinding)
		if rawGRPC == "" {
			rawGRPC = cfg.Binding
		}
		snapshot.GRPCBinding = normalizeBinding(rawGRPC)
		cert := strings.TrimSpace(cfg.TLSCertPath)
		key := strings.TrimSpace(cfg.TLSKeyPath)
		if cert != "" && key != "" {
			snapshot.TLSEnabled = true
		}
		if snapshot.GRPCPort == 0 && cfg.GRPCPort > 0 {
			snapshot.GRPCPort = cfg.GRPCPort
		}
	}

	return snapshot, nil
}

// ---------------------------------------------------------------------------
// Quickstart / adapter helpers
// ---------------------------------------------------------------------------

func (s *APIServer) quickstartAdapterStatuses(ctx context.Context) ([]adapters.BindingStatus, error) {
	if s.adapters == nil {
		return nil, errAdaptersServiceUnavailable
	}
	statuses, err := s.adapters.Overview(ctx)
	if err != nil {
		return nil, err
	}
	return statuses, nil
}

func (s *APIServer) missingReferenceAdapters(ctx context.Context) ([]string, error) {
	if s.configStore == nil {
		return nil, nil
	}
	missing := make([]string, 0, len(adapters.RequiredReferenceAdapters))
	for _, id := range adapters.RequiredReferenceAdapters {
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

// ---------------------------------------------------------------------------
// Audio / voice helpers
// ---------------------------------------------------------------------------

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

	eventbus.Publish(context.Background(), s.eventBus, eventbus.Audio.Interrupt, eventbus.SourceClient, event)
}

func (s *APIServer) voiceReadiness(ctx context.Context) (configstore.VoiceReadiness, error) {
	if s.configStore == nil {
		readiness := configstore.VoiceReadiness{
			CaptureEnabled:  s.audioIngress != nil,
			PlaybackEnabled: s.audioEgress != nil,
			Issues:          nil,
		}
		if s.audioIngress == nil {
			readiness.Issues = appendVoiceIssue(readiness.Issues, slots.STT, voiceIssueCodeServiceUnavailable, "audio ingress service unavailable")
		}
		if s.audioEgress == nil {
			readiness.Issues = appendVoiceIssue(readiness.Issues, slots.TTS, voiceIssueCodeServiceUnavailable, "audio egress service unavailable")
		}
		return readiness, nil
	}
	readiness, err := s.configStore.VoiceReadiness(ctx)
	if err != nil {
		return configstore.VoiceReadiness{}, err
	}
	if s.audioIngress == nil {
		readiness.CaptureEnabled = false
		readiness.Issues = appendVoiceIssue(readiness.Issues, slots.STT, voiceIssueCodeServiceUnavailable, "audio ingress service unavailable")
	}
	if s.audioEgress == nil {
		readiness.PlaybackEnabled = false
		readiness.Issues = appendVoiceIssue(readiness.Issues, slots.TTS, voiceIssueCodeServiceUnavailable, "audio egress service unavailable")
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

// ---------------------------------------------------------------------------
// Utility
// ---------------------------------------------------------------------------

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	return maps.Clone(in)
}
