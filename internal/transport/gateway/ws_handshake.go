package gateway

import (
	"context"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"github.com/nupi-ai/nupi/internal/server"
)

// wsHandshake executes common pre-upgrade checks for WebSocket handlers.
// It writes HTTP errors for validation/auth/session failures and returns
// conn == nil with err == nil in those cases.
func wsHandshake(
	apiServer *server.APIServer,
	shutdownCtx context.Context,
	pathPrefix string,
	w http.ResponseWriter,
	r *http.Request,
) (sessionID string, conn *websocket.Conn, ctx context.Context, cancel context.CancelFunc, err error) {
	sessionID = strings.TrimPrefix(r.URL.Path, pathPrefix)
	sessionID = strings.TrimSuffix(sessionID, "/")
	if sessionID == "" || strings.Contains(sessionID, "/") {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return "", nil, nil, nil, nil
	}

	if apiServer.AuthRequired() {
		token := r.URL.Query().Get("token")
		if token == "" {
			token = parseBearer(r.Header.Get("Authorization"))
		}
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return sessionID, nil, nil, nil, nil
		}
		if _, ok := apiServer.AuthenticateToken(token); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return sessionID, nil, nil, nil, nil
		}
	}

	if _, getErr := apiServer.SessionMgr().GetSession(sessionID); getErr != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return sessionID, nil, nil, nil, nil
	}

	conn, err = websocket.Accept(w, r, nil)
	if err != nil {
		return sessionID, nil, nil, nil, err
	}

	ctx, cancel = context.WithCancel(shutdownCtx)
	return sessionID, conn, ctx, cancel, nil
}
