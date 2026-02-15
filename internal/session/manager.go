package session

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nupi-ai/nupi/internal/eventbus"
	toolhandlers "github.com/nupi-ai/nupi/internal/plugins/tool_handlers"
	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/recording"
)

// Status represents session status
type Status string

const (
	StatusRunning  Status = "running"
	StatusStopped  Status = "stopped"
	StatusDetached Status = "detached"
)

// EventNotifier is an interface for notifying about session events
type EventNotifier interface {
	NotifyEvent(eventType string, exitCode int)
}

// Session represents a single PTY session
type Session struct {
	ID               string
	Command          string
	Args             []string
	WorkDir          string // Working directory
	StartTime        time.Time
	Status           Status
	PTY              *pty.Wrapper
	Handler          *toolhandlers.ToolHandler // Tool handler
	Inspect          bool                        // Whether inspection mode is enabled
	RecordingEnabled bool                        // Whether asciicast recording is enabled

	clientSinks int // Number of attached client sinks (excludes system sinks)
	mu          sync.RWMutex
	notifiers   []EventNotifier
	outputSeq   uint64
}

// SetStatus updates the session status in a threadsafe way
func (s *Session) SetStatus(status Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = status
}

// AddNotifier adds an event notifier to the session
func (s *Session) AddNotifier(notifier EventNotifier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifiers = append(s.notifiers, notifier)
}

// NotifyAll notifies all attached notifiers about an event
func (s *Session) NotifyAll(eventType string, exitCode int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, n := range s.notifiers {
		n.NotifyEvent(eventType, exitCode)
	}
}

// CurrentStatus returns the session status
func (s *Session) CurrentStatus() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Status
}

// GetDetectedTool returns the detected tool name, if any
func (s *Session) GetDetectedTool() string {
	if s.Handler == nil {
		return ""
	}
	return s.Handler.GetDetectedTool()
}

// GetDetectedIcon returns the detected tool's icon filename, if any
func (s *Session) GetDetectedIcon() string {
	if s.Handler == nil {
		return ""
	}
	return s.Handler.GetDetectedIcon()
}

func (s *Session) nextOutputSequence() uint64 {
	return atomic.AddUint64(&s.outputSeq, 1)
}

// SessionEventListener is called when session events occur
type SessionEventListener func(event string, session *Session)

// Manager manages multiple PTY sessions
// Implements multi-session support missing in tty-share
type Manager struct {
	sessions       map[string]*Session
	mu             sync.RWMutex
	listeners      []SessionEventListener
	pluginDir      string           // Directory for tool detection plugins
	recordingStore *recording.Store // Recording metadata store
	eventBus       *eventbus.Bus
	jsRuntimeFunc  toolhandlers.JSRuntimeFunc // Function to get JS runtime for tool detection
}

// NewManager creates a new session manager
func NewManager() *Manager {
	// Default plugin directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Printf("[Manager] Failed to determine home directory: %v", err)
		homeDir = ""
	}
	pluginDir := filepath.Join(homeDir, ".nupi", "plugins")

	// Initialize recording store
	recordingStore, err := recording.NewStore()
	if err != nil {
		log.Printf("[Manager] Failed to initialize recording store: %v", err)
	}

	return &Manager{
		sessions:       make(map[string]*Session),
		listeners:      make([]SessionEventListener, 0),
		pluginDir:      pluginDir,
		recordingStore: recordingStore,
	}
}

// SetPluginDir sets the plugin directory for tool detection
func (m *Manager) SetPluginDir(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pluginDir = dir
}

// UseEventBus wires the manager with the shared event bus.
func (m *Manager) UseEventBus(bus *eventbus.Bus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.eventBus = bus
}

// SetJSRuntimeFunc sets the function to get the JS runtime for tool handling.
// This allows handlers to always get the current runtime, even after restarts.
func (m *Manager) SetJSRuntimeFunc(fn toolhandlers.JSRuntimeFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jsRuntimeFunc = fn
}

