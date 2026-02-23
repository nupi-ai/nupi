package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	"github.com/nupi-ai/nupi/internal/audio/napstream"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/mapper"
	"github.com/nupi-ai/nupi/internal/napdial"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type napSynthesizer struct {
	params   SessionParams
	conn     *grpc.ClientConn
	endpoint configstore.AdapterEndpoint
}

func newNAPSynthesizer(ctx context.Context, params SessionParams, endpoint configstore.AdapterEndpoint) (Synthesizer, error) {
	conn, err := napdial.DialAdapter(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("tts: %w", err)
	}

	return &napSynthesizer{
		params:   params,
		conn:     conn,
		endpoint: endpoint,
	}, nil
}

func (s *napSynthesizer) Speak(ctx context.Context, req SpeakRequest) ([]SynthesisChunk, error) {
	client := napv1.NewTextToSpeechServiceClient(s.conn)

	configJSON, err := napstream.MarshalConfigJSON("tts", s.params.Config)
	if err != nil {
		return nil, err
	}

	grpcReq := &napv1.StreamSynthesisRequest{
		SessionId:  req.SessionID,
		StreamId:   req.StreamID,
		Text:       req.Text,
		ConfigJson: configJSON,
		Metadata:   maputil.Clone(req.Metadata),
	}

	stream, err := client.StreamSynthesis(ctx, grpcReq)
	if err != nil {
		return nil, napstream.WrapGRPCError("tts", "open synthesis stream", err, ErrAdapterUnavailable)
	}

	var (
		chunks      []SynthesisChunk
		pendingMeta map[string]string // metadata from status-only responses before first chunk
	)
	for {
		resp, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			if isStreamCancelled(err) {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return chunks, ctxErr
				}
				// Server-side cancel — not initiated by us, treat as real error.
				return chunks, fmt.Errorf("tts: receive synthesis chunk: %w", err)
			}
			return chunks, napstream.WrapGRPCError("tts", "receive synthesis chunk", err, ErrAdapterUnavailable)
		}

		if resp.GetStatus() == napv1.SynthesisStatus_SYNTHESIS_STATUS_ERROR {
			return chunks, fmt.Errorf("tts: adapter error: %s", resp.GetErrorMessage())
		}

		chunk := resp.GetChunk()
		respMeta := resp.GetMetadata()

		if chunk == nil {
			// Status-only response — buffer or attach metadata.
			// Later values override earlier ones (respMeta wins).
			if len(respMeta) > 0 {
				if len(chunks) > 0 {
					last := &chunks[len(chunks)-1]
					last.Metadata = mergeSynthMeta(respMeta, last.Metadata)
				} else {
					pendingMeta = mergeSynthMeta(respMeta, pendingMeta)
				}
			}
			continue
		}

		meta := mergeSynthMeta(maputil.Clone(chunk.GetMetadata()), respMeta)
		if len(pendingMeta) > 0 {
			meta = mergeSynthMeta(meta, pendingMeta)
			pendingMeta = nil
		}

		chunks = append(chunks, SynthesisChunk{
			Data:     chunk.GetData(),
			Duration: time.Duration(chunk.GetDurationMs()) * time.Millisecond,
			Final:    chunk.GetLast(),
			Metadata: meta,
		})
	}

	// Emit buffered metadata as a metadata-only chunk if no audio was produced.
	if len(pendingMeta) > 0 && len(chunks) == 0 {
		chunks = append(chunks, SynthesisChunk{
			Final:    true,
			Metadata: pendingMeta,
		})
	}

	// Ensure the last chunk is marked Final so consumers don't hang.
	// The stream has ended (EOF or terminal status); if no chunk carried
	// last=true, force it on the last emitted chunk.
	if n := len(chunks); n > 0 && !chunks[n-1].Final {
		chunks[n-1].Final = true
	}

	return chunks, nil
}

func (s *napSynthesizer) Close(ctx context.Context) ([]SynthesisChunk, error) {
	if s.conn != nil {
		if err := s.conn.Close(); err != nil {
			return nil, fmt.Errorf("tts: close connection: %w", err)
		}
	}
	return nil, nil
}

// mergeSynthMeta merges two metadata maps. The first argument (primary)
// takes precedence over the second argument (secondary) on key conflicts.
func mergeSynthMeta(chunkMeta, respMeta map[string]string) map[string]string {
	if len(respMeta) == 0 {
		return chunkMeta
	}
	if len(chunkMeta) == 0 {
		return maputil.Clone(respMeta)
	}
	return mapper.MergeStringMaps(respMeta, chunkMeta)
}

func isStreamCancelled(err error) bool {
	if err == nil {
		return false
	}
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

// ContextWithDialer attaches a custom gRPC dialer to the context for testing.
func ContextWithDialer(ctx context.Context, dialer func(context.Context, string) (net.Conn, error)) context.Context {
	return napdial.ContextWithDialer(ctx, dialer)
}
