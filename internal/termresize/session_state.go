package termresize

import "time"

// SessionState keeps bookkeeping for a single PTY session.
type SessionState struct {
	ID               string
	Host             *ViewportSnapshot
	Clients          map[string]*ViewportSnapshot
	ActiveController *ControllerRef
	Metadata         map[string]any
	Connected        map[string]*ClientRegistration
}

// ViewportSnapshot tracks the last known viewport for a participant.
type ViewportSnapshot struct {
	Size      ViewportSize
	Source    Source
	ClientID  string
	UpdatedAt time.Time
	Metadata  map[string]any
}

// ClientRegistration keeps metadata about an attached client.
type ClientRegistration struct {
	ClientID     string
	Metadata     map[string]any
	RegisteredAt time.Time
	UpdatedAt    time.Time
}

func newSessionState(id string) *SessionState {
	return &SessionState{
		ID:        id,
		Clients:   make(map[string]*ViewportSnapshot),
		Metadata:  map[string]any{},
		Connected: make(map[string]*ClientRegistration),
	}
}

// Clone produces a deep copy that modes can safely inspect without mutating shared state.
func (s *SessionState) Clone() *SessionState {
	if s == nil {
		return nil
	}

	clone := &SessionState{
		ID:       s.ID,
		Metadata: make(map[string]any, len(s.Metadata)),
	}

	if s.Host != nil {
		hostCopy := *s.Host
		if hostCopy.Metadata != nil {
			hostCopy.Metadata = copyMap(hostCopy.Metadata)
		}
		clone.Host = &hostCopy
	}

	if len(s.Clients) > 0 {
		clone.Clients = make(map[string]*ViewportSnapshot, len(s.Clients))
		for id, snapshot := range s.Clients {
			if snapshot == nil {
				continue
			}
			copySnap := *snapshot
			if copySnap.Metadata != nil {
				copySnap.Metadata = copyMap(copySnap.Metadata)
			}
			clone.Clients[id] = &copySnap
		}
	} else {
		clone.Clients = make(map[string]*ViewportSnapshot)
	}

	if s.ActiveController != nil {
		ctrlCopy := *s.ActiveController
		clone.ActiveController = &ctrlCopy
	}

	for k, v := range s.Metadata {
		clone.Metadata[k] = v
	}

	if len(s.Connected) > 0 {
		clone.Connected = make(map[string]*ClientRegistration, len(s.Connected))
		for id, info := range s.Connected {
			if info == nil {
				continue
			}
			copyInfo := *info
			if copyInfo.Metadata != nil {
				copyInfo.Metadata = copyMap(copyInfo.Metadata)
			}
			clone.Connected[id] = &copyInfo
		}
	} else {
		clone.Connected = make(map[string]*ClientRegistration)
	}

	return clone
}

// recordRequest updates the raw session state with the latest viewport information.
func (s *SessionState) recordRequest(req ResizeRequest) {
	snapshot := &ViewportSnapshot{
		Size:      req.Size,
		Source:    req.Source,
		ClientID:  req.ClientID,
		UpdatedAt: req.SentAt,
		Metadata:  copyMap(req.Metadata),
	}

	switch req.Source {
	case SourceHost:
		s.Host = snapshot
	case SourceClient:
		if s.Clients == nil {
			s.Clients = make(map[string]*ViewportSnapshot)
		}
		s.Clients[req.ClientID] = snapshot
	}
}

func (s *SessionState) applyStateUpdate(update StateUpdate) {
	if update.ActiveController != nil {
		ctrlCopy := *update.ActiveController
		s.ActiveController = &ctrlCopy
	}
	if len(update.Metadata) > 0 {
		if s.Metadata == nil {
			s.Metadata = make(map[string]any)
		}
		for k, v := range update.Metadata {
			s.Metadata[k] = v
		}
	}
}

func copyMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func (s *SessionState) registerClient(clientID string, metadata map[string]any, registeredAt time.Time) {
	if s.Connected == nil {
		s.Connected = make(map[string]*ClientRegistration)
	}
	info, exists := s.Connected[clientID]
	if !exists {
		info = &ClientRegistration{ClientID: clientID}
		s.Connected[clientID] = info
	}
	info.RegisteredAt = registeredAt
	info.UpdatedAt = registeredAt
	info.Metadata = copyMap(metadata)
}

func (s *SessionState) unregisterClient(clientID string) {
	if s.Connected == nil {
		return
	}
	delete(s.Connected, clientID)
}
