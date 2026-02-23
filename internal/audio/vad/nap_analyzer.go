package vad

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

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

type napAnalyzer struct {
	params    SessionParams
	conn      *grpc.ClientConn
	stream    napv1.VoiceActivityDetectionService_DetectSpeechClient
	ctx       context.Context
	cancel    context.CancelFunc
	responses chan Detection
	errCh     chan error
	wg        sync.WaitGroup
}

func newNAPAnalyzer(ctx context.Context, params SessionParams, endpoint configstore.AdapterEndpoint) (Analyzer, error) {
	conn, stream, streamCtx, cancel, err := napstream.OpenBidi(
		ctx,
		endpoint,
		"vad",
		"open detect speech stream",
		ErrAdapterUnavailable,
		func(streamCtx context.Context, conn *grpc.ClientConn) (napv1.VoiceActivityDetectionService_DetectSpeechClient, error) {
			return napv1.NewVoiceActivityDetectionServiceClient(conn).DetectSpeech(streamCtx)
		},
	)
	if err != nil {
		return nil, err
	}

	a := &napAnalyzer{
		params:    params,
		conn:      conn,
		stream:    stream,
		ctx:       streamCtx,
		cancel:    cancel,
		responses: make(chan Detection, 16),
		errCh:     make(chan error, 1),
	}
	a.startReceiver()

	configJSON, err := napstream.MarshalConfigJSON("vad", params.Config)
	if err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), constants.Duration2Seconds)
		a.Close(closeCtx)
		closeCancel()
		return nil, err
	}

	initReq := &napv1.DetectSpeechRequest{
		SessionId:  params.SessionID,
		StreamId:   params.StreamID,
		Format:     mapper.ToNAPAudioFormat(params.Format),
		ConfigJson: configJSON,
	}

	if err := stream.Send(initReq); err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), constants.Duration2Seconds)
		a.Close(closeCtx)
		closeCancel()
		return nil, napstream.WrapGRPCError("vad", "send init request", err, ErrAdapterUnavailable)
	}

	return a, nil
}

func (a *napAnalyzer) startReceiver() {
	napstream.StartReceiver(
		a.ctx,
		&a.wg,
		a.stream.Recv,
		a.responses,
		a.errCh,
		speechEventToDetection,
	)
}

func (a *napAnalyzer) OnSegment(_ context.Context, segment eventbus.AudioIngressSegmentEvent) ([]Detection, error) {
	req := &napv1.DetectSpeechRequest{
		SessionId: a.params.SessionID,
		StreamId:  a.params.StreamID,
		PcmData:   segment.Data,
		Format:    mapper.ToNAPAudioFormat(segment.Format),
		Timestamp: mapper.ToProtoTimestampChecked(segment.EndedAt),
	}

	if err := a.stream.Send(req); err != nil {
		return nil, napstream.WrapGRPCError("vad", "send segment", err, ErrAdapterUnavailable)
	}

	return a.collectDetections(segment.Last), a.drainError()
}

func (a *napAnalyzer) Close(ctx context.Context) ([]Detection, error) {
	_ = a.stream.CloseSend()

	detections := a.drainDetections(ctx)
	a.cancel()
	a.wg.Wait()
	if err := a.conn.Close(); err != nil {
		return detections, fmt.Errorf("vad: close connection: %w", err)
	}
	if err := a.drainError(); err != nil && !errors.Is(err, ErrAdapterUnavailable) {
		return detections, err
	}
	return detections, nil
}

const vadResponseGracePeriod = constants.Duration10Milliseconds

func (a *napAnalyzer) collectDetections(wait bool) []Detection {
	return napstream.CollectBufferedOrGrace(a.responses, wait, vadResponseGracePeriod)
}

func (a *napAnalyzer) drainDetections(ctx context.Context) []Detection {
	return napstream.DrainUntilDone(ctx, a.responses)
}

func (a *napAnalyzer) drainError() error {
	return napstream.DrainReceiveError(a.errCh, "vad", "receive speech event", ErrAdapterUnavailable)
}

func speechEventToDetection(evt *napv1.SpeechEvent) Detection {
	det := Detection{
		Confidence: evt.GetConfidence(),
		Metadata:   maputil.Clone(evt.GetMetadata()),
	}
	switch evt.GetType() {
	case napv1.SpeechEventType_SPEECH_EVENT_TYPE_START,
		napv1.SpeechEventType_SPEECH_EVENT_TYPE_ONGOING:
		det.Active = true
	case napv1.SpeechEventType_SPEECH_EVENT_TYPE_END:
		det.Active = false
	default:
		det.Active = false
	}
	return det
}

// ContextWithDialer attaches a custom dialer to the context.
// It is primarily intended for tests that exercise gRPC connectivity via bufconn
// without opening real network sockets.
func ContextWithDialer(ctx context.Context, dialer func(context.Context, string) (net.Conn, error)) context.Context {
	return napdial.ContextWithDialer(ctx, dialer)
}
