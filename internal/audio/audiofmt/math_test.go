package audiofmt

import (
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

func pcm16Mono16k() eventbus.AudioFormat {
	return eventbus.AudioFormat{Encoding: eventbus.AudioEncodingPCM16, SampleRate: 16000, Channels: 1, BitDepth: 16}
}

func TestDurationFromPCMBytesTruncatesPartialFrame(t *testing.T) {
	format := pcm16Mono16k()
	want := 62500 * time.Nanosecond
	if got := DurationFromPCMBytes(format, 3); got != want {
		t.Fatalf("unexpected duration: got %s want %s", got, want)
	}
}

func TestDurationFromBytesCountsAllBytes(t *testing.T) {
	format := pcm16Mono16k()
	want := 93750 * time.Nanosecond
	if got := DurationFromBytes(3, format); got != want {
		t.Fatalf("unexpected duration: got %s want %s", got, want)
	}
}

func TestSegmentSizeBytesRoundsToSamples(t *testing.T) {
	format := pcm16Mono16k()
	if got := SegmentSizeBytes(format, 20*time.Millisecond); got != 640 {
		t.Fatalf("unexpected segment size: got %d want %d", got, 640)
	}
}

func TestPCMFrameCountFromBytes(t *testing.T) {
	format := eventbus.AudioFormat{SampleRate: 16000, Channels: 2, BitDepth: 16}
	if got := PCMFrameCountFromBytes(format, 13); got != 3 {
		t.Fatalf("unexpected frame count: got %d want %d", got, 3)
	}
}
