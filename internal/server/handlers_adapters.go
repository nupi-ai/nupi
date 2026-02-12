package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	apihttp "github.com/nupi-ai/nupi/internal/api/http"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	adapters "github.com/nupi-ai/nupi/internal/plugins/adapters"
)

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

type adapterActionRequest struct {
	Slot      string          `json:"slot"`
	AdapterID string          `json:"adapter_id,omitempty"`
	Config    json.RawMessage `json:"config,omitempty"`
}

type configMigrationResponse struct {
	UpdatedSlots         []string `json:"updated_slots"`
	PendingSlots         []string `json:"pending_slots"`
	AudioSettingsUpdated bool     `json:"audio_settings_updated"`
}

func (s *APIServer) handleConfigAdapters(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		s.handleConfigAdaptersGet(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleConfigAdaptersGet(w http.ResponseWriter, r *http.Request) {
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

func (s *APIServer) handleConfigAdapterBindings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		s.handleConfigAdapterBindingsGet(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleConfigAdapterBindingsGet(w http.ResponseWriter, r *http.Request) {
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

func (s *APIServer) handleAdapters(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		s.handleAdaptersGet(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleAdaptersLogs(w http.ResponseWriter, r *http.Request) {
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
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()

	subLogs := s.eventBus.Subscribe(eventbus.TopicAdaptersLog)
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
			entry, emit := filterAdapterLogEvent(env, slotFilter, adapterFilter)
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

func (s *APIServer) handleAdaptersRegister(w http.ResponseWriter, r *http.Request) {
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

	var payload apihttp.AdapterRegistrationRequest
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
			http.Error(w, fmt.Sprintf("invalid adapter type: %s (expected: stt, tts, ai, vad, tunnel, tool-handler, pipeline-cleaner)", adapterType), http.StatusBadRequest)
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
			if _, ok := allowedAdapterTransports[transport]; !ok {
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

			endpoint := configstore.AdapterEndpoint{
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
			if err := s.configStore.UpsertAdapterEndpoint(r.Context(), endpoint); err != nil {
				http.Error(w, fmt.Sprintf("register adapter endpoint failed: %v", err), http.StatusInternalServerError)
				return
			}
		}
	}

	result := apihttp.AdapterRegistrationResult{
		Adapter: apihttp.AdapterDescriptor{
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
		log.Printf("[APIServer] failed to encode adapter registration result: %v", err)
	}
}

func filterAdapterLogEvent(env eventbus.Envelope, slotFilter, adapterFilter string) (apihttp.AdapterLogStreamEntry, bool) {
	event, ok := env.Payload.(eventbus.AdapterLogEvent)
	if !ok {
		return apihttp.AdapterLogStreamEntry{}, false
	}

	slotValue := strings.TrimSpace(event.Fields["slot"])
	if slotFilter != "" && strings.ToLower(slotValue) != slotFilter {
		return apihttp.AdapterLogStreamEntry{}, false
	}
	if adapterFilter != "" && strings.ToLower(strings.TrimSpace(event.AdapterID)) != adapterFilter {
		return apihttp.AdapterLogStreamEntry{}, false
	}

	timestamp := env.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}

	return apihttp.AdapterLogStreamEntry{
		Type:      "log",
		Timestamp: timestamp,
		AdapterID: event.AdapterID,
		Slot:      slotValue,
		Level:     string(event.Level),
		Message:   event.Message,
	}, true
}

func makeTranscriptEntry(env eventbus.Envelope) (apihttp.AdapterLogStreamEntry, bool) {
	event, ok := env.Payload.(eventbus.SpeechTranscriptEvent)
	if !ok {
		return apihttp.AdapterLogStreamEntry{}, false
	}

	timestamp := env.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}

	return apihttp.AdapterLogStreamEntry{
		Type:       "transcript",
		Timestamp:  timestamp,
		SessionID:  event.SessionID,
		StreamID:   event.StreamID,
		Text:       event.Text,
		Confidence: float64(event.Confidence),
		Final:      event.Final,
	}, true
}

func (s *APIServer) handleAdaptersGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
		return
	}
	if s.adapters == nil {
		http.Error(w, "adapter service unavailable", http.StatusServiceUnavailable)
		return
	}

	overview, err := s.adapters.Overview(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	adaptersOut := make([]apihttp.AdapterEntry, 0, len(overview))
	for _, status := range overview {
		adaptersOut = append(adaptersOut, bindingStatusToResponse(status))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(apihttp.AdaptersOverview{Adapters: adaptersOut})
}

func (s *APIServer) handleAdaptersBind(w http.ResponseWriter, r *http.Request) {
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
	if s.adapters == nil {
		http.Error(w, "adapter service unavailable", http.StatusServiceUnavailable)
		return
	}

	var payload adapterActionRequest
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

	status, err := s.adapters.StartSlot(r.Context(), adapters.Slot(slot))
	if err != nil {
		http.Error(w, fmt.Sprintf("start adapter failed: %v", err), http.StatusInternalServerError)
		return
	}

	response := apihttp.AdapterActionResult{Adapter: bindingStatusToResponse(*status)}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *APIServer) handleAdaptersStart(w http.ResponseWriter, r *http.Request) {
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
	if s.adapters == nil {
		http.Error(w, "adapter service unavailable", http.StatusServiceUnavailable)
		return
	}

	var payload adapterActionRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(payload.Slot) == "" {
		http.Error(w, "slot is required", http.StatusBadRequest)
		return
	}

	status, err := s.adapters.StartSlot(r.Context(), adapters.Slot(payload.Slot))
	if err != nil {
		http.Error(w, fmt.Sprintf("start adapter failed: %v", err), http.StatusInternalServerError)
		return
	}

	response := apihttp.AdapterActionResult{Adapter: bindingStatusToResponse(*status)}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *APIServer) handleAdaptersStop(w http.ResponseWriter, r *http.Request) {
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
	if s.adapters == nil {
		http.Error(w, "adapter service unavailable", http.StatusServiceUnavailable)
		return
	}

	var payload adapterActionRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(payload.Slot) == "" {
		http.Error(w, "slot is required", http.StatusBadRequest)
		return
	}

	status, err := s.adapters.StopSlot(r.Context(), adapters.Slot(payload.Slot))
	if err != nil {
		http.Error(w, fmt.Sprintf("stop adapter failed: %v", err), http.StatusInternalServerError)
		return
	}

	response := apihttp.AdapterActionResult{Adapter: bindingStatusToResponse(*status)}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func bindingStatusToResponse(status adapters.BindingStatus) apihttp.AdapterEntry {
	resp := apihttp.AdapterEntry{
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
		runtime := apihttp.AdapterRuntime{
			AdapterID: status.Runtime.AdapterID,
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
