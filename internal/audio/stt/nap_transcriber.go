package stt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/napdial"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type napTranscriber struct {
	params    SessionParams
	conn      *grpc.ClientConn
	stream    napv1.SpeechToTextService_StreamTranscriptionClient
	ctx       context.Context
	cancel    context.CancelFunc
	responses chan Transcription
	errCh     chan error
	wg        sync.WaitGroup
}

func newNAPTranscriber(ctx context.Context, params SessionParams, endpoint configstore.AdapterEndpoint) (Transcriber, error) {
	conn, err := napdial.DialAdapter(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("stt: %w", err)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	client := napv1.NewSpeechToTextServiceClient(conn)
	stream, err := client.StreamTranscription(streamCtx)
	if err != nil {
		cancel()
		conn.Close()
		if s, ok := status.FromError(err); ok && s.Code() == codes.Unavailable {
			return nil, fmt.Errorf("stt: open transcription stream: %w: %w", err, ErrAdapterUnavailable)
		}
		return nil, fmt.Errorf("stt: open transcription stream: %w", err)
	}

	t := &napTranscriber{
		params:    params,
		conn:      conn,
		stream:    stream,
		ctx:       streamCtx,
		cancel:    cancel,
		responses: make(chan Transcription, 16),
		errCh:     make(chan error, 1),
	}
	t.startReceiver()

	configJSON := ""
	if len(params.Config) > 0 {
		raw, err := json.Marshal(params.Config)
		if err != nil {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
			t.Close(closeCtx)
			closeCancel()
			return nil, fmt.Errorf("stt: marshal adapter config: %w", err)
		}
		configJSON = string(raw)
	}

	initReq := &napv1.StreamTranscriptionRequest{
		SessionId:  params.SessionID,
		StreamId:   params.StreamID,
		Format:     marshalFormat(params.Format),
		Metadata:   copyStringMap(params.Metadata),
		ConfigJson: configJSON,
	}

	if err := stream.Send(initReq); err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		t.Close(closeCtx)
		closeCancel()
		if s, ok := status.FromError(err); ok && s.Code() == codes.Unavailable {
			return nil, fmt.Errorf("stt: send init request: %w: %w", err, ErrAdapterUnavailable)
		}
		return nil, fmt.Errorf("stt: send init request: %w", err)
	}

	return t, nil
}

func (t *napTranscriber) startReceiver() {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		for {
			resp, err := t.stream.Recv()
			if err != nil {
				if err != io.EOF {
					select {
					case t.errCh <- err:
					default:
					}
				}
				close(t.responses)
				return
			}

			tr := Transcription{
				Text:       resp.GetText(),
				Confidence: resp.GetConfidence(),
				Final:      resp.GetFinal(),
				Metadata:   copyStringMap(resp.GetMetadata()),
			}
			if resp.GetStartedAt() != nil {
				tr.StartedAt = resp.GetStartedAt().AsTime()
			}
			if resp.GetEndedAt() != nil {
				tr.EndedAt = resp.GetEndedAt().AsTime()
			}

			select {
			case t.responses <- tr:
			case <-t.ctx.Done():
				return
			}
		}
	}()
}

func (t *napTranscriber) OnSegment(ctx context.Context, segment eventbus.AudioIngressSegmentEvent) ([]Transcription, error) {
	req := &napv1.StreamTranscriptionRequest{
		SessionId: t.params.SessionID,
		StreamId:  t.params.StreamID,
		Segment: &napv1.Segment{
			Sequence:   segment.Sequence,
			Audio:      segment.Data,
			First:      segment.First,
			Last:       segment.Last,
			Metadata:   copyStringMap(segment.Metadata),
			DurationMs: uint32(segment.Duration / time.Millisecond),
			StartedAt:  timestampOrNil(segment.StartedAt),
			EndedAt:    timestampOrNil(segment.EndedAt),
		},
	}

	if err := t.stream.Send(req); err != nil {
		if s, ok := status.FromError(err); ok && s.Code() == codes.Unavailable {
			return nil, fmt.Errorf("stt: send segment: %w: %w", err, ErrAdapterUnavailable)
		}
		return nil, fmt.Errorf("stt: send segment: %w", err)
	}

	return t.collectResponses(segment.Last), t.drainError()
}

