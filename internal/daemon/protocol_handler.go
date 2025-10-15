package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nupi-ai/nupi/internal/protocol"
	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/server"
	"github.com/nupi-ai/nupi/internal/session"
	"github.com/nupi-ai/nupi/internal/termresize"
)

// ProtocolHandler handles Unix socket protocol messages
type ProtocolHandler struct {
	sessionManager  *session.Manager
	resizeManager   *termresize.Manager
	apiServer       *server.APIServer
	runtimeInfo     *RuntimeInfo
	conn            net.Conn
	encoder         *json.Encoder
	decoder         *json.Decoder
	encoderMu       sync.Mutex // Protects encoder for concurrent writes
	attachedSession string
	attachedSink    pty.OutputSink // Track the sink so we can remove it
	clientID        string
}

// NewProtocolHandler creates a new protocol handler
func NewProtocolHandler(sm *session.Manager, api *server.APIServer, info *RuntimeInfo, conn net.Conn) *ProtocolHandler {
	var rm *termresize.Manager
	if api != nil {
		rm = api.ResizeManager()
	}

	return &ProtocolHandler{
		sessionManager: sm,
		resizeManager:  rm,
		apiServer:      api,
		runtimeInfo:    info,
		conn:           conn,
		encoder:        json.NewEncoder(conn),
		decoder:        json.NewDecoder(conn),
		clientID:       uuid.NewString(),
	}
}

// Handle processes incoming messages
func (h *ProtocolHandler) Handle() {
	defer func() {
		// Clean up any attached session on disconnect
		if h.attachedSession != "" && h.attachedSink != nil {
			h.sessionManager.DetachFromSession(h.attachedSession, h.attachedSink)
			if h.resizeManager != nil {
				h.resizeManager.UnregisterClient(h.attachedSession, h.clientID)
			}
		}
		h.conn.Close()
	}()

	for {
		var req protocol.Request
		if err := h.decoder.Decode(&req); err != nil {
			if err == io.EOF {
				break
			}
			h.sendError(req.ID, fmt.Sprintf("failed to decode request: %v", err))
			continue
		}

		h.handleRequest(&req)
	}
}

func (h *ProtocolHandler) handleRequest(req *protocol.Request) {
	switch req.Type {
	case "create_session":
		h.handleCreateSession(req)
	case "list_sessions":
		h.handleListSessions(req)
	case "attach_session":
		h.handleAttachSession(req)
	case "detach_session":
		h.handleDetachSession(req)
	case "kill_session":
		h.handleKillSession(req)
	case "send_input":
		h.handleSendInput(req)
	case "daemon_status":
		h.handleDaemonStatus(req)
	case protocol.RequestResizeSession:
		h.handleResizeSession(req)
	case protocol.RequestShutdown:
		h.handleShutdown(req)
	default:
		h.sendError(req.ID, fmt.Sprintf("unknown request type: %s", req.Type))
	}
}

func (h *ProtocolHandler) handleCreateSession(req *protocol.Request) {
	data, ok := req.Data.(map[string]interface{})
	if !ok {
		h.sendError(req.ID, "invalid data format")
		return
	}

	// Parse options
	opts := pty.StartOptions{
		Rows: 24,
		Cols: 80,
		Env:  []string{},
	}

	if cmd, ok := data["command"].(string); ok {
		opts.Command = cmd
	} else {
		h.sendError(req.ID, "command is required")
		return
	}

	if args, ok := data["args"].([]interface{}); ok {
		for _, arg := range args {
			if str, ok := arg.(string); ok {
				opts.Args = append(opts.Args, str)
			}
		}
	}

	if workdir, ok := data["workdir"].(string); ok {
		opts.WorkingDir = workdir
	}

	if rows, ok := data["rows"].(float64); ok {
		opts.Rows = uint16(rows)
	}

	if cols, ok := data["cols"].(float64); ok {
		opts.Cols = uint16(cols)
	}

	if rawEnv, ok := data["env"]; ok {
		envList, err := parseEnvList(rawEnv)
		if err != nil {
			h.sendError(req.ID, fmt.Sprintf("invalid environment variables: %v", err))
			return
		}
		opts.Env = envList
	}

	// Check if inspection mode is enabled
	inspect := false
	if i, ok := data["inspect"].(bool); ok {
		inspect = i
	}

	// Create session
	sess, err := h.sessionManager.CreateSession(opts, inspect)
	if err != nil {
		h.sendError(req.ID, fmt.Sprintf("failed to create session: %v", err))
		return
	}

	detached := false
	if d, ok := data["detached"].(bool); ok {
		detached = d
	}

	if detached {
		sess.SetStatus(session.StatusDetached)
	} else {
		sess.SetStatus(session.StatusRunning)
	}

	// Send response
	resp := protocol.Response{
		ID:      req.ID,
		Success: true,
		Data: map[string]interface{}{
			"session_id": sess.ID,
			"pid":        sess.PTY.GetPID(),
			"created_at": sess.StartTime,
		},
	}

	h.encoderMu.Lock()
	h.encoder.Encode(resp)
	h.encoderMu.Unlock()
}

