package ingress

import (
	"context"
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
	rawPayload, ok := rawEvt.Payload.(eventbus.AudioIngressRawEvent)
	if !ok {
		t.Fatalf("expected AudioIngressRawEvent, got %T", rawEvt.Payload)
	}
	if rawPayload.Sequence != 1 {
		t.Fatalf("unexpected raw sequence")
	}

	segEvt := receiveEvent(t, segSub)
	segment, ok := segEvt.Payload.(eventbus.AudioIngressSegmentEvent)
	if !ok {
		t.Fatalf("expected AudioIngressSegmentEvent, got %T", segEvt.Payload)
	}
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
	finalSegment, ok := finalEvt.Payload.(eventbus.AudioIngressSegmentEvent)
	if !ok {
		t.Fatalf("expected AudioIngressSegmentEvent, got %T", finalEvt.Payload)
	}
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

func TestStreamCloseOnBoundaryEmitsTerminalSegment(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus, WithSegmentDuration(20*time.Millisecond))

	format := eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}

	stream, err := svc.OpenStream("sess-boundary", "mic", format, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	segSub := bus.Subscribe(eventbus.TopicAudioIngressSegment)

	data := make([]byte, 640)
	if err := stream.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}

	firstEvt := receiveEvent(t, segSub)
	firstSegment, ok := firstEvt.Payload.(eventbus.AudioIngressSegmentEvent)
	if !ok {
		t.Fatalf("expected AudioIngressSegmentEvent, got %T", firstEvt.Payload)
	}
	if firstSegment.Last {
		t.Fatalf("expected first segment not to be last")
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	terminalEvt := receiveEvent(t, segSub)
	terminal, ok := terminalEvt.Payload.(eventbus.AudioIngressSegmentEvent)
	if !ok {
		t.Fatalf("expected AudioIngressSegmentEvent, got %T", terminalEvt.Payload)
	}
	if !terminal.Last {
		t.Fatalf("expected terminal envelope with Last=true")
	}
	if terminal.First {
		t.Fatalf("terminal envelope should not be marked first")
	}
	if len(terminal.Data) != 0 {
		t.Fatalf("terminal envelope should not contain audio data")
	}
	if terminal.Duration != 0 {
		t.Fatalf("terminal envelope duration should be zero")
	}
	if terminal.Sequence != firstSegment.Sequence+1 {
		t.Fatalf("unexpected terminal sequence: got %d want %d", terminal.Sequence, firstSegment.Sequence+1)
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

func TestServiceShutdownClosesStreams(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus, WithSegmentDuration(20*time.Millisecond))

	format := eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}

	stream, err := svc.OpenStream("sess-shutdown", "mic", format, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	segSub := bus.Subscribe(eventbus.TopicAudioIngressSegment)

	segmentData := make([]byte, 640)
	if err := stream.Write(segmentData); err != nil {
		t.Fatalf("write: %v", err)
	}

	firstEvt := receiveEvent(t, segSub)
	first, ok := firstEvt.Payload.(eventbus.AudioIngressSegmentEvent)
	if !ok {
		t.Fatalf("expected AudioIngressSegmentEvent, got %T", firstEvt.Payload)
	}
	if first.Last {
		t.Fatalf("expected first segment to not be last before shutdown")
	}

	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	terminalEvt := receiveEvent(t, segSub)
	terminal, ok := terminalEvt.Payload.(eventbus.AudioIngressSegmentEvent)
	if !ok {
		t.Fatalf("expected AudioIngressSegmentEvent, got %T", terminalEvt.Payload)
	}
	if !terminal.Last {
		t.Fatalf("expected shutdown to emit terminal segment")
	}
	if terminal.Sequence != first.Sequence+1 {
		t.Fatalf("unexpected terminal sequence after shutdown: got %d want %d", terminal.Sequence, first.Sequence+1)
	}

	if _, ok := svc.Stream("sess-shutdown", "mic"); ok {
		t.Fatalf("stream should be removed after shutdown")
	}
}

func TestServiceMetricsTracksBytes(t *testing.T) {
	bus := eventbus.New()
	svc := New(bus, WithSegmentDuration(20*time.Millisecond))

	if metrics := svc.Metrics(); metrics.BytesTotal != 0 {
		t.Fatalf("expected initial BytesTotal to be 0, got %d", metrics.BytesTotal)
	}

	format := eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}

	stream, err := svc.OpenStream("sess-metrics", "mic", format, nil)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	data := make([]byte, 640)
	if err := stream.Write(data); err != nil {
		t.Fatalf("write data: %v", err)
	}

	more := make([]byte, 320)
	if err := stream.Write(more); err != nil {
		t.Fatalf("write more data: %v", err)
	}

	want := uint64(len(data) + len(more))
	if metrics := svc.Metrics(); metrics.BytesTotal != want {
		t.Fatalf("expected BytesTotal %d, got %d", want, metrics.BytesTotal)
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
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
