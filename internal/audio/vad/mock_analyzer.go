package vad

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/nupi-ai/nupi/internal/audio/adapterutil"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

const (
	defaultThreshold = 0.02
	defaultMinFrames = 3
)

func newMockAnalyzer(params SessionParams) (Analyzer, error) {
	cfg := adapterutil.NewConfigReader(params.Config)
	return &mockAnalyzer{
		threshold: cfg.Float64("threshold", defaultThreshold),
		minFrames: cfg.Int("min_frames", defaultMinFrames),
	}, nil
}

type mockAnalyzer struct {
	threshold float64
	minFrames int

	activeFrames   int
	inactiveFrames int
	lastState      bool
}

func (m *mockAnalyzer) OnSegment(_ context.Context, segment eventbus.AudioIngressSegmentEvent) ([]Detection, error) {
	if len(segment.Data) == 0 {
		return nil, nil
	}

	rms := rmsPCM16(segment.Data)
	active := rms >= m.threshold
	if active {
		m.activeFrames++
		m.inactiveFrames = 0
	} else {
		m.inactiveFrames++
		m.activeFrames = 0
	}

	var detections []Detection
	switch {
	case active && !m.lastState && m.activeFrames >= m.minFrames:
		m.lastState = true
		detections = append(detections, Detection{
			Active:     true,
			Confidence: float32(clamp(rms/defaultThreshold, 0, 1)),
			Metadata: map[string]string{
				"rms": formatFloat(rms),
			},
		})
	case !active && m.lastState && m.inactiveFrames >= m.minFrames:
		m.lastState = false
		detections = append(detections, Detection{
			Active:     false,
			Confidence: float32(1 - clamp(rms/defaultThreshold, 0, 1)),
			Metadata: map[string]string{
				"rms": formatFloat(rms),
			},
		})
	}
	return detections, nil
}

func (m *mockAnalyzer) Close(context.Context) ([]Detection, error) {
	if m.lastState {
		m.lastState = false
		return []Detection{{
			Active:     false,
			Confidence: 0.5,
		}}, nil
	}
	return nil, nil
}

func rmsPCM16(data []byte) float64 {
	if len(data) < 2 {
		return 0
	}
	var sum float64
	samples := len(data) / 2
	for i := 0; i < samples; i++ {
		value := int16(binary.LittleEndian.Uint16(data[i*2:]))
		norm := float64(value) / 32768.0
		sum += norm * norm
	}
	mean := sum / float64(samples)
	return math.Sqrt(mean)
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func formatFloat(v float64) string {
	return fmt.Sprintf("%.4f", v)
}
