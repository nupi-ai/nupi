package session

import (
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/pty"
)

// mockEventNotifier tracks events for testing
type mockEventNotifier struct {
	mu     sync.Mutex
	events []string
}

func (m *mockEventNotifier) NotifyEvent(eventType string, exitCode int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, eventType)
}

func (m *mockEventNotifier) getEvents() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.events...)
}

// mockEventListener tracks session events
type mockEventListener struct {
	mu     sync.Mutex
	events []struct {
		event   string
		session *Session
	}
}

func (m *mockEventListener) record(event string, session *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, struct {
		event   string
		session *Session
	}{event, session})
}

func (m *mockEventListener) getEventCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events)
}

func (m *mockEventListener) hasEvent(eventType string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		if e.event == eventType {
			return true
		}
	}
	return false
}

// TestEventListenerNotifications tests that event listeners are properly notified
func TestEventListenerNotifications(t *testing.T) {
	manager := NewManager()

	// Add multiple listeners
	listener1 := &mockEventListener{}
	listener2 := &mockEventListener{}

	manager.AddEventListener(func(event string, session *Session) {
		listener1.record(event, session)
	})

	manager.AddEventListener(func(event string, session *Session) {
		listener2.record(event, session)
	})

	// Create a session - should trigger session_created event
	opts := pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "echo test"},
		Rows:    24,
		Cols:    80,
	}

	sess, err := manager.CreateSession(opts, false)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Give listeners time to process
	time.Sleep(50 * time.Millisecond)

	// Both listeners should have received session_created event
	if !listener1.hasEvent("session_created") {
		t.Error("Listener 1 did not receive session_created event")
	}
	if !listener2.hasEvent("session_created") {
		t.Error("Listener 2 did not receive session_created event")
	}

	// Kill session - should trigger session_killed event
	err = manager.KillSession(sess.ID)
	if err != nil {
		t.Fatalf("Failed to kill session: %v", err)
	}

	// Give listeners time to process
	time.Sleep(50 * time.Millisecond)

	// Both listeners should have received session_killed event
	if !listener1.hasEvent("session_killed") {
		t.Error("Listener 1 did not receive session_killed event")
	}
	if !listener2.hasEvent("session_killed") {
		t.Error("Listener 2 did not receive session_killed event")
	}

	// Each listener should have received at least 2 events (created and killed)
	// There might be additional status change events
	if count := listener1.getEventCount(); count < 2 {
		t.Errorf("Listener 1: expected at least 2 events, got %d", count)
	}
	if count := listener2.getEventCount(); count < 2 {
		t.Errorf("Listener 2: expected at least 2 events, got %d", count)
	}
}

// TestProcessExitNotifications tests that process exit events are properly notified
func TestProcessExitNotifications(t *testing.T) {
	manager := NewManager()

	// Track process exit notifications
	notifier := &mockEventNotifier{}

	// Create a session that exits quickly
	opts := pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "exit 0"},
		Rows:    24,
		Cols:    80,
	}

	sess, err := manager.CreateSession(opts, false)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Add notifier to the session
	sess.AddNotifier(notifier)

	// Wait for process to exit
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("Process did not exit in time")
		}
		if sess.CurrentStatus() == StatusStopped {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Check that notifier received process_exited event
	events := notifier.getEvents()
	found := false
	for _, event := range events {
		if event == "process_exited" {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("Notifier did not receive process_exited event. Got events: %v", events)
	}
}

// TestStatusChangeNotifications tests that status changes trigger notifications
func TestStatusChangeNotifications(t *testing.T) {
	manager := NewManager()

	statusChanges := []string{}
	var mu sync.Mutex

	// Add listener for status changes
	manager.AddEventListener(func(event string, session *Session) {
		if event == "session_status_changed" {
			mu.Lock()
			statusChanges = append(statusChanges, string(session.CurrentStatus()))
			mu.Unlock()
		}
	})

	// Create a session
	opts := pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 0.1"},
		Rows:    24,
		Cols:    80,
	}

	sess, err := manager.CreateSession(opts, false)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Session should start as running
	if status := sess.CurrentStatus(); status != StatusRunning {
		t.Errorf("Expected initial status running, got %s", status)
	}

	// Create a sink to attach
	sink := &fakeSink{}

	// Detach to trigger status change
	sess.SetStatus(StatusDetached)

	// Attach to change status back
	err = manager.AttachToSession(sess.ID, sink, false)
	if err != nil {
		t.Fatalf("Failed to attach: %v", err)
	}

	// Wait for process to complete
	time.Sleep(200 * time.Millisecond)

	// Kill to ensure stopped status
	manager.KillSession(sess.ID)

	// Give time for events to process
	time.Sleep(100 * time.Millisecond)

	// Should have recorded multiple status changes
	mu.Lock()
	numChanges := len(statusChanges)
	mu.Unlock()

	if numChanges == 0 {
		t.Error("No status changes were recorded")
	}
}

