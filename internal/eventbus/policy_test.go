package eventbus

import "testing"

func TestDefaultPolicies(t *testing.T) {
	critical := []Topic{
		TopicConversationPrompt,
		TopicConversationReply,
		TopicSpeechTranscriptFinal,
		TopicSessionsLifecycle,
	}
	for _, topic := range critical {
		p, ok := defaultPolicies[topic]
		if !ok {
			t.Fatalf("expected defaultPolicies entry for %s", topic)
		}
		if p.Strategy != StrategyOverflow {
			t.Fatalf("expected overflow strategy for %s, got %s", topic, p.Strategy)
		}
		if p.Priority != PriorityCritical {
			t.Fatalf("expected critical priority for %s, got %d", topic, p.Priority)
		}
	}

	normal := []Topic{
		TopicSessionsOutput,
		TopicAudioIngressSegment,
		TopicPipelineCleaned,
		TopicAudioEgressPlayback,
		TopicSpeechTranscriptPartial,
		TopicSpeechVADDetected,
		TopicConversationSpeak,
		TopicSessionsTool,
		TopicSessionsToolChanged,
		TopicAudioIngressRaw,
		TopicPipelineError,
		TopicAudioInterrupt,
		TopicSpeechBargeIn,
	}
	for _, topic := range normal {
		p, ok := defaultPolicies[topic]
		if !ok {
			t.Fatalf("expected defaultPolicies entry for %s", topic)
		}
		if p.Strategy != StrategyDropOldest {
			t.Fatalf("expected drop-oldest strategy for %s, got %s", topic, p.Strategy)
		}
		if p.Priority != PriorityNormal {
			t.Fatalf("expected normal priority for %s, got %d", topic, p.Priority)
		}
	}

	low := []Topic{
		TopicAdaptersLog,
		TopicAdaptersStatus,
		TopicIntentRouterDiagnostics,
	}
	for _, topic := range low {
		p, ok := defaultPolicies[topic]
		if !ok {
			t.Fatalf("expected defaultPolicies entry for %s", topic)
		}
		if p.Strategy != StrategyDropNewest {
			t.Fatalf("expected drop-newest strategy for %s, got %s", topic, p.Strategy)
		}
		if p.Priority != PriorityLow {
			t.Fatalf("expected low priority for %s, got %d", topic, p.Priority)
		}
	}
}

func TestPolicyForFallback(t *testing.T) {
	unknown := Topic("some.unknown.topic")
	p := policyFor(unknown, nil)
	if p.Strategy != StrategyDropOldest {
		t.Fatalf("expected drop-oldest for unknown topic, got %s", p.Strategy)
	}
	if p.Priority != PriorityNormal {
		t.Fatalf("expected normal priority for unknown topic, got %d", p.Priority)
	}
}

func TestPolicyForOverride(t *testing.T) {
	overrides := map[Topic]DeliveryPolicy{
		TopicSessionsOutput: {Strategy: StrategyDropNewest, Priority: PriorityLow},
	}
	p := policyFor(TopicSessionsOutput, overrides)
	if p.Strategy != StrategyDropNewest {
		t.Fatalf("expected override to take effect, got %s", p.Strategy)
	}
}
