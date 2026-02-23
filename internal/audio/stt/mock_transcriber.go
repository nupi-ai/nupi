package stt

import (
	"context"
	"fmt"

	"github.com/nupi-ai/nupi/internal/audio/adapterutil"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

const defaultMockConfidence = 0.6

func newMockTranscriber(params SessionParams) (Transcriber, error) {
	cfg := adapterutil.NewConfigReader(params.Config)
	return &mockTranscriber{
		text:        cfg.String("text"),
		phrases:     cfg.StringSlice("phrases"),
		confidence:  cfg.Float32("confidence", defaultMockConfidence),
		emitPartial: cfg.Bool("emit_partial", false),
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
