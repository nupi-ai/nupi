package vad

import (
	"context"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	"github.com/nupi-ai/nupi/internal/audio/napstream"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/mapper"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
	"google.golang.org/grpc"
)

type napAnalyzer struct {
	params SessionParams
	client *napstream.BidiClient[napv1.DetectSpeechRequest, napv1.SpeechEvent, Detection]
}

func newNAPAnalyzer(ctx context.Context, params SessionParams, endpoint configstore.AdapterEndpoint) (Analyzer, error) {
	client, err := napstream.NewBidiClient(
		ctx,
		endpoint,
		"vad",
		"open detect speech stream",
		"receive speech event",
		ErrAdapterUnavailable,
		func(streamCtx context.Context, conn *grpc.ClientConn) (napv1.VoiceActivityDetectionService_DetectSpeechClient, error) {
			return napv1.NewVoiceActivityDetectionServiceClient(conn).DetectSpeech(streamCtx)
		},
		speechEventToDetection,
		16,
		1,
		vadResponseGracePeriod,
	)
	if err != nil {
		return nil, err
	}

	a := &napAnalyzer{
		params: params,
		client: client,
	}

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

	if err := client.Send(initReq, "send init request"); err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), constants.Duration2Seconds)
		a.Close(closeCtx)
		closeCancel()
		return nil, err
	}

	return a, nil
}

func (a *napAnalyzer) OnSegment(_ context.Context, segment eventbus.AudioIngressSegmentEvent) ([]Detection, error) {
	req := &napv1.DetectSpeechRequest{
		SessionId: a.params.SessionID,
		StreamId:  a.params.StreamID,
		PcmData:   segment.Data,
		Format:    mapper.ToNAPAudioFormat(segment.Format),
		Timestamp: mapper.ToProtoTimestampChecked(segment.EndedAt),
	}

	if err := a.client.Send(req, "send segment"); err != nil {
		return nil, err
	}

	return a.client.Collect(segment.Last), a.client.DrainError()
}

func (a *napAnalyzer) Close(ctx context.Context) ([]Detection, error) {
	return a.client.Close(ctx, nil)
}

const vadResponseGracePeriod = constants.Duration10Milliseconds

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
