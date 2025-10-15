package termresize

import "time"

// HostLockMode enforces host-originated sizes for the PTY and broadcasts to clients.
type HostLockMode struct{}

// NewHostLockMode creates the HostLock strategy instance.
func NewHostLockMode() Mode {
	return &HostLockMode{}
}

func (m *HostLockMode) Name() string {
	return "host_lock"
}

func (m *HostLockMode) Handle(ctx *Context) (ResizeDecision, error) {
	req := ctx.Request
	state := ctx.State

	decision := ResizeDecision{
		SessionID: req.SessionID,
	}

	switch req.Source {
	case SourceHost:
		// Host is authoritative: update PTY size and notify everyone.
		size := req.Size
		decision.PTYSize = &ViewportSize{Cols: size.Cols, Rows: size.Rows}

		// Promote host to active controller to inform clients who drives resizes.
		decision.State.ActiveController = &ControllerRef{
			Source:    SourceHost,
			ClientID:  req.ClientID,
			Reason:    "host resize",
			UpdatedAt: time.Now(),
		}

		// Broadcast resize instruction to all clients (empty TargetIDs => broadcast).
		decision.Broadcast = []BroadcastInstruction{{
			TargetIDs: nil,
			Size:      size,
			Reason:    "host_lock",
		}}

	case SourceClient:
		// Clients are informative only: store viewport snapshot but do not change PTY.
		if state != nil {
			if snap, ok := state.Clients[req.ClientID]; ok {
				if snap.Metadata == nil {
					snap.Metadata = map[string]any{}
				}
			}
		}

		decision.Notes = append(decision.Notes, "host_lock: client resize ignored")
	}

	return decision, nil
}
