package server

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/nupi-ai/nupi/internal/api"
	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/termresize"
)

// Message represents a WebSocket message
type Message struct {
	Type      string      `json:"type"`
	SessionID string      `json:"sessionId,omitempty"`
	Data      interface{} `json:"data,omitempty"`
	Encoding  string      `json:"encoding,omitempty"`
	Timestamp time.Time   `json:"timestamp"`
}

const (
	binaryMagic       = 0xBF
	binaryFrameOutput = 0x01
)

type outboundMessage struct {
	messageType int
	payload     []byte
}

// Client represents a WebSocket client
type Client struct {
	id           string
	conn         *websocket.Conn
	send         chan outboundMessage
	server       *Server
	sessionSinks map[string]*WebSocketSink // Track sinks per session
	mu           sync.RWMutex
}

// WebSocketSink implements pty.OutputSink for streaming to WebSocket
type WebSocketSink struct {
	client    *Client
	sessionID string
	utf8acc   *utf8Accumulator
}

func (ws *WebSocketSink) Write(data []byte) error {
	chunk := ws.utf8acc.Take(data)
	if len(chunk) == 0 {
		return nil
	}

	frame := encodeBinaryFrame(ws.sessionID, chunk)
	message := outboundMessage{
		messageType: websocket.BinaryMessage,
		payload:     frame,
	}

	select {
	case ws.client.send <- message:
	default:
		// Client's send channel is full, skip
	}

	return nil
}

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

// Server manages WebSocket connections and session streaming
type Server struct {
	sessionManager SessionManager
	clients        map[*Client]bool
	broadcast      chan outboundMessage
	register       chan *Client
	unregister     chan *Client
	upgrader       websocket.Upgrader
	mu             sync.RWMutex
	resizeManager  *termresize.Manager
}

// NewServer creates a new WebSocket server.
// The originAllowed function is used to validate the Origin header on upgrade requests.
func NewServer(sessionManager SessionManager, resizeManager *termresize.Manager, originAllowed func(string) bool) *Server {
	return &Server{
		sessionManager: sessionManager,
		clients:        make(map[*Client]bool),
		broadcast:      make(chan outboundMessage, 256),
		register:       make(chan *Client),
		unregister:     make(chan *Client),
		resizeManager:  resizeManager,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true
				}
				if originAllowed != nil {
					return originAllowed(origin)
				}
				return false
			},
		},
	}
}

// GetClientCount returns the number of connected clients (thread-safe)
func (s *Server) GetClientCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}

// Run starts the WebSocket server event loop
func (s *Server) Run() {
	for {
		select {
		case client := <-s.register:
			s.mu.Lock()
			s.clients[client] = true
			s.mu.Unlock()

			// Send current sessions list to new client
			s.sendSessionsList(client)

		case client := <-s.unregister:
			s.mu.Lock()
			if _, ok := s.clients[client]; ok {
				// Detach from all sessions
				client.detachAll()
				delete(s.clients, client)
				close(client.send)
			}
			s.mu.Unlock()

		case message := <-s.broadcast:
			s.mu.RLock()
			for client := range s.clients {
				select {
				case client.send <- message:
				default:
					// Client's send channel is full, skip
				}
			}
			s.mu.RUnlock()
		}
	}
}

// HandleWebSocket handles WebSocket connection upgrades
func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	client := &Client{
		id:           uuid.NewString(),
		conn:         conn,
		send:         make(chan outboundMessage, 1024),
		server:       s,
		sessionSinks: make(map[string]*WebSocketSink),
	}

	s.register <- client

	// Start goroutines for reading and writing
	go client.writePump()
	go client.readPump()
}

// sendSessionsList sends the current sessions list to a client
func (s *Server) sendSessionsList(client *Client) {
	if s.sessionManager == nil {
		return
	}
	sessions := s.sessionManager.ListSessions()
	sessionDTOs := api.ToDTOList(sessions)

	if s.resizeManager != nil {
		for i := range sessionDTOs {
			sessionDTOs[i].Mode = s.resizeManager.GetSessionMode(sessionDTOs[i].ID)
		}
	}

	msg := Message{
		Type:      "sessions_list",
		Data:      sessionDTOs,
		Timestamp: time.Now(),
	}

	jsonData, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling sessions list: %v", err)
		return
	}

	select {
	case client.send <- outboundMessage{messageType: websocket.TextMessage, payload: jsonData}:
	default:
		// Client's send channel is full
	}
}

// BroadcastSessionEvent broadcasts session lifecycle events
func (s *Server) BroadcastSessionEvent(eventType string, sessionID string, data interface{}) {
	msg := Message{
		Type:      eventType,
		SessionID: sessionID,
		Data:      data,
		Timestamp: time.Now(),
	}

	jsonData, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling event: %v", err)
		return
	}

	s.broadcast <- outboundMessage{messageType: websocket.TextMessage, payload: jsonData}
}

