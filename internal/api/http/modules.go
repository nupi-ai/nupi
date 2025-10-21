package http

import "encoding/json"

// ModuleRuntime captures runtime health metadata emitted by modules.Service.
type ModuleRuntime struct {
	ModuleID  string            `json:"module_id,omitempty"`
	Health    string            `json:"health"`
	Message   string            `json:"message,omitempty"`
	StartedAt *string           `json:"started_at,omitempty"`
	UpdatedAt string            `json:"updated_at"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// ModuleEntry describes a single module slot, its binding, and runtime status.
type ModuleEntry struct {
	Slot      string         `json:"slot"`
	AdapterID *string        `json:"adapter_id,omitempty"`
	Status    string         `json:"status"`
	Config    string         `json:"config,omitempty"`
	UpdatedAt string         `json:"updated_at"`
	Runtime   *ModuleRuntime `json:"runtime,omitempty"`
}

// ModulesOverview is returned by GET /modules.
type ModulesOverview struct {
	Modules []ModuleEntry `json:"modules"`
}

// ModuleActionResult wraps an updated module entry returned by write endpoints.
type ModuleActionResult struct {
	Module ModuleEntry `json:"module"`
}

// ModuleRegistrationRequest is accepted by POST /modules/register.
type ModuleRegistrationRequest struct {
	AdapterID string                `json:"adapter_id"`
	Source    string                `json:"source,omitempty"`
	Version   string                `json:"version,omitempty"`
	Type      string                `json:"type,omitempty"`
	Name      string                `json:"name,omitempty"`
	Manifest  json.RawMessage       `json:"manifest,omitempty"`
	Endpoint  *ModuleEndpointConfig `json:"endpoint,omitempty"`
}

// ModuleEndpointConfig describes how the daemon should reach a module.
type ModuleEndpointConfig struct {
	Transport string            `json:"transport,omitempty"`
	Address   string            `json:"address,omitempty"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// ModuleAdapter captures adapter metadata persisted in the configuration store.
type ModuleAdapter struct {
	ID       string          `json:"id"`
	Source   string          `json:"source,omitempty"`
	Version  string          `json:"version,omitempty"`
	Type     string          `json:"type,omitempty"`
	Name     string          `json:"name,omitempty"`
	Manifest json.RawMessage `json:"manifest,omitempty"`
}

// ModuleRegistrationResult wraps the response returned by POST /modules/register.
type ModuleRegistrationResult struct {
	Adapter ModuleAdapter `json:"adapter"`
}
