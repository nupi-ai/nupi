package config

import (
	"os"
	"path/filepath"
)

const (
	DefaultInstance = "default"
	DefaultProfile  = "default"
)

// InstancePaths contains all paths for a Nupi instance.
type InstancePaths struct {
	Home        string // Instance home directory
	Config      string // Legacy YAML configuration file path (to be removed)
	ConfigDB    string // SQLite configuration store path
	Socket      string // Unix socket path
	Lock        string // Daemon lock file path
	Logs        string // Logs directory
	PluginsDir  string // Plugins directory (adapters, detectors, pipeline cleaners)
	TempDir     string // Temporary files directory
	RunDir      string // Runtime assets directory
	BinDir      string // Shared binaries directory (~/.nupi/bin)
	RunnerRoot  string // Adapter-runner root directory
	PipelineDir string // Pipeline cache/output directory (indexes)
}

// GetInstancePaths returns all paths for a given instance.
// Empty instance name defaults to "default".
func GetInstancePaths(instanceName string) InstancePaths {
	if instanceName == "" {
		instanceName = DefaultInstance
	}

	instanceDir := filepath.Join(GetNupiHome(), "instances", instanceName)
	binDir := filepath.Join(GetNupiHome(), "bin")

	return InstancePaths{
		Home:        instanceDir,
		Config:      filepath.Join(instanceDir, "config.yaml"),
		ConfigDB:    filepath.Join(instanceDir, "config.db"),
		Socket:      filepath.Join(instanceDir, "nupi.sock"),
		Lock:        filepath.Join(instanceDir, "daemon.lock"),
		Logs:        filepath.Join(instanceDir, "logs"),
		PluginsDir:  filepath.Join(instanceDir, "plugins"),
		TempDir:     filepath.Join(instanceDir, "tmp"),
		RunDir:      filepath.Join(instanceDir, "run"),
		BinDir:      binDir,
		RunnerRoot:  filepath.Join(binDir, "adapter-runner"),
		PipelineDir: filepath.Join(instanceDir, "pipeline"),
	}
}

// ProfilePaths contains metadata associated with a specific profile.
type ProfilePaths struct {
	Instance InstancePaths // Parent instance paths
	Name     string        // Profile name
}

// GetProfilePaths returns metadata for a given instance/profile combination.
func GetProfilePaths(instanceName, profileName string) ProfilePaths {
	if instanceName == "" {
		instanceName = DefaultInstance
	}
	if profileName == "" {
		profileName = DefaultProfile
	}

	instance := GetInstancePaths(instanceName)

	return ProfilePaths{
		Instance: instance,
		Name:     profileName,
	}
}

// GetNupiHome returns the Nupi home directory.
// Uses NUPI_HOME environment variable if set, otherwise defaults to ~/.nupi.
func GetNupiHome() string {
	if home := os.Getenv("NUPI_HOME"); home != "" {
		return home
	}
	userHome, _ := os.UserHomeDir()
	return filepath.Join(userHome, ".nupi")
}

// ExpandPath expands ~ to the user home directory.
func ExpandPath(path string) string {
	if len(path) == 0 {
		return path
	}
	if path[0] == '~' {
		home, _ := os.UserHomeDir()
		if len(path) == 1 {
			return home
		}
		if path[1] == '/' || path[1] == os.PathSeparator {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// EnsureInstanceDirs creates the directory structure for the given instance if it does not exist.
func EnsureInstanceDirs(instanceName string) (InstancePaths, error) {
	paths := GetInstancePaths(instanceName)

	dirs := []string{
		paths.Home,
		paths.Logs,
		paths.PluginsDir,
		paths.TempDir,
		paths.RunDir,
		paths.BinDir,
		paths.PipelineDir,
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return paths, err
		}
	}

	return paths, nil
}

// EnsureProfileDirs ensures that the parent instance directories exist for the given profile.
func EnsureProfileDirs(instanceName, profileName string) (ProfilePaths, error) {
	profilePaths := GetProfilePaths(instanceName, profileName)

	if _, err := EnsureInstanceDirs(instanceName); err != nil {
		return profilePaths, err
	}

	return profilePaths, nil
}
