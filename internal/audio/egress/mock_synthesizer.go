package egress

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

const (
	defaultMockFrequency = 440.0
	defaultMockDuration  = 500 * time.Millisecond
	defaultSampleRate    = 16000
)

func newMockSynthesizer(params SessionParams) (Synthesizer, error) {
	cfg := params.Config
	sampleRate := params.Format.SampleRate
	if sampleRate <= 0 {
		sampleRate = defaultSampleRate
	}
	return &mockSynthesizer{
		sampleRate: sampleRate,
		frequency: floatConfig(cfg, "frequency", defaultMockFrequency),
		duration:  durationConfig(cfg, "duration_ms", defaultMockDuration),
		metadata:  mapConfig(cfg, "metadata"),
	}, nil
}

type mockSynthesizer struct {
	sampleRate int
	frequency float64
	duration  time.Duration
	metadata  map[string]string
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
		"adapter": "mock",
	}
	for k, v := range m.metadata {
		meta[k] = v
	}
	meta["text_length"] = intToString(len(req.Text))

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

func floatConfig(cfg map[string]any, key string, def float64) float64 {
	if cfg == nil {
		return def
	}
	if v, ok := cfg[key]; ok {
		switch value := v.(type) {
		case float64:
			return value
		case float32:
			return float64(value)
		case int:
			return float64(value)
		}
	}
	return def
}

func durationConfig(cfg map[string]any, key string, def time.Duration) time.Duration {
	if cfg == nil {
		return def
	}
	if v, ok := cfg[key]; ok {
		switch value := v.(type) {
		case float64:
			return time.Duration(value) * time.Millisecond
		case float32:
			return time.Duration(value) * time.Millisecond
		case int:
			return time.Duration(value) * time.Millisecond
		}
	}
	return def
}

func mapConfig(cfg map[string]any, key string) map[string]string {
	if cfg == nil {
		return nil
	}
	val, ok := cfg[key]
	if !ok {
		return nil
	}
	out := make(map[string]string)
	switch raw := val.(type) {
	case map[string]any:
		for k, v := range raw {
			if str, ok := v.(string); ok {
				out[k] = str
			}
		}
	case map[string]string:
		for k, v := range raw {
			out[k] = v
		}
	}
	return out
}

func intToString(v int) string {
	return fmt.Sprintf("%d", v)
}
