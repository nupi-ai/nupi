package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/pty"
)

func TestWSHandshakeMissingSessionIDReturnsHTTPErrorAndNilConn(t *testing.T) {
	apiServer, _ := newGatewayTestAPIServer(t)

	req := httptest.NewRequest(http.MethodGet, "http://example/ws/session/", nil)
	rec := httptest.NewRecorder()

	sessionID, conn, ctx, cancel, err := wsHandshake(apiServer, context.Background(), "/ws/session/", rec, req)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if sessionID != "" {
		t.Fatalf("expected empty session id, got %q", sessionID)
	}
	if conn != nil || ctx != nil || cancel != nil {
		t.Fatalf("expected nil conn/ctx/cancel on HTTP error, got conn=%v ctx=%v cancel=%v", conn, ctx, cancel)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", rec.Code)
	}
}

func TestWSHandshakeSessionNotFoundReturnsHTTPErrorAndNilConn(t *testing.T) {
	apiServer, _ := newGatewayTestAPIServer(t)

	req := httptest.NewRequest(http.MethodGet, "http://example/ws/session/does-not-exist", nil)
	rec := httptest.NewRecorder()

	sessionID, conn, ctx, cancel, err := wsHandshake(apiServer, context.Background(), "/ws/session/", rec, req)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if sessionID != "does-not-exist" {
		t.Fatalf("expected session id to be extracted, got %q", sessionID)
	}
	if conn != nil || ctx != nil || cancel != nil {
		t.Fatalf("expected nil conn/ctx/cancel on HTTP error, got conn=%v ctx=%v cancel=%v", conn, ctx, cancel)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", rec.Code)
	}
}

func TestWSHandshakeAuthRequiredWithoutTokenReturnsUnauthorizedAndNilConn(t *testing.T) {
	apiServer, store := newGatewayTestAPIServer(t)

	certPath, keyPath := generateTestCert(t, t.TempDir())
	if err := store.SaveTransportConfig(context.Background(), configTransportConfigWithTLS(certPath, keyPath)); err != nil {
		t.Fatalf("failed to save transport config: %v", err)
	}
	if _, err := apiServer.Prepare(context.Background()); err != nil {
		t.Fatalf("failed to prepare api server transport: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example/ws/session/sess-123", nil)
	rec := httptest.NewRecorder()

	sessionID, conn, ctx, cancel, err := wsHandshake(apiServer, context.Background(), "/ws/session/", rec, req)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if sessionID != "sess-123" {
		t.Fatalf("expected session id to be extracted, got %q", sessionID)
	}
	if conn != nil || ctx != nil || cancel != nil {
		t.Fatalf("expected nil conn/ctx/cancel on HTTP error, got conn=%v ctx=%v cancel=%v", conn, ctx, cancel)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", rec.Code)
	}
}

func TestWSHandshakeSuccessReturnsConnAndContext(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _ := newGatewayTestAPIServer(t)
	sess, err := apiServer.SessionMgr().CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		Rows:    24,
		Cols:    80,
	}, false)
	if err != nil {
		t.Fatalf("failed to create test session: %v", err)
	}
	t.Cleanup(func() { _ = apiServer.SessionMgr().KillSession(sess.ID) })

	done := make(chan struct{})
	serverErr := make(chan error, 1)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionID, conn, ctx, cancel, hErr := wsHandshake(apiServer, context.Background(), "/ws/session/", w, r)
		if hErr != nil {
			serverErr <- hErr
			return
		}
		if conn == nil || ctx == nil || cancel == nil {
			serverErr <- errors.New("expected non-nil conn/ctx/cancel")
			return
		}
		if sessionID != sess.ID {
			serverErr <- errors.New("session id mismatch")
			conn.CloseNow()
			cancel()
			return
		}
		cancel()
		conn.Close(websocket.StatusNormalClosure, "ok")
		close(done)
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/session/" + sess.ID
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()

	conn, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("expected successful websocket dial, got %v", err)
	}
	conn.Close(websocket.StatusNormalClosure, "client done")

	select {
	case <-done:
	case err := <-serverErr:
		t.Fatalf("server assertion failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for handshake completion")
	}
}

func configTransportConfigWithTLS(certPath, keyPath string) configstore.TransportConfig {
	return configstore.TransportConfig{
		Binding:     "lan",
		TLSCertPath: certPath,
		TLSKeyPath:  keyPath,
	}
}
