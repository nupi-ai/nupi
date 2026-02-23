package mapper

import (
	"fmt"
	"strings"
	"time"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

// FromProtoAudioFormat converts a gRPC AudioFormat to the internal domain format.
// It is permissive and returns a zero-value format when pb is nil.
func FromProtoAudioFormat(pb *apiv1.AudioFormat) eventbus.AudioFormat {
	if pb == nil {
		return eventbus.AudioFormat{}
	}
	return eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncoding(pb.GetEncoding()),
		SampleRate:    int(pb.GetSampleRate()),
		Channels:      int(pb.GetChannels()),
		BitDepth:      int(pb.GetBitDepth()),
		FrameDuration: time.Duration(pb.GetFrameDurationMs()) * time.Millisecond,
	}
}

// ParseProtoAudioFormat validates and converts a gRPC AudioFormat to the internal domain format.
func ParseProtoAudioFormat(pb *apiv1.AudioFormat) (eventbus.AudioFormat, error) {
	if pb == nil {
		return eventbus.AudioFormat{}, fmt.Errorf("audio format is required")
	}

	encoding := strings.TrimSpace(strings.ToLower(pb.GetEncoding()))
	if encoding == "" {
		encoding = string(eventbus.AudioEncodingPCM16)
	}
	if encoding != string(eventbus.AudioEncodingPCM16) {
		return eventbus.AudioFormat{}, fmt.Errorf("unsupported encoding %q", pb.GetEncoding())
	}

	format := FromProtoAudioFormat(pb)
	format.Encoding = eventbus.AudioEncodingPCM16

	if format.SampleRate <= 0 {
		return eventbus.AudioFormat{}, fmt.Errorf("sample_rate must be positive")
	}
	if format.Channels <= 0 {
		return eventbus.AudioFormat{}, fmt.Errorf("channels must be positive")
	}
	if format.BitDepth <= 0 || format.BitDepth%8 != 0 {
		return eventbus.AudioFormat{}, fmt.Errorf("bit_depth must be divisible by 8")
	}
	return format, nil
}

// ToProtoAudioFormat converts an internal domain audio format to gRPC AudioFormat.
func ToProtoAudioFormat(format eventbus.AudioFormat) *apiv1.AudioFormat {
	return &apiv1.AudioFormat{
		Encoding:        string(format.Encoding),
		SampleRate:      uint32(nonNegativeInt(format.SampleRate)),
		Channels:        uint32(nonNegativeInt(format.Channels)),
		BitDepth:        uint32(nonNegativeInt(format.BitDepth)),
		FrameDurationMs: durationToMillis(format.FrameDuration),
	}
}

// ToNAPAudioFormat converts an internal domain audio format to NAP AudioFormat.
func ToNAPAudioFormat(format eventbus.AudioFormat) *napv1.AudioFormat {
	return &napv1.AudioFormat{
		Encoding:        string(format.Encoding),
		SampleRate:      uint32(format.SampleRate),
		Channels:        uint32(format.Channels),
		BitDepth:        uint32(format.BitDepth),
		FrameDurationMs: uint32(format.FrameDuration / time.Millisecond),
	}
}

func nonNegativeInt(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

func durationToMillis(d time.Duration) uint32 {
	if d <= 0 {
		return 0
	}
	return uint32(d / time.Millisecond)
}
