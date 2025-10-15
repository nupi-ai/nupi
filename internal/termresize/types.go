package termresize

import "time"

// Source identifies the originator of a resize request.
type Source string

const (
	// SourceHost represents updates coming from the host PTY / original terminal.
	SourceHost Source = "host"
	// SourceClient represents updates emitted by a remote client (browser, mobile, etc.).
	SourceClient Source = "client"
)

// ViewportSize stores row/column geometry reported by a participant.
type ViewportSize struct {
	Cols int
	Rows int
}

// ResizeRequest captures an incoming resize signal for a session.
type ResizeRequest struct {
	SessionID string
	Source    Source
	ClientID  string // optional: filled when SourceClient
	Size      ViewportSize
	SentAt    time.Time
	Metadata  map[string]any
}

// ControllerRef tracks which endpoint currently controls the authoritative size.
type ControllerRef struct {
	Source    Source
	ClientID  string
	Reason    string
	UpdatedAt time.Time
}

// BroadcastInstruction describes resize messages that need to be delivered to clients.
type BroadcastInstruction struct {
	TargetIDs []string // nil/empty means broadcast to all listeners
	Size      ViewportSize
	Reason    string
}

// StateUpdate contains high-level mutations produced by a mode.
type StateUpdate struct {
	ActiveController *ControllerRef
	Metadata         map[string]any
}

// ResizeDecision contains the outcome of processing a resize request within a mode.
type ResizeDecision struct {
	SessionID string
	PTYSize   *ViewportSize // nil means no change requested for PTY
	Broadcast []BroadcastInstruction
	Notes     []string
	State     StateUpdate
}

// Context bundles the immutable data a mode receives for evaluation.
type Context struct {
	Request ResizeRequest
	State   *SessionState // cloned snapshot, safe for inspection
}

// Helper constructor for timestamps when callers omit SentAt.
func normalizeRequest(req ResizeRequest) ResizeRequest {
	if req.SentAt.IsZero() {
		req.SentAt = time.Now()
	}
	return req
}
