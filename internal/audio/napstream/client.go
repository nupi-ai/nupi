package napstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/napdial"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// OpenBidi opens a NAP bidi stream with a dedicated cancelable stream context.
func OpenBidi[Stream any](
	ctx context.Context,
	endpoint configstore.AdapterEndpoint,
	prefix string,
	openOp string,
	errAdapterUnavailable error,
	open func(context.Context, *grpc.ClientConn) (Stream, error),
) (*grpc.ClientConn, Stream, context.Context, context.CancelFunc, error) {
	var zero Stream

	conn, err := napdial.DialAdapter(ctx, endpoint)
	if err != nil {
		return nil, zero, nil, nil, fmt.Errorf("%s: %w", prefix, err)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := open(streamCtx, conn)
	if err != nil {
		cancel()
		_ = conn.Close()
		return nil, zero, nil, nil, WrapGRPCError(prefix, openOp, err, errAdapterUnavailable)
	}

	return conn, stream, streamCtx, cancel, nil
}

// MarshalConfigJSON serializes adapter config into a JSON string.
func MarshalConfigJSON(prefix string, config map[string]any) (string, error) {
	if len(config) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("%s: marshal adapter config: %w", prefix, err)
	}
	return string(raw), nil
}

// WrapGRPCError wraps an operation error and attaches adapter-unavailable sentinel
// when grpc status code is Unavailable.
func WrapGRPCError(prefix, op string, err, errAdapterUnavailable error) error {
	if IsUnavailableError(err) {
		return fmt.Errorf("%s: %s: %w: %w", prefix, op, err, errAdapterUnavailable)
	}
	return fmt.Errorf("%s: %s: %w", prefix, op, err)
}

// IsUnavailableError reports whether err maps to grpc Unavailable.
func IsUnavailableError(err error) bool {
	if s, ok := status.FromError(err); ok && s.Code() == codes.Unavailable {
		return true
	}
	return false
}

// StartReceiver forwards stream Recv responses to a channel until EOF/cancel/error.
func StartReceiver[Resp, Out any](
	ctx context.Context,
	wg *sync.WaitGroup,
	recv func() (*Resp, error),
	outCh chan<- Out,
	errCh chan<- error,
	mapper func(*Resp) Out,
) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(outCh)
		for {
			resp, err := recv()
			if err != nil {
				if err != io.EOF {
					select {
					case errCh <- err:
					default:
					}
				}
				return
			}

			select {
			case outCh <- mapper(resp):
			case <-ctx.Done():
				return
			}
		}
	}()
}

// CollectBufferedOrGrace drains immediate buffered items; optionally waits a
// short grace period for late final events.
func CollectBufferedOrGrace[T any](items <-chan T, wait bool, grace time.Duration) []T {
	var out []T

drainBuffered:
	for {
		select {
		case item, ok := <-items:
			if !ok {
				return out
			}
			out = append(out, item)
		default:
			break drainBuffered
		}
	}

	if len(out) > 0 || !wait {
		return out
	}

	timer := time.NewTimer(grace)
	defer timer.Stop()

	for {
		select {
		case item, ok := <-items:
			if !ok {
				return out
			}
			out = append(out, item)
			for {
				select {
				case item, ok := <-items:
					if !ok {
						return out
					}
					out = append(out, item)
				default:
					return out
				}
			}
		default:
			select {
			case item, ok := <-items:
				if !ok {
					return out
				}
				out = append(out, item)
			case <-timer.C:
				return out
			}
		}
	}
}

// DrainUntilDone drains items until channel close or context cancellation.
func DrainUntilDone[T any](ctx context.Context, items <-chan T) []T {
	var out []T
	for {
		select {
		case item, ok := <-items:
			if !ok {
				return out
			}
			out = append(out, item)
		case <-ctx.Done():
			return out
		}
	}
}

// DrainReceiveError consumes a non-blocking receive-loop error and maps it to
// transport-aware output.
func DrainReceiveError(errCh <-chan error, prefix, op string, errAdapterUnavailable error) error {
	select {
	case err := <-errCh:
		if err == nil || errors.Is(err, io.EOF) {
			return nil
		}
		if IsUnavailableError(err) {
			return fmt.Errorf("%s: %s: %w: %w", prefix, op, err, errAdapterUnavailable)
		}
		if isIgnorableRecvError(err) {
			return nil
		}
		return fmt.Errorf("%s: %s: %w", prefix, op, err)
	default:
		return nil
	}
}

func isIgnorableRecvError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.Canceled, codes.DeadlineExceeded:
			return true
		}
	}
	return false
}
