//go:build !windows

package jsruntime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	// frameHeaderSize is the size of the length prefix (uint32 big-endian).
	frameHeaderSize = 4
	// maxFramePayload is the maximum payload size (16 MB).
	maxFramePayload = 16 << 20
	// acceptTimeout is how long we wait for the JS process to connect.
	acceptTimeout = 10 * time.Second
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

// acceptSingleConn waits for exactly one client to connect within the timeout.
func acceptSingleConn(listener net.Listener, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = acceptTimeout
	}
	deadline := time.Now().Add(timeout)
	if dl, ok := listener.(interface{ SetDeadline(time.Time) error }); ok {
		dl.SetDeadline(deadline)
	}

	conn, err := listener.Accept()
	if err != nil {
		return nil, fmt.Errorf("jsruntime: accept IPC connection: %w", err)
	}

	// Prevent the listener from removing the socket file on Close.
	// We handle cleanup explicitly in cleanupSocket.
	if ul, ok := listener.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}

	// Close the listener - we only accept one connection.
	listener.Close()
	return conn, nil
}

// writeFrame writes a length-prefixed frame: [4 bytes big-endian length][payload].
func writeFrame(conn net.Conn, data []byte) error {
	if len(data) > maxFramePayload {
		return fmt.Errorf("jsruntime: frame payload too large (%d > %d)", len(data), maxFramePayload)
	}

	var header [frameHeaderSize]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))

	if len(data) == 0 {
		_, err := conn.Write(header[:])
		return err
	}

	// Write header + payload via writev (net.Buffers).
	bufs := net.Buffers{header[:], data}
	_, err := bufs.WriteTo(conn)
	return err
}

// readFrame reads a single length-prefixed frame from r.
func readFrame(r io.Reader) ([]byte, error) {
	var header [frameHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}

	length := binary.BigEndian.Uint32(header[:])
	if length == 0 {
		return []byte{}, nil
	}
	if length > maxFramePayload {
		return nil, fmt.Errorf("jsruntime: frame too large (%d > %d)", length, maxFramePayload)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// cleanupSocket removes the socket file.
func cleanupSocket(socketPath string) {
	if socketPath != "" {
		os.Remove(socketPath)
	}
}
