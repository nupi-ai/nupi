package intentrouter

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// mockToolHandler implements ToolHandler for testing.
type mockToolHandler struct {
	def    ToolDefinition
	result json.RawMessage
	err    error
	called int
	mu     sync.Mutex
}

func (h *mockToolHandler) Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.called++
	if h.err != nil {
		return nil, h.err
	}
	return h.result, nil
}

func (h *mockToolHandler) Definition() ToolDefinition {
	return h.def
}

func (h *mockToolHandler) getCalled() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.called
}

func newMockHandler(name, description string) *mockToolHandler {
	return &mockToolHandler{
		def: ToolDefinition{
			Name:           name,
			Description:    description,
			ParametersJSON: `{"type":"object","properties":{}}`,
		},
		result: json.RawMessage(`{"ok":true}`),
	}
}

func TestNewToolRegistry(t *testing.T) {
	reg := NewToolRegistry()
	if reg == nil {
		t.Fatal("NewToolRegistry returned nil")
	}
}

func TestRegisterAndGetToolsForEventType(t *testing.T) {
	reg := NewToolRegistry()

	// Register tools that are in the user_intent mapping
	memSearch := newMockHandler("memory_search", "Search memory")
	memWrite := newMockHandler("memory_write", "Write memory")
	coreUpdate := newMockHandler("core_memory_update", "Update core memory")

	reg.Register(memSearch)
	reg.Register(memWrite)
	reg.Register(coreUpdate)

	// user_intent should return all three registered tools
	defs := reg.GetToolsForEventType(EventTypeUserIntent)
	if len(defs) != 3 {
		t.Fatalf("Expected 3 tools for user_intent, got %d", len(defs))
	}

	names := make(map[string]bool)
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, expected := range []string{"memory_search", "memory_write", "core_memory_update"} {
		if !names[expected] {
			t.Errorf("Expected tool %q in user_intent results", expected)
		}
	}

	// session_output should return only memory_search
	defs = reg.GetToolsForEventType(EventTypeSessionOutput)
	if len(defs) != 1 {
		t.Fatalf("Expected 1 tool for session_output, got %d", len(defs))
	}
	if defs[0].Name != "memory_search" {
		t.Fatalf("Expected memory_search for session_output, got %s", defs[0].Name)
	}
}

func TestGetToolsForEventTypeEmptyMapping(t *testing.T) {
	reg := NewToolRegistry()

	// Register a tool
	reg.Register(newMockHandler("memory_search", "Search memory"))

	// history_summary and clarification have empty mappings — should return empty (non-nil) slice
	defs := reg.GetToolsForEventType(EventTypeHistorySummary)
	if len(defs) != 0 {
		t.Fatalf("Expected 0 tools for history_summary, got %d", len(defs))
	}

	defs = reg.GetToolsForEventType(EventTypeClarification)
	if len(defs) != 0 {
		t.Fatalf("Expected 0 tools for clarification, got %d", len(defs))
	}

	// session_slug also has empty mapping
	defs = reg.GetToolsForEventType(EventTypeSessionSlug)
	if len(defs) != 0 {
		t.Fatalf("Expected 0 tools for session_slug, got %d", len(defs))
	}
}

func TestGetToolsForEventTypeUnknownEventType(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(newMockHandler("memory_search", "Search memory"))

	// Unknown event type not in mapping — should return nil
	defs := reg.GetToolsForEventType("unknown_event")
	if len(defs) != 0 {
		t.Fatalf("Expected 0 tools for unknown event type, got %d", len(defs))
	}
}

func TestGetToolsReturnsOnlyRegistered(t *testing.T) {
	reg := NewToolRegistry()

	// user_intent maps 6 tools, but only register memory_search
	reg.Register(newMockHandler("memory_search", "Search memory"))

	defs := reg.GetToolsForEventType(EventTypeUserIntent)
	if len(defs) != 1 {
		t.Fatalf("Expected 1 tool (only registered one), got %d", len(defs))
	}
	if defs[0].Name != "memory_search" {
		t.Fatalf("Expected memory_search, got %s", defs[0].Name)
	}
}

