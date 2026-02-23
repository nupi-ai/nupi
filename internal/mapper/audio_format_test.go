package mapper

import (
	"testing"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

func TestFromProtoAudioFormatNil(t *testing.T) {
	got := FromProtoAudioFormat(nil)
	if got != (eventbus.AudioFormat{}) {
		t.Fatalf("expected zero-value format, got %+v", got)
	}
}

func TestParseProtoAudioFormat(t *testing.T) {
	tests := []struct {
		name    string
		input   *apiv1.AudioFormat
		wantErr bool
	}{
		{
			name: "valid_pcm16",
			input: &apiv1.AudioFormat{
				Encoding:        "pcm_s16le",
				SampleRate:      16000,
				Channels:        1,
				BitDepth:        16,
				FrameDurationMs: 20,
			},
		},
		{
			name: "empty_encoding_defaults_to_pcm16",
			input: &apiv1.AudioFormat{
				SampleRate:      16000,
				Channels:        1,
				BitDepth:        16,
				FrameDurationMs: 20,
			},
		},
		{
			name: "unsupported_encoding",
			input: &apiv1.AudioFormat{
				Encoding:   "opus",
				SampleRate: 16000,
				Channels:   1,
				BitDepth:   16,
			},
			wantErr: true,
		},
		{
			name: "invalid_sample_rate",
			input: &apiv1.AudioFormat{
				Encoding:   "pcm_s16le",
				SampleRate: 0,
				Channels:   1,
				BitDepth:   16,
			},
			wantErr: true,
		},
		{
			name: "invalid_channels",
			input: &apiv1.AudioFormat{
				Encoding:   "pcm_s16le",
				SampleRate: 16000,
				Channels:   0,
				BitDepth:   16,
			},
			wantErr: true,
		},
		{
			name: "invalid_bit_depth",
			input: &apiv1.AudioFormat{
				Encoding:   "pcm_s16le",
				SampleRate: 16000,
				Channels:   1,
				BitDepth:   10,
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseProtoAudioFormat(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Encoding != eventbus.AudioEncodingPCM16 {
				t.Fatalf("expected encoding pcm_s16le, got %q", got.Encoding)
			}
		})
	}
}

func TestToProtoAudioFormat(t *testing.T) {
	pb := ToProtoAudioFormat(eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    -1,
		Channels:      -2,
		BitDepth:      -16,
		FrameDuration: -20 * time.Millisecond,
	})
	if pb.GetSampleRate() != 0 || pb.GetChannels() != 0 || pb.GetBitDepth() != 0 || pb.GetFrameDurationMs() != 0 {
		t.Fatalf("expected non-negative clamped proto values, got %+v", pb)
	}
}
