package main

import (
	"strings"
	"testing"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/server"
)

func TestFormatCapabilitiesProtoJSON_ReadyNoDiagnostics(t *testing.T) {
	caps := []*apiv1.AudioCapability{
		{
			StreamId:    "mic",
			Ready:       true,
			Recommended: true,
			Diagnostics: "",
			Format: &apiv1.AudioFormat{
				Encoding:        "pcm_s16le",
				SampleRate:      16000,
				Channels:        1,
				BitDepth:        16,
				FrameDurationMs: 20,
			},
		},
	}

	result := formatCapabilitiesProtoJSON(caps)
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}

	entry := result[0]
	if entry["stream_id"] != "mic" {
		t.Fatalf("expected stream_id=mic, got %v", entry["stream_id"])
	}
	if entry["ready"] != true {
		t.Fatalf("expected ready=true, got %v", entry["ready"])
	}
	if entry["recommended"] != true {
		t.Fatalf("expected recommended=true, got %v", entry["recommended"])
	}
	if entry["diagnostics"] != "" {
		t.Fatalf("expected diagnostics=\"\" when ready, got %v", entry["diagnostics"])
	}
	if entry["encoding"] != "pcm_s16le" {
		t.Fatalf("expected encoding=pcm_s16le, got %v", entry["encoding"])
	}
	if entry["sample_rate"] != uint32(16000) {
		t.Fatalf("expected sample_rate=16000, got %v", entry["sample_rate"])
	}
}

func TestFormatCapabilitiesProtoJSON_NotReadyWithDiagnostics(t *testing.T) {
	caps := []*apiv1.AudioCapability{
		{
			StreamId:    "mic",
			Ready:       false,
			Recommended: true,
			Diagnostics: server.DiagnosticsCaptureUnavailable,
			Format: &apiv1.AudioFormat{
				SampleRate: 16000,
				Channels:   1,
				BitDepth:   16,
			},
		},
	}

	result := formatCapabilitiesProtoJSON(caps)
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}

	entry := result[0]
	if entry["ready"] != false {
		t.Fatalf("expected ready=false, got %v", entry["ready"])
	}
	if entry["diagnostics"] != server.DiagnosticsCaptureUnavailable {
		t.Fatalf("expected diagnostics string, got %v", entry["diagnostics"])
	}
	// Encoding is empty in proto — should appear as "n/a" fallback in JSON output.
	if entry["encoding"] != "n/a" {
		t.Fatalf("expected encoding=\"n/a\" (fallback for empty), got %v", entry["encoding"])
	}
	if entry["sample_rate"] != uint32(16000) {
		t.Fatalf("expected sample_rate=16000, got %v", entry["sample_rate"])
	}
	if entry["channels"] != uint32(1) {
		t.Fatalf("expected channels=1, got %v", entry["channels"])
	}
	if entry["bit_depth"] != uint32(16) {
		t.Fatalf("expected bit_depth=16, got %v", entry["bit_depth"])
	}
}

func TestFormatCapabilitiesProtoJSON_WithMetadata(t *testing.T) {
	caps := []*apiv1.AudioCapability{
		{
			StreamId:    "mic",
			Ready:       true,
			Recommended: true,
			Metadata:    map[string]string{"custom_key": "custom_value"},
		},
	}

	result := formatCapabilitiesProtoJSON(caps)
	entry := result[0]

	meta, ok := entry["metadata"].(map[string]string)
	if !ok {
		t.Fatalf("expected metadata map, got %T", entry["metadata"])
	}
	if meta["custom_key"] != "custom_value" {
		t.Fatalf("expected custom_key=custom_value, got %v", meta["custom_key"])
	}
	if entry["diagnostics"] != "" {
		t.Fatalf("expected diagnostics=\"\" when ready, got %v", entry["diagnostics"])
	}
	// Format is nil — encoding defaults to "n/a", numeric fields default to zero.
	if entry["encoding"] != "n/a" {
		t.Fatalf("expected encoding=\"n/a\" when Format is nil, got %v", entry["encoding"])
	}
	if entry["sample_rate"] != uint32(0) {
		t.Fatalf("expected sample_rate=0 when Format is nil, got %v", entry["sample_rate"])
	}
	if entry["channels"] != uint32(0) {
		t.Fatalf("expected channels=0 when Format is nil, got %v", entry["channels"])
	}
}

