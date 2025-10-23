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

const runnerBinaryName = "adapter-runner"

// Layout captures the directory layout for adapter-runner distribution.
type Layout struct {
	BinDir     string
	BinaryPath string
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

// EnsureLayout ensures the directory layout for adapter-runner exists.
func (m *Manager) EnsureLayout() error {
	if strings.TrimSpace(m.layout.BinDir) == "" {
		return errors.New("adapter-runner: bin directory not configured")
	}
	return ensureDirectory(m.layout.BinDir, 0o755)
}

func ensureDirectory(path string, perm os.FileMode) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if info.IsDir() {
			return nil
		}
		if removeErr := os.Remove(path); removeErr != nil {
			return removeErr
		}
	case os.IsNotExist(err):
		// proceed to create directory
	default:
		return err
	}
	return os.MkdirAll(path, perm)
}

// InstallFromFile copies the supplied binary into the active location.
func (m *Manager) InstallFromFile(srcPath string) error {
	if err := m.EnsureLayout(); err != nil {
		return err
	}

	srcPath = strings.TrimSpace(srcPath)
	if srcPath == "" {
		return errors.New("adapter-runner: source path is empty")
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("adapter-runner open source %q: %w", srcPath, err)
	}
	defer src.Close()

	tmpFile := m.layout.BinaryPath + ".tmp"
	dst, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("adapter-runner create target %q: %w", tmpFile, err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		_ = os.Remove(tmpFile)
		return fmt.Errorf("adapter-runner copy %q -> %q: %w", srcPath, tmpFile, err)
	}

	if err := dst.Chmod(0o755); err != nil {
		dst.Close()
		_ = os.Remove(tmpFile)
		return fmt.Errorf("adapter-runner chmod target %q: %w", tmpFile, err)
	}

	if err := dst.Close(); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("adapter-runner close target %q: %w", tmpFile, err)
	}

	if err := os.Rename(tmpFile, m.layout.BinaryPath); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("adapter-runner activate binary: %w", err)
	}

	return nil
}

func determineLayout(baseDir string) Layout {
	binDir := filepath.Join(baseDir, "bin")
	return Layout{
		BinDir:     binDir,
		BinaryPath: filepath.Join(binDir, binaryName()),
	}
}

func binaryName() string {
	name := runnerBinaryName
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// BinaryName returns the platform-specific adapter-runner binary name.
func BinaryName() string {
	return binaryName()
}
