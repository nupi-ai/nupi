package napstream

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type testReq struct {
	ID int
}

type testResp struct {
	ID int
}

type testOut struct {
	ID int
}

type recvResult struct {
	resp *testResp
	err  error
}

type fakeStream struct {
	mu        sync.Mutex
	recvQueue []recvResult
	sent      []*testReq
	closed    bool
}

func (s *fakeStream) Send(req *testReq) error {
	s.mu.Lock()
	s.sent = append(s.sent, req)
	s.mu.Unlock()
	return nil
}

func (s *fakeStream) Recv() (*testResp, error) {
	s.mu.Lock()
	if len(s.recvQueue) == 0 {
		s.mu.Unlock()
		return nil, io.EOF
	}
	item := s.recvQueue[0]
	s.recvQueue = s.recvQueue[1:]
	s.mu.Unlock()
	if item.err != nil {
		return nil, item.err
	}
	return item.resp, nil
}

func (s *fakeStream) CloseSend() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func newBufConn(t *testing.T) (*grpc.ClientConn, func()) {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	go func() {
		_ = srv.Serve(lis)
	}()
	ctx := context.Background()
	dialer := func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	}
	conn, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(dialer), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		srv.Stop()
		lis.Close()
		t.Fatalf("dial bufconn: %v", err)
	}
	cleanup := func() {
		srv.Stop()
		_ = lis.Close()
	}
	return conn, cleanup
}

func newTestBidiClient(t *testing.T, conn *grpc.ClientConn, stream *fakeStream, recvOp string, errAdapterUnavailable error) *BidiClient[testReq, testResp, testOut] {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	client := &BidiClient[testReq, testResp, testOut]{
		prefix:                "test",
		conn:                  conn,
		stream:                stream,
		ctx:                   ctx,
		cancel:                cancel,
		responses:             make(chan testOut, 16),
		errCh:                 make(chan error, 1),
		grace:                 time.Millisecond,
		errAdapterUnavailable: errAdapterUnavailable,
		recvOp:                recvOp,
	}

	StartReceiver(
		client.ctx,
		&client.wg,
		client.stream.Recv,
		client.responses,
		client.errCh,
		func(resp *testResp) testOut {
			return testOut{ID: resp.ID}
		},
	)

	return client
}

func TestBidiClientDrain(t *testing.T) {
	stream := &fakeStream{
		recvQueue: []recvResult{
			{resp: &testResp{ID: 1}},
		},
	}
	client := newTestBidiClient(t, nil, stream, "receive", errors.New("adapter unavailable"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	out := client.Drain(ctx)
	if len(out) != 1 || out[0].ID != 1 {
		t.Fatalf("unexpected drain output: %+v", out)
	}
	if err := client.DrainError(); err != nil {
		t.Fatalf("unexpected drain error: %v", err)
	}
}

func TestBidiClientDrainErrorUnavailable(t *testing.T) {
	adapterUnavailable := errors.New("adapter unavailable")
	stream := &fakeStream{
		recvQueue: []recvResult{
			{err: status.Error(codes.Unavailable, "down")},
		},
	}
	client := newTestBidiClient(t, nil, stream, "receive", adapterUnavailable)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = client.Drain(ctx)

	err := client.DrainError()
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, adapterUnavailable) {
		t.Fatalf("expected adapter unavailable sentinel, got: %v", err)
	}
}

func TestBidiClientClose(t *testing.T) {
	conn, cleanup := newBufConn(t)
	defer cleanup()

	stream := &fakeStream{}
	client := newTestBidiClient(t, conn, stream, "receive", errors.New("adapter unavailable"))

	called := false
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Close(ctx, func() { called = true }); err != nil {
		t.Fatalf("unexpected close error: %v", err)
	}
	if !called {
		t.Fatal("expected preClose to be called")
	}
	stream.mu.Lock()
	closed := stream.closed
	stream.mu.Unlock()
	if !closed {
		t.Fatal("expected CloseSend to be called")
	}
}
