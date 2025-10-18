package ingress

import (
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

func TestStreamSegmentation(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus, WithSegmentDuration(20*time.Millisecond))

	format := eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}

	stream, err := svc.OpenStream("sess-1", "mic", format, map[string]string{"client": "test"})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	rawSub := bus.Subscribe(eventbus.TopicAudioIngressRaw)
	segSub := bus.Subscribe(eventbus.TopicAudioIngressSegment)

	// Write one full segment (640 bytes)
	data := make([]byte, 640)
	if err := stream.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}

	rawEvt := receiveEvent(t, rawSub)
	if rawEvt.Payload.(eventbus.AudioIngressRawEvent).Sequence != 1 {
		t.Fatalf("unexpected raw sequence")
	}

	segEvt := receiveEvent(t, segSub)
	segment := segEvt.Payload.(eventbus.AudioIngressSegmentEvent)
	if !segment.First {
		t.Fatalf("expected first segment")
	}
	if len(segment.Data) != 640 {
		t.Fatalf("segment size mismatch: %d", len(segment.Data))
	}
	if segment.Duration != 20*time.Millisecond {
		t.Fatalf("segment duration: %s", segment.Duration)
	}
	if segment.Last {
		t.Fatalf("last should be false for full segment")
	}

	// Write partial data to trigger final segment on Close
	partial := make([]byte, 320)
	if err := stream.Write(partial); err != nil {
		t.Fatalf("write partial: %v", err)
	}

	// Nothing flushed yet
	select {
	case <-segSub.C():
		t.Fatalf("unexpected segment before close")
	case <-time.After(15 * time.Millisecond):
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	finalEvt := receiveEvent(t, segSub)
	finalSegment := finalEvt.Payload.(eventbus.AudioIngressSegmentEvent)
	if finalSegment.Last != true {
		t.Fatalf("expected last segment flag")
	}
	if len(finalSegment.Data) != 320 {
		t.Fatalf("final segment length %d", len(finalSegment.Data))
	}

	// stream removed
	if _, ok := svc.Stream("sess-1", "mic"); ok {
		t.Fatalf("stream should be removed after close")
	}
}

func TestOpenStreamValidation(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus)

	_, err := svc.OpenStream("", "mic", eventbus.AudioFormat{SampleRate: 16000, Channels: 1, BitDepth: 16}, nil)
	if err == nil {
		t.Fatalf("expected error for empty session id")
	}

	_, err = svc.OpenStream("sess", "mic", eventbus.AudioFormat{SampleRate: 16000, Channels: 1, BitDepth: 15}, nil)
	if err == nil {
		t.Fatalf("expected error for invalid bit depth")
	}
}

func receiveEvent(t *testing.T, sub *eventbus.Subscription) eventbus.Envelope {
	t.Helper()
	select {
	case env := <-sub.C():
		return env
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for event")
	}
	return eventbus.Envelope{}
}
