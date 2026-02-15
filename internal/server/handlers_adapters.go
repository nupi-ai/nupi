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
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *APIServer) handleConfigAdaptersGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
		return
	}
	if s.configStore == nil {
		writeError(w, http.StatusServiceUnavailable, "configuration store not available")
		return
	}

	adapters, err := s.configStore.ListAdapters(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
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
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *APIServer) handleConfigAdapterBindingsGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
		return
	}
	if s.configStore == nil {
		writeError(w, http.StatusServiceUnavailable, "configuration store not available")
		return
	}

	bindings, err := s.configStore.ListAdapterBindings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
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
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.configStore == nil {
		writeError(w, http.StatusServiceUnavailable, "configuration store not available")
		return
	}

	result, err := s.configStore.EnsureRequiredAdapterSlots(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("migration failed: %v", err))
		return
	}

	audioUpdated, err := s.configStore.EnsureAudioSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("audio settings migration failed: %v", err))
		return
	}

	response := configMigrationResponse{
		UpdatedSlots:         append([]string{}, result.UpdatedSlots...),
		PendingSlots:         append([]string{}, result.PendingSlots...),
		AudioSettingsUpdated: audioUpdated,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("[APIServer] failed to encode migration response: %v", err)
	}
}

func (s *APIServer) handleAdapters(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		s.handleAdaptersGet(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *APIServer) handleAdaptersLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin, roleReadOnly); !ok {
		return
	}
	if s.eventBus == nil {
		writeError(w, http.StatusServiceUnavailable, "event bus unavailable")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	slotFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("slot")))
	adapterFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("adapter")))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()

	subLogs := eventbus.Subscribe[eventbus.AdapterLogEvent](s.eventBus, eventbus.TopicAdaptersLog)
	defer subLogs.Close()
	subPartial := eventbus.Subscribe[eventbus.SpeechTranscriptEvent](s.eventBus, eventbus.TopicSpeechTranscriptPartial)
	defer subPartial.Close()
	subFinal := eventbus.Subscribe[eventbus.SpeechTranscriptEvent](s.eventBus, eventbus.TopicSpeechTranscriptFinal)
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
			entry, emit := filterAdapterLogEvent(env.Payload, env.Timestamp, slotFilter, adapterFilter)
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
			entry, emit := makeTranscriptEntry(env.Payload, env.Timestamp)
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
			entry, emit := makeTranscriptEntry(env.Payload, env.Timestamp)
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
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.configStore == nil {
		writeError(w, http.StatusServiceUnavailable, "configuration store unavailable")
		return
	}

	var payload apihttp.AdapterRegistrationRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON payload: %v", err))
		return
	}

	adapterID := strings.TrimSpace(payload.AdapterID)
	if adapterID == "" {
		writeError(w, http.StatusBadRequest, "adapter_id is required")
		return
	}
	if len(adapterID) > maxAdapterIDLength {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("adapter_id too long (max %d chars)", maxAdapterIDLength))
		return
	}

	adapterType := strings.TrimSpace(payload.Type)
	if adapterType != "" {
		if _, ok := allowedAdapterTypes[adapterType]; !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid adapter type: %s (expected: stt, tts, ai, vad, tunnel, tool-handler, pipeline-cleaner)", adapterType))
			return
		}
	}

	adapterName := strings.TrimSpace(payload.Name)
	if len(adapterName) > maxAdapterNameLength {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("name too long (max %d chars)", maxAdapterNameLength))
		return
	}

	adapterVersion := strings.TrimSpace(payload.Version)
	if len(adapterVersion) > maxAdapterVersionLength {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("version too long (max %d chars)", maxAdapterVersionLength))
		return
	}

	if len(payload.Manifest) > maxAdapterManifestBytes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("manifest too large (max %d bytes)", maxAdapterManifestBytes))
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
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("register adapter failed: %v", err))
		return
	}

	if payload.Endpoint != nil {
		transport := strings.TrimSpace(payload.Endpoint.Transport)
		address := strings.TrimSpace(payload.Endpoint.Address)
		command := strings.TrimSpace(payload.Endpoint.Command)

		if transport == "" {
			if address != "" || command != "" || len(payload.Endpoint.Args) > 0 || len(payload.Endpoint.Env) > 0 {
				writeError(w, http.StatusBadRequest, "endpoint transport required when endpoint fields are provided")
				return
			}
		} else {
			if _, ok := allowedAdapterTransports[transport]; !ok {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid transport: %s (expected: grpc, http, process)", transport))
				return
			}
			switch transport {
			case "grpc":
				if address == "" {
					writeError(w, http.StatusBadRequest, "endpoint address required for grpc transport")
					return
				}
			case "http":
				if address == "" {
					writeError(w, http.StatusBadRequest, "endpoint address required for http transport")
					return
				}
				if command != "" || len(payload.Endpoint.Args) > 0 {
					writeError(w, http.StatusBadRequest, "endpoint command/args not allowed for http transport")
					return
				}
			case "process":
				if command == "" {
					writeError(w, http.StatusBadRequest, "endpoint command required for process transport")
					return
				}
				if address != "" {
					writeError(w, http.StatusBadRequest, "endpoint address not used for process transport")
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
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("register adapter endpoint failed: %v", err))
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

func filterAdapterLogEvent(event eventbus.AdapterLogEvent, timestamp time.Time, slotFilter, adapterFilter string) (apihttp.AdapterLogStreamEntry, bool) {
	slotValue := strings.TrimSpace(event.Fields["slot"])
	if slotFilter != "" && strings.ToLower(slotValue) != slotFilter {
		return apihttp.AdapterLogStreamEntry{}, false
	}
	if adapterFilter != "" && strings.ToLower(strings.TrimSpace(event.AdapterID)) != adapterFilter {
		return apihttp.AdapterLogStreamEntry{}, false
	}

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

func makeTranscriptEntry(event eventbus.SpeechTranscriptEvent, timestamp time.Time) (apihttp.AdapterLogStreamEntry, bool) {
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
		writeError(w, http.StatusServiceUnavailable, "adapter service unavailable")
		return
	}

	overview, err := s.adapters.Overview(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
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
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.configStore == nil {
		writeError(w, http.StatusServiceUnavailable, "configuration store not available")
		return
	}
	if s.adapters == nil {
		writeError(w, http.StatusServiceUnavailable, "adapter service unavailable")
		return
	}

	var payload adapterActionRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON payload: %v", err))
		return
	}

	slot := strings.TrimSpace(payload.Slot)
	adapterID := strings.TrimSpace(payload.AdapterID)
	if slot == "" || adapterID == "" {
		writeError(w, http.StatusBadRequest, "slot and adapter_id are required")
		return
	}

	var cfg map[string]any
	if len(payload.Config) > 0 {
		if err := json.Unmarshal(payload.Config, &cfg); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid config payload: %v", err))
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
		writeError(w, code, fmt.Sprintf("set adapter binding failed: %v", err))
		return
	}

	status, err := s.adapters.StartSlot(r.Context(), adapters.Slot(slot))
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("start adapter failed: %v", err))
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
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.adapters == nil {
		writeError(w, http.StatusServiceUnavailable, "adapter service unavailable")
		return
	}

	var payload adapterActionRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON payload: %v", err))
		return
	}
	if strings.TrimSpace(payload.Slot) == "" {
		writeError(w, http.StatusBadRequest, "slot is required")
		return
	}

	status, err := s.adapters.StartSlot(r.Context(), adapters.Slot(payload.Slot))
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("start adapter failed: %v", err))
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
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}
	if s.adapters == nil {
		writeError(w, http.StatusServiceUnavailable, "adapter service unavailable")
		return
	}

	var payload adapterActionRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON payload: %v", err))
		return
	}
	if strings.TrimSpace(payload.Slot) == "" {
		writeError(w, http.StatusBadRequest, "slot is required")
		return
	}

	status, err := s.adapters.StopSlot(r.Context(), adapters.Slot(payload.Slot))
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("stop adapter failed: %v", err))
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
