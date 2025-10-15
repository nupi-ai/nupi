package bootstrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config stores remote connection details for CLI and GUI clients so they can
// connect to a daemon without reading the local config.db directly.
type Config struct {
	BaseURL   string       `json:"base_url"`
	APIToken  string       `json:"api_token,omitempty"`
	TLS       *TLSConfig   `json:"tls,omitempty"`
	UpdatedAt time.Time    `json:"updated_at"`
	Metadata  *MetaSection `json:"meta,omitempty"`
}

// TLSConfig contains optional TLS overrides for remote connections.
type TLSConfig struct {
	Insecure   bool   `json:"insecure,omitempty"`
	CACertPath string `json:"ca_cert_path,omitempty"`
	ServerName string `json:"server_name,omitempty"`
}

// MetaSection allows callers to store additional information (e.g. name).
type MetaSection struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// Path returns the absolute filesystem location of the bootstrap file.
func Path() (string, error) {
	return resolvePath()
}

// resolvePath returns the filesystem path of the bootstrap configuration file.
func resolvePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("bootstrap: resolve home directory: %w", err)
	}
	return filepath.Join(home, ".nupi", "bootstrap.json"), nil
}

// Load returns the stored bootstrap configuration. If the file does not exist,
// (nil, nil) is returned.
func Load() (*Config, error) {
	p, err := resolvePath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("bootstrap: read file: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("bootstrap: decode file: %w", err)
	}

	return &cfg, nil
}

// Save persists the given bootstrap configuration to disk, creating
// intermediate directories as needed.
func Save(cfg *Config) error {
	if cfg == nil {
		return errors.New("bootstrap: config is nil")
	}

	p, err := resolvePath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("bootstrap: create directory: %w", err)
	}

	cfg.UpdatedAt = time.Now().UTC()

	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("bootstrap: encode config: %w", err)
	}

	if err := os.WriteFile(p, encoded, 0o600); err != nil {
		return fmt.Errorf("bootstrap: write file: %w", err)
	}

	return nil
}

// Remove deletes the bootstrap configuration. It is not considered an error
// when the file does not exist.
func Remove() error {
	p, err := resolvePath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("bootstrap: remove file: %w", err)
	}
	return nil
}