func parseEnvList(raw interface{}) ([]string, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case []string:
		return validateEnvEntries(v)
	case []interface{}:
		env := make([]string, 0, len(v))
		for _, entry := range v {
			str, ok := entry.(string)
			if !ok {
				return nil, fmt.Errorf("non-string value %v", entry)
			}
			env = append(env, str)
		}
		return validateEnvEntries(env)
	default:
		return nil, fmt.Errorf("unexpected type %T", raw)
	}
}

func validateEnvEntries(entries []string) ([]string, error) {
	validated := make([]string, 0, len(entries))
	for idx, entry := range entries {
		if entry == "" {
			return nil, fmt.Errorf("entry %d is empty", idx)
		}
		if strings.IndexByte(entry, 0) >= 0 {
			return nil, fmt.Errorf("entry %d contains null byte", idx)
		}
		if eq := strings.IndexByte(entry, '='); eq <= 0 {
			return nil, fmt.Errorf("entry %d must be in KEY=VALUE form", idx)
		}
		validated = append(validated, entry)
	}
	return validated, nil
}

func (h *ProtocolHandler) handleListSessions(req *protocol.Request) {
	sessions := h.sessionManager.ListSessions()

	sessionList := make([]map[string]interface{}, 0, len(sessions))
	for _, sess := range sessions {
		sessionList = append(sessionList, map[string]interface{}{
			"id":         sess.ID,
			"command":    sess.Command,
			"args":       sess.Args,
			"status":     sess.CurrentStatus(),
			"pid":        sess.PTY.GetPID(),
			"created_at": sess.StartTime,
		})
	}

	resp := protocol.Response{
		ID:      req.ID,
		Success: true,
		Data: map[string]interface{}{
			"sessions": sessionList,
		},
	}

	h.encoderMu.Lock()
	h.encoder.Encode(resp)
	h.encoderMu.Unlock()
}

func (h *ProtocolHandler) handleAttachSession(req *protocol.Request) {
	data, ok := req.Data.(map[string]interface{})
	if !ok {
		h.sendError(req.ID, "invalid data format")
		return
	}

	sessionID, ok := data["session_id"].(string)
	if !ok {
		h.sendError(req.ID, "session_id is required")
		return
	}

	includeHistory := true
	if ih, ok := data["include_history"].(bool); ok {
		includeHistory = ih
	}

	// Create stream sink
	sink := &StreamSink{
		handler:   h,
		sessionID: sessionID,
		utf8acc:   &utf8Accumulator{},
	}

	// Attach to session
	if err := h.sessionManager.AttachToSession(sessionID, sink, includeHistory); err != nil {
		h.sendError(req.ID, fmt.Sprintf("failed to attach: %v", err))
		return
	}

	// Register sink as event notifier
	session, err := h.sessionManager.GetSession(sessionID)
	if err == nil {
		session.AddNotifier(sink)
	}

	h.attachedSession = sessionID
	h.attachedSink = sink

	if h.resizeManager != nil {
		h.resizeManager.RegisterClient(sessionID, h.clientID, nil)
	}

	// Send success response
	resp := protocol.Response{
		ID:      req.ID,
		Success: true,
		Data: map[string]interface{}{
			"attached":  true,
			"stream_id": uuid.New().String(),
		},
	}

	h.encoderMu.Lock()
	h.encoder.Encode(resp)
	h.encoderMu.Unlock()
}

func (h *ProtocolHandler) handleDetachSession(req *protocol.Request) {
	if h.attachedSession == "" {
		h.sendError(req.ID, "not attached to any session")
		return
	}

	// Detach is handled by closing connection
	resp := protocol.Response{
		ID:      req.ID,
		Success: true,
		Data: map[string]interface{}{
			"detached": true,
		},
	}

	h.encoderMu.Lock()
	h.encoder.Encode(resp)
	h.encoderMu.Unlock()

	// Actually detach from the session
	if h.attachedSink != nil {
		h.sessionManager.DetachFromSession(h.attachedSession, h.attachedSink)
		h.attachedSink = nil
	}
	if h.resizeManager != nil {
		h.resizeManager.UnregisterClient(h.attachedSession, h.clientID)
	}
	h.attachedSession = ""
}

