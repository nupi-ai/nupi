package server

import (
	"strings"

	"github.com/nupi-ai/nupi/internal/session"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// validateAndGetSession trims and validates a required session id, then resolves the session.
func (s *APIServer) validateAndGetSession(sessionIDRaw string) (*session.Session, string, error) {
	sessionID := strings.TrimSpace(sessionIDRaw)
	if sessionID == "" {
		return nil, "", status.Error(codes.InvalidArgument, "session_id is required")
	}

	sess, err := s.sessionManager.GetSession(sessionID)
	if err != nil {
		return nil, "", status.Errorf(codes.NotFound, "session %s not found", sessionID)
	}
	return sess, sessionID, nil
}
