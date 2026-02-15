package server

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nupi-ai/nupi/internal/api"
	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/session"
	"github.com/nupi-ai/nupi/internal/termresize"
)

// skipIfNoNetwork skips the test if network binding is not available
// (e.g., in sandboxed environments without network access).
func skipIfNoNetwork(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "operation not permitted") ||
			strings.Contains(msg, "permission denied") ||
			strings.Contains(msg, "bind") {
			t.Skipf("Network binding not available: %v", err)
		}
	}
	if ln != nil {
		ln.Close()
	}
}

func buildTestServer(t *testing.T, sessionManager SessionManager) *Server {
	resizeManager, err := termresize.NewManagerWithDefaults()
	if err != nil {
		t.Fatalf("failed to build resize manager: %v", err)
	}
	return NewServer(sessionManager, resizeManager, func(string) bool { return true })
}

// TestWebSocketBroadcast tests that events are broadcast to all connected clients
func TestWebSocketBroadcast(t *testing.T) {
	skipIfNoPTY(t)
	skipIfNoNetwork(t)

	// Create session manager and WebSocket server
	sessionManager := session.NewManager()
	wsServer := buildTestServer(t, sessionManager)

	// Start WebSocket server
	go wsServer.Run()

	// Create HTTP test server
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsServer.HandleWebSocket)
	server := httptest.NewServer(mux)
	defer server.Close()

	// Convert http:// to ws://
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	// Connect multiple clients
	numClients := 3
	clients := make([]*websocket.Conn, numClients)
	clientMessages := make([]chan Message, numClients)

	for i := 0; i < numClients; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("Failed to connect client %d: %v", i, err)
		}
		defer conn.Close()
		clients[i] = conn

		// Create message channel for this client
		msgChan := make(chan Message, 10)
		clientMessages[i] = msgChan

		// Start reading messages for this client
		go func(c *websocket.Conn, ch chan Message) {
			for {
				var msg Message
				err := c.ReadJSON(&msg)
				if err != nil {
					return
				}
				ch <- msg
			}
		}(conn, msgChan)
	}

	// Give clients time to connect
	time.Sleep(100 * time.Millisecond)

	// Create a session
	opts := pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "echo test"},
		Rows:    24,
		Cols:    80,
	}

	sess, err := sessionManager.CreateSession(opts, false)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Broadcast session created event
	sessionDTO := api.ToDTO(sess)
	wsServer.BroadcastSessionEvent("session_created", sess.ID, sessionDTO)

	// Verify all clients received the broadcast
	for i := 0; i < numClients; i++ {
		select {
		case msg := <-clientMessages[i]:
			// Skip sessions_list message sent on connect
			if msg.Type == "sessions_list" {
				// Read next message
				select {
				case msg = <-clientMessages[i]:
				case <-time.After(2 * time.Second):
					t.Fatalf("Client %d did not receive broadcast", i)
				}
			}

			if msg.Type != "session_created" {
				t.Errorf("Client %d: expected type 'session_created', got %s", i, msg.Type)
			}
			if msg.SessionID != sess.ID {
				t.Errorf("Client %d: wrong session ID", i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("Client %d did not receive broadcast", i)
		}
	}
}

// TestWebSocketGetClientCount tests thread-safe client counting
func TestWebSocketGetClientCount(t *testing.T) {
	skipIfNoPTY(t)
	skipIfNoNetwork(t)

	sessionManager := session.NewManager()
	wsServer := buildTestServer(t, sessionManager)

	// Start WebSocket server
	go wsServer.Run()

	// Create HTTP test server
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsServer.HandleWebSocket)
	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	// Initially should have 0 clients
	if count := wsServer.GetClientCount(); count != 0 {
		t.Errorf("Expected 0 clients, got %d", count)
	}

	// Connect multiple clients concurrently
	numClients := 10
	var wg sync.WaitGroup
	clients := make([]*websocket.Conn, numClients)

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				t.Errorf("Failed to connect client %d: %v", idx, err)
				return
			}
			clients[idx] = conn
		}(i)
	}

	wg.Wait()
	time.Sleep(200 * time.Millisecond) // Let connections register

	// Should have numClients connected
	if count := wsServer.GetClientCount(); count != numClients {
		t.Errorf("Expected %d clients, got %d", numClients, count)
	}

	// Disconnect half the clients
	for i := 0; i < numClients/2; i++ {
		if clients[i] != nil {
			clients[i].Close()
		}
	}

	time.Sleep(200 * time.Millisecond) // Let disconnections process

	// Should have half the clients
	expectedCount := numClients - numClients/2
	if count := wsServer.GetClientCount(); count != expectedCount {
		t.Errorf("Expected %d clients after disconnect, got %d", expectedCount, count)
	}

	// Clean up remaining connections
	for i := numClients / 2; i < numClients; i++ {
		if clients[i] != nil {
			clients[i].Close()
		}
	}
}

