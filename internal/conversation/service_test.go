package conversation

import (
	"context"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

func TestConversationStoresHistoryWithLimit(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus, WithHistoryLimit(2))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: eventbus.PipelineMessageEvent{SessionID: "s", Origin: eventbus.OriginTool, Text: "first"},
	})
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: eventbus.PipelineMessageEvent{SessionID: "s", Origin: eventbus.OriginTool, Text: "second"},
	})
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: eventbus.PipelineMessageEvent{SessionID: "s", Origin: eventbus.OriginTool, Text: "third"},
	})

	time.Sleep(50 * time.Millisecond)

	history := svc.Context("s")
	if len(history) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(history))
	}
	if history[0].Text != "second" || history[1].Text != "third" {
		t.Fatalf("unexpected history order: %+v", history)
	}
}

func TestConversationPublishesPromptOnUserInput(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	promptSub := bus.Subscribe(eventbus.TopicConversationPrompt)
	defer promptSub.Close()

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: eventbus.PipelineMessageEvent{SessionID: "chat", Origin: eventbus.OriginTool, Text: "previous"},
	})

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: eventbus.PipelineMessageEvent{SessionID: "chat", Origin: eventbus.OriginUser, Text: "hello"},
	})

	select {
	case env := <-promptSub.C():
		prompt, ok := env.Payload.(eventbus.ConversationPromptEvent)
		if !ok {
			t.Fatalf("unexpected payload: %T", env.Payload)
		}
		if prompt.SessionID != "chat" {
			t.Fatalf("unexpected session id: %s", prompt.SessionID)
		}
		if len(prompt.Context) != 1 || prompt.Context[0].Text != "previous" {
			t.Fatalf("unexpected context: %+v", prompt.Context)
		}
		if prompt.NewMessage.Text != "hello" {
			t.Fatalf("unexpected new message: %+v", prompt.NewMessage)
		}
		if prompt.PromptID == "" {
			t.Fatalf("expected prompt id")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for prompt")
	}
}

func TestConversationKeepsHistoryUntilDetachTimeout(t *testing.T) {
	bus := eventbus.New()
	ttl := 50 * time.Millisecond
	svc := NewService(bus, WithDetachTTL(ttl))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: eventbus.PipelineMessageEvent{SessionID: "detach", Origin: eventbus.OriginUser, Text: "hello"},
	})

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicSessionsLifecycle,
		Source:  eventbus.SourceSessionManager,
		Payload: eventbus.SessionLifecycleEvent{SessionID: "detach", State: eventbus.SessionStateDetached},
	})

	time.Sleep(ttl / 2)
	if len(svc.Context("detach")) == 0 {
		t.Fatalf("history cleared too early")
	}

	deadlineDetach := time.Now().Add(2 * ttl)
	for {
		if len(svc.Context("detach")) == 0 {
			break
		}
		if time.Now().After(deadlineDetach) {
			t.Fatalf("expected history cleared after ttl")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConversationStoresReplies(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: eventbus.PipelineMessageEvent{SessionID: "reply", Origin: eventbus.OriginUser, Text: "hi"},
	})

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicConversationReply,
		Source:  eventbus.SourceConversation,
		Payload: eventbus.ConversationReplyEvent{SessionID: "reply", Text: "hello there"},
	})

	deadlineReply := time.Now().Add(500 * time.Millisecond)
	for {
		turns := svc.Context("reply")
		if len(turns) >= 2 && turns[len(turns)-1].Origin == eventbus.OriginAI {
			if turns[len(turns)-1].Text != "hello there" {
				t.Fatalf("unexpected reply text: %+v", turns[len(turns)-1])
			}
			break
		}
		if time.Now().After(deadlineReply) {
			t.Fatalf("timeout waiting for AI reply, context: %+v", turns)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
