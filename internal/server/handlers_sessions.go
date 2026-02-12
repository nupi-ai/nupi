package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/nupi-ai/nupi/internal/api"
	"github.com/nupi-ai/nupi/internal/protocol"
	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/session"
	"github.com/nupi-ai/nupi/internal/termresize"
)

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

	offset, _, err := parseQueryIntParam(query, "offset")
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid offset: %v", err), http.StatusBadRequest)
		return
	}

	limit, providedLimit, err := parseQueryIntParam(query, "limit")
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

func (s *APIServer) handleGlobalConversation(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
	default:
		w.Header().Set("Allow", "GET,OPTIONS")
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

	query := r.URL.Query()

	offset, _, err := parseQueryIntParam(query, "offset")
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid offset: %v", err), http.StatusBadRequest)
		return
	}

	limit, providedLimit, err := parseQueryIntParam(query, "limit")
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid limit: %v", err), http.StatusBadRequest)
		return
	}
	if providedLimit && limit > conversationMaxPageLimit {
		limit = conversationMaxPageLimit
	}

	total, turns := s.conversation.GlobalSlice(offset, limit)
	state := api.ToConversationState("", total, offset, limit, turns)

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