func TestGetToolsFutureEventTypes(t *testing.T) {
	reg := NewToolRegistry()

	reg.Register(newMockHandler("memory_write", "Write memory"))
	reg.Register(newMockHandler("core_memory_update", "Update core"))
	reg.Register(newMockHandler("onboarding_complete", "Complete onboarding"))

	// memory_flush should return only memory_write
	defs := reg.GetToolsForEventType(EventTypeMemoryFlush)
	if len(defs) != 1 {
		t.Fatalf("Expected 1 tool for memory_flush, got %d", len(defs))
	}
	if defs[0].Name != "memory_write" {
		t.Fatalf("Expected memory_write for memory_flush, got %s", defs[0].Name)
	}

	// scheduled_task should return memory_search and memory_write
	reg.Register(newMockHandler("memory_search", "Search memory"))
	defs = reg.GetToolsForEventType(EventTypeScheduledTask)
	if len(defs) != 2 {
		t.Fatalf("Expected 2 tools for scheduled_task, got %d", len(defs))
	}
	schedNames := make(map[string]bool)
	for _, d := range defs {
		schedNames[d.Name] = true
	}
	if !schedNames["memory_search"] {
		t.Fatal("Expected memory_search in scheduled_task results")
	}
	if !schedNames["memory_write"] {
		t.Fatal("Expected memory_write in scheduled_task results")
	}

	// onboarding should return core_memory_update and onboarding_complete
	defs = reg.GetToolsForEventType(EventTypeOnboarding)
	if len(defs) != 2 {
		t.Fatalf("Expected 2 tools for onboarding, got %d", len(defs))
	}
	names := make(map[string]bool)
	for _, d := range defs {
		names[d.Name] = true
	}
	if !names["core_memory_update"] {
		t.Fatal("Expected core_memory_update in onboarding results")
	}
	if !names["onboarding_complete"] {
		t.Fatal("Expected onboarding_complete in onboarding results")
	}
}

func TestExecuteSuccess(t *testing.T) {
	reg := NewToolRegistry()

	handler := newMockHandler("memory_search", "Search memory")
	handler.result = json.RawMessage(`{"results":["found"]}`)
	reg.Register(handler)

	result, err := reg.Execute(context.Background(), "memory_search", json.RawMessage(`{"query":"test"}`))
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}
	results, ok := parsed["results"]
	if !ok {
		t.Fatal("Expected 'results' key in response")
	}
	arr, ok := results.([]interface{})
	if !ok || len(arr) != 1 || arr[0] != "found" {
		t.Fatalf("Expected results=[\"found\"], got %v", results)
	}

	if handler.getCalled() != 1 {
		t.Fatalf("Expected handler called 1 time, got %d", handler.getCalled())
	}
}

func TestExecuteToolNotFound(t *testing.T) {
	reg := NewToolRegistry()

	_, err := reg.Execute(context.Background(), "nonexistent_tool", nil)
	if err == nil {
		t.Fatal("Expected error for nonexistent tool")
	}
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("Expected ErrToolNotFound, got: %v", err)
	}
}

func TestExecuteHandlerError(t *testing.T) {
	reg := NewToolRegistry()

	handlerErr := errors.New("handler failed")
	handler := newMockHandler("memory_search", "Search memory")
	handler.err = handlerErr
	reg.Register(handler)

	_, err := reg.Execute(context.Background(), "memory_search", json.RawMessage(`{"query":"test"}`))
	if err == nil {
		t.Fatal("Expected error from handler")
	}
	if !errors.Is(err, handlerErr) {
		t.Fatalf("Expected handler error, got: %v", err)
	}
}

func TestRegisterReplacesExisting(t *testing.T) {
	reg := NewToolRegistry()

	handler1 := newMockHandler("memory_search", "First version")
	handler2 := newMockHandler("memory_search", "Second version")
	handler2.result = json.RawMessage(`{"version":2}`)

	reg.Register(handler1)
	reg.Register(handler2)

	result, err := reg.Execute(context.Background(), "memory_search", nil)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}
	if v, ok := parsed["version"]; !ok || v != float64(2) {
		t.Fatalf("Expected version 2 from replaced handler, got %v", parsed)
	}
}

func TestConcurrentRegisterAndGet(t *testing.T) {
	reg := NewToolRegistry()

	// Seed one handler so Execute calls don't all fail with ErrToolNotFound
	reg.Register(newMockHandler("memory_search", "Search"))

	var wg sync.WaitGroup

	// Concurrent registrations with different tool names
	toolNames := []string{"memory_search", "memory_write", "memory_get", "core_memory_update", "schedule_add"}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := toolNames[idx%len(toolNames)]
			handler := newMockHandler(name, "Tool "+name)
			reg.Register(handler)
		}(i)
	}

	// Concurrent reads across different event types
	eventTypes := []EventType{EventTypeUserIntent, EventTypeSessionOutput, EventTypeHistorySummary, EventTypeMemoryFlush}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_ = reg.GetToolsForEventType(eventTypes[idx%len(eventTypes)])
		}(i)
	}

	// Concurrent executes
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = reg.Execute(context.Background(), "memory_search", nil)
		}()
	}

	wg.Wait()
}