// readPump reads messages from the WebSocket connection
func (c *Client) readPump() {
	defer func() {
		c.server.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		messageType, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			break
		}

		if messageType != websocket.TextMessage {
			// Currently we ignore non-text messages from clients
			continue
		}

		// Parse incoming message
		var msg Message
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("Error parsing message: %v", err)
			continue
		}

		// Handle different message types
		switch msg.Type {
		case "attach":
			if sessionID, ok := msg.Data.(string); ok {
				c.attachToSession(sessionID)
			}

		case "detach":
			if sessionID, ok := msg.Data.(string); ok {
				c.detachFromSession(sessionID)
			}

		case "input":
			// Handle input to session
			if data, ok := msg.Data.(map[string]interface{}); ok {
				if sessionID, ok := data["sessionId"].(string); ok {
					if input, ok := data["input"].(string); ok {
						c.sendInputToSession(sessionID, []byte(input))
					}
				}
			}

		case "list":
			c.server.sendSessionsList(c)

		case "create":
			// Create new session via WebSocket
			if data, ok := msg.Data.(map[string]interface{}); ok {
				if command, ok := data["command"].(string); ok {
					c.createSession(command, data["args"])
				}
			}

		case "resize":
			if data, ok := msg.Data.(map[string]interface{}); ok {
				sessionID, _ := data["sessionId"].(string)
				if sessionID == "" {
					c.sendError("resize message missing sessionId")
					continue
				}

				cols, okCols := toInt(data["cols"])
				rows, okRows := toInt(data["rows"])
				if !okCols || !okRows {
					c.sendError("resize message missing cols/rows")
					continue
				}

				metadata := convertMetadata(data["meta"])
				source := termresize.SourceClient
				if src, ok := data["source"].(string); ok && src == string(termresize.SourceHost) {
					source = termresize.SourceHost
				}

				var decision termresize.ResizeDecision
				var err error
				size := termresize.ViewportSize{Cols: cols, Rows: rows}

				if c.server.resizeManager == nil {
					log.Printf("[WebSocket] resize manager not initialised, dropping resize for session %s", sessionID)
					continue
				}

				switch source {
				case termresize.SourceHost:
					decision, err = c.server.resizeManager.HandleHostResize(sessionID, size, metadata)
				default:
					clientID := c.id
					if customID, ok := data["clientId"].(string); ok && customID != "" {
						clientID = customID
					}
					decision, err = c.server.resizeManager.HandleClientResize(sessionID, clientID, size, metadata)
				}

				if err != nil {
					c.sendError(fmt.Sprintf("resize handling failed: %v", err))
					continue
				}

				c.server.applyResizeDecision(decision)
			}

		case "kill":
			if sessionID, ok := msg.Data.(string); ok {
				c.killSession(sessionID)
			}
		}
	}
}

// writePump writes messages to the WebSocket connection
func (c *Client) writePump() {
	ticker := time.NewTicker(54 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.conn.WriteMessage(message.messageType, message.payload); err != nil {
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// attachToSession attaches client to a session for output streaming
func (c *Client) attachToSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	log.Printf("[WebSocket] Client attempting to attach to session %s", sessionID)

	// Check if already attached - if so, just return (don't re-attach)
	if _, exists := c.sessionSinks[sessionID]; exists {
		log.Printf("[WebSocket] Client already attached to session %s, skipping re-attach", sessionID)
		return
	}

	if c.server.resizeManager != nil {
		c.server.resizeManager.UnregisterClient(sessionID, c.id)
	}

	if c.server.sessionManager == nil {
		c.sendError("session manager unavailable")
		return
	}

	// Create WebSocket sink for this session
	sink := &WebSocketSink{
		client:    c,
		sessionID: sessionID,
		utf8acc:   &utf8Accumulator{},
	}

	// Attach to session with history
	err := c.server.sessionManager.AttachToSession(sessionID, sink, true)
	if err != nil {
		c.sendError(fmt.Sprintf("Failed to attach to session: %v", err))
		return
	}

	c.sessionSinks[sessionID] = sink

	if c.server.resizeManager != nil {
		c.server.resizeManager.RegisterClient(sessionID, c.id, nil)

		if state := c.server.resizeManager.Snapshot(sessionID); state != nil && state.Host != nil {
			instruction := termresize.BroadcastInstruction{
				TargetIDs: []string{c.id},
				Size:      state.Host.Size,
				Reason:    "host_lock_sync",
			}
			payload := map[string]interface{}{
				"instructions": []termresize.BroadcastInstruction{instruction},
			}

			msg := Message{
				Type:      "resize_instruction",
				SessionID: sessionID,
				Data:      payload,
				Timestamp: time.Now(),
			}

			if data, err := json.Marshal(msg); err == nil {
				select {
				case c.send <- outboundMessage{messageType: websocket.TextMessage, payload: data}:
				default:
				}
			}
		}
	}

	// Notify client of successful attachment
	msg := Message{
		Type:      "attached",
		SessionID: sessionID,
		Data:      nil,
		Timestamp: time.Now(),
	}

	jsonData, _ := json.Marshal(msg)
	select {
	case c.send <- outboundMessage{messageType: websocket.TextMessage, payload: jsonData}:
	default:
	}
}

// detachFromSession detaches client from a session
func (c *Client) detachFromSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	log.Printf("[WebSocket] Client detaching from session %s", sessionID)

	sink, exists := c.sessionSinks[sessionID]
	if !exists {
		log.Printf("[WebSocket] Client not attached to session %s, skipping detach", sessionID)
		return
	}

	if c.server.sessionManager != nil {
		if err := c.server.sessionManager.DetachFromSession(sessionID, sink); err != nil {
			log.Printf("[WebSocket] detach from session %s: %v", sessionID, err)
		}
	}
	delete(c.sessionSinks, sessionID)
	log.Printf("[WebSocket] Client detached from session %s", sessionID)

	if c.server.resizeManager != nil {
		c.server.resizeManager.UnregisterClient(sessionID, c.id)
	}

	// Notify client of detachment
	msg := Message{
		Type:      "detached",
		SessionID: sessionID,
		Data:      nil,
		Timestamp: time.Now(),
	}

	jsonData, _ := json.Marshal(msg)
	select {
	case c.send <- outboundMessage{messageType: websocket.TextMessage, payload: jsonData}:
	default:
	}
}

// detachAll detaches from all sessions
func (c *Client) detachAll() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for sessionID, sink := range c.sessionSinks {
		if c.server.sessionManager != nil {
			if err := c.server.sessionManager.DetachFromSession(sessionID, sink); err != nil {
				log.Printf("[WebSocket] detach from session %s: %v", sessionID, err)
			}
		}
		if c.server.resizeManager != nil {
			c.server.resizeManager.UnregisterClient(sessionID, c.id)
		}
	}
	c.sessionSinks = make(map[string]*WebSocketSink)
}

