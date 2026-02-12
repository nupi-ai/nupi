package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	apihttp "github.com/nupi-ai/nupi/internal/api/http"
	adapters "github.com/nupi-ai/nupi/internal/plugins/adapters"
)

type quickstartStatusResponse struct {
	Completed                bool                   `json:"completed"`
	CompletedAt              string                 `json:"completed_at,omitempty"`
	PendingSlots             []string               `json:"pending_slots"`
	Adapters                 []apihttp.AdapterEntry `json:"adapters,omitempty"`
	MissingReferenceAdapters []string               `json:"missing_reference_adapters,omitempty"`
}

type quickstartBinding struct {
	Slot      string `json:"slot"`
	AdapterID string `json:"adapter_id"`
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

func (s *APIServer) handlePluginWarnings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.pluginWarnings == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"count":    0,
			"warnings": []any{},
		})
		return
	}

	warnings := s.pluginWarnings.GetDiscoveryWarnings()
	response := map[string]any{
		"count":    len(warnings),
		"warnings": warnings,
	}
	if warnings == nil {
		response["warnings"] = []any{}
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

	adapterStatuses, err := s.quickstartAdapterStatuses(r.Context())
	if err != nil {
		if errors.Is(err, errAdaptersServiceUnavailable) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		} else {
			http.Error(w, fmt.Sprintf("adapters overview failed: %v", err), http.StatusInternalServerError)
		}
		return
	}

	adapterEntries := make([]apihttp.AdapterEntry, 0, len(adapterStatuses))
	for _, status := range adapterStatuses {
		adapterEntries = append(adapterEntries, bindingStatusToResponse(status))
	}

	missingRefs, err := s.missingReferenceAdapters(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("reference adapter check failed: %v", err), http.StatusInternalServerError)
		return
	}

	resp := quickstartStatusResponse{
		Completed:                completed,
		PendingSlots:             pending,
		Adapters:                 adapterEntries,
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

	adapterStatuses, err := s.quickstartAdapterStatuses(ctx)
	if err != nil {
		if errors.Is(err, errAdaptersServiceUnavailable) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		} else {
			http.Error(w, fmt.Sprintf("adapters overview failed: %v", err), http.StatusInternalServerError)
		}
		return
	}

	adapterEntries := make([]apihttp.AdapterEntry, 0, len(adapterStatuses))
	for _, status := range adapterStatuses {
		adapterEntries = append(adapterEntries, bindingStatusToResponse(status))
	}

	resp := quickstartStatusResponse{
		Completed:                completed,
		PendingSlots:             pending,
		Adapters:                 adapterEntries,
		MissingReferenceAdapters: missingRefs,
	}

	if completedAt != nil {
		resp.CompletedAt = completedAt.UTC().Format(time.RFC3339)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

var errAdaptersServiceUnavailable = errors.New("adapter service unavailable")

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