// TestConcurrentEventListeners tests thread-safety with multiple concurrent listeners
func TestConcurrentEventListeners(t *testing.T) {
	manager := NewManager()

	// Track events from multiple concurrent listeners
	const numListeners = 10
	listeners := make([]*mockEventListener, numListeners)

	for i := 0; i < numListeners; i++ {
		listener := &mockEventListener{}
		listeners[i] = listener

		manager.AddEventListener(func(event string, session *Session) {
			listener.record(event, session)
			// Simulate some processing time
			time.Sleep(1 * time.Millisecond)
		})
	}

	// Create multiple sessions concurrently
	var wg sync.WaitGroup
	const numSessions = 5

	for i := 0; i < numSessions; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			opts := pty.StartOptions{
				Command: "/bin/sh",
				Args:    []string{"-c", "echo test"},
				Rows:    24,
				Cols:    80,
			}

			sess, err := manager.CreateSession(opts, false)
			if err != nil {
				t.Errorf("Failed to create session: %v", err)
				return
			}

			// Kill session immediately
			manager.KillSession(sess.ID)
		}()
	}

	wg.Wait()
	time.Sleep(100 * time.Millisecond) // Let events propagate

	// Each listener should have received events for all sessions
	for i, listener := range listeners {
		eventCount := listener.getEventCount()
		// Each session generates at least 2 events (created, killed)
		expectedMin := numSessions * 2
		if eventCount < expectedMin {
			t.Errorf("Listener %d: expected at least %d events, got %d",
				i, expectedMin, eventCount)
		}
	}
}

// TestEventNotifierInterface tests the EventNotifier interface implementation
func TestEventNotifierInterface(t *testing.T) {
	manager := NewManager()

	// Create a session with a command that exits with specific code
	opts := pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "exit 42"},
		Rows:    24,
		Cols:    80,
	}

	sess, err := manager.CreateSession(opts, false)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Create and add multiple notifiers
	notifier1 := &mockEventNotifier{}
	notifier2 := &mockEventNotifier{}

	sess.AddNotifier(notifier1)
	sess.AddNotifier(notifier2)

	// Wait for process to exit
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("Process did not exit in time")
		}
		if sess.CurrentStatus() == StatusStopped {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Both notifiers should have received the event
	events1 := notifier1.getEvents()
	events2 := notifier2.getEvents()

	if len(events1) == 0 {
		t.Error("Notifier 1 received no events")
	}
	if len(events2) == 0 {
		t.Error("Notifier 2 received no events")
	}

	// Check that process_exited was received
	hasExitEvent := func(events []string) bool {
		for _, e := range events {
			if e == "process_exited" {
				return true
			}
		}
		return false
	}

	if !hasExitEvent(events1) {
		t.Error("Notifier 1 did not receive process_exited event")
	}
	if !hasExitEvent(events2) {
		t.Error("Notifier 2 did not receive process_exited event")
	}
}
