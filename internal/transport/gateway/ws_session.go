package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/server"
	"github.com/nupi-ai/nupi/internal/termresize"
)

// wsSessionHandler serves WebSocket connections for PTY session streaming.
// It translates WebSocket frames into session manager calls, enabling mobile
// clients to attach to terminal sessions where gRPC bidi-streaming is unavailable.
type wsSessionHandler struct {
	apiServer   *server.APIServer
	shutdownCtx context.Context
}

// wsControlMessage represents a JSON control message from the client.
type wsControlMessage struct {
	Type string `json:"type"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// wsEventMessage represents a JSON event message sent to the client.
type wsEventMessage struct {
	Type      string            `json:"type"`
	EventType string            `json:"event_type"`
	SessionID string            `json:"session_id"`
	Data      map[string]string `json:"data,omitempty"`
}

// wsSink implements pty.OutputSink, forwarding PTY output as WebSocket binary messages.
var _ pty.OutputSink = (*wsSink)(nil)

type wsSink struct {
	conn   *websocket.Conn
	ctx    context.Context
	mu     sync.Mutex
	closed bool
}

func (s *wsSink) Write(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.conn.Write(s.ctx, websocket.MessageBinary, data)
}

func (s *wsSink) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func newWSSessionHandler(apiServer *server.APIServer, shutdownCtx context.Context) *wsSessionHandler {
	return &wsSessionHandler{apiServer: apiServer, shutdownCtx: shutdownCtx}
}

func (h *wsSessionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sessionID, conn, ctx, cancel, err := wsHandshake(h.apiServer, h.shutdownCtx, "/ws/session/", w, r)
	if err != nil {
		log.Printf("[WS Session] accept error for session %s: %v", sessionID, err)
		return
	}
	if conn == nil {
		return
	}
	defer conn.CloseNow()

	sm := h.apiServer.SessionMgr()
	defer cancel()

	conn.SetReadLimit(64 * 1024) // 64 KB max incoming message (control JSON + PTY input)

	// Create output sink.
	sink := &wsSink{conn: conn, ctx: ctx}
	defer sink.close()

	// Attach to session. Clients can opt out of history replay via ?history=false.
	includeHistory := r.URL.Query().Get("history") != "false"
	if err := sm.AttachToSession(sessionID, sink, includeHistory); err != nil {
		log.Printf("[WS Session] attach to session %s failed: %v", sessionID, err)
		conn.Close(websocket.StatusInternalError, "attach failed")
		return
	}
	defer func() {
		if dErr := sm.DetachFromSession(sessionID, sink); dErr != nil {
			log.Printf("[WS Session] detach from session %s: %v", sessionID, dErr)
		}
	}()

	// Register with resize manager for proper multi-client resize negotiation,
	// matching the gRPC AttachSession pattern.
	clientID := fmt.Sprintf("ws_%s", uuid.NewString())
	if rm := h.apiServer.ResizeManager(); rm != nil {
		rm.RegisterClient(sessionID, clientID, nil)
		defer rm.UnregisterClient(sessionID, clientID)

		// Send initial resize state so the client knows the current terminal
		// dimensions, matching the gRPC AttachSession pattern.
		if state := rm.Snapshot(sessionID); state != nil && state.Host != nil {
			initMsg := wsEventMessage{
				Type:      "event",
				EventType: "resize_instruction",
				SessionID: sessionID,
				Data: map[string]string{
					"cols":   fmt.Sprintf("%d", state.Host.Size.Cols),
					"rows":   fmt.Sprintf("%d", state.Host.Size.Rows),
					"reason": "host_lock_sync",
				},
			}
			data, err := json.Marshal(initMsg)
			if err != nil {
				log.Printf("[WS Session] initial resize marshal error for session %s: %v", sessionID, err)
			} else {
				if wErr := conn.Write(ctx, websocket.MessageText, data); wErr != nil {
					if !isExpectedWSClose(wErr) {
						log.Printf("[WS Session] initial resize write error for session %s: %v", sessionID, wErr)
					}
				}
			}
		}
	}

	// Subscribe to lifecycle events and forward as JSON text messages.
	bus := h.apiServer.EventBus()
	if bus != nil {
		lifecycleSub := eventbus.Subscribe[eventbus.SessionLifecycleEvent](bus,
			eventbus.TopicSessionsLifecycle,
			eventbus.WithSubscriptionName("ws_lifecycle_"+clientID),
			eventbus.WithSubscriptionBuffer(16),
		)
		defer lifecycleSub.Close()

		go h.pumpEvents(ctx, cancel, conn, lifecycleSub, sessionID)
	}

	// Start WebSocket keepalive for mobile networks where NAT gateways
	// drop idle connections after ~30-60s of inactivity.
	go h.pumpPing(ctx, cancel, conn, sessionID)

	// Input pump: read WebSocket messages and dispatch to session.
	h.pumpInput(ctx, conn, sm, sessionID)
}

// pumpEvents forwards session lifecycle events as JSON text messages until
// ctx is cancelled or the subscription closes.
func (h *wsSessionHandler) pumpEvents(
	ctx context.Context,
	cancel context.CancelFunc,
	conn *websocket.Conn,
	sub *eventbus.TypedSubscription[eventbus.SessionLifecycleEvent],
	sessionID string,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			evt := env.Payload
			if evt.SessionID != sessionID {
				continue
			}
			msg := wsEventMessage{
				Type:      "event",
				EventType: string(evt.State),
				SessionID: sessionID,
			}
			if evt.ExitCode != nil {
				if msg.Data == nil {
					msg.Data = make(map[string]string)
				}
				msg.Data["exit_code"] = fmt.Sprintf("%d", *evt.ExitCode)
			}
			if evt.Reason != "" {
				if msg.Data == nil {
					msg.Data = make(map[string]string)
				}
				msg.Data["reason"] = evt.Reason
			}
			data, err := json.Marshal(msg)
			if err != nil {
				log.Printf("[WS Session] event marshal error for session %s: %v", sessionID, err)
				continue
			}
			if wErr := conn.Write(ctx, websocket.MessageText, data); wErr != nil {
				if !isExpectedWSClose(wErr) {
					log.Printf("[WS Session] event write error for session %s: %v", sessionID, wErr)
				}
				return
			}

			// If session stopped, send a clean close frame (matching kill/detach
			// behavior) so mobile clients see StatusNormalClosure instead of
			// an abrupt disconnect after the stopped event.
			if evt.State == eventbus.SessionStateStopped {
				conn.Close(websocket.StatusNormalClosure, "session stopped")
				cancel()
				return
			}
		}
	}
}

// pumpInput reads WebSocket messages and forwards them to the session.
// Binary frames are terminal input; text frames are JSON control messages.
func (h *wsSessionHandler) pumpInput(
	ctx context.Context,
	conn *websocket.Conn,
	sm server.SessionManager,
	sessionID string,
) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			// Only log unexpected errors — EOF, normal closure, and context
			// cancellation are expected during normal disconnect/shutdown.
			if !isExpectedWSClose(err) {
				log.Printf("[WS Session] read error for session %s: %v", sessionID, err)
			}
			return
		}

		switch typ {
		case websocket.MessageBinary:
			if len(data) > 0 {
				if wErr := sm.WriteToSession(sessionID, data); wErr != nil {
					log.Printf("[WS Session] write to session %s: %v", sessionID, wErr)
				}
			}
		case websocket.MessageText:
			var ctrl wsControlMessage
			if err := json.Unmarshal(data, &ctrl); err != nil {
				log.Printf("[WS Session] invalid control JSON for session %s: %v", sessionID, err)
				continue
			}
			switch ctrl.Type {
			case "resize":
				if ctrl.Cols > 0 && ctrl.Rows > 0 {
					h.handleResize(ctx, conn, sm, sessionID, ctrl.Cols, ctrl.Rows)
				} else {
					log.Printf("[WS Session] ignoring resize with invalid dimensions cols=%d rows=%d for session %s", ctrl.Cols, ctrl.Rows, sessionID)
				}
			case "kill":
				conn.Close(websocket.StatusNormalClosure, "killed")
				go func() {
					if kErr := sm.KillSession(sessionID); kErr != nil {
						log.Printf("[WS Session] kill session %s: %v", sessionID, kErr)
					}
				}()
				return
			case "detach":
				conn.Close(websocket.StatusNormalClosure, "detached")
				return
			default:
				if ctrl.Type != "" {
					log.Printf("[WS Session] unknown control type %q for session %s", ctrl.Type, sessionID)
				}
			}
		}
	}
}

// pumpPing sends periodic WebSocket pings to detect dead connections on
// mobile networks where NAT gateways drop idle connections.
func (h *wsSessionHandler) pumpPing(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, sessionID string) {
	ticker := time.NewTicker(constants.Duration30Seconds)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, constants.Duration5Seconds)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				if !isExpectedWSClose(err) {
					log.Printf("[WS Session] ping failed for session %s: %v", sessionID, err)
				}
				cancel()
				return
			}
		}
	}
}

// handleResize processes a resize request through the resize manager when
// available, falling back to direct session resize otherwise. WebSocket
// clients are treated as host sources (SourceHost) because mobile devices
// are the primary display — matching the gRPC pattern where the display
// device sends source=host to apply resize immediately under host_lock mode.
func (h *wsSessionHandler) handleResize(ctx context.Context, conn *websocket.Conn, sm server.SessionManager, sessionID string, cols, rows int) {
	rm := h.apiServer.ResizeManager()
	if rm != nil {
		size := termresize.ViewportSize{Cols: cols, Rows: rows}
		decision, err := rm.HandleHostResize(sessionID, size, nil)
		if err != nil {
			log.Printf("[WS Session] resize session %s: %v", sessionID, err)
			return
		}
		if decision.PTYSize != nil {
			if rErr := sm.ResizeSession(sessionID, decision.PTYSize.Rows, decision.PTYSize.Cols); rErr != nil {
				log.Printf("[WS Session] apply resize session %s: %v", sessionID, rErr)
			}
		}
		// Forward broadcast resize instructions to the WS client,
		// matching the gRPC handleStreamResize pattern.
		for _, instruction := range decision.Broadcast {
			msg := wsEventMessage{
				Type:      "event",
				EventType: "resize_instruction",
				SessionID: sessionID,
				Data: map[string]string{
					"cols":   fmt.Sprintf("%d", instruction.Size.Cols),
					"rows":   fmt.Sprintf("%d", instruction.Size.Rows),
					"reason": instruction.Reason,
				},
			}
			data, mErr := json.Marshal(msg)
			if mErr != nil {
				log.Printf("[WS Session] resize broadcast marshal error for session %s: %v", sessionID, mErr)
				continue
			}
			if wErr := conn.Write(ctx, websocket.MessageText, data); wErr != nil {
				if !isExpectedWSClose(wErr) {
					log.Printf("[WS Session] resize broadcast write error for session %s: %v", sessionID, wErr)
				}
				return
			}
		}
		return
	}
	// Fallback: no resize manager available, apply directly.
	if rErr := sm.ResizeSession(sessionID, rows, cols); rErr != nil {
		log.Printf("[WS Session] resize session %s: %v", sessionID, rErr)
	}
}

// isExpectedWSClose returns true for errors that occur during normal
// WebSocket disconnection (client closed, server shutdown, etc.).
func isExpectedWSClose(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, net.ErrClosed) {
		return true
	}
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
}
