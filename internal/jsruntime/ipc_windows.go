//go:build windows

package jsruntime

import (
	"errors"
	"io"
	"net"
	"time"
)

const (
	frameHeaderSize = 4
	maxFramePayload = 16 << 20
	acceptTimeout   = 10 * time.Second
)

// ErrIPCNotSupported indicates that IPC via Unix domain sockets is not
// supported on this platform. The runtime falls back to stdio transport.
var ErrIPCNotSupported = errors.New("jsruntime: IPC not supported on windows")

func createIPCSocket(runDir string, suffix string) (string, net.Listener, error) {
	return "", nil, ErrIPCNotSupported
}

func acceptSingleConn(listener net.Listener, timeout time.Duration) (net.Conn, error) {
	return nil, ErrIPCNotSupported
}

func writeFrame(conn net.Conn, data []byte) error {
	return ErrIPCNotSupported
}

func readFrame(r io.Reader) ([]byte, error) {
	return nil, ErrIPCNotSupported
}

func cleanupSocket(socketPath string) {}
