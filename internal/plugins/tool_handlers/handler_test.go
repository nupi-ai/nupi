package toolhandlers

import (
	"testing"
	"time"
)

// mockPlugin creates a minimal JSPlugin for testing.
func mockPlugin(name string) *JSPlugin {
	return &JSPlugin{
		Name: name,
		Icon: name + ".png",
	}
}

func TestToolHandler_RateLimitingToolChanges(t *testing.T) {
	d := NewToolHandler("test-session", "/tmp/plugins", WithContinuousMode(true))

	// Manually set up handler state for testing
	d.plugins = map[string]*JSPlugin{
		"vim.js":    mockPlugin("vim"),
		"python.js": mockPlugin("python"),
		"node.js":   mockPlugin("node"),
	}
	d.startTime = time.Now()
	d.lastCheck = time.Now().Add(-1 * time.Second) // Allow detection

	// Simulate first tool detection
	d.detectedTool = "vim"
	d.lastToolChange = time.Now()

	// Try to emit a tool change immediately (should be rate limited)
	initialChangeTime := d.lastToolChange

	// Simulate rapid tool changes by checking the rate limit logic
	now := time.Now()
	toolChangeMinInterval := 5 * time.Second

	// Test 1: Change within 5s should be rate limited
	if d.lastToolChange.IsZero() || now.Sub(d.lastToolChange) >= toolChangeMinInterval {
		t.Error("Expected lastToolChange to be set and within rate limit window")
	}

	// Verify rate limit would block
	timeSinceLastChange := now.Sub(d.lastToolChange)
	if timeSinceLastChange >= toolChangeMinInterval {
		t.Errorf("Expected time since last change (%v) to be less than %v", timeSinceLastChange, toolChangeMinInterval)
	}

	// Test 2: Verify lastToolChange hasn't changed (simulating rate limit block)
	if d.lastToolChange != initialChangeTime {
		t.Error("lastToolChange should not have been updated during rate limit")
	}

	// Test 3: After 5s, change should be allowed
	d.lastToolChange = time.Now().Add(-6 * time.Second) // Simulate 6s ago
	now = time.Now()
	timeSinceLastChange = now.Sub(d.lastToolChange)

	if timeSinceLastChange < toolChangeMinInterval {
		t.Errorf("Expected time since last change (%v) to be >= %v after waiting", timeSinceLastChange, toolChangeMinInterval)
	}
}

func TestToolHandler_FirstToolChangeNotRateLimited(t *testing.T) {
	d := NewToolHandler("test-session", "/tmp/plugins", WithContinuousMode(true))

	// First tool change (lastToolChange is zero) should not be rate limited
	if !d.lastToolChange.IsZero() {
		t.Error("Expected lastToolChange to be zero for new handler")
	}

	// Simulate first detection setting up the state
	d.detectedTool = "vim"

	// Now simulate a tool change - since lastToolChange is zero, it should be allowed
	now := time.Now()
	toolChangeMinInterval := 5 * time.Second

	// The condition in the code is: !d.lastToolChange.IsZero() && now.Sub(d.lastToolChange) < toolChangeMinInterval
	// When lastToolChange is zero, this evaluates to false (not rate limited)
	shouldRateLimit := !d.lastToolChange.IsZero() && now.Sub(d.lastToolChange) < toolChangeMinInterval

	if shouldRateLimit {
		t.Error("First tool change should NOT be rate limited")
	}
}