func (h *ProtocolHandler) handleKillSession(req *protocol.Request) {
	data, ok := req.Data.(map[string]interface{})
	if !ok {
		h.sendError(req.ID, "invalid data format")
		return
	}

	sessionID, ok := data["session_id"].(string)
	if !ok {
		h.sendError(req.ID, "session_id is required")
		return
	}

	if err := h.sessionManager.KillSession(sessionID); err != nil {
		h.sendError(req.ID, fmt.Sprintf("failed to kill session: %v", err))
		return
	}

	resp := protocol.Response{
		ID:      req.ID,
		Success: true,
		Data: map[string]interface{}{
			"killed": true,
		},
	}

	h.encoderMu.Lock()
	h.encoder.Encode(resp)
	h.encoderMu.Unlock()
}

func (h *ProtocolHandler) handleSendInput(req *protocol.Request) {
	data, ok := req.Data.(map[string]interface{})
	if !ok {
		h.sendError(req.ID, "invalid data format")
		return
	}

	sessionID, ok := data["session_id"].(string)
	if !ok {
		h.sendError(req.ID, "session_id is required")
		return
	}

	input, ok := data["input"].(string)
	if !ok {
		h.sendError(req.ID, "input is required")
		return
	}

	if err := h.sessionManager.WriteToSession(sessionID, []byte(input)); err != nil {
		h.sendError(req.ID, fmt.Sprintf("failed to send input: %v", err))
		return
	}

	resp := protocol.Response{
		ID:      req.ID,
		Success: true,
	}

	h.encoderMu.Lock()
	h.encoder.Encode(resp)
	h.encoderMu.Unlock()
}

func (h *ProtocolHandler) handleDaemonStatus(req *protocol.Request) {
	sessions := h.sessionManager.ListSessions()
	port := 0
	if h.runtimeInfo != nil {
		port = h.runtimeInfo.Port()
	}

	resp := protocol.Response{
		ID:      req.ID,
		Success: true,
		Data: map[string]interface{}{
			"version":        "0.2.0",
			"sessions_count": len(sessions),
			"port":           port,
		},
	}

	// Add uptime if start_time is set
	if h.runtimeInfo != nil {
		if startTime := h.runtimeInfo.StartTime(); !startTime.IsZero() {
			uptime := time.Since(startTime).Seconds()
			resp.Data.(map[string]interface{})["uptime"] = uptime
		}
	}

	h.encoderMu.Lock()
	h.encoder.Encode(resp)
	h.encoderMu.Unlock()
}

func (h *ProtocolHandler) handleShutdown(req *protocol.Request) {
	// Send success response first
	resp := protocol.Response{
		ID:      req.ID,
		Success: true,
		Data: map[string]interface{}{
			"message": "Daemon shutting down",
		},
	}

	h.encoderMu.Lock()
	h.encoder.Encode(resp)
	h.encoderMu.Unlock()

	// Shutdown daemon gracefully in a goroutine
	// to allow response to be sent
	go func() {
		time.Sleep(100 * time.Millisecond)
		// Send SIGTERM to self
		syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	}()
}

func (h *ProtocolHandler) handleResizeSession(req *protocol.Request) {
	if h.resizeManager == nil {
		h.sendError(req.ID, "resize manager unavailable")
		return
	}

	data, ok := req.Data.(map[string]interface{})
	if !ok {
		h.sendError(req.ID, "invalid data format")
		return
	}

	sessionID, ok := data["session_id"].(string)
	if !ok || sessionID == "" {
		h.sendError(req.ID, "session_id is required")
		return
	}

	cols, ok := toInt(data["cols"])
	if !ok {
		h.sendError(req.ID, "cols is required")
		return
	}

	rows, ok := toInt(data["rows"])
	if !ok {
		h.sendError(req.ID, "rows is required")
		return
	}

	if _, err := h.sessionManager.GetSession(sessionID); err != nil {
		h.sendError(req.ID, fmt.Sprintf("session %s not found", sessionID))
		return
	}

	metadata := convertMetadata(data["meta"])

	decision, err := h.resizeManager.HandleHostResize(sessionID, termresize.ViewportSize{Cols: cols, Rows: rows}, metadata)
	if err != nil {
		h.sendError(req.ID, fmt.Sprintf("resize handling failed: %v", err))
		return
	}

	h.applyResizeDecision(decision)

	resp := protocol.Response{
		ID:      req.ID,
		Success: true,
	}

	h.encoderMu.Lock()
	h.encoder.Encode(resp)
	h.encoderMu.Unlock()
}