func TestFormatCapabilitiesProtoJSON_Empty(t *testing.T) {
	result := formatCapabilitiesProtoJSON(nil)
	if len(result) != 0 {
		t.Fatalf("expected empty result for nil input, got %d entries", len(result))
	}
}

func TestFormatCapabilitiesProtoJSON_NotReadyNoDiagnostics(t *testing.T) {
	caps := []*apiv1.AudioCapability{
		{
			StreamId:    "mic",
			Ready:       false,
			Recommended: true,
			Diagnostics: "",
		},
	}

	result := formatCapabilitiesProtoJSON(caps)
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}

	entry := result[0]
	if entry["ready"] != false {
		t.Fatalf("expected ready=false, got %v", entry["ready"])
	}
	if entry["diagnostics"] != "" {
		t.Fatalf("expected diagnostics=\"\" when not ready but no message, got %v", entry["diagnostics"])
	}
}

func TestFormatCapabilityHumanLine_NilFormat(t *testing.T) {
	cap := &apiv1.AudioCapability{
		StreamId:    "mic",
		Ready:       true,
		Recommended: true,
	}

	line := formatCapabilityHumanLine(cap)
	if !strings.Contains(line, "stream=mic") {
		t.Fatalf("expected stream=mic in line: %s", line)
	}
	if !strings.Contains(line, "encoding=n/a") {
		t.Fatalf("expected encoding=n/a fallback in line: %s", line)
	}
	if !strings.Contains(line, "0Hz") {
		t.Fatalf("expected 0Hz for nil format in line: %s", line)
	}
	if !strings.Contains(line, "0bit") {
		t.Fatalf("expected 0bit for nil format in line: %s", line)
	}
	if !strings.Contains(line, "0ch") {
		t.Fatalf("expected 0ch for nil format in line: %s", line)
	}
	if !strings.Contains(line, "0ms") {
		t.Fatalf("expected 0ms for nil format in line: %s", line)
	}
}

func TestFormatCapabilityHumanLine_Ready(t *testing.T) {
	cap := &apiv1.AudioCapability{
		StreamId:    "mic",
		Ready:       true,
		Recommended: true,
		Format: &apiv1.AudioFormat{
			Encoding:   "pcm_s16le",
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
	}

	line := formatCapabilityHumanLine(cap)
	expected := "  - stream=mic ready=true recommended=true encoding=pcm_s16le 16000Hz 16bit 1ch 0ms"
	if line != expected {
		t.Fatalf("unexpected line:\n  got:  %q\n  want: %q", line, expected)
	}
}

func TestFormatCapabilityHumanLine_NotReadyWithDiagnostics(t *testing.T) {
	cap := &apiv1.AudioCapability{
		StreamId:    "spk",
		Ready:       false,
		Recommended: true,
		Diagnostics: server.DiagnosticsPlaybackUnavailable,
		Format: &apiv1.AudioFormat{
			SampleRate: 48000,
			Channels:   2,
			BitDepth:   16,
		},
	}

	line := formatCapabilityHumanLine(cap)
	if !strings.Contains(line, "ready=false") {
		t.Fatalf("expected ready=false in line: %s", line)
	}
	if !strings.Contains(line, "encoding=n/a") {
		t.Fatalf("expected encoding=n/a fallback for empty encoding in line: %s", line)
	}
	if !strings.Contains(line, "48000Hz") {
		t.Fatalf("expected 48000Hz in line: %s", line)
	}
	if !strings.Contains(line, "diagnostics: "+server.DiagnosticsPlaybackUnavailable) {
		t.Fatalf("expected diagnostics line, got: %s", line)
	}
}

func TestFormatCapabilityHumanLine_WithMetadata(t *testing.T) {
	cap := &apiv1.AudioCapability{
		StreamId:    "mic",
		Ready:       true,
		Recommended: true,
		Metadata:    map[string]string{"custom_key": "custom_value"},
		Format: &apiv1.AudioFormat{
			Encoding:   "pcm_s16le",
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
	}

	line := formatCapabilityHumanLine(cap)
	if !strings.Contains(line, "metadata:") {
		t.Fatalf("expected metadata in human-readable line, got: %s", line)
	}
	if !strings.Contains(line, "custom_key") {
		t.Fatalf("expected custom_key in metadata output, got: %s", line)
	}
}
