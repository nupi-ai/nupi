package stt

import (
	"context"
	"fmt"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

const defaultMockConfidence = 0.6

func newMockTranscriber(params SessionParams) (Transcriber, error) {
	cfg := params.Config
	return &mockTranscriber{
		text:        stringValue(cfg, "text"),
		phrases:     stringSlice(cfg, "phrases"),
		confidence:  floatValue(cfg, "confidence", defaultMockConfidence),
		emitPartial: boolValue(cfg, "emit_partial", false),
	}, nil
}

type mockTranscriber struct {
	text        string
	phrases     []string
	confidence  float32
	emitPartial bool
	index       int
}

func (m *mockTranscriber) OnSegment(_ context.Context, segment eventbus.AudioIngressSegmentEvent) ([]Transcription, error) {
	text := m.selectText(segment)
	if text == "" {
		return nil, nil
	}

	final := segment.Last
	if len(m.phrases) > 0 {
		allPhrasesEmitted := m.index >= len(m.phrases)
		if m.emitPartial {
			final = segment.Last && allPhrasesEmitted
		} else {
			final = allPhrasesEmitted
		}
	}

	transcript := Transcription{
		Text:       text,
		Confidence: m.confidence,
		Final:      final,
		StartedAt:  segment.StartedAt,
		EndedAt:    segment.EndedAt,
		Metadata: map[string]string{
			"adapter": "mock",
			"mode":    m.mode(),
		},
	}
	return []Transcription{transcript}, nil
}

func (m *mockTranscriber) Close(context.Context) ([]Transcription, error) {
	return nil, nil
}

func (m *mockTranscriber) selectText(segment eventbus.AudioIngressSegmentEvent) string {
	if len(m.phrases) > 0 {
		if m.index < len(m.phrases) {
			text := m.phrases[m.index]
			m.index++
			return text
		}
		if segment.Last {
			return m.phrases[len(m.phrases)-1]
		}
	}

	if text := segment.Metadata["mock_text"]; text != "" {
		return text
	}
	if m.text != "" {
		return m.text
	}
	return fmt.Sprintf("mock(seq=%d,len=%d)", segment.Sequence, len(segment.Data))
}

func (m *mockTranscriber) mode() string {
	switch {
	case len(m.phrases) > 0:
		return "phrases"
	case m.text != "":
		return "fixed"
	default:
		return "auto"
	}
}

func stringValue(cfg map[string]any, key string) string {
	if cfg == nil {
		return ""
	}
	if value, ok := cfg[key]; ok {
		switch v := value.(type) {
		case string:
			return v
		}
	}
	return ""
}

func stringSlice(cfg map[string]any, key string) []string {
	if cfg == nil {
		return nil
	}
	val, ok := cfg[key]
	if !ok {
		return nil
	}

	switch raw := val.(type) {
	case []any:
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			switch v := item.(type) {
			case string:
				out = append(out, v)
			}
		}
		return out
	case []string:
		return append([]string(nil), raw...)
	default:
		return nil
	}
}

func floatValue(cfg map[string]any, key string, def float32) float32 {
	if cfg == nil {
		return def
	}
	if value, ok := cfg[key]; ok {
		switch v := value.(type) {
		case float64:
			return float32(v)
		case float32:
			return v
		case int:
			return float32(v)
		}
	}
	return def
}

func boolValue(cfg map[string]any, key string, def bool) bool {
	if cfg == nil {
		return def
	}
	if value, ok := cfg[key]; ok {
		switch v := value.(type) {
		case bool:
			return v
		}
	}
	return def
}