func (t *napTranscriber) Close(ctx context.Context) ([]Transcription, error) {
	flushReq := &napv1.StreamTranscriptionRequest{
		SessionId: t.params.SessionID,
		StreamId:  t.params.StreamID,
		Flush:     true,
	}
	_ = t.stream.Send(flushReq) // Ignore error; CloseSend below handles final state.
	_ = t.stream.CloseSend()

	transcripts := t.drainResponses(ctx)
	t.cancel()
	t.wg.Wait()
	if err := t.conn.Close(); err != nil {
		return transcripts, fmt.Errorf("stt: close connection: %w", err)
	}
	if err := t.drainError(); err != nil && !errors.Is(err, ErrAdapterUnavailable) {
		return transcripts, err
	}
	return transcripts, nil
}

const sttResponseGracePeriod = 10 * time.Millisecond

func (t *napTranscriber) collectResponses(wait bool) []Transcription {
	var transcripts []Transcription

	// Drain anything already buffered without blocking.
drainBuffered:
	for {
		select {
		case tr, ok := <-t.responses:
			if !ok {
				return transcripts
			}
			transcripts = append(transcripts, tr)
		default:
			break drainBuffered
		}
	}

	if len(transcripts) > 0 || !wait {
		return transcripts
	}

	// Allow a brief grace period for final segments to deliver late transcripts.
	timer := time.NewTimer(sttResponseGracePeriod)
	defer timer.Stop()

	for {
		select {
		case tr, ok := <-t.responses:
			if !ok {
				return transcripts
			}
			transcripts = append(transcripts, tr)
			// After receiving the first item, drain whatever else is pending.
			for {
				select {
				case tr, ok := <-t.responses:
					if !ok {
						return transcripts
					}
					transcripts = append(transcripts, tr)
				default:
					return transcripts
				}
			}
		default:
			select {
			case tr, ok := <-t.responses:
				if !ok {
					return transcripts
				}
				transcripts = append(transcripts, tr)
			case <-timer.C:
				return transcripts
			}
		}
	}
}

func (t *napTranscriber) drainResponses(ctx context.Context) []Transcription {
	var transcripts []Transcription
	for {
		select {
		case tr, ok := <-t.responses:
			if !ok {
				return transcripts
			}
			transcripts = append(transcripts, tr)
		case <-ctx.Done():
			return transcripts
		}
	}
}

func (t *napTranscriber) drainError() error {
	select {
	case err := <-t.errCh:
		if err == nil || errors.Is(err, io.EOF) {
			return nil
		}
		if s, ok := status.FromError(err); ok {
			switch s.Code() {
			case codes.Canceled:
				return nil
			case codes.Unavailable:
				return fmt.Errorf("stt: receive transcript: %w: %w", err, ErrAdapterUnavailable)
			}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return fmt.Errorf("stt: receive transcript: %w", err)
	default:
		return nil
	}
}

func marshalFormat(format eventbus.AudioFormat) *napv1.AudioFormat {
	return &napv1.AudioFormat{
		Encoding:        string(format.Encoding),
		SampleRate:      uint32(format.SampleRate),
		Channels:        uint32(format.Channels),
		BitDepth:        uint32(format.BitDepth),
		FrameDurationMs: uint32(format.FrameDuration / time.Millisecond),
	}
}

func copyStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func timestampOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	ts := timestamppb.New(t)
	if err := ts.CheckValid(); err != nil {
		return nil
	}
	return ts
}

// ContextWithDialer attaches a custom dialer to the context.
// It is primarily intended for tests that exercise gRPC connectivity via bufconn
// without opening real network sockets.
func ContextWithDialer(ctx context.Context, dialer func(context.Context, string) (net.Conn, error)) context.Context {
	return napdial.ContextWithDialer(ctx, dialer)
}
