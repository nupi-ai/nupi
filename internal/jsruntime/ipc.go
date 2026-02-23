package jsruntime

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/nupi-ai/nupi/internal/constants"
)

const (
	// frameHeaderSize is the size of the length prefix (uint32 big-endian).
	frameHeaderSize = 4
	// maxFramePayload is the maximum payload size (16 MB).
	maxFramePayload = 16 << 20
	// acceptTimeout is how long we wait for the JS process to connect.
	acceptTimeout = constants.Duration10Seconds
)

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
	// On Windows this assertion is simply false (TCP listener), which is fine.
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
