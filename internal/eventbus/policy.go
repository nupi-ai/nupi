package eventbus

// Priority classifies a topic's importance for delivery guarantees.
type Priority int

const (
	PriorityLow      Priority = 0
	PriorityNormal   Priority = 1
	PriorityCritical Priority = 2
)

// DeliveryStrategy determines behaviour when a subscriber's channel is full.
type DeliveryStrategy string

const (
	// StrategyDropOldest removes the oldest event from the channel and enqueues the new one.
	StrategyDropOldest DeliveryStrategy = "drop-oldest"
	// StrategyDropNewest discards the incoming event when the channel is full.
	StrategyDropNewest DeliveryStrategy = "drop-newest"
	// StrategyOverflow spills into a capped ring buffer; a background goroutine drains it back.
	StrategyOverflow DeliveryStrategy = "overflow"
)

// DeliveryPolicy controls how a topic handles backpressure.
type DeliveryPolicy struct {
	Strategy    DeliveryStrategy
	Priority    Priority
	MaxOverflow int // ring buffer cap for StrategyOverflow (0 = defaultMaxOverflow)
}

const defaultMaxOverflow = 512

// defaultPolicy is used for topics without an explicit entry in defaultPolicies.
var defaultPolicy = DeliveryPolicy{
	Strategy: StrategyDropOldest,
	Priority: PriorityNormal,
}

// defaultPolicies maps known topics to their delivery policies.
var defaultPolicies = map[Topic]DeliveryPolicy{
	// Critical — drops mean lost user input or broken state.
	TopicConversationPrompt:    {Strategy: StrategyOverflow, Priority: PriorityCritical, MaxOverflow: defaultMaxOverflow},
	TopicConversationReply:     {Strategy: StrategyOverflow, Priority: PriorityCritical, MaxOverflow: defaultMaxOverflow},
	TopicSpeechTranscriptFinal: {Strategy: StrategyOverflow, Priority: PriorityCritical, MaxOverflow: defaultMaxOverflow},
	TopicSessionsLifecycle:     {Strategy: StrategyOverflow, Priority: PriorityCritical, MaxOverflow: defaultMaxOverflow},

	// Normal — high-volume or tolerant of occasional drops.
	TopicSessionsOutput:          {Strategy: StrategyDropOldest, Priority: PriorityNormal},
	TopicAudioIngressSegment:     {Strategy: StrategyDropOldest, Priority: PriorityNormal},
	TopicPipelineCleaned:         {Strategy: StrategyDropOldest, Priority: PriorityNormal},
	TopicAudioEgressPlayback:     {Strategy: StrategyDropOldest, Priority: PriorityNormal},
	TopicSpeechTranscriptPartial: {Strategy: StrategyDropOldest, Priority: PriorityNormal},
	TopicSpeechVADDetected:       {Strategy: StrategyDropOldest, Priority: PriorityNormal},
	TopicConversationSpeak:       {Strategy: StrategyDropOldest, Priority: PriorityNormal},
	TopicSessionsTool:            {Strategy: StrategyDropOldest, Priority: PriorityNormal},
	TopicSessionsToolChanged:     {Strategy: StrategyDropOldest, Priority: PriorityNormal},
	TopicAudioIngressRaw:         {Strategy: StrategyDropOldest, Priority: PriorityNormal},
	TopicPipelineError:           {Strategy: StrategyDropOldest, Priority: PriorityNormal},
	TopicAudioInterrupt:          {Strategy: StrategyDropOldest, Priority: PriorityNormal},
	TopicSpeechBargeIn:           {Strategy: StrategyDropOldest, Priority: PriorityNormal},

	// Low — informational, already rate-limited or infrequent.
	TopicAdaptersLog:             {Strategy: StrategyDropNewest, Priority: PriorityLow},
	TopicAdaptersStatus:          {Strategy: StrategyDropNewest, Priority: PriorityLow},
	TopicIntentRouterDiagnostics: {Strategy: StrategyDropNewest, Priority: PriorityLow},
}

// policyFor returns the delivery policy for a topic, falling back to defaultPolicy.
func policyFor(topic Topic, overrides map[Topic]DeliveryPolicy) DeliveryPolicy {
	if overrides != nil {
		if p, ok := overrides[topic]; ok {
			return p
		}
	}
	if p, ok := defaultPolicies[topic]; ok {
		return p
	}
	return defaultPolicy
}
