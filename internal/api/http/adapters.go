package http

import (
	"encoding/json"
	"time"
)

// AdapterRuntime captures runtime health metadata emitted by adapters.Service.
type AdapterRuntime struct {
	AdapterID string            `json:"adapter_id,omitempty"`
	Health    string            `json:"health"`
	Message   string            `json:"message,omitempty"`
	StartedAt *string           `json:"started_at,omitempty"`
	UpdatedAt string            `json:"updated_at"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// AdapterEntry describes a single adapter slot, its binding, and runtime status.
type AdapterEntry struct {
	Slot      string          `json:"slot"`
	AdapterID *string         `json:"adapter_id,omitempty"`
	Status    string          `json:"status"`
	Config    string          `json:"config,omitempty"`
	UpdatedAt string          `json:"updated_at"`
	Runtime   *AdapterRuntime `json:"runtime,omitempty"`
}

// AdaptersOverview is returned by GET /adapters.
type AdaptersOverview struct {
	Adapters []AdapterEntry `json:"adapters"`
}

// AdapterLogStreamEntry represents a single item in the adapters log stream.
type AdapterLogStreamEntry struct {
	Type       string    `json:"type"`
	Timestamp  time.Time `json:"timestamp"`
	AdapterID  string    `json:"adapter_id,omitempty"`
	Slot       string    `json:"slot,omitempty"`
	Level      string    `json:"level,omitempty"`
	Message    string    `json:"message,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	StreamID   string    `json:"stream_id,omitempty"`
	Text       string    `json:"text,omitempty"`
	Confidence float64   `json:"confidence,omitempty"`
	Final      bool      `json:"final,omitempty"`
}

// AdapterActionResult wraps an updated adapter entry returned by write endpoints.
type AdapterActionResult struct {
	Adapter AdapterEntry `json:"adapter"`
}

// AdapterRegistrationRequest is accepted by POST /adapters/register.
type AdapterRegistrationRequest struct {
	AdapterID string                 `json:"adapter_id"`
	Source    string                 `json:"source,omitempty"`
	Version   string                 `json:"version,omitempty"`
	Type      string                 `json:"type,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Manifest  json.RawMessage        `json:"manifest,omitempty"`
	Endpoint  *AdapterEndpointConfig `json:"endpoint,omitempty"`
}

// AdapterEndpointConfig describes how the daemon should reach an adapter.
type AdapterEndpointConfig struct {
	Transport string            `json:"transport,omitempty"`
	Address   string            `json:"address,omitempty"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// AdapterDescriptor captures adapter metadata persisted in the configuration store.
type AdapterDescriptor struct {
	ID       string          `json:"id"`
	Source   string          `json:"source,omitempty"`
	Version  string          `json:"version,omitempty"`
	Type     string          `json:"type,omitempty"`
	Name     string          `json:"name,omitempty"`
	Manifest json.RawMessage `json:"manifest,omitempty"`
}

// AdapterRegistrationResult wraps the response returned by POST /adapters/register.
type AdapterRegistrationResult struct {
	Adapter AdapterDescriptor `json:"adapter"`
}
