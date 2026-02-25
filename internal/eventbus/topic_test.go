package eventbus

import (
	"context"
	"testing"
	"time"
)

func TestPublishSubscribeToRoundtrip(t *testing.T) {
	bus := New()
	defer bus.Shutdown()

	sub := SubscribeTo(bus, Sessions.Output, WithSubscriptionName("test"))
	defer sub.Close()

	payload := SessionOutputEvent{
		SessionID: "s1",
		Sequence:  1,
		Data:      []byte("hello"),
	}

	Publish(context.Background(), bus, Sessions.Output, SourceSessionManager, payload)

	select {
	case env := <-sub.C():
		if env.Payload.SessionID != "s1" {
			t.Fatalf("expected SessionID=s1, got %s", env.Payload.SessionID)
		}
		if string(env.Payload.Data) != "hello" {
			t.Fatalf("expected Data=hello, got %s", string(env.Payload.Data))
		}
		if env.Source != SourceSessionManager {
			t.Fatalf("expected Source=%s, got %s", SourceSessionManager, env.Source)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestPublishWithOptsTimestamp(t *testing.T) {
	bus := New()
	defer bus.Shutdown()

	sub := SubscribeTo(bus, Adapters.Log, WithSubscriptionName("test"))
	defer sub.Close()

	ts := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	payload := AdapterLogEvent{
		AdapterID: "test-adapter",
		Level:     LogLevelInfo,
		Message:   "hello",
		Timestamp: ts,
	}

	PublishWithOpts(context.Background(), bus, Adapters.Log, SourceAdapterProcess, payload, WithTimestamp(ts))

	select {
	case env := <-sub.C():
		if env.Payload.Message != "hello" {
			t.Fatalf("expected Message=hello, got %s", env.Payload.Message)
		}
		if !env.Timestamp.Equal(ts) {
			t.Fatalf("expected Timestamp=%v, got %v", ts, env.Timestamp)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestPublishNilBusNoPanic(t *testing.T) {
	// Should not panic.
	Publish(context.Background(), nil, Sessions.Output, SourceSessionManager, SessionOutputEvent{})
	PublishWithOpts(context.Background(), nil, Adapters.Log, SourceAdapterProcess, AdapterLogEvent{}, WithTimestamp(time.Now()))
}

func TestSubscribeToNilBus(t *testing.T) {
	sub := SubscribeTo[SessionOutputEvent](nil, Sessions.Output)
	defer sub.Close()

	// Channel should be closed immediately.
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("expected closed channel for nil bus")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out - channel should be closed for nil bus")
	}
}

func TestTopicDefTopic(t *testing.T) {
	td := NewTopicDef[SessionOutputEvent](TopicSessionsOutput)
	if td.Topic() != TopicSessionsOutput {
		t.Fatalf("expected %s, got %s", TopicSessionsOutput, td.Topic())
	}
}

func TestSerializeTurns(t *testing.T) {
	tests := []struct {
		name  string
		turns []ConversationTurn
		want  string
	}{
		{"nil", nil, ""},
		{"empty", []ConversationTurn{}, ""},
		{"single", []ConversationTurn{
			{Origin: OriginUser, Text: "hello"},
		}, "[user] hello"},
		{"multiple", []ConversationTurn{
			{Origin: OriginUser, Text: "hello"},
			{Origin: OriginAI, Text: "hi there"},
			{Origin: OriginSystem, Text: "status ok"},
		}, "[user] hello\n[assistant] hi there\n[system] status ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SerializeTurns(tt.turns)
			if got != tt.want {
				t.Fatalf("SerializeTurns() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDescriptorTopicsMatch(t *testing.T) {
	tests := []struct {
		name  string
		got   Topic
		want  Topic
	}{
		{"Sessions.Output", Sessions.Output.Topic(), TopicSessionsOutput},
		{"Sessions.Lifecycle", Sessions.Lifecycle.Topic(), TopicSessionsLifecycle},
		{"Sessions.Tool", Sessions.Tool.Topic(), TopicSessionsTool},
		{"Sessions.ToolChanged", Sessions.ToolChanged.Topic(), TopicSessionsToolChanged},
		{"Pipeline.Cleaned", Pipeline.Cleaned.Topic(), TopicPipelineCleaned},
		{"Pipeline.Error", Pipeline.Error.Topic(), TopicPipelineError},
		{"Conversation.Prompt", Conversation.Prompt.Topic(), TopicConversationPrompt},
		{"Conversation.Reply", Conversation.Reply.Topic(), TopicConversationReply},
		{"Conversation.Speak", Conversation.Speak.Topic(), TopicConversationSpeak},
		{"Conversation.Turn", Conversation.Turn.Topic(), TopicConversationTurn},
		{"Audio.IngressRaw", Audio.IngressRaw.Topic(), TopicAudioIngressRaw},
		{"Audio.IngressSegment", Audio.IngressSegment.Topic(), TopicAudioIngressSegment},
		{"Audio.EgressPlayback", Audio.EgressPlayback.Topic(), TopicAudioEgressPlayback},
		{"Audio.Interrupt", Audio.Interrupt.Topic(), TopicAudioInterrupt},
		{"Speech.TranscriptPartial", Speech.TranscriptPartial.Topic(), TopicSpeechTranscriptPartial},
		{"Speech.TranscriptFinal", Speech.TranscriptFinal.Topic(), TopicSpeechTranscriptFinal},
		{"Speech.VADDetected", Speech.VADDetected.Topic(), TopicSpeechVADDetected},
		{"Speech.BargeIn", Speech.BargeIn.Topic(), TopicSpeechBargeIn},
		{"Adapters.Status", Adapters.Status.Topic(), TopicAdaptersStatus},
		{"Adapters.Log", Adapters.Log.Topic(), TopicAdaptersLog},
		{"Pairing.Created", Pairing.Created.Topic(), TopicPairingCreated},
		{"Pairing.Claimed", Pairing.Claimed.Topic(), TopicPairingClaimed},
		{"Memory.Sync", Memory.Sync.Topic(), TopicAwarenessSync},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("got %s, want %s", tt.got, tt.want)
			}
		})
	}
}
