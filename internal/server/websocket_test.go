package server

import (
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

func buildTestServer(t *testing.T, sessionManager *session.Manager) *Server {
	resizeManager, err := termresize.NewManagerWithDefaults()
	if err != nil {
		t.Fatalf("failed to build resize manager: %v", err)
	}
	return NewServer(sessionManager, resizeManager)
}

// TestWebSocketBroadcast tests that events are broadcast to all connected clients
func TestWebSocketBroadcast(t *testing.T) {
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
