package stt

import (
	"context"
	"time"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	"github.com/nupi-ai/nupi/internal/audio/napstream"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/mapper"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
	"google.golang.org/grpc"
)

type napTranscriber struct {
	params SessionParams
	client *napstream.BidiClient[napv1.StreamTranscriptionRequest, napv1.Transcript, Transcription]
}

func newNAPTranscriber(ctx context.Context, params SessionParams, endpoint configstore.AdapterEndpoint) (Transcriber, error) {
	client, err := napstream.NewBidiClient(
		ctx,
		endpoint,
		"stt",
		"open transcription stream",
		"receive transcript",
		ErrAdapterUnavailable,
		func(streamCtx context.Context, conn *grpc.ClientConn) (napv1.SpeechToTextService_StreamTranscriptionClient, error) {
			return napv1.NewSpeechToTextServiceClient(conn).StreamTranscription(streamCtx)
		},
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
		16,
		1,
		sttResponseGracePeriod,
	)
	if err != nil {
		return nil, err
	}

	t := &napTranscriber{
		params: params,
		client: client,
	}

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

	if err := client.Send(initReq, "send init request"); err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), constants.Duration2Seconds)
		t.Close(closeCtx)
		closeCancel()
		return nil, err
	}

	return t, nil
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

	if err := t.client.Send(req, "send segment"); err != nil {
		return nil, err
	}

	return t.client.Collect(segment.Last), t.client.DrainError()
}

func (t *napTranscriber) Close(ctx context.Context) ([]Transcription, error) {
	flushReq := &napv1.StreamTranscriptionRequest{
		SessionId: t.params.SessionID,
		StreamId:  t.params.StreamID,
		Flush:     true,
	}
	return t.client.Close(ctx, func() {
		_ = t.client.RawSend(flushReq) // Ignore error; CloseSend below handles final state.
	})
}

const sttResponseGracePeriod = constants.Duration10Milliseconds
