package intentrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
				"core_memory_update", "heartbeat_add", "heartbeat_remove",
				"heartbeat_list",
			},
			EventTypeSessionOutput: {
				"memory_search",
			},
			EventTypeClarification: {},
			// NOTE: heartbeat.txt declares "You only have access to these tools:
			// memory_search, memory_write." — if this list changes, update
			// internal/config/store/prompts/heartbeat.txt to match.
			EventTypeHeartbeat:  {"memory_search", "memory_write"},
			EventTypeOnboarding:             {"core_memory_update", "onboarding_complete"},
			EventTypeJournalCompaction:      {},
			EventTypeConversationCompaction: {},
		},
	}
	return r
}

// Register adds a tool handler to the registry, keyed by Definition().Name.
// If a handler with the same name is already registered, it is replaced.
// Nil handlers and handlers with empty names are silently ignored.
func (r *toolRegistryImpl) Register(handler ToolHandler) {
	if handler == nil {
		return
	}
	name := handler.Definition().Name
	if name == "" {
		return
	}
	r.mu.Lock()
	_, replaced := r.handlers[name]
	r.handlers[name] = handler
	r.mu.Unlock()
	if replaced {
		log.Printf("[ToolRegistry] Replaced handler for tool %q", name)
	} else {
		log.Printf("[ToolRegistry] Registered tool %q", name)
	}
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

	defs := make([]ToolDefinition, 0, len(allowed))
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
