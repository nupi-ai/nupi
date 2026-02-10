package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/napdial"
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
	transport := strings.TrimSpace(endpoint.Transport)
	if transport == "" {
		transport = "process"
	}
	switch transport {
	case "grpc", "process":
	default:
		return nil, fmt.Errorf("tts: unsupported transport %q for adapter %s", endpoint.Transport, endpoint.AdapterID)
	}
	address := strings.TrimSpace(endpoint.Address)
	if address == "" {
		return nil, fmt.Errorf("tts: adapter %s missing address", endpoint.AdapterID)
	}

	dialOpts := napdial.DialOptions(ctx)
	conn, err := grpc.DialContext(ctx, address, dialOpts...)
	if err != nil {
		if s, ok := status.FromError(err); ok && s.Code() == codes.Unavailable {
			return nil, fmt.Errorf("tts: dial adapter %s: %w: %w", endpoint.AdapterID, err, ErrAdapterUnavailable)
		}
		return nil, fmt.Errorf("tts: dial adapter %s: %w", endpoint.AdapterID, err)
	}

	return &napSynthesizer{
		params:   params,
		conn:     conn,
		endpoint: endpoint,
	}, nil
}

func (s *napSynthesizer) Speak(ctx context.Context, req SpeakRequest) ([]SynthesisChunk, error) {
	client := napv1.NewTextToSpeechServiceClient(s.conn)

	configJSON := ""
	if len(s.params.Config) > 0 {
		raw, err := json.Marshal(s.params.Config)
		if err != nil {
			return nil, fmt.Errorf("tts: marshal adapter config: %w", err)
		}
		configJSON = string(raw)
	}

	grpcReq := &napv1.StreamSynthesisRequest{
		SessionId:  req.SessionID,
		StreamId:   req.StreamID,
		Text:       req.Text,
		ConfigJson: configJSON,
		Metadata:   copyMetadata(req.Metadata),
	}

	stream, err := client.StreamSynthesis(ctx, grpcReq)
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.Unavailable {
			return nil, fmt.Errorf("tts: open synthesis stream: %w: %w", err, ErrAdapterUnavailable)
		}
		return nil, fmt.Errorf("tts: open synthesis stream: %w", err)
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
			if st, ok := status.FromError(err); ok && st.Code() == codes.Unavailable {
				return chunks, fmt.Errorf("tts: receive synthesis chunk: %w: %w", err, ErrAdapterUnavailable)
			}
			return chunks, fmt.Errorf("tts: receive synthesis chunk: %w", err)
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

		meta := mergeSynthMeta(copyMetadata(chunk.GetMetadata()), respMeta)
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
		return copyMetadata(respMeta)
	}
	merged := make(map[string]string, len(chunkMeta)+len(respMeta))
	for k, v := range respMeta {
		merged[k] = v
	}
	for k, v := range chunkMeta {
		merged[k] = v
	}
	return merged
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
