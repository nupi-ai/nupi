package audiofmt

import (
	"math"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// BytesPerSample returns bytes used to encode a single sample.
func BytesPerSample(format eventbus.AudioFormat) int {
	if format.BitDepth <= 0 {
		return 0
	}
	bytes := format.BitDepth / 8
	if bytes <= 0 {
		return 0
	}
	return bytes
}

// FrameSize returns PCM frame size in bytes (all channels for one sample point).
func FrameSize(format eventbus.AudioFormat) int {
	if format.Channels <= 0 {
		return 0
	}
	bytesPerSample := BytesPerSample(format)
	if bytesPerSample <= 0 {
		return 0
	}
	return format.Channels * bytesPerSample
}

// BytesPerSecond returns byte throughput for PCM format.
func BytesPerSecond(format eventbus.AudioFormat) int {
	if format.SampleRate <= 0 {
		return 0
	}
	frameSize := FrameSize(format)
	if frameSize <= 0 {
		return 0
	}
	return format.SampleRate * frameSize
}

// PCMFrameCountFromBytes converts raw PCM byte length into complete frame count.
func PCMFrameCountFromBytes(format eventbus.AudioFormat, dataLen int) int {
	if dataLen <= 0 {
		return 0
	}
	frameSize := FrameSize(format)
	if frameSize <= 0 {
		return 0
	}
	return dataLen / frameSize
}

// DurationFromFrames converts PCM frame count into duration using sample rate.
func DurationFromFrames(sampleRate, frames int) time.Duration {
	if sampleRate <= 0 || frames <= 0 {
		return 0
	}
	return time.Duration(float64(frames) / float64(sampleRate) * float64(time.Second))
}

// DurationFromPCMBytes converts raw PCM byte length into duration.
func DurationFromPCMBytes(format eventbus.AudioFormat, dataLen int) time.Duration {
	return DurationFromFrames(format.SampleRate, PCMFrameCountFromBytes(format, dataLen))
}

// DurationFromBytes calculates duration from byte length and PCM byte throughput.
func DurationFromBytes(dataLen int, format eventbus.AudioFormat) time.Duration {
	if dataLen <= 0 {
		return 0
	}
	bytesPerSecond := BytesPerSecond(format)
	if bytesPerSecond <= 0 {
		return 0
	}
	seconds := float64(dataLen) / float64(bytesPerSecond)
	return time.Duration(seconds * float64(time.Second))
}

// SegmentSizeBytes calculates target segment size for a PCM format and duration.
func SegmentSizeBytes(format eventbus.AudioFormat, segmentDuration time.Duration) int {
	if segmentDuration <= 0 {
		return 0
	}
	frameSize := FrameSize(format)
	if frameSize <= 0 || format.SampleRate <= 0 {
		return 0
	}
	samples := float64(format.SampleRate) * segmentDuration.Seconds()
	return int(math.Round(samples)) * frameSize
}