// AddEventListener adds a listener for session events
func (m *Manager) AddEventListener(listener SessionEventListener) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listeners = append(m.listeners, listener)
}

// notifyListeners notifies all listeners about an event
func (m *Manager) notifyListeners(event string, session *Session) {
	// Call listeners without holding the write lock to avoid deadlock
	m.mu.RLock()
	listeners := append([]SessionEventListener(nil), m.listeners...)
	m.mu.RUnlock()

	for _, listener := range listeners {
		listener(event, session)
	}
}

func (m *Manager) getBus() *eventbus.Bus {
	m.mu.RLock()
	bus := m.eventBus
	m.mu.RUnlock()
	return bus
}

func (m *Manager) publishLifecycle(session *Session, state eventbus.SessionState, exitCode *int, reason string) {
	eventbus.Publish(context.Background(), m.getBus(), eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: session.ID,
		State:     state,
		ExitCode:  exitCode,
		Reason:    reason,
	})
}

// CreateSession creates a new PTY session
func (m *Manager) CreateSession(opts pty.StartOptions, inspect bool) (*Session, error) {
	// Generate unique session ID
	sessionID := uuid.New().String()[:8] // Short ID for convenience

	// Create PTY wrapper
	ptyWrapper := pty.New()

	// Start the PTY
	if err := ptyWrapper.Start(opts); err != nil {
		return nil, fmt.Errorf("failed to start PTY: %w", err)
	}

	// Create session
	session := &Session{
		ID:               sessionID,
		Command:          opts.Command,
		Args:             opts.Args,
		WorkDir:          opts.WorkingDir,
		StartTime:        time.Now(),
		Status:           StatusRunning,
		PTY:              ptyWrapper,
		Inspect:          inspect,
		RecordingEnabled: true, // Enable recording by default
	}

	// Set up asciicast recording if enabled
	if session.RecordingEnabled {
		if err := m.setupRecording(session); err != nil {
			log.Printf("[Manager] Failed to setup recording: %v", err)
			// Continue without recording
			session.RecordingEnabled = false
		}
	}

	// Set up inspection if enabled
	if inspect {
		if err := m.setupInspection(session); err != nil {
			log.Printf("[Manager] Failed to setup inspection: %v", err)
			// Continue without inspection
		}
	}

	m.setupToolHandler(session, opts)
	m.setupEventBusSink(session)

	// Monitor session status
	go m.monitorSession(session)

	// Store session
	m.mu.Lock()
	m.sessions[sessionID] = session
	m.mu.Unlock()

	// Notify listeners about new session
	m.notifyListeners("session_created", session)

	m.publishLifecycle(session, eventbus.SessionStateCreated, nil, "session_created")
	m.publishLifecycle(session, eventbus.SessionStateRunning, nil, "session_started")

	return session, nil
}

// setupToolHandler initialises the tool-handler plugin for a session, starts
// detection monitoring, and wires a handler output sink into the PTY.
func (m *Manager) setupToolHandler(session *Session, opts pty.StartOptions) {
	m.mu.RLock()
	pluginDir := m.pluginDir
	runtimeFunc := m.jsRuntimeFunc
	m.mu.RUnlock()

	if pluginDir == "" {
		return
	}
	handlerOpts := []toolhandlers.HandlerOption{toolhandlers.WithContinuousMode(true)}
	if runtimeFunc != nil {
		handlerOpts = append(handlerOpts, toolhandlers.WithJSRuntimeFunc(runtimeFunc))
	}
	toolHandler := toolhandlers.NewToolHandler(session.ID, pluginDir, handlerOpts...)
	if err := toolHandler.Initialize(); err != nil {
		log.Printf("[Manager] Failed to initialize handler: %v", err)
		return
	}
	session.Handler = toolHandler

	if err := toolHandler.OnSessionStart(opts.Command, opts.Args); err != nil {
		log.Printf("[Manager] Failed to start detection: %v", err)
	}

	go m.monitorDetection(session, toolHandler)

	session.PTY.AddSink(&handlerSink{
		handler: toolHandler,
	})
}