func (h *ProtocolHandler) sendError(requestID string, message string) {
	resp := protocol.Response{
		ID:      requestID,
		Success: false,
		Error:   message,
	}

	h.encoderMu.Lock()
	h.encoder.Encode(resp)
	h.encoderMu.Unlock()
}

// monitorSessionEvents monitors PTY events and sends them to client
func (h *ProtocolHandler) monitorSessionEvents(sessionID string) {
	// Get session
	session, err := h.sessionManager.GetSession(sessionID)
	if err != nil {
		return
	}

	// Get PTY events channel
	events := session.PTY.Events()

	for event := range events {
		// Send event to client as StreamMessage
		msg := protocol.StreamMessage{
			Type:      "event",
			SessionID: sessionID,
			Data:      event.Type, // "process_exited", etc.
		}

		// Additional data for specific events
		if event.Type == "process_exited" {
			msg.Data = fmt.Sprintf("%s:%d", event.Type, event.ExitCode)
		}

		h.encoderMu.Lock()
		h.encoder.Encode(msg)
		h.encoderMu.Unlock()

		// If process exited, stop monitoring
		if event.Type == "process_exited" {
			return
		}
	}
}

func (h *ProtocolHandler) applyResizeDecision(decision termresize.ResizeDecision) {
	if decision.SessionID == "" {
		return
	}

	if decision.PTYSize != nil {
		if err := h.sessionManager.ResizeSession(decision.SessionID, decision.PTYSize.Rows, decision.PTYSize.Cols); err != nil {
			log.Printf("[Daemon] failed to apply PTY resize for session %s: %v", decision.SessionID, err)
		}
	}

	if len(decision.Broadcast) > 0 && h.apiServer != nil {
		payload := map[string]any{
			"instructions": decision.Broadcast,
		}
		h.apiServer.BroadcastSessionEvent("resize_instruction", decision.SessionID, payload)
	}

	for _, note := range decision.Notes {
		if note != "" {
			log.Printf("[ResizeMode] %s", note)
		}
	}
}

func convertMetadata(raw interface{}) map[string]any {
	meta := make(map[string]any)
	if raw == nil {
		return meta
	}

	if generic, ok := raw.(map[string]interface{}); ok {
		for k, v := range generic {
			meta[k] = v
		}
	}

	return meta
}

func toInt(value interface{}) (int, bool) {
	switch v := value.(type) {
	case nil:
		return 0, false
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case float32:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return int(i), true
		}
		if f, err := v.Float64(); err == nil {
			return int(f), true
		}
	case string:
		if i, err := strconv.Atoi(v); err == nil {
			return i, true
		}
	}
	return 0, false
}

// StreamSink implements pty.OutputSink for streaming to Unix socket
type StreamSink struct {
	handler   *ProtocolHandler
	sessionID string
	utf8acc   *utf8Accumulator
}

func (s *StreamSink) Write(data []byte) error {
	chunk := s.utf8acc.Take(data)
	if len(chunk) == 0 {
		return nil
	}

	msg := protocol.StreamMessage{
		Type:      "output",
		SessionID: s.sessionID,
		Data:      string(chunk),
	}

	s.handler.encoderMu.Lock()
	defer s.handler.encoderMu.Unlock()
	return s.handler.encoder.Encode(msg)
}

// NotifyEvent sends an event notification to the client
func (s *StreamSink) NotifyEvent(eventType string, exitCode int) {
	msg := protocol.StreamMessage{
		Type:      "event",
		SessionID: s.sessionID,
		Data:      fmt.Sprintf("%s:%d", eventType, exitCode),
	}

	s.handler.encoderMu.Lock()
	defer s.handler.encoderMu.Unlock()
	s.handler.encoder.Encode(msg)
}

// utf8Accumulator collects bytes until a full UTF-8 sequence boundary is reached.
type utf8Accumulator struct {
	mu      sync.Mutex
	pending []byte
}

func (u *utf8Accumulator) Take(data []byte) []byte {
	u.mu.Lock()
	defer u.mu.Unlock()

	if len(data) == 0 && len(u.pending) == 0 {
		return nil
	}

	buf := append(append([]byte{}, u.pending...), data...)
	if len(buf) == 0 {
		return nil
	}

	var out bytes.Buffer
	i := 0
	for i < len(buf) {
		r, size := utf8.DecodeRune(buf[i:])
		if r == utf8.RuneError && size == 1 && !utf8.FullRune(buf[i:]) {
			break
		}
		out.Write(buf[i : i+size])
		i += size
	}

	if i < len(buf) {
		u.pending = append(u.pending[:0], buf[i:]...)
	} else {
		u.pending = u.pending[:0]
	}

	return out.Bytes()
}