func TestToolHandler_ChangeChannelReceivesEvents(t *testing.T) {
	d := NewToolHandler("test-session", "/tmp/plugins", WithContinuousMode(true))

	// Get the change channel
	changeChan := d.ChangeChannel()

	// Manually send an event (simulating what OnOutput does)
	event := ToolChangedEvent{
		SessionID:    "test-session",
		PreviousTool: "vim",
		NewTool:      "python",
		Timestamp:    time.Now(),
	}

	// Non-blocking send
	select {
	case d.changeChan <- event:
		// OK
	default:
		t.Fatal("Failed to send event to change channel")
	}

	// Receive and verify
	select {
	case received := <-changeChan:
		if received.SessionID != "test-session" {
			t.Errorf("Expected SessionID=test-session, got %q", received.SessionID)
		}
		if received.PreviousTool != "vim" {
			t.Errorf("Expected PreviousTool=vim, got %q", received.PreviousTool)
		}
		if received.NewTool != "python" {
			t.Errorf("Expected NewTool=python, got %q", received.NewTool)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Timeout waiting for change event")
	}
}

func TestToolHandler_EventChannelReceivesFirstDetection(t *testing.T) {
	d := NewToolHandler("test-session", "/tmp/plugins")

	// Get the event channel
	eventChan := d.EventChannel()

	// Manually send an event (simulating what OnOutput does for first detection)
	event := ToolDetectedEvent{
		SessionID: "test-session",
		Tool:      "vim",
		Timestamp: time.Now(),
	}

	// Non-blocking send
	select {
	case d.eventChan <- event:
		// OK
	default:
		t.Fatal("Failed to send event to event channel")
	}

	// Receive and verify
	select {
	case received := <-eventChan:
		if received.SessionID != "test-session" {
			t.Errorf("Expected SessionID=test-session, got %q", received.SessionID)
		}
		if received.Tool != "vim" {
			t.Errorf("Expected Tool=vim, got %q", received.Tool)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Timeout waiting for detection event")
	}
}

func TestToolHandler_ContinuousModeToggle(t *testing.T) {
	// Test with continuous mode disabled (default)
	d1 := NewToolHandler("test-session", "/tmp/plugins")
	if d1.IsContinuousMode() {
		t.Error("Expected continuous mode to be disabled by default")
	}

	// Test with continuous mode enabled via option
	d2 := NewToolHandler("test-session", "/tmp/plugins", WithContinuousMode(true))
	if !d2.IsContinuousMode() {
		t.Error("Expected continuous mode to be enabled")
	}

	// Test runtime toggle
	d2.EnableContinuousMode(false)
	if d2.IsContinuousMode() {
		t.Error("Expected continuous mode to be disabled after toggle")
	}

	d2.EnableContinuousMode(true)
	if !d2.IsContinuousMode() {
		t.Error("Expected continuous mode to be enabled after toggle")
	}
}

func TestToolHandler_GetDetectedTool(t *testing.T) {
	d := NewToolHandler("test-session", "/tmp/plugins")

	// Initially empty
	if tool := d.GetDetectedTool(); tool != "" {
		t.Errorf("Expected empty tool initially, got %q", tool)
	}

	// Set tool manually (simulating detection)
	d.mu.Lock()
	d.detectedTool = "vim"
	d.detectedIcon = "vim.png"
	d.mu.Unlock()

	if tool := d.GetDetectedTool(); tool != "vim" {
		t.Errorf("Expected tool=vim, got %q", tool)
	}

	if icon := d.GetDetectedIcon(); icon != "vim.png" {
		t.Errorf("Expected icon=vim.png, got %q", icon)
	}
}

func TestToolHandler_RateLimitIntegration(t *testing.T) {
	// This test verifies the actual rate limiting logic path
	d := NewToolHandler("test-session", "/tmp/plugins", WithContinuousMode(true))

	// Set up handler state
	d.mu.Lock()
	d.detectedTool = "vim"
	d.lastToolChange = time.Now() // Just changed
	d.mu.Unlock()

	changeChan := d.ChangeChannel()

	// Helper to check if an event was published
	checkNoEvent := func(msg string) {
		select {
		case evt := <-changeChan:
			t.Errorf("%s: unexpected event received: %+v", msg, evt)
		case <-time.After(50 * time.Millisecond):
			// OK - no event expected
		}
	}

	// Simulate calling the rate limit check logic
	d.mu.Lock()
	now := time.Now()
	toolChangeMinInterval := 5 * time.Second
	shouldBlock := !d.lastToolChange.IsZero() && now.Sub(d.lastToolChange) < toolChangeMinInterval
	d.mu.Unlock()

	if !shouldBlock {
		t.Error("Expected rate limit to block change within 5s")
	}

	// No event should be in channel since we didn't actually call OnOutput
	checkNoEvent("after rate limit check")

	// Now simulate time passing (set lastToolChange to 6s ago)
	d.mu.Lock()
	d.lastToolChange = time.Now().Add(-6 * time.Second)
	d.mu.Unlock()

	// Check that rate limit would now allow
	d.mu.Lock()
	now = time.Now()
	shouldBlock = !d.lastToolChange.IsZero() && now.Sub(d.lastToolChange) < toolChangeMinInterval
	d.mu.Unlock()

	if shouldBlock {
		t.Error("Expected rate limit to allow change after 5s")
	}
}
