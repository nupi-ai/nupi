package observability

import (
	"context"
	"strings"
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
