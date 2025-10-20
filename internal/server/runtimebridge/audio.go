package runtimebridge

import (
	"errors"

	"github.com/nupi-ai/nupi/internal/audio/egress"
	"github.com/nupi-ai/nupi/internal/audio/ingress"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/server"
)

// AudioIngressProvider wraps ingress.Service into the API-facing AudioCaptureProvider interface.
func AudioIngressProvider(service *ingress.Service) server.AudioCaptureProvider {
	if service == nil {
		return nil
	}
	return &audioIngressAdapter{service: service}
}

type audioIngressAdapter struct {
	service *ingress.Service
}

func (a *audioIngressAdapter) OpenStream(sessionID, streamID string, format eventbus.AudioFormat, metadata map[string]string) (server.AudioCaptureStream, error) {
	stream, err := a.service.OpenStream(sessionID, streamID, format, metadata)
	if err != nil {
		if errors.Is(err, ingress.ErrStreamExists) {
			return nil, server.ErrAudioStreamExists
		}
		return nil, err
	}
	return &audioIngressStream{stream: stream}, nil
}

type audioIngressStream struct {
	stream *ingress.Stream
}

func (s *audioIngressStream) Write(p []byte) error {
	return s.stream.Write(p)
}

func (s *audioIngressStream) Close() error {
	return s.stream.Close()
}

// AudioEgressController wraps egress.Service for use by API handlers.
func AudioEgressController(service *egress.Service) server.AudioPlaybackController {
	if service == nil {
		return nil
	}
	return &audioEgressAdapter{service: service}
}

type audioEgressAdapter struct {
	service *egress.Service
}

func (a *audioEgressAdapter) DefaultStreamID() string {
	return a.service.DefaultStreamID()
}

func (a *audioEgressAdapter) PlaybackFormat() eventbus.AudioFormat {
	return a.service.PlaybackFormat()
}

func (a *audioEgressAdapter) Interrupt(sessionID, streamID, reason string, metadata map[string]string) {
	a.service.Interrupt(sessionID, streamID, reason, metadata)
}
