package protocol

import "time"

// Request represents a client request to the daemon
type Request struct {
	ID   string      `json:"id"`
	Type string      `json:"type"`
	Data interface{} `json:"data,omitempty"`
}

// Response represents a daemon response to client
type Response struct {
	ID      string      `json:"id"`
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// StreamMessage represents a streaming output message
type StreamMessage struct {
	Type      string    `json:"type"` // "output" or "event"
	SessionID string    `json:"session_id"`
	Data      string    `json:"data,omitempty"`
	Event     string    `json:"event,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// CreateSessionData contains data for creating a session
type CreateSessionData struct {
	Command    string   `json:"command"`
	Args       []string `json:"args,omitempty"`
	WorkingDir string   `json:"workdir,omitempty"`
	Env        []string `json:"env,omitempty"`
	Rows       uint16   `json:"rows,omitempty"`
	Cols       uint16   `json:"cols,omitempty"`
	Detached   bool     `json:"detached,omitempty"`
	Inspect    bool     `json:"inspect,omitempty"`
}

// SessionInfo contains session information
type SessionInfo struct {
	ID        string    `json:"id"`
	Command   string    `json:"command"`
	Args      []string  `json:"args"`
	Status    string    `json:"status"`
	PID       int       `json:"pid"`
	CreatedAt time.Time `json:"created_at"`
}

// Request types
const (
	RequestCreateSession = "create_session"
	RequestListSessions  = "list_sessions"
	RequestAttachSession = "attach_session"
	RequestDetachSession = "detach_session"
	RequestKillSession   = "kill_session"
	RequestSendInput     = "send_input"
	RequestDaemonStatus  = "daemon_status"
	RequestResizeSession = "resize_session"
	RequestShutdown      = "shutdown"
)

// Event types
const (
	EventProcessStarted = "process_started"
	EventProcessExited  = "process_exited"
	EventError          = "error"
)

// Error codes
const (
	ErrorSessionNotFound  = 1001
	ErrorSessionExists    = 1002
	ErrorInvalidCommand   = 1003
	ErrorPermissionDenied = 1004
	ErrorDaemonNotRunning = 1005
	ErrorInvalidRequest   = 2001
	ErrorInternalError    = 5000
)
