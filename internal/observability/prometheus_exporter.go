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
	bus             *eventbus.Bus
	counter         *EventCounter
	pipeline        PipelineMetricsProvider
	audio           func() AudioMetricsSnapshot
	pluginWarnings  PluginWarningsProvider
}

// PipelineMetricsProvider exposes a Metrics method compatible with content pipeline services.
type PipelineMetricsProvider interface {
	Metrics() contentpipeline.Metrics
}

// PluginWarningsProvider exposes the count of plugin discovery warnings.
type PluginWarningsProvider interface {
	WarningsCount() int
}

// AudioMetricsSnapshot represents a point-in-time snapshot of audio-related counters.
type AudioMetricsSnapshot struct {
	AudioIngressBytes  uint64
	STTSegments        uint64
	TTSActiveStreams   int64
	SpeechBargeInTotal uint64
	VADDetections      uint64
	VADRetryAttempts   uint64
	VADRetryFailures   uint64
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

// WithAudioMetrics enables exporting snapshot-based audio metrics via the exporter.
func (e *PrometheusExporter) WithAudioMetrics(provider func() AudioMetricsSnapshot) {
	e.audio = provider
}

// WithPluginWarnings enables exporting plugin discovery warning metrics.
func (e *PrometheusExporter) WithPluginWarnings(provider PluginWarningsProvider) {
	e.pluginWarnings = provider
}

// Export produces the metrics payload in Prometheus' text exposition format.
func (e *PrometheusExporter) Export() []byte {
	var buf bytes.Buffer

	e.writeEventCounters(&buf)
	e.writeBusMetrics(&buf)
	e.writePipelineMetrics(&buf)
	e.writeAudioMetrics(&buf)
	e.writePluginWarningsMetrics(&buf)

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

func (e *PrometheusExporter) writeAudioMetrics(buf *bytes.Buffer) {
	if e.audio == nil {
		return
	}

	snapshot := e.audio()

	buf.WriteString("# HELP nupi_audio_ingress_bytes_total Total number of bytes received by audio ingress.\n")
	buf.WriteString("# TYPE nupi_audio_ingress_bytes_total counter\n")
	buf.WriteString(fmt.Sprintf("nupi_audio_ingress_bytes_total %d\n", snapshot.AudioIngressBytes))

	buf.WriteString("# HELP nupi_stt_segments_total Total number of audio segments processed by STT.\n")
	buf.WriteString("# TYPE nupi_stt_segments_total counter\n")
	buf.WriteString(fmt.Sprintf("nupi_stt_segments_total %d\n", snapshot.STTSegments))

	buf.WriteString("# HELP nupi_tts_active_streams Number of active TTS playback streams.\n")
	buf.WriteString("# TYPE nupi_tts_active_streams gauge\n")
	buf.WriteString(fmt.Sprintf("nupi_tts_active_streams %d\n", snapshot.TTSActiveStreams))

	buf.WriteString("# HELP nupi_speech_barge_in_total Total number of speech barge-in events emitted.\n")
	buf.WriteString("# TYPE nupi_speech_barge_in_total counter\n")
	buf.WriteString(fmt.Sprintf("nupi_speech_barge_in_total %d\n", snapshot.SpeechBargeInTotal))

	buf.WriteString("# HELP nupi_vad_detections_total Total number of VAD detections published.\n")
	buf.WriteString("# TYPE nupi_vad_detections_total counter\n")
	buf.WriteString(fmt.Sprintf("nupi_vad_detections_total %d\n", snapshot.VADDetections))

	buf.WriteString("# HELP nupi_vad_retry_attempts_total Total number of VAD adapter retry attempts.\n")
	buf.WriteString("# TYPE nupi_vad_retry_attempts_total counter\n")
	buf.WriteString(fmt.Sprintf("nupi_vad_retry_attempts_total %d\n", snapshot.VADRetryAttempts))

	buf.WriteString("# HELP nupi_vad_retry_failures_total Total number of VAD adapter retry attempts that failed.\n")
	buf.WriteString("# TYPE nupi_vad_retry_failures_total counter\n")
	buf.WriteString(fmt.Sprintf("nupi_vad_retry_failures_total %d\n", snapshot.VADRetryFailures))
}

func (e *PrometheusExporter) writePluginWarningsMetrics(buf *bytes.Buffer) {
	if e.pluginWarnings == nil {
		return
	}

	count := e.pluginWarnings.WarningsCount()

	buf.WriteString("# HELP nupi_plugins_discovery_warnings Current number of plugins skipped during last discovery due to manifest errors.\n")
	buf.WriteString("# TYPE nupi_plugins_discovery_warnings gauge\n")
	buf.WriteString(fmt.Sprintf("nupi_plugins_discovery_warnings %d\n", count))
}

func durationSeconds(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return d.Seconds()
}
