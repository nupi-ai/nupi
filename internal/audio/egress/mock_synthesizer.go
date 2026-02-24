package egress

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/nupi-ai/nupi/internal/audio/adapterutil"
	"github.com/nupi-ai/nupi/internal/constants"
)

const (
	defaultMockFrequency = 440.0
	defaultMockDuration  = 500 * time.Millisecond
	defaultSampleRate    = 16000
)

func newMockSynthesizer(params SessionParams) (Synthesizer, error) {
	cfg := adapterutil.NewConfigReader(params.Config)
	sampleRate := params.Format.SampleRate
	if sampleRate <= 0 {
		sampleRate = defaultSampleRate
	}
	return &mockSynthesizer{
		sampleRate: sampleRate,
		frequency:  cfg.Float64("frequency", defaultMockFrequency),
		duration:   cfg.Duration("duration_ms", defaultMockDuration),
		metadata:   cfg.StringMap("metadata"),
	}, nil
}

type mockSynthesizer struct {
	sampleRate int
	frequency  float64
	duration   time.Duration
	metadata   map[string]string
}

func (m *mockSynthesizer) Speak(ctx context.Context, req SpeakRequest) ([]SynthesisChunk, error) {
	sampleRate := m.sampleRate
	if sampleRate <= 0 {
		sampleRate = defaultSampleRate
	}
	samples := int(float64(sampleRate) * m.duration.Seconds())
	if samples <= 0 {
		samples = sampleRate / 2
	}
	buffer := make([]byte, samples*2) // pcm16 mono
	for i := 0; i < samples; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		theta := 2 * math.Pi * float64(i) * m.frequency / float64(sampleRate)
		value := int16(32767 * math.Sin(theta))
		binary.LittleEndian.PutUint16(buffer[i*2:], uint16(value))
	}

	meta := map[string]string{
		constants.MetadataKeyAdapter: "mock",
	}
	for k, v := range m.metadata {
		meta[k] = v
	}
	meta[constants.MetadataKeyTextLength] = intToString(len(req.Text))

	return []SynthesisChunk{{
		Data:     buffer,
		Duration: m.duration,
		Final:    true,
		Metadata: meta,
	}}, nil
}

func (m *mockSynthesizer) Close(context.Context) ([]SynthesisChunk, error) {
	return nil, nil
}

func intToString(v int) string {
	return fmt.Sprintf("%d", v)
}