func TestRegisterNilHandler(t *testing.T) {
	reg := NewToolRegistry()

	// Should not panic
	reg.Register(nil)

	// Registry should still be functional
	defs := reg.GetToolsForEventType(EventTypeUserIntent)
	if len(defs) != 0 {
		t.Fatalf("Expected 0 tools after nil registration, got %d", len(defs))
	}
}

func TestWithToolRegistryOption(t *testing.T) {
	bus := newTestBus(t)
	defer bus.Shutdown()

	reg := NewToolRegistry()
	reg.Register(newMockHandler("memory_search", "Search"))

	svc := NewService(bus, WithToolRegistry(reg))
	if svc.toolRegistry == nil {
		t.Fatal("Expected toolRegistry to be set via WithToolRegistry option")
	}
}

func TestSetToolRegistryRuntime(t *testing.T) {
	bus := newTestBus(t)
	defer bus.Shutdown()

	svc := NewService(bus)
	if svc.toolRegistry != nil {
		t.Fatal("Expected toolRegistry to be nil initially")
	}

	reg := NewToolRegistry()
	svc.SetToolRegistry(reg)

	svc.mu.RLock()
	got := svc.toolRegistry
	svc.mu.RUnlock()

	if got == nil {
		t.Fatal("Expected toolRegistry to be set after SetToolRegistry")
	}
}

func TestRegisterEmptyNameHandler(t *testing.T) {
	reg := NewToolRegistry()

	// Handler with empty name should be silently ignored
	emptyHandler := &mockToolHandler{
		def: ToolDefinition{
			Name:           "",
			Description:    "Ghost handler",
			ParametersJSON: `{}`,
		},
		result: json.RawMessage(`{"ghost":true}`),
	}
	reg.Register(emptyHandler)

	// Should not be executable via empty string
	_, err := reg.Execute(context.Background(), "", nil)
	if err == nil {
		t.Fatal("Expected error when executing empty-name tool")
	}
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("Expected ErrToolNotFound, got: %v", err)
	}

	// Should not appear in any event type results
	defs := reg.GetToolsForEventType(EventTypeUserIntent)
	if len(defs) != 0 {
		t.Fatalf("Expected 0 tools after empty-name registration, got %d", len(defs))
	}
}

func TestExecutePassesArgsToHandler(t *testing.T) {
	reg := NewToolRegistry()

	captureHandler := &argCapturingHandler{
		def: ToolDefinition{
			Name:           "memory_search",
			Description:    "Search memory",
			ParametersJSON: `{"type":"object","properties":{"query":{"type":"string"}}}`,
		},
		result: json.RawMessage(`{"results":[]}`),
	}
	reg.Register(captureHandler)

	inputArgs := json.RawMessage(`{"query":"hello world"}`)
	_, err := reg.Execute(context.Background(), "memory_search", inputArgs)
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}

	capturedArgs := captureHandler.lastArgs
	if string(capturedArgs) != `{"query":"hello world"}` {
		t.Fatalf("Expected args %q, got %q", `{"query":"hello world"}`, string(capturedArgs))
	}
}

func TestGetToolsForEventTypeDistinguishesUnknownFromEmpty(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(newMockHandler("memory_search", "Search memory"))

	// Known event type with empty mapping (history_summary) returns empty slice
	defs := reg.GetToolsForEventType(EventTypeHistorySummary)
	if defs == nil {
		t.Fatal("Expected non-nil empty slice for known event type with empty mapping")
	}
	if len(defs) != 0 {
		t.Fatalf("Expected 0 tools for history_summary, got %d", len(defs))
	}

	// Unknown event type returns nil
	defs = reg.GetToolsForEventType("totally_unknown_type")
	if defs != nil {
		t.Fatalf("Expected nil for unknown event type, got %v", defs)
	}
}

// argCapturingHandler captures the args passed to Execute for verification.
type argCapturingHandler struct {
	def      ToolDefinition
	result   json.RawMessage
	lastArgs json.RawMessage
	mu       sync.Mutex
}

func (h *argCapturingHandler) Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastArgs = args
	return h.result, nil
}

func (h *argCapturingHandler) Definition() ToolDefinition {
	return h.def
}

// newTestBus creates an event bus for testing.
func newTestBus(t *testing.T) *eventbus.Bus {
	t.Helper()
	return eventbus.New()
}