// setupEventBusSink adds an event-bus output sink to the session PTY so that
// session output is published on the bus.
func (m *Manager) setupEventBusSink(session *Session) {
	m.mu.RLock()
	bus := m.eventBus
	m.mu.RUnlock()
	if bus != nil {
		session.PTY.AddSink(&eventBusSink{
			bus:     bus,
			session: session,
		})
	}
}

// monitorSession monitors PTY events and updates session status
func (m *Manager) monitorSession(session *Session) {
	events := session.PTY.Events()
	for event := range events {
		switch event.Type {
		case "process_exited":
			session.SetStatus(StatusStopped)
			// Stop handler to clean up goroutines
			if session.Handler != nil {
				session.Handler.StopDetection()
			}
			// Notify all attached notifiers about the event
			session.NotifyAll(event.Type, event.ExitCode)
			exitCode := event.ExitCode
			var exitPtr *int
			if exitCode >= 0 {
				exitPtr = &exitCode
			}
			m.publishLifecycle(session, eventbus.SessionStateStopped, exitPtr, "process_exit")
			// Notify listeners about status change
			m.notifyListeners("session_status_changed", session)
		}
	}
}

// GetSession returns a session by ID
func (m *Manager) GetSession(id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	session, exists := m.sessions[id]
	if !exists {
		return nil, fmt.Errorf("session %s not found", id)
	}

	return session, nil
}

// ListSessions returns all sessions
func (m *Manager) ListSessions() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sessions := make([]*Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}

	return sessions
}

// KillSession stops a session
func (m *Manager) KillSession(id string) error {
	session, err := m.GetSession(id)
	if err != nil {
		return err
	}

	// Get exit code before stopping (will be 0 for manual kill)
	exitCode, _ := session.PTY.GetExitCode()
	if exitCode == -1 {
		exitCode = 0 // Manual kill = exit code 0
	}

	// Stop PTY with 5 second timeout
	if err := session.PTY.Stop(5 * time.Second); err != nil {
		return fmt.Errorf("failed to stop session: %w", err)
	}

	session.SetStatus(StatusStopped)

	// Notify all attached notifiers (including recording notifier) about process exit
	// This ensures metadata is saved even when session is manually killed
	session.NotifyAll("process_exited", exitCode)

	// Notify listeners about killed session
	m.notifyListeners("session_killed", session)

	// Notify about status change so UI updates
	m.notifyListeners("session_status_changed", session)

	var exitPtr *int
	if exitCode >= 0 {
		exitPtr = &exitCode
	}
	m.publishLifecycle(session, eventbus.SessionStateStopped, exitPtr, "session_killed")

	return nil
}

// AttachToSession attaches an output sink to a session
// Implements the generic OutputSink pattern from analysis
func (m *Manager) AttachToSession(id string, sink pty.OutputSink, includeHistory bool) error {
	session, err := m.GetSession(id)
	if err != nil {
		return err
	}

	// Send buffered history if requested BEFORE adding sink
	// This prevents duplicate data between history and new stream
	if includeHistory {
		history := session.PTY.GetBuffer()
		if len(history) > 0 {
			if err := sink.Write(history); err != nil {
				return fmt.Errorf("failed to send history: %w", err)
			}
		}
	}

	// Add sink for future output AFTER sending history
	// This ensures sink only gets new data after the history snapshot
	session.PTY.AddSink(sink)

	// Increment client sink counter
	session.mu.Lock()
	session.clientSinks++
	clientCount := session.clientSinks
	session.mu.Unlock()

	log.Printf("[Manager] Client attached to session %s, client sinks: %d", id, clientCount)

	// Update status to running if process is still running and status changed
	if session.PTY.IsRunning() {
		oldStatus := session.CurrentStatus()
		if oldStatus != StatusRunning {
			session.SetStatus(StatusRunning)
			m.notifyListeners("session_status_changed", session)
			log.Printf("[Manager] Session %s status changed: %s → %s", id, oldStatus, StatusRunning)
			m.publishLifecycle(session, eventbus.SessionStateRunning, nil, "client_attached")
		}
	}

	return nil
}

