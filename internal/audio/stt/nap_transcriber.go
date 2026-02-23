package stt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	"github.com/nupi-ai/nupi/internal/audio/napstream"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/mapper"
	"github.com/nupi-ai/nupi/internal/napdial"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
	"google.golang.org/grpc"
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
	conn, stream, streamCtx, cancel, err := napstream.OpenBidi(
		ctx,
		endpoint,
		"stt",
		"open transcription stream",
		ErrAdapterUnavailable,
		func(streamCtx context.Context, conn *grpc.ClientConn) (napv1.SpeechToTextService_StreamTranscriptionClient, error) {
			return napv1.NewSpeechToTextServiceClient(conn).StreamTranscription(streamCtx)
		},
	)
	if err != nil {
		return nil, err
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

	configJSON, err := napstream.MarshalConfigJSON("stt", params.Config)
	if err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), constants.Duration2Seconds)
		t.Close(closeCtx)
		closeCancel()
		return nil, err
	}

	initReq := &napv1.StreamTranscriptionRequest{
		SessionId:  params.SessionID,
		StreamId:   params.StreamID,
		Format:     mapper.ToNAPAudioFormat(params.Format),
		Metadata:   maputil.Clone(params.Metadata),
		ConfigJson: configJSON,
	}

	if err := stream.Send(initReq); err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), constants.Duration2Seconds)
		t.Close(closeCtx)
		closeCancel()
		return nil, napstream.WrapGRPCError("stt", "send init request", err, ErrAdapterUnavailable)
	}

	return t, nil
}

func (t *napTranscriber) startReceiver() {
	napstream.StartReceiver(
		t.ctx,
		&t.wg,
		t.stream.Recv,
		t.responses,
		t.errCh,
		func(resp *napv1.Transcript) Transcription {
			tr := Transcription{
				Text:       resp.GetText(),
				Confidence: resp.GetConfidence(),
				Final:      resp.GetFinal(),
				Metadata:   maputil.Clone(resp.GetMetadata()),
			}
			if resp.GetStartedAt() != nil {
				tr.StartedAt = resp.GetStartedAt().AsTime()
			}
			if resp.GetEndedAt() != nil {
				tr.EndedAt = resp.GetEndedAt().AsTime()
			}
			return tr
		},
	)
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
			Metadata:   maputil.Clone(segment.Metadata),
			DurationMs: uint32(segment.Duration / time.Millisecond),
			StartedAt:  mapper.ToProtoTimestampChecked(segment.StartedAt),
			EndedAt:    mapper.ToProtoTimestampChecked(segment.EndedAt),
		},
	}

	if err := t.stream.Send(req); err != nil {
		return nil, napstream.WrapGRPCError("stt", "send segment", err, ErrAdapterUnavailable)
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

const sttResponseGracePeriod = constants.Duration10Milliseconds

func (t *napTranscriber) collectResponses(wait bool) []Transcription {
	return napstream.CollectBufferedOrGrace(t.responses, wait, sttResponseGracePeriod)
}

func (t *napTranscriber) drainResponses(ctx context.Context) []Transcription {
	return napstream.DrainUntilDone(ctx, t.responses)
}

func (t *napTranscriber) drainError() error {
	return napstream.DrainReceiveError(t.errCh, "stt", "receive transcript", ErrAdapterUnavailable)
}

// ContextWithDialer attaches a custom dialer to the context.
// It is primarily intended for tests that exercise gRPC connectivity via bufconn
// without opening real network sockets.
func ContextWithDialer(ctx context.Context, dialer func(context.Context, string) (net.Conn, error)) context.Context {
	return napdial.ContextWithDialer(ctx, dialer)
}
