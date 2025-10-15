package adapterrunner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nupi-ai/nupi/internal/config"
)

const (
	runnerDirectoryName = "adapter-runner"
	versionFileName     = "VERSION"
)

// ErrNotInstalled is returned when adapter-runner assets are not yet present.
var ErrNotInstalled = errors.New("adapter-runner not installed")

// Layout captures the directory layout for adapter-runner distribution.
type Layout struct {
	BaseDir     string
	VersionsDir string
	CurrentDir  string
	BinaryPath  string
	VersionFile string
}

// Manager coordinates adapter-runner binary distribution and metadata.
type Manager struct {
	layout Layout
}

// NewManager constructs a manager rooted at the provided Nupi home directory.
// If baseDir is empty, it falls back to the default (~/.nupi).
func NewManager(baseDir string) *Manager {
	if strings.TrimSpace(baseDir) == "" {
		baseDir = config.GetNupiHome()
	}
	layout := determineLayout(baseDir)
	return &Manager{layout: layout}
}

// Layout returns the resolved directory layout.
func (m *Manager) Layout() Layout {
	return m.layout
}

// BinaryPath returns the path to the active adapter-runner binary.
func (m *Manager) BinaryPath() string {
	return m.layout.BinaryPath
}

// VersionFile returns the path to the file tracking the active adapter-runner version.
func (m *Manager) VersionFile() string {
	return m.layout.VersionFile
}

// EnsureLayout ensures the directory layout for adapter-runner exists.
func (m *Manager) EnsureLayout() error {
	dirs := []string{
		filepath.Dir(m.layout.BaseDir),
		m.layout.BaseDir,
		m.layout.VersionsDir,
		m.layout.CurrentDir,
	}

	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create adapter-runner directory %q: %w", dir, err)
		}
	}
	return nil
}

// VersionRoot returns the directory containing assets for the provided version.
func (m *Manager) VersionRoot(version string) string {
	return filepath.Join(m.layout.VersionsDir, sanitizeVersion(version))
}

// BinaryForVersion returns the expected path to the adapter-runner binary for a specific version.
func (m *Manager) BinaryForVersion(version string) string {
	return filepath.Join(m.VersionRoot(version), binaryName())
}

// ActivateVersion makes the supplied version active by copying its binary into the current directory.
func (m *Manager) ActivateVersion(version string) error {
	if err := m.EnsureLayout(); err != nil {
		return err
	}

	src := m.BinaryForVersion(version)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("adapter-runner version %q not prepared: %w", version, err)
	}

	if err := os.RemoveAll(m.layout.CurrentDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("adapter-runner remove current dir: %w", err)
	}

	if err := os.MkdirAll(m.layout.CurrentDir, 0o755); err != nil {
		return fmt.Errorf("adapter-runner create current dir: %w", err)
	}

	if err := copyFile(src, m.layout.BinaryPath); err != nil {
		return err
	}

	if err := m.RecordInstalledVersion(version); err != nil {
		return err
	}
	return nil
}

// InstalledVersion returns the current adapter-runner version if present.
func (m *Manager) InstalledVersion() (string, error) {
	data, err := os.ReadFile(m.layout.VersionFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNotInstalled
		}
		return "", fmt.Errorf("read adapter-runner version marker: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// RecordInstalledVersion persists the current adapter-runner version marker.
func (m *Manager) RecordInstalledVersion(version string) error {
	if err := m.EnsureLayout(); err != nil {
		return err
	}

	tmpFile := m.layout.VersionFile + ".tmp"
	contents := strings.TrimSpace(version)
	if contents != "" {
		contents += "\n"
	}
	if err := os.WriteFile(tmpFile, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("write version marker: %w", err)
	}
	if err := os.Rename(tmpFile, m.layout.VersionFile); err != nil {
		return fmt.Errorf("activate version marker: %w", err)
	}
	return nil
}

func determineLayout(baseDir string) Layout {
	binDir := filepath.Join(baseDir, "bin")
	base := filepath.Join(binDir, runnerDirectoryName)
	current := filepath.Join(base, "current")
	return Layout{
		BaseDir:     base,
		VersionsDir: filepath.Join(base, "versions"),
		CurrentDir:  current,
		BinaryPath:  filepath.Join(current, binaryName()),
		VersionFile: filepath.Join(current, versionFileName),
	}
}

func sanitizeVersion(version string) string {
	v := strings.TrimSpace(version)
	if v == "" {
		return "unknown"
	}
	v = strings.ReplaceAll(v, "/", "_")
	v = strings.ReplaceAll(v, `\`, "_")
	v = strings.ReplaceAll(v, string(os.PathSeparator), "_")
	return v
}

func binaryName() string {
	name := runnerDirectoryName
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("adapter-runner open source %q: %w", src, err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("adapter-runner create target %q: %w", dst, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("adapter-runner copy %q -> %q: %w", src, dst, err)
	}

	if err := out.Chmod(0o755); err != nil {
		out.Close()
		return fmt.Errorf("adapter-runner chmod target %q: %w", dst, err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("adapter-runner close target %q: %w", dst, err)
	}
	return nil
}
