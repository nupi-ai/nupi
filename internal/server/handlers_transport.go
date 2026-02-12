package server

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
)

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