// DetachFromSession removes an output sink from a session
func (m *Manager) DetachFromSession(id string, sink pty.OutputSink) error {
	session, err := m.GetSession(id)
	if err != nil {
		return err
	}

	session.PTY.RemoveSink(sink)

	// Decrement client sink counter
	session.mu.Lock()
	if session.clientSinks > 0 {
		session.clientSinks--
	}
	clientCount := session.clientSinks
	session.mu.Unlock()

	log.Printf("[Manager] Client detached from session %s, remaining client sinks: %d", id, clientCount)

	// Only change to detached if NO clients remain and process is still running
	if session.PTY.IsRunning() && clientCount == 0 {
		oldStatus := session.CurrentStatus()
		if oldStatus != StatusDetached {
			session.SetStatus(StatusDetached)
			m.notifyListeners("session_status_changed", session)
			log.Printf("[Manager] Session %s status changed: %s → %s", id, oldStatus, StatusDetached)
			m.publishLifecycle(session, eventbus.SessionStateDetached, nil, "client_detached")
		}
	}

	return nil
}

// WriteToSession sends input to a session
func (m *Manager) WriteToSession(id string, data []byte) error {
	session, err := m.GetSession(id)
	if err != nil {
		return err
	}

	_, err = session.PTY.Write(data)
	return err
}

// ResizeSession updates the PTY window size for the given session.
func (m *Manager) ResizeSession(id string, rows, cols int) error {
	session, err := m.GetSession(id)
	if err != nil {
		return err
	}

	if session.PTY == nil {
		return fmt.Errorf("session %s has no PTY", id)
	}

	return session.PTY.SetWinSize(rows, cols)
}

// GetSessionOutput returns the buffered output for a session
func (m *Manager) GetSessionOutput(id string) ([]byte, error) {
	session, err := m.GetSession(id)
	if err != nil {
		return nil, err
	}

	return session.PTY.GetBuffer(), nil
}

// GetRecordingStore returns the recording metadata store
func (m *Manager) GetRecordingStore() *recording.Store {
	return m.recordingStore
}

// CleanupStopped removes stopped sessions older than the given duration
func (m *Manager) CleanupStopped(olderThan time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-olderThan)
	removed := 0

	for id, session := range m.sessions {
		session.mu.RLock()
		status := session.Status
		startTime := session.StartTime
		session.mu.RUnlock()

		if status == StatusStopped && startTime.Before(cutoff) {
			delete(m.sessions, id)
			removed++
		}
	}

	return removed
}

// handlerSink implements pty.OutputSink for tool handling
type handlerSink struct {
	handler *toolhandlers.ToolHandler
}

func (h *handlerSink) Write(data []byte) error {
	h.handler.OnOutput(data)
	return nil
}

type eventBusSink struct {
	bus     *eventbus.Bus
	session *Session
}

func (s *eventBusSink) Write(data []byte) error {
	if s.bus == nil || len(data) == 0 || s.session == nil {
		return nil
	}

	payload := eventbus.SessionOutputEvent{
		SessionID: s.session.ID,
		Sequence:  s.session.nextOutputSequence(),
		Data:      append([]byte(nil), data...),
		Origin:    eventbus.OriginTool,
		Mode:      string(s.session.CurrentStatus()),
	}

	eventbus.Publish(context.Background(), s.bus, eventbus.Sessions.Output, eventbus.SourceSessionManager, payload)

	return nil
}

func (s *eventBusSink) NotifyEvent(eventType string, exitCode int) {
	// No-op for lifecycle - handled separately.
}

