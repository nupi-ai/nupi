package http

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
