//go:build !windows

package jsruntime

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// maxUnixSocketPath is the maximum length for a Unix domain socket path.
// macOS limits this to 104 bytes; Linux allows 108.
const maxUnixSocketPath = 104

// createIPCSocket creates a Unix domain socket and returns the socket path
// and listener. The socket file has permissions 0600.
//
// It first tries the requested runDir. If the resulting path would exceed
// the OS limit for Unix domain sockets (104 bytes on macOS), it falls back
// to os.TempDir().
func createIPCSocket(runDir string, suffix string) (socketPath string, listener net.Listener, err error) {
	filename := fmt.Sprintf("jsrt-%s.sock", suffix)

	socketPath = filepath.Join(runDir, filename)
	if len(socketPath) >= maxUnixSocketPath {
		// Path too long for UDS — fall back to the system temp directory.
		runDir = os.TempDir()
		socketPath = filepath.Join(runDir, filename)
	}

	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("jsruntime: create run dir: %w", err)
	}

	// Clean up any stale socket from a previous run.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", nil, fmt.Errorf("jsruntime: remove stale socket: %w", err)
	}

	listener, err = net.Listen("unix", socketPath)
	if err != nil {
		return "", nil, fmt.Errorf("jsruntime: listen on %s: %w", socketPath, err)
	}

	if err := os.Chmod(socketPath, 0o600); err != nil {
		listener.Close()
		os.Remove(socketPath)
		return "", nil, fmt.Errorf("jsruntime: chmod socket: %w", err)
	}

	return socketPath, listener, nil
}

// cleanupSocket removes the socket file.
func cleanupSocket(socketPath string) {
	if socketPath != "" {
		os.Remove(socketPath)
	}
}
