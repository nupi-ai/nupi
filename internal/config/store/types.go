package store

// Setting represents a simple key-value pair scoped to instance/profile.
type Setting struct {
	Key       string
	Value     string
	UpdatedAt string
}

// SecurityEntry stores secrets or access control data.
type SecurityEntry struct {
	Key       string
	Value     string
	UpdatedAt string
}

// Adapter describes an installed adapter plugin.
type Adapter struct {
	ID        string
	Source    string
	Version   string
	Type      string
	Name      string
	Manifest  string
	CreatedAt string
	UpdatedAt string
}

// AdapterBinding maps an adapter to a functional slot for the active profile.
type AdapterBinding struct {
	Slot      string
	AdapterID *string
	Config    string
	Status    string
	UpdatedAt string
}

const (
	BindingStatusActive   = "active"
	BindingStatusInactive = "inactive"
	BindingStatusRequired = "required"
)

// AdapterEndpoint describes how to reach or launch an adapter.
type AdapterEndpoint struct {
	AdapterID string
	Transport string
	Address   string
	Command   string
	Args      []string
	Env       map[string]string
	CreatedAt string
	UpdatedAt string
}

// Profile contains metadata about available profiles.
type Profile struct {
	Name      string
	IsDefault bool
	CreatedAt string
	UpdatedAt string
}

// TransportConfig captures daemon binding- and TLS-related settings.
type TransportConfig struct {
	Port           int
	Binding        string   // loopback/lan/public
	TLSCertPath    string   // optional path to TLS certificate
	TLSKeyPath     string   // optional path to TLS key
	AllowedOrigins []string // CORS/WebSocket allowlist
	GRPCPort       int
	GRPCBinding    string
}