// monitorDetection monitors tool detection events (initial and changes)
func (m *Manager) monitorDetection(session *Session, toolHandler *toolhandlers.ToolHandler) {
	eventChan := toolHandler.EventChannel()
	changeChan := toolHandler.ChangeChannel()

	// Wait for initial tool detection
	select {
	case event, ok := <-eventChan:
		if !ok {
			// Channel closed, detection ended without finding tool
			log.Printf("[Manager] Detection ended for session %s (channel closed)", session.ID)
			return
		}

		log.Printf("[Manager] Tool detected for session %s: %s", session.ID, event.Tool)

		eventbus.Publish(context.Background(), m.getBus(), eventbus.Sessions.Tool, eventbus.SourcePluginService, eventbus.SessionToolEvent{
			SessionID: session.ID,
			ToolName:  event.Tool,
			ToolID:    event.Tool,
			IconPath:  session.GetDetectedIcon(),
		})

		// Notify listeners about tool detection
		m.notifyListeners("tool_detected", session)
	}

	// Continue monitoring for tool changes in continuous mode
	for {
		select {
		case changeEvent, ok := <-changeChan:
			if !ok {
				// Channel closed, stop monitoring
				log.Printf("[Manager] Tool change monitoring ended for session %s", session.ID)
				return
			}

			log.Printf("[Manager] Tool changed for session %s: %s -> %s",
				session.ID, changeEvent.PreviousTool, changeEvent.NewTool)

			// Publish tool change event
			eventbus.Publish(context.Background(), m.getBus(), eventbus.Sessions.ToolChanged, eventbus.SourcePluginService, eventbus.SessionToolChangedEvent{
				SessionID:    session.ID,
				PreviousTool: changeEvent.PreviousTool,
				NewTool:      changeEvent.NewTool,
				Timestamp:    changeEvent.Timestamp,
			})

			// Also publish updated tool event for current state
			eventbus.Publish(context.Background(), m.getBus(), eventbus.Sessions.Tool, eventbus.SourcePluginService, eventbus.SessionToolEvent{
				SessionID: session.ID,
				ToolName:  changeEvent.NewTool,
				ToolID:    changeEvent.NewTool,
				IconPath:  session.GetDetectedIcon(),
			})

			// Notify listeners about tool change
			m.notifyListeners("tool_changed", session)
		}
	}
}

// setupInspection sets up inspection logging for a session
func (m *Manager) setupInspection(session *Session) error {
	// Create inspection directory
	homeDir, _ := os.UserHomeDir()
	inspectDir := filepath.Join(homeDir, ".nupi", "inspect")
	if err := os.MkdirAll(inspectDir, 0755); err != nil {
		return fmt.Errorf("failed to create inspect directory: %w", err)
	}

	// Create inspection file
	inspectPath := filepath.Join(inspectDir, session.ID+".raw")
	file, err := os.Create(inspectPath)
	if err != nil {
		return fmt.Errorf("failed to create inspect file: %w", err)
	}

	// Write header
	header := fmt.Sprintf("=== NUPI INSPECT SESSION ===\n")
	header += fmt.Sprintf("Timestamp: %s\n", session.StartTime.Format(time.RFC3339))
	header += fmt.Sprintf("Session ID: %s\n", session.ID)
	header += fmt.Sprintf("Command: %s\n", session.Command)
	if len(session.Args) > 0 {
		header += fmt.Sprintf("Arguments: %v\n", session.Args)
	}
	header += fmt.Sprintf("Working Dir: %s\n", session.WorkDir)
	header += fmt.Sprintf("=== RAW OUTPUT START ===\n")

	if _, err := file.WriteString(header); err != nil {
		file.Close()
		return fmt.Errorf("failed to write header: %w", err)
	}

	// Add inspection sink to PTY and register as notifier for lifecycle cleanup
	sink := &inspectionSink{file: file}
	session.PTY.AddSink(sink)
	session.AddNotifier(sink)

	log.Printf("[Manager] Inspection enabled for session %s: %s", session.ID, inspectPath)
	return nil
}

// inspectionSink implements pty.OutputSink and EventNotifier for raw output logging.
type inspectionSink struct {
	file   *os.File
	mu     sync.Mutex
	closed bool
}

func (s *inspectionSink) Write(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	_, err := s.file.Write(data)
	return err
}

