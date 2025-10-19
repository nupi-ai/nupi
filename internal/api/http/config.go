package http

// ConfigMigrationResult represents the outcome of a configuration repair run.
type ConfigMigrationResult struct {
	UpdatedSlots         []string `json:"updated_slots"`
	PendingSlots         []string `json:"pending_slots"`
	AudioSettingsUpdated bool     `json:"audio_settings_updated"`
}
