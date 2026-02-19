package intentrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ErrToolNotFound is returned when Execute is called with an unregistered tool name.
var ErrToolNotFound = errors.New("intentrouter: tool not found")

// toolRegistryImpl is the default implementation of ToolRegistry.
type toolRegistryImpl struct {
	mu               sync.RWMutex
	handlers         map[string]ToolHandler
	eventTypeMapping map[EventType][]string
}

// NewToolRegistry creates a new tool registry with the predefined event-type-to-tool
// mapping from docs/memory-system-plan.md Section 6.5.
func NewToolRegistry() ToolRegistry {
	r := &toolRegistryImpl{
		handlers: make(map[string]ToolHandler),
		eventTypeMapping: map[EventType][]string{
			EventTypeUserIntent: {
				"memory_search", "memory_get", "memory_write",
				"core_memory_update", "schedule_add", "schedule_remove",
			},
			EventTypeSessionOutput: {
				"memory_search",
			},
			EventTypeHistorySummary: {},
			EventTypeClarification:  {},

			// Future event types — mapping defined now so the registry is ready
			// when these EventType constants are added in later stories.
			"memory_flush":  {"memory_write"},
			"scheduled_task": {"memory_search", "memory_write"},
			"onboarding":    {"core_memory_update", "onboarding_complete"},
			"session_slug":  {},
		},
	}
	return r
}

// Register adds a tool handler to the registry, keyed by Definition().Name.
// If a handler with the same name is already registered, it is replaced.
// Nil handlers are silently ignored.
func (r *toolRegistryImpl) Register(handler ToolHandler) {
	if handler == nil {
		return
	}
	name := handler.Definition().Name
	r.mu.Lock()
	r.handlers[name] = handler
	r.mu.Unlock()
}

// GetToolsForEventType returns definitions for tools that are both mapped to the given
// event type AND have been registered. Unregistered tools in the mapping are skipped.
func (r *toolRegistryImpl) GetToolsForEventType(eventType EventType) []ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	allowed, ok := r.eventTypeMapping[eventType]
	if !ok {
		return nil
	}

	var defs []ToolDefinition
	for _, name := range allowed {
		if handler, exists := r.handlers[name]; exists {
			defs = append(defs, handler.Definition())
		}
	}
	return defs
}

// Execute looks up a handler by name and calls it with the given arguments.
func (r *toolRegistryImpl) Execute(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	r.mu.RLock()
	handler, exists := r.handlers[name]
	r.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrToolNotFound, name)
	}

	return handler.Execute(ctx, args)
}
