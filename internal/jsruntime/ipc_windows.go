//go:build windows

package jsruntime

import (
	"fmt"
	"net"
)

// createIPCSocket creates a TCP localhost listener with a dynamic port.
// On Windows, Unix domain sockets are not reliably available, so we use
// TCP on the loopback interface instead. The returned "socketPath" is
// the TCP address (e.g. "127.0.0.1:12345").
//
// Security note: unlike Unix domain sockets (which use file permissions),
// TCP loopback can be connected to by any local process. The risk is
// mitigated by acceptSingleConn which accepts exactly one connection and
// closes the listener, leaving a very narrow race window between Listen
// and Accept. The port is passed to the child process via environment
// variable, not exposed externally.
func createIPCSocket(runDir string, suffix string) (string, net.Listener, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("jsruntime: listen tcp loopback: %w", err)
	}
	addr := listener.Addr().String()
	return addr, listener, nil
}

// cleanupSocket is a no-op on Windows — TCP listeners don't create files.
func cleanupSocket(socketPath string) {}
