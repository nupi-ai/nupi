package vad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	"github.com/nupi-ai/nupi/internal/audio/streammanager"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/napdial"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
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
	transport := strings.TrimSpace(endpoint.Transport)
	if transport == "" {
		transport = "process"
	}
	switch transport {
	case "grpc", "process":
	default:
		return nil, fmt.Errorf("vad: unsupported transport %q for adapter %s", endpoint.Transport, endpoint.AdapterID)
	}
	address := strings.TrimSpace(endpoint.Address)
	if address == "" {
		return nil, fmt.Errorf("vad: adapter %s missing address", endpoint.AdapterID)
	}

	tlsCfg := napdial.TLSConfigFromFields(endpoint.TLSCertPath, endpoint.TLSKeyPath, endpoint.TLSCACertPath, endpoint.TLSInsecure)
	dialOpts, err := napdial.DialOptions(ctx, tlsCfg, napdial.DefaultQoS())
	if err != nil {
		return nil, fmt.Errorf("vad: adapter %s: %w", endpoint.AdapterID, err)
	}
	conn, err := grpc.NewClient(napdial.PassthroughPrefix+address, dialOpts...)
	if err != nil {
		if s, ok := status.FromError(err); ok && s.Code() == codes.Unavailable {
			return nil, fmt.Errorf("vad: connect adapter %s: %w: %w", endpoint.AdapterID, err, ErrAdapterUnavailable)
		}
		return nil, fmt.Errorf("vad: connect adapter %s: %w", endpoint.AdapterID, err)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	client := napv1.NewVoiceActivityDetectionServiceClient(conn)
	stream, err := client.DetectSpeech(streamCtx)
	if err != nil {
		cancel()
		conn.Close()
		if s, ok := status.FromError(err); ok && s.Code() == codes.Unavailable {
			return nil, fmt.Errorf("vad: open detect speech stream: %w: %w", err, ErrAdapterUnavailable)
		}
		return nil, fmt.Errorf("vad: open detect speech stream: %w", err)
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

	configJSON := ""
	if len(params.Config) > 0 {
		raw, err := json.Marshal(params.Config)
		if err != nil {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
			a.Close(closeCtx)
			closeCancel()
			return nil, fmt.Errorf("vad: marshal adapter config: %w", err)
		}
		configJSON = string(raw)
	}

	initReq := &napv1.DetectSpeechRequest{
		SessionId:  params.SessionID,
		StreamId:   params.StreamID,
		Format:     marshalAudioFormat(params.Format),
		ConfigJson: configJSON,
	}

	if err := stream.Send(initReq); err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		a.Close(closeCtx)
		closeCancel()
		if s, ok := status.FromError(err); ok && s.Code() == codes.Unavailable {
			return nil, fmt.Errorf("vad: send init request: %w: %w", err, ErrAdapterUnavailable)
		}
		return nil, fmt.Errorf("vad: send init request: %w", err)
	}

	return a, nil
}

func (a *napAnalyzer) startReceiver() {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		for {
			resp, err := a.stream.Recv()
			if err != nil {
				if err != io.EOF {
					select {
					case a.errCh <- err:
					default:
					}
				}
				close(a.responses)
				return
			}

			det := speechEventToDetection(resp)
			select {
			case a.responses <- det:
			case <-a.ctx.Done():
				return
			}
		}
	}()
}

func (a *napAnalyzer) OnSegment(_ context.Context, segment eventbus.AudioIngressSegmentEvent) ([]Detection, error) {
	req := &napv1.DetectSpeechRequest{
		SessionId: a.params.SessionID,
		StreamId:  a.params.StreamID,
		PcmData:   segment.Data,
		Format:    marshalAudioFormat(segment.Format),
		Timestamp: timestampOrNil(segment.EndedAt),
	}

	if err := a.stream.Send(req); err != nil {
		if s, ok := status.FromError(err); ok && s.Code() == codes.Unavailable {
			return nil, fmt.Errorf("vad: send segment: %w: %w", err, ErrAdapterUnavailable)
		}
		return nil, fmt.Errorf("vad: send segment: %w", err)
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

const vadResponseGracePeriod = 10 * time.Millisecond

func (a *napAnalyzer) collectDetections(wait bool) []Detection {
	var detections []Detection

drainBuffered:
	for {
		select {
		case det, ok := <-a.responses:
			if !ok {
				return detections
			}
			detections = append(detections, det)
		default:
			break drainBuffered
		}
	}

	if len(detections) > 0 || !wait {
		return detections
	}

	timer := time.NewTimer(vadResponseGracePeriod)
	defer timer.Stop()

	for {
		select {
		case det, ok := <-a.responses:
			if !ok {
				return detections
			}
			detections = append(detections, det)
			for {
				select {
				case det, ok := <-a.responses:
					if !ok {
						return detections
					}
					detections = append(detections, det)
				default:
					return detections
				}
			}
		default:
			select {
			case det, ok := <-a.responses:
				if !ok {
					return detections
				}
				detections = append(detections, det)
			case <-timer.C:
				return detections
			}
		}
	}
}

func (a *napAnalyzer) drainDetections(ctx context.Context) []Detection {
	var detections []Detection
	for {
		select {
		case det, ok := <-a.responses:
			if !ok {
				return detections
			}
			detections = append(detections, det)
		case <-ctx.Done():
			return detections
		}
	}
}

func (a *napAnalyzer) drainError() error {
	select {
	case err := <-a.errCh:
		if err == nil || errors.Is(err, io.EOF) {
			return nil
		}
		if s, ok := status.FromError(err); ok {
			switch s.Code() {
			case codes.Canceled:
				return nil
			case codes.Unavailable:
				return fmt.Errorf("vad: receive speech event: %w: %w", err, ErrAdapterUnavailable)
			}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return fmt.Errorf("vad: receive speech event: %w", err)
	default:
		return nil
	}
}

func speechEventToDetection(evt *napv1.SpeechEvent) Detection {
	det := Detection{
		Confidence: evt.GetConfidence(),
		Metadata:   streammanager.CopyMetadata(evt.GetMetadata()),
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

func marshalAudioFormat(format eventbus.AudioFormat) *napv1.AudioFormat {
	return &napv1.AudioFormat{
		Encoding:        string(format.Encoding),
		SampleRate:      uint32(format.SampleRate),
		Channels:        uint32(format.Channels),
		BitDepth:        uint32(format.BitDepth),
		FrameDurationMs: uint32(format.FrameDuration / time.Millisecond),
	}
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
