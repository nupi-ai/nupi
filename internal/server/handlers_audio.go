package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/voice/slots"
)

const defaultTTSStreamID = slots.TTS
const voiceIssueCodeServiceUnavailable = "service_unavailable"

const (
	defaultIngressBuffer       = 32 * 1024
	maxAudioUploadDuration     = 5 * time.Minute
	maxWebSocketMessageBytes   = 4 * defaultIngressBuffer
	websocketWriteTimeout      = 10 * time.Second
	websocketHeartbeatInterval = 30 * time.Second
	websocketHeartbeatTimeout  = 60 * time.Second
)

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

type audioInterruptRequest struct {
	SessionID string            `json:"session_id"`
	StreamID  string            `json:"stream_id"`
	Reason    string            `json:"reason"`
	Metadata  map[string]string `json:"metadata"`
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
		diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.STT)
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
		diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.TTS)
		message := voiceIssueSummary(diags, "voice playback unavailable")
		writeVoiceError(w, http.StatusPreconditionFailed, message, diags, readiness)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	sub := eventbus.Subscribe[eventbus.AudioEgressPlaybackEvent](s.eventBus,
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
			evt := env.Payload
			if evt.SessionID != sessionID || evt.StreamID != streamID {
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
		diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.STT)
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
		diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.TTS)
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

	sub := eventbus.Subscribe[eventbus.AudioEgressPlaybackEvent](s.eventBus,
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
			evt := env.Payload
			if evt.SessionID != sessionID || evt.StreamID != streamID {
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
				diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.STT)
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
				diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.TTS)
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
