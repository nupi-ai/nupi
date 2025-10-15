package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/nupi-ai/nupi/internal/server"
	"github.com/nupi-ai/nupi/internal/session"
)

type unixSocketService struct {
	socketPath     string
	sessionManager *session.Manager
	apiServer      *server.APIServer
	runtimeInfo    *RuntimeInfo

	mu       sync.Mutex
	listener net.Listener
	wg       sync.WaitGroup
}

func newUnixSocketService(socketPath string, sm *session.Manager, api *server.APIServer, info *RuntimeInfo) *unixSocketService {
	return &unixSocketService{
		socketPath:     socketPath,
		sessionManager: sm,
		apiServer:      api,
		runtimeInfo:    info,
	}
}

func (s *unixSocketService) Start(ctx context.Context) error {
	if s.socketPath == "" {
		return fmt.Errorf("socket path is empty")
	}

	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o755); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}

	if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("failed to create Unix socket: %w", err)
	}

	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		listener.Close()
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()

	s.wg.Add(1)
	go s.acceptLoop(ctx, listener)

	return nil
}

func (s *unixSocketService) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	listener := s.listener
	s.listener = nil
	s.mu.Unlock()

	if listener != nil {
		listener.Close()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}

	if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to remove socket: %w", err)
	}

	return nil
}

func (s *unixSocketService) acceptLoop(ctx context.Context, listener net.Listener) {
	defer s.wg.Done()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}

			if errors.Is(err, net.ErrClosed) {
				return
			}

			fmt.Fprintf(os.Stderr, "Failed to accept connection: %v\n", err)
			continue
		}

		go s.handleConnection(conn)
	}
}

func (s *unixSocketService) handleConnection(conn net.Conn) {
	handler := NewProtocolHandler(s.sessionManager, s.apiServer, s.runtimeInfo, conn)
	handler.Handle()
}
