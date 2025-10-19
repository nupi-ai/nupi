package observability

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nupi-ai/nupi/internal/contentpipeline"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

func TestEventCounterSnapshot(t *testing.T) {
	counter := NewEventCounter()

	counter.OnPublish(eventbus.Envelope{Topic: eventbus.TopicSessionsOutput})
	counter.OnPublish(eventbus.Envelope{Topic: eventbus.TopicSessionsOutput})
	counter.OnPublish(eventbus.Envelope{Topic: eventbus.TopicPipelineCleaned})

	snapshot := counter.Snapshot()

	if snapshot[eventbus.TopicSessionsOutput] != 2 {
		t.Fatalf("expected sessions.output count 2, got %d", snapshot[eventbus.TopicSessionsOutput])
	}
	if snapshot[eventbus.TopicPipelineCleaned] != 1 {
		t.Fatalf("expected pipeline.cleaned count 1, got %d", snapshot[eventbus.TopicPipelineCleaned])
	}
	if _, exists := snapshot[""]; exists {
		t.Fatalf("expected empty topic to be ignored in snapshot")
	}
}

func TestPrometheusExporter(t *testing.T) {
	bus := eventbus.New()
	counter := NewEventCounter()
	bus.AddObserver(counter)

	exporter := NewPrometheusExporter(bus, counter)
	exporter.WithPipeline(pipelineStub{})
	exporter.WithAudioMetrics(func() AudioMetricsSnapshot {
		return AudioMetricsSnapshot{
			AudioIngressBytes:  1024,
			STTSegments:        12,
			TTSActiveStreams:   3,
			SpeechBargeInTotal: 4,
		}
	})

	bus.Publish(context.Background(), eventbus.Envelope{Topic: eventbus.TopicSessionsOutput})
	bus.Publish(context.Background(), eventbus.Envelope{Topic: eventbus.TopicPipelineCleaned})

	metrics := string(exporter.Export())

	if !strings.Contains(metrics, `nupi_eventbus_events_total{topic="pipeline.cleaned"} 1`) {
		t.Fatalf("expected pipeline.cleaned counter in metrics output:\n%s", metrics)
	}
	if !strings.Contains(metrics, `nupi_eventbus_publish_total 2`) {
		t.Fatalf("expected publish_total counter in metrics output:\n%s", metrics)
	}
	if !strings.Contains(metrics, `nupi_pipeline_processed_total 42`) {
		t.Fatalf("expected pipeline processed counter in metrics output:\n%s", metrics)
	}
	if !strings.Contains(metrics, `nupi_pipeline_errors_total 3`) {
		t.Fatalf("expected pipeline errors counter in metrics output:\n%s", metrics)
	}
	if !strings.Contains(metrics, `nupi_audio_ingress_bytes_total 1024`) {
		t.Fatalf("expected audio ingress bytes counter in metrics output:\n%s", metrics)
	}
	if !strings.Contains(metrics, `nupi_stt_segments_total 12`) {
		t.Fatalf("expected STT segments counter in metrics output:\n%s", metrics)
	}
	if !strings.Contains(metrics, `nupi_tts_active_streams 3`) {
		t.Fatalf("expected TTS active streams gauge in metrics output:\n%s", metrics)
	}
	if !strings.Contains(metrics, `nupi_speech_barge_in_total 4`) {
		t.Fatalf("expected speech barge-in counter in metrics output:\n%s", metrics)
	}
	if !strings.Contains(metrics, `nupi_eventbus_latency_seconds{quantile="0.50"}`) {
		t.Fatalf("expected latency quantile metric in output:\n%s", metrics)
	}
}

type pipelineStub struct{}

func (pipelineStub) Metrics() contentpipeline.Metrics {
	return contentpipeline.Metrics{
		Processed: 42,
		Errors:    3,
	}
}

func TestPrometheusExporterConcurrency(t *testing.T) {
	bus := eventbus.New()
	counter := NewEventCounter()
	bus.AddObserver(counter)

	var audioBytes atomic.Uint64

	exporter := NewPrometheusExporter(bus, counter)
	exporter.WithAudioMetrics(func() AudioMetricsSnapshot {
		return AudioMetricsSnapshot{
			AudioIngressBytes:  audioBytes.Load(),
			STTSegments:        12,
			TTSActiveStreams:   3,
			SpeechBargeInTotal: 4,
		}
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			audioBytes.Add(256)
			bus.Publish(context.Background(), eventbus.Envelope{
				Topic: eventbus.TopicSessionsOutput,
			})
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if payload := exporter.Export(); len(payload) == 0 {
				t.Errorf("expected metrics output to be non-empty")
			}
		}
	}()

	wg.Wait()
}
