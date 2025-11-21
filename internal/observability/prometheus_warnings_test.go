package observability

import (
	"strings"
	"testing"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// mockPluginWarningsProvider implements PluginWarningsProvider for testing.
type mockPluginWarningsProvider struct {
	count int
}

func (m *mockPluginWarningsProvider) WarningsCount() int {
	return m.count
}

func TestPrometheusExporterWithPluginWarnings(t *testing.T) {
	bus := eventbus.New()
	counter := NewEventCounter()
	exporter := NewPrometheusExporter(bus, counter)

	// Test with warnings
	warnings := &mockPluginWarningsProvider{count: 3}
	exporter.WithPluginWarnings(warnings)

	output := string(exporter.Export())

	// Verify metric is present with correct value
	if !strings.Contains(output, "nupi_plugins_discovery_warnings 3") {
		t.Errorf("Expected metric 'nupi_plugins_discovery_warnings 3' in output, got:\n%s", output)
	}

	// Verify metric metadata
	if !strings.Contains(output, "# HELP nupi_plugins_discovery_warnings") {
		t.Error("Expected HELP comment for plugin warnings metric")
	}
	if !strings.Contains(output, "# TYPE nupi_plugins_discovery_warnings gauge") {
		t.Error("Expected TYPE gauge for plugin warnings metric")
	}
}

func TestPrometheusExporterWithoutPluginWarnings(t *testing.T) {
	bus := eventbus.New()
	counter := NewEventCounter()
	exporter := NewPrometheusExporter(bus, counter)

	// Don't set plugin warnings provider
	output := string(exporter.Export())

	// Verify metric is not present
	if strings.Contains(output, "nupi_plugins_discovery_warnings") {
		t.Errorf("Expected no plugin warnings metric in output, but found it:\n%s", output)
	}
}

func TestPrometheusExporterWithZeroWarnings(t *testing.T) {
	bus := eventbus.New()
	counter := NewEventCounter()
	exporter := NewPrometheusExporter(bus, counter)

	// Test with zero warnings
	warnings := &mockPluginWarningsProvider{count: 0}
	exporter.WithPluginWarnings(warnings)

	output := string(exporter.Export())

	// Verify metric is present with zero value
	if !strings.Contains(output, "nupi_plugins_discovery_warnings 0") {
		t.Errorf("Expected metric 'nupi_plugins_discovery_warnings 0' in output, got:\n%s", output)
	}
}
