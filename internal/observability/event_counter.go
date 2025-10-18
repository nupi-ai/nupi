package observability

import (
	"sync"
	"sync/atomic"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// EventCounter counts published events grouped by topic.
type EventCounter struct {
	counts sync.Map // map[eventbus.Topic]*atomic.Uint64
}

// NewEventCounter creates a counter that can be registered as an event bus observer.
func NewEventCounter() *EventCounter {
	return &EventCounter{}
}

// OnPublish implements eventbus.Observer by tracking published events per topic.
func (c *EventCounter) OnPublish(env eventbus.Envelope) {
	if env.Topic == "" {
		return
	}
	counter := c.counterFor(env.Topic)
	counter.Add(1)
}

// Snapshot exposes a stable copy of the current counts.
func (c *EventCounter) Snapshot() map[eventbus.Topic]uint64 {
	out := make(map[eventbus.Topic]uint64)
	c.counts.Range(func(key, value any) bool {
		topic, ok := key.(eventbus.Topic)
		if !ok {
			return true
		}
		counter, ok := value.(*atomic.Uint64)
		if !ok || counter == nil {
			return true
		}
		out[topic] = counter.Load()
		return true
	})
	return out
}

func (c *EventCounter) counterFor(topic eventbus.Topic) *atomic.Uint64 {
	if topic == "" {
		return &atomic.Uint64{}
	}
	if counter, ok := c.counts.Load(topic); ok {
		if typed, ok := counter.(*atomic.Uint64); ok && typed != nil {
			return typed
		}
	}
	newCounter := &atomic.Uint64{}
	actual, _ := c.counts.LoadOrStore(topic, newCounter)
	if typed, ok := actual.(*atomic.Uint64); ok && typed != nil {
		return typed
	}
	return newCounter
}