// TestWebSocketRaceConditions tests for race conditions with concurrent operations
func TestWebSocketRaceConditions(t *testing.T) {
	skipIfNoPTY(t)
	skipIfNoNetwork(t)

	sessionManager := session.NewManager()
	wsServer := buildTestServer(t, sessionManager)

	// Start WebSocket server
	go wsServer.Run()

	// Create HTTP test server
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsServer.HandleWebSocket)
	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	// Run multiple goroutines doing various operations
	var wg sync.WaitGroup
	numGoroutines := 20

	// Goroutines that connect/disconnect clients
	for i := 0; i < numGoroutines/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
				if err != nil {
					continue
				}
				// Call GetClientCount while connected
				_ = wsServer.GetClientCount()
				time.Sleep(10 * time.Millisecond)
				conn.Close()
			}
		}()
	}

	// Goroutines that broadcast events
	for i := 0; i < numGoroutines/2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				wsServer.BroadcastSessionEvent("test_event",
					"session_"+string(rune(id)),
					map[string]interface{}{"test": j})
				time.Sleep(5 * time.Millisecond)
			}
		}(i)
	}

	// Goroutines that check client count
	for i := 0; i < numGoroutines/4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				count := wsServer.GetClientCount()
				if count < 0 {
					t.Errorf("Invalid client count: %d", count)
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}

	wg.Wait()

	// If we get here without panics or deadlocks, the test passes
}

// TestWebSocketSessionStatusBroadcast tests session status change broadcasts
func TestWebSocketSessionStatusBroadcast(t *testing.T) {
	skipIfNoPTY(t)
	skipIfNoNetwork(t)

	sessionManager := session.NewManager()
	wsServer := buildTestServer(t, sessionManager)

	// Start WebSocket server
	go wsServer.Run()

	// Create HTTP test server
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsServer.HandleWebSocket)
	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	// Connect a client
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	messages := make(chan Message, 10)
	go func() {
		for {
			var msg Message
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			messages <- msg
		}
	}()

	// Skip initial sessions_list message
	select {
	case <-messages:
		// Got sessions_list, continue
	case <-time.After(1 * time.Second):
		t.Fatal("Did not receive initial sessions_list")
	}

	// Broadcast a status change event
	sessionID := "test-session-123"
	newStatus := "stopped"
	wsServer.BroadcastSessionEvent("session_status_changed", sessionID, newStatus)

	// Verify client received the status change
	select {
	case msg := <-messages:
		if msg.Type != "session_status_changed" {
			t.Errorf("Expected type 'session_status_changed', got %s", msg.Type)
		}
		if msg.SessionID != sessionID {
			t.Errorf("Wrong session ID: expected %s, got %s", sessionID, msg.SessionID)
		}
		if statusStr, ok := msg.Data.(string); !ok || statusStr != newStatus {
			t.Errorf("Wrong status data: expected %s, got %v", newStatus, msg.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Did not receive status change broadcast")
	}
}

// TestWebSocketSessionListOnConnect validates sessions_list message
// sent on initial WebSocket connection with correct session fields.
func TestWebSocketSessionListOnConnect(t *testing.T) {
	skipIfNoPTY(t)
	skipIfNoNetwork(t)

	sessionManager := session.NewManager()

	sess, err := sessionManager.CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 10"},
		Rows:    24,
		Cols:    80,
	}, false)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer sessionManager.KillSession(sess.ID)

	wsServer := buildTestServer(t, sessionManager)
	go wsServer.Run()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsServer.HandleWebSocket)
	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read sessions_list: %v", err)
	}

	var msg struct {
		Type string            `json:"type"`
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Type != "sessions_list" {
		t.Fatalf("expected sessions_list, got %s", msg.Type)
	}
	if len(msg.Data) != 1 {
		t.Fatalf("expected 1 session, got %d", len(msg.Data))
	}

	var dto api.SessionDTO
	if err := json.Unmarshal(msg.Data[0], &dto); err != nil {
		t.Fatalf("unmarshal session DTO: %v", err)
	}
	if dto.ID != sess.ID {
		t.Fatalf("expected session ID %s, got %s", sess.ID, dto.ID)
	}
	if dto.Command != "/bin/sh" {
		t.Fatalf("expected command /bin/sh, got %s", dto.Command)
	}
	if dto.Status != "running" {
		t.Fatalf("expected status running, got %s", dto.Status)
	}
	if dto.PID == 0 {
		t.Fatal("expected non-zero PID")
	}
	if dto.StartTime.IsZero() {
		t.Fatal("expected non-zero start_time")
	}
}