// sendInputToSession sends input to a session
func (c *Client) sendInputToSession(sessionID string, input []byte) {
	if c.server.sessionManager == nil {
		c.sendError("session manager unavailable")
		return
	}
	err := c.server.sessionManager.WriteToSession(sessionID, input)
	if err != nil {
		c.sendError(fmt.Sprintf("Failed to send input: %v", err))
	}
}

// createSession creates a new session via WebSocket
func (c *Client) createSession(command string, argsInterface interface{}) {
	if c.server.sessionManager == nil {
		c.sendError("session manager unavailable")
		return
	}

	var args []string
	if argsArray, ok := argsInterface.([]interface{}); ok {
		for _, arg := range argsArray {
			if str, ok := arg.(string); ok {
				args = append(args, str)
			}
		}
	}

	opts := pty.StartOptions{
		Command: command,
		Args:    args,
		Rows:    24,
		Cols:    80,
	}

	session, err := c.server.sessionManager.CreateSession(opts, false)
	if err != nil {
		c.sendError(fmt.Sprintf("Failed to create session: %v", err))
		return
	}

	// Notify all clients about new session
	c.server.BroadcastSessionEvent("session_created", session.ID, api.ToDTO(session))

	// Auto-attach creator to the new session
	c.attachToSession(session.ID)
}

// killSession kills a session
func (c *Client) killSession(sessionID string) {
	if c.server.sessionManager == nil {
		c.sendError("session manager unavailable")
		return
	}
	err := c.server.sessionManager.KillSession(sessionID)
	if err != nil {
		c.sendError(fmt.Sprintf("Failed to kill session: %v", err))
		return
	}

	// Notify all clients
	c.server.BroadcastSessionEvent("session_killed", sessionID, nil)
}

// sendError sends an error message to the client
func (c *Client) sendError(errMsg string) {
	msg := Message{
		Type:      "error",
		Data:      errMsg,
		Timestamp: time.Now(),
	}

	jsonData, _ := json.Marshal(msg)
	select {
	case c.send <- outboundMessage{messageType: websocket.TextMessage, payload: jsonData}:
	default:
	}
}

func (s *Server) applyResizeDecision(decision termresize.ResizeDecision) {
	if decision.SessionID == "" {
		return
	}

	if decision.PTYSize != nil && s.sessionManager != nil {
		if err := s.sessionManager.ResizeSession(decision.SessionID, decision.PTYSize.Rows, decision.PTYSize.Cols); err != nil {
			log.Printf("[WebSocket] failed to apply PTY resize for session %s: %v", decision.SessionID, err)
		}
	}

	if len(decision.Broadcast) > 0 {
		payload := map[string]any{
			"instructions": decision.Broadcast,
		}
		s.BroadcastSessionEvent("resize_instruction", decision.SessionID, payload)
	}

	for _, note := range decision.Notes {
		if note != "" {
			log.Printf("[ResizeMode] %s", note)
		}
	}
}

const binaryHeaderSize = 4

func encodeBinaryFrame(sessionID string, payload []byte) []byte {
	sessionIDBytes := []byte(sessionID)
	totalLen := binaryHeaderSize + len(sessionIDBytes) + len(payload)
	frame := make([]byte, totalLen)

	frame[0] = binaryMagic
	frame[1] = binaryFrameOutput
	binary.LittleEndian.PutUint16(frame[2:4], uint16(len(sessionIDBytes)))
	copy(frame[binaryHeaderSize:], sessionIDBytes)
	copy(frame[binaryHeaderSize+len(sessionIDBytes):], payload)

	return frame
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
