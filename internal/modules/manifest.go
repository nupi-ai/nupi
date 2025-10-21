package modules

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// ModuleManifest describes metadata stored in adapter manifest files.
type ModuleManifest struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   ModuleMetadata     `yaml:"metadata"`
	Spec       ModuleManifestSpec `yaml:"spec"`
}

// ModuleMetadata captures descriptive fields.
type ModuleMetadata struct {
	Name        string `yaml:"name"`
	Slug        string `yaml:"slug"`
	Description string `yaml:"description"`
	Version     string `yaml:"version"`
}

// ModuleManifestSpec captures runtime configuration for a module.
type ModuleManifestSpec struct {
	ModuleType string              `yaml:"moduleType"`
	Mode       string              `yaml:"mode"`
	Entrypoint ModuleEntrypoint    `yaml:"entrypoint"`
	Assets     ModuleAssets        `yaml:"assets"`
	Telemetry  ModuleTelemetrySpec `yaml:"telemetry"`
}

// ModuleEntrypoint defines how the module should be started.
type ModuleEntrypoint struct {
	Command    string   `yaml:"command"`
	Args       []string `yaml:"args"`
	Transport  string   `yaml:"transport"`
	ListenEnv  string   `yaml:"listenEnv"`
	WorkingDir string   `yaml:"workingDir"`
}

// ModuleAssets describes static requirements such as data directories.
type ModuleAssets struct {
	Models ModuleModelAssets `yaml:"models"`
}

// ModuleModelAssets captures model cache configuration.
type ModuleModelAssets struct {
	CacheDirEnv string `yaml:"cacheDirEnv"`
}

// ModuleTelemetrySpec toggles stdout/stderr collection.
type ModuleTelemetrySpec struct {
	Stdout *bool `yaml:"stdout"`
	Stderr *bool `yaml:"stderr"`
}

// ParseModuleManifest decodes the provided manifest string into a structured representation.
func ParseModuleManifest(raw string) (*ModuleManifest, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var manifest ModuleManifest
	if err := yaml.Unmarshal([]byte(raw), &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}
