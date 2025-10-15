package termresize

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Manager orchestrates resize modes across sessions.
type Manager struct {
	mu           sync.RWMutex
	sessions     map[string]*SessionState
	modes        map[string]Mode
	defaultMode  string
	sessionModes map[string]string
}

// NewManager constructs a Manager with the provided default mode and optional additional modes.
func NewManager(defaultMode Mode, others ...Mode) (*Manager, error) {
	m := &Manager{
		sessions:     make(map[string]*SessionState),
		modes:        make(map[string]Mode),
		sessionModes: make(map[string]string),
	}

	if defaultMode != nil {
		if err := m.RegisterMode(defaultMode); err != nil {
			return nil, err
		}
		m.defaultMode = defaultMode.Name()
	}

	for _, mode := range others {
		if err := m.RegisterMode(mode); err != nil {
			return nil, err
		}
	}

	return m, nil
}

// RegisterMode stores a mode in the manager; attempting to register duplicate names returns an error.
func (m *Manager) RegisterMode(mode Mode) error {
	if mode == nil {
		return fmt.Errorf("cannot register nil mode")
	}

	name := mode.Name()
	if name == "" {
		return fmt.Errorf("mode must expose a non-empty name")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.modes[name]; exists {
		return fmt.Errorf("mode %q already registered", name)
	}

	m.modes[name] = mode
	if m.defaultMode == "" {
		m.defaultMode = name
	}
	return nil
}

// AvailableModes returns the sorted list of registered mode names.
func (m *Manager) AvailableModes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.modes))
	for name := range m.modes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SetDefaultMode switches the fallback mode used for sessions without explicit assignment.
func (m *Manager) SetDefaultMode(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.modes[name]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownMode, name)
	}

	m.defaultMode = name
	return nil
}

// SetSessionMode binds a specific mode to a session.
func (m *Manager) SetSessionMode(sessionID, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.modes[name]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownMode, name)
	}

	m.sessionModes[sessionID] = name
	if _, exists := m.sessions[sessionID]; !exists {
		m.sessions[sessionID] = newSessionState(sessionID)
	}
	return nil
}

// GetSessionMode returns the mode bound to the session (or the default mode if none assigned).
func (m *Manager) GetSessionMode(sessionID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if name, ok := m.sessionModes[sessionID]; ok {
		return name
	}
	return m.defaultMode
}

// ForgetSession removes session-specific bookkeeping.
func (m *Manager) ForgetSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.sessions, sessionID)
	delete(m.sessionModes, sessionID)
}

// RegisterClient stores metadata about a connected client for the session.
func (m *Manager) RegisterClient(sessionID, clientID string, metadata map[string]any) {
	if clientID == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.ensureStateLocked(sessionID)
	state.registerClient(clientID, metadata, time.Now())
}

// UnregisterClient removes a client from the session state.
func (m *Manager) UnregisterClient(sessionID, clientID string) {
	if clientID == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if state, ok := m.sessions[sessionID]; ok {
		state.unregisterClient(clientID)
	}
}

// Snapshot returns a deep copy of the session state if it exists.
func (m *Manager) Snapshot(sessionID string) *SessionState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if state, ok := m.sessions[sessionID]; ok {
		return state.Clone()
	}
	return nil
}

// HandleHostResize is a helper for host-originated updates.
func (m *Manager) HandleHostResize(sessionID string, size ViewportSize, metadata map[string]any) (ResizeDecision, error) {
	req := ResizeRequest{
		SessionID: sessionID,
		Source:    SourceHost,
		Size:      size,
		Metadata:  metadata,
		SentAt:    time.Now(),
	}
	return m.handle(req)
}

// HandleClientResize is a helper for client-originated updates.
func (m *Manager) HandleClientResize(sessionID, clientID string, size ViewportSize, metadata map[string]any) (ResizeDecision, error) {
	req := ResizeRequest{
		SessionID: sessionID,
		Source:    SourceClient,
		ClientID:  clientID,
		Size:      size,
		Metadata:  metadata,
		SentAt:    time.Now(),
	}
	return m.handle(req)
}

// Handle accepts a pre-constructed request, normalises timestamps, and forwards to the active mode.
func (m *Manager) Handle(req ResizeRequest) (ResizeDecision, error) {
	return m.handle(req)
}

func (m *Manager) handle(req ResizeRequest) (ResizeDecision, error) {
	req = normalizeRequest(req)

	m.mu.Lock()
	state := m.ensureStateLocked(req.SessionID)
	state.recordRequest(req)

	mode := m.resolveModeLocked(req.SessionID)
	snapshot := state.Clone()
	m.mu.Unlock()

	if mode == nil {
		return ResizeDecision{}, fmt.Errorf("no active mode for session %s", req.SessionID)
	}

	decision, err := mode.Handle(&Context{
		Request: req,
		State:   snapshot,
	})
	if err != nil {
		return ResizeDecision{}, err
	}

	if decision.SessionID == "" {
		decision.SessionID = req.SessionID
	}

	m.mu.Lock()
	state = m.ensureStateLocked(req.SessionID)
	state.applyStateUpdate(decision.State)
	m.mu.Unlock()

	return decision, nil
}

func (m *Manager) ensureStateLocked(sessionID string) *SessionState {
	state, ok := m.sessions[sessionID]
	if !ok {
		state = newSessionState(sessionID)
		m.sessions[sessionID] = state
	}
	return state
}

func (m *Manager) resolveModeLocked(sessionID string) Mode {
	name := m.defaultMode
	if explicit, ok := m.sessionModes[sessionID]; ok {
		name = explicit
	}
	if name == "" {
		return nil
	}
	return m.modes[name]
}