// TestWebSocketSessionCreatedBroadcast tests that creating a session via
// WebSocket create message broadcasts session_created to connected clients.
func TestWebSocketSessionCreatedBroadcast(t *testing.T) {
	skipIfNoPTY(t)
	skipIfNoNetwork(t)

	sessionManager := session.NewManager()
	t.Cleanup(func() {
		for _, s := range sessionManager.ListSessions() {
			sessionManager.KillSession(s.ID)
		}
	})
	wsServer := buildTestServer(t, sessionManager)
	go wsServer.Run()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsServer.HandleWebSocket)
	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	messages := make(chan json.RawMessage, 10)
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			messages <- raw
		}
	}()

	// Skip initial sessions_list
	select {
	case <-messages:
	case <-time.After(2 * time.Second):
		t.Fatal("no sessions_list received")
	}

	// Send create message via WebSocket
	createMsg := Message{
		Type: "create",
		Data: map[string]interface{}{
			"command": "/bin/sh",
			"args":    []string{"-c", "sleep 10"},
		},
	}
	if err := conn.WriteJSON(createMsg); err != nil {
		t.Fatalf("write create: %v", err)
	}

	// Read messages until session_created
	deadline := time.After(3 * time.Second)
	for {
		select {
		case raw := <-messages:
			var msg struct {
				Type      string          `json:"type"`
				SessionID string          `json:"sessionId"`
				Data      json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			if msg.Type == "session_created" {
				if msg.SessionID == "" {
					t.Fatal("session_created missing sessionId")
				}
				var dto api.SessionDTO
				if err := json.Unmarshal(msg.Data, &dto); err != nil {
					t.Fatalf("unmarshal DTO: %v", err)
				}
				if dto.ID != msg.SessionID {
					t.Fatalf("DTO ID %s != sessionId %s", dto.ID, msg.SessionID)
				}
				if dto.Command != "/bin/sh" {
					t.Fatalf("expected command /bin/sh, got %s", dto.Command)
				}
				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for session_created")
		}
	}
}

// TestWebSocketSessionKilledBroadcast tests that killing a session via
// WebSocket kill message broadcasts session_killed and stops the session.
func TestWebSocketSessionKilledBroadcast(t *testing.T) {
	skipIfNoPTY(t)
	skipIfNoNetwork(t)

	sessionManager := session.NewManager()
	sess, err := sessionManager.CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 30"},
		Rows:    24,
		Cols:    80,
	}, false)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() { _ = sessionManager.KillSession(sess.ID) })

	wsServer := buildTestServer(t, sessionManager)
	go wsServer.Run()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsServer.HandleWebSocket)
	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	messages := make(chan json.RawMessage, 10)
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			messages <- raw
		}
	}()

	// Skip sessions_list
	select {
	case <-messages:
	case <-time.After(2 * time.Second):
		t.Fatal("no sessions_list")
	}

	// Kill session via WebSocket
	killMsg := Message{Type: "kill", Data: sess.ID}
	if err := conn.WriteJSON(killMsg); err != nil {
		t.Fatalf("write kill: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case raw := <-messages:
			var msg struct {
				Type      string `json:"type"`
				SessionID string `json:"sessionId"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			if msg.Type == "session_killed" {
				if msg.SessionID != sess.ID {
					t.Fatalf("expected sessionId %s, got %s", sess.ID, msg.SessionID)
				}
				// Verify session is actually stopped
				got, err := sessionManager.GetSession(sess.ID)
				if err != nil {
					t.Fatalf("get session: %v", err)
				}
				status := string(got.CurrentStatus())
				if status != "stopped" {
					t.Fatalf("expected status stopped, got %s", status)
				}
				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for session_killed")
		}
	}
}

// TestWebSocketBinaryTerminalOutput validates the binary frame protocol
// used for streaming terminal output from an attached session.
func TestWebSocketBinaryTerminalOutput(t *testing.T) {
	skipIfNoPTY(t)
	skipIfNoNetwork(t)

	sessionManager := session.NewManager()
	sess, err := sessionManager.CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 0.1 && echo hello_from_pty"},
		Rows:    24,
		Cols:    80,
	}, false)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer sessionManager.KillSession(sess.ID)

	wsServer := buildTestServer(t, sessionManager)
	go wsServer.Run()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsServer.HandleWebSocket)
	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Skip sessions_list
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read sessions_list: %v", err)
	}

	// Attach to session
	attachMsg := Message{Type: "attach", Data: sess.ID}
	if err := conn.WriteJSON(attachMsg); err != nil {
		t.Fatalf("write attach: %v", err)
	}

	// Read messages until we get a binary frame
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read message: %v", err)
		}
		if msgType != websocket.BinaryMessage {
			continue
		}

		// Validate binary frame format: [0xBF][0x01][sessionIDLen:2LE][sessionID][payload]
		if len(data) < binaryHeaderSize {
			t.Fatalf("binary frame too short: %d bytes", len(data))
		}
		if data[0] != binaryMagic {
			t.Fatalf("expected magic byte 0x%02X, got 0x%02X", binaryMagic, data[0])
		}
		if data[1] != binaryFrameOutput {
			t.Fatalf("expected frame type 0x%02X, got 0x%02X", binaryFrameOutput, data[1])
		}

		idLen := binary.LittleEndian.Uint16(data[2:4])
		if int(idLen) != len(sess.ID) {
			t.Fatalf("expected session ID length %d, got %d", len(sess.ID), idLen)
		}
		if len(data) < binaryHeaderSize+int(idLen) {
			t.Fatalf("binary frame too short for session ID: need %d bytes, got %d", binaryHeaderSize+int(idLen), len(data))
		}
		frameSessionID := string(data[binaryHeaderSize : binaryHeaderSize+int(idLen)])
		if frameSessionID != sess.ID {
			t.Fatalf("expected session ID %s, got %s", sess.ID, frameSessionID)
		}

		payload := data[binaryHeaderSize+int(idLen):]
		if len(payload) == 0 {
			t.Fatal("expected non-empty payload")
		}
		// Binary frame verified — we got terminal output in the correct format
		return
	}
}

// TestWebSocketSessionStatusChangedOnDetach validates that detaching from a
// session sends a detached confirmation back to the client.
func TestWebSocketSessionStatusChangedOnDetach(t *testing.T) {
	skipIfNoPTY(t)
	skipIfNoNetwork(t)

	sessionManager := session.NewManager()
	sess, err := sessionManager.CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 30"},
		Rows:    24,
		Cols:    80,
	}, false)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer sessionManager.KillSession(sess.ID)

	wsServer := buildTestServer(t, sessionManager)
	go wsServer.Run()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsServer.HandleWebSocket)
	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	textMessages := make(chan json.RawMessage, 20)
	go func() {
		for {
			msgType, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if msgType == websocket.TextMessage {
				textMessages <- raw
			}
		}
	}()

	// Skip sessions_list
	select {
	case <-textMessages:
	case <-time.After(2 * time.Second):
		t.Fatal("no sessions_list")
	}

	// Attach
	if err := conn.WriteJSON(Message{Type: "attach", Data: sess.ID}); err != nil {
		t.Fatalf("write attach: %v", err)
	}

	// Wait for "attached" confirmation
	waitForWSMessageType(t, textMessages, "attached", 2*time.Second)

	// Detach
	if err := conn.WriteJSON(Message{Type: "detach", Data: sess.ID}); err != nil {
		t.Fatalf("write detach: %v", err)
	}

	// Wait for "detached" confirmation
	msg := waitForWSMessageType(t, textMessages, "detached", 2*time.Second)

	var parsed struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(msg, &parsed); err != nil {
		t.Fatalf("unmarshal detached: %v", err)
	}
	if parsed.SessionID != sess.ID {
		t.Fatalf("expected sessionId %s, got %s", sess.ID, parsed.SessionID)
	}
}

func waitForWSMessageType(t *testing.T, ch <-chan json.RawMessage, msgType string, timeout time.Duration) json.RawMessage {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case raw := <-ch:
			var msg struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &msg); err == nil && msg.Type == msgType {
				return raw
			}
		case <-deadline:
			t.Fatalf("timeout waiting for message type %q", msgType)
		}
	}
}