func (s *inspectionSink) NotifyEvent(eventType string, exitCode int) {
	if eventType == "process_exited" {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return
		}
		s.closed = true
		footer := fmt.Sprintf("\n=== PROCESS EXITED (code: %d) ===\n", exitCode)
		s.file.WriteString(footer)
		s.file.Close()
	}
}

// setupRecording sets up asciicast recording for a session
func (m *Manager) setupRecording(session *Session) error {
	// Create recordings directory
	homeDir, _ := os.UserHomeDir()
	recordingsDir := filepath.Join(homeDir, ".nupi", "recordings")
	if err := os.MkdirAll(recordingsDir, 0755); err != nil {
		return fmt.Errorf("failed to create recordings directory: %w", err)
	}

	// Create recording file with timestamp and session ID
	timestamp := session.StartTime.Format("20060102-150405")
	filename := fmt.Sprintf("%s-%s.cast", timestamp, session.ID)
	recordingPath := filepath.Join(recordingsDir, filename)

	// Generate title from command
	title := session.Command
	if len(session.Args) > 0 {
		title = fmt.Sprintf("%s %s", session.Command, session.Args[0])
	}

	// Create recorder with terminal dimensions (use defaults if not set)
	rows, cols := 24, 80
	if pr, pc := session.PTY.GetWinSize(); pr > 0 && pc > 0 {
		rows, cols = pr, pc
	}
	recorder, err := pty.NewAsciicastRecorder(recordingPath, rows, cols, title)
	if err != nil {
		return fmt.Errorf("failed to create recorder: %w", err)
	}

	// Add recorder as sink to PTY
	session.PTY.AddSink(recorder)

	// Add cleanup handler for process exit
	session.AddNotifier(&recordingNotifier{
		recorder:       recorder,
		session:        session,
		recordingPath:  recordingPath,
		recordingStore: m.recordingStore,
	})

	log.Printf("[Manager] Recording enabled for session %s: %s", session.ID, recordingPath)
	return nil
}

// recordingNotifier handles recording lifecycle events
type recordingNotifier struct {
	recorder       *pty.AsciicastRecorder
	session        *Session
	recordingPath  string
	recordingStore *recording.Store
}

func (r *recordingNotifier) NotifyEvent(eventType string, exitCode int) {
	log.Printf("[RecordingNotifier] NotifyEvent called: type=%s, exitCode=%d, sessionID=%s", eventType, exitCode, r.session.ID)

	if eventType == "process_exited" {
		log.Printf("[RecordingNotifier] Process exited for session %s, closing recorder", r.session.ID)

		if err := r.recorder.Close(); err != nil {
			log.Printf("[Recording] Failed to close recorder: %v", err)
		} else {
			duration := r.recorder.GetDuration()
			log.Printf("[Recording] Recording closed successfully, duration: %v", duration)

			// Save metadata
			if r.recordingStore == nil {
				log.Printf("[Recording] WARNING: recordingStore is nil for session %s!", r.session.ID)
				return
			}

			log.Printf("[Recording] Preparing metadata for session %s", r.session.ID)
			rows, cols := 24, 80
			if r.session.PTY != nil {
				if pr, pc := r.session.PTY.GetWinSize(); pr > 0 && pc > 0 {
					rows, cols = pr, pc
				}
			}
			metadata := recording.Metadata{
				SessionID:     r.session.ID,
				Filename:      filepath.Base(r.recordingPath),
				Command:       r.session.Command,
				Args:          r.session.Args,
				WorkDir:       r.session.WorkDir,
				StartTime:     r.session.StartTime,
				Duration:      duration.Seconds(),
				Rows:          rows,
				Cols:          cols,
				Title:         r.session.Command,
				Tool:          r.session.GetDetectedTool(),
				RecordingPath: r.recordingPath,
			}

			log.Printf("[Recording] Saving metadata for session %s: %+v", r.session.ID, metadata)
			if err := r.recordingStore.SaveMetadata(metadata); err != nil {
				log.Printf("[Recording] Failed to save metadata: %v", err)
			} else {
				log.Printf("[Recording] Metadata saved successfully for session %s", r.session.ID)
			}
		}
	}
}
