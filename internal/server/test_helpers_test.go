package server

import (
	"errors"

	"github.com/nupi-ai/nupi/internal/audio/egress"
	"github.com/nupi-ai/nupi/internal/audio/ingress"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

func newTestAudioIngressProvider(svc *ingress.Service) AudioCaptureProvider {
	if svc == nil {
		return nil
	}
	return &testAudioIngressProvider{service: svc}
}

type testAudioIngressProvider struct {
	service *ingress.Service
}

func (p *testAudioIngressProvider) OpenStream(sessionID, streamID string, format eventbus.AudioFormat, metadata map[string]string) (AudioCaptureStream, error) {
	stream, err := p.service.OpenStream(sessionID, streamID, format, metadata)
	if err != nil {
		if errors.Is(err, ingress.ErrStreamExists) {
			return nil, ErrAudioStreamExists
		}
		return nil, err
	}
	return &testAudioIngressStream{stream: stream}, nil
}

type testAudioIngressStream struct {
	stream *ingress.Stream
}

func (s *testAudioIngressStream) Write(p []byte) error {
	return s.stream.Write(p)
}

func (s *testAudioIngressStream) Close() error {
	return s.stream.Close()
}

func newTestAudioEgressController(svc *egress.Service) AudioPlaybackController {
	if svc == nil {
		return nil
	}
	return &testAudioEgressController{service: svc}
}

type testAudioEgressController struct {
	service *egress.Service
}

func (c *testAudioEgressController) DefaultStreamID() string {
	return c.service.DefaultStreamID()
}

func (c *testAudioEgressController) PlaybackFormat() eventbus.AudioFormat {
	return c.service.PlaybackFormat()
}

func (c *testAudioEgressController) Interrupt(sessionID, streamID, reason string, metadata map[string]string) {
	c.service.Interrupt(sessionID, streamID, reason, metadata)
}
