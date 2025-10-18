package observability

import (
	"bytes"
	"fmt"
	"sort"
	"time"

	"github.com/nupi-ai/nupi/internal/contentpipeline"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

// PrometheusExporter renders observability metrics in Prometheus text format.
type PrometheusExporter struct {
	bus      *eventbus.Bus
	counter  *EventCounter
	pipeline PipelineMetricsProvider
}

// PipelineMetricsProvider exposes a Metrics method compatible with content pipeline services.
type PipelineMetricsProvider interface {
	Metrics() contentpipeline.Metrics
}

// NewPrometheusExporter constructs an exporter backed by the provided bus and event counter.
func NewPrometheusExporter(bus *eventbus.Bus, counter *EventCounter) *PrometheusExporter {
	return &PrometheusExporter{
		bus:     bus,
		counter: counter,
	}
}

// WithPipeline enables exporting content pipeline metrics.
func (e *PrometheusExporter) WithPipeline(provider PipelineMetricsProvider) {
	e.pipeline = provider
}

// Export produces the metrics payload in Prometheus' text exposition format.
func (e *PrometheusExporter) Export() []byte {
	var buf bytes.Buffer

	e.writeEventCounters(&buf)
	e.writeBusMetrics(&buf)
	e.writePipelineMetrics(&buf)

	return buf.Bytes()
}

func (e *PrometheusExporter) writeEventCounters(buf *bytes.Buffer) {
	if e.counter == nil {
		return
	}

	counts := e.counter.Snapshot()
	if len(counts) == 0 {
		return
	}

	buf.WriteString("# HELP nupi_eventbus_events_total Total number of published events per topic.\n")
	buf.WriteString("# TYPE nupi_eventbus_events_total counter\n")

	topics := make([]string, 0, len(counts))
	for topic := range counts {
		topics = append(topics, string(topic))
	}
	sort.Strings(topics)
	for _, topicName := range topics {
		value := counts[eventbus.Topic(topicName)]
		buf.WriteString(fmt.Sprintf("nupi_eventbus_events_total{topic=%q} %d\n", topicName, value))
	}
}

func (e *PrometheusExporter) writeBusMetrics(buf *bytes.Buffer) {
	if e.bus == nil {
		return
	}

	metrics := e.bus.Metrics()

	buf.WriteString("# HELP nupi_eventbus_publish_total Total number of events published on the bus.\n")
	buf.WriteString("# TYPE nupi_eventbus_publish_total counter\n")
	buf.WriteString(fmt.Sprintf("nupi_eventbus_publish_total %d\n", metrics.PublishTotal))

	buf.WriteString("# HELP nupi_eventbus_dropped_total Total number of events dropped by the bus.\n")
	buf.WriteString("# TYPE nupi_eventbus_dropped_total counter\n")
	buf.WriteString(fmt.Sprintf("nupi_eventbus_dropped_total %d\n", metrics.DroppedTotal))

	buf.WriteString("# HELP nupi_eventbus_latency_seconds Event bus publish latency quantiles.\n")
	buf.WriteString("# TYPE nupi_eventbus_latency_seconds summary\n")
	buf.WriteString(fmt.Sprintf("nupi_eventbus_latency_seconds{quantile=\"0.50\"} %.9f\n", durationSeconds(metrics.LatencyP50)))
	buf.WriteString(fmt.Sprintf("nupi_eventbus_latency_seconds{quantile=\"0.99\"} %.9f\n", durationSeconds(metrics.LatencyP99)))
}

func (e *PrometheusExporter) writePipelineMetrics(buf *bytes.Buffer) {
	if e.pipeline == nil {
		return
	}
	metrics := e.pipeline.Metrics()

	buf.WriteString("# HELP nupi_pipeline_processed_total Total number of messages processed by the content pipeline.\n")
	buf.WriteString("# TYPE nupi_pipeline_processed_total counter\n")
	buf.WriteString(fmt.Sprintf("nupi_pipeline_processed_total %d\n", metrics.Processed))

	buf.WriteString("# HELP nupi_pipeline_errors_total Total number of content pipeline processing errors.\n")
	buf.WriteString("# TYPE nupi_pipeline_errors_total counter\n")
	buf.WriteString(fmt.Sprintf("nupi_pipeline_errors_total %d\n", metrics.Errors))
}

func durationSeconds(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return d.Seconds()
}
