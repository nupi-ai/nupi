package conversation

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

func TestConversationMarksBargeInOnLastAITurn(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	now := time.Now().UTC()
	svc.handlePipelineMessage(now, eventbus.PipelineMessageEvent{
		SessionID: "session",
		Origin:    eventbus.OriginUser,
		Text:      "question",
	})

	svc.handleReplyMessage(now.Add(10*time.Millisecond), eventbus.ConversationReplyEvent{
		SessionID: "session",
		PromptID:  "prompt-1",
		Text:      "answer",
	})

	bargeEvent := eventbus.SpeechBargeInEvent{
		SessionID: "session",
		StreamID:  "mic",
		Reason:    "client_interrupt",
		Timestamp: now.Add(20 * time.Millisecond),
		Metadata: map[string]string{
			"origin": "test",
		},
	}
	svc.handleBargeEvent(bargeEvent)

	history := svc.Context("session")
	if len(history) != 2 {
		t.Fatalf("unexpected history length: %d", len(history))
	}
	aiTurn := history[1]
	if aiTurn.Origin != eventbus.OriginAI {
		t.Fatalf("expected AI origin for last turn, got %s", aiTurn.Origin)
	}
	if aiTurn.Meta["barge_in"] != "true" {
		t.Fatalf("expected barge_in metadata, got %+v", aiTurn.Meta)
	}
	if aiTurn.Meta["barge_in_reason"] != "client_interrupt" {
		t.Fatalf("unexpected barge_in_reason: %s", aiTurn.Meta["barge_in_reason"])
	}
	if aiTurn.Meta["barge_origin"] != "test" {
		t.Fatalf("expected barge metadata to include origin, got %+v", aiTurn.Meta)
	}
}

func TestConversationKeepsHistoryUntilDetachTimeout(t *testing.T) {
	bus := eventbus.New()
	ttl := 100 * time.Millisecond
	svc := NewService(bus, WithDetachTTL(ttl))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	svc.mu.Lock()
	svc.sessions["detach"] = []eventbus.ConversationTurn{
		{
			Origin: eventbus.OriginUser,
			Text:   "hello",
			At:     time.Now(),
			Meta:   map[string]string{},
		},
	}
	svc.mu.Unlock()

	svc.scheduleDetachCleanup("detach")

	time.Sleep(ttl / 2)
	if len(svc.Context("detach")) == 0 {
		t.Fatalf("history cleared too early")
	}

	deadlineDetach := time.Now().Add(5 * ttl)
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

func TestConversationSliceWindow(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	for i := 0; i < 5; i++ {
		bus.Publish(context.Background(), eventbus.Envelope{
			Topic:   eventbus.TopicPipelineCleaned,
			Source:  eventbus.SourceContentPipeline,
			Payload: eventbus.PipelineMessageEvent{SessionID: "window", Origin: eventbus.OriginUser, Text: strconv.Itoa(i)},
		})
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		total, window := svc.Slice("window", 1, 2)
		if total == 5 && len(window) == 2 {
			if window[0].Text != "1" || window[1].Text != "2" {
				t.Fatalf("unexpected window data: %+v", window)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for slice window, total=%d len=%d", total, len(window))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConversationMetadataLimits(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	annotations := make(map[string]string)
	for i := 0; i < maxMetadataEntries+10; i++ {
		key := "  key-" + strconv.Itoa(i) + strings.Repeat("x", maxMetadataKeyRunes)
		value := "   value-" + strconv.Itoa(i) + strings.Repeat("y", maxMetadataValueRunes)
		annotations[key] = value
	}

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: eventbus.PipelineMessageEvent{SessionID: "metadata", Origin: eventbus.OriginUser, Text: "payload", Annotations: annotations},
	})

	replyMeta := map[string]string{
		" prompt ": strings.Repeat("z", maxMetadataValueRunes+50),
	}
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:  eventbus.TopicConversationReply,
		Source: eventbus.SourceConversation,
		Payload: eventbus.ConversationReplyEvent{
			SessionID: "metadata",
			Text:      "reply",
			Metadata:  replyMeta,
			PromptID:  strings.Repeat("p", maxMetadataValueRunes+20),
			Actions: []eventbus.ConversationAction{
				{
					Type:   strings.Repeat("t", maxMetadataValueRunes+20),
					Target: strings.Repeat("target", 30),
					Args: map[string]string{
						"payload": strings.Repeat("value", 100),
					},
				},
			},
		},
	})

	deadline := time.Now().Add(500 * time.Millisecond)
	var turns []eventbus.ConversationTurn
	for {
		turns = svc.Context("metadata")
		if len(turns) >= 2 && len(turns[len(turns)-1].Meta) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for metadata accumulation")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(turns[0].Meta) > maxMetadataEntries {
		t.Fatalf("expected metadata entries <= %d, got %d", maxMetadataEntries, len(turns[0].Meta))
	}
	for k, v := range turns[0].Meta {
		if strings.TrimSpace(k) != k {
			t.Fatalf("expected trimmed key, got %q", k)
		}
		if utf8.RuneCountInString(k) > maxMetadataKeyRunes {
			t.Fatalf("key exceeds rune limit: %d > %d", utf8.RuneCountInString(k), maxMetadataKeyRunes)
		}
		if utf8.RuneCountInString(v) > maxMetadataValueRunes {
			t.Fatalf("value exceeds rune limit: %d > %d", utf8.RuneCountInString(v), maxMetadataValueRunes)
		}
	}

	reply := turns[len(turns)-1]
	if reply.Meta["prompt_id"] == "" {
		t.Fatalf("expected prompt_id to be present")
	}
	if len(reply.Meta) > maxMetadataEntries {
		t.Fatalf("expected reply metadata entries <= %d, got %d", maxMetadataEntries, len(reply.Meta))
	}
	for k, v := range reply.Meta {
		if utf8.RuneCountInString(k) > maxMetadataKeyRunes {
			t.Fatalf("reply key exceeds rune limit: %d", utf8.RuneCountInString(k))
		}
		if utf8.RuneCountInString(v) > maxMetadataValueRunes {
			t.Fatalf("reply value exceeds rune limit: %d", utf8.RuneCountInString(v))
		}
	}
}

func TestConversationSessionlessMessage(t *testing.T) {
	bus := eventbus.New()
	globalStore := NewGlobalStore()
	svc := NewService(bus, WithGlobalStore(globalStore))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// Send a sessionless message (empty SessionID)
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: eventbus.PipelineMessageEvent{SessionID: "", Origin: eventbus.OriginUser, Text: "hello world"},
	})

	time.Sleep(50 * time.Millisecond)

	// Should be stored in GlobalStore
	if globalStore.Len() != 1 {
		t.Fatalf("expected 1 turn in GlobalStore, got %d", globalStore.Len())
	}

	ctx2 := globalStore.GetContext()
	if ctx2[0].Text != "hello world" {
		t.Fatalf("unexpected text in GlobalStore: %s", ctx2[0].Text)
	}

	// Should NOT be in session-based storage
	history := svc.Context("")
	if len(history) != 0 {
		t.Fatalf("expected no turns in session storage for empty session, got %d", len(history))
	}
}

func TestConversationSessionlessPrompt(t *testing.T) {
	bus := eventbus.New()
	globalStore := NewGlobalStore()
	svc := NewService(bus, WithGlobalStore(globalStore))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	promptSub := bus.Subscribe(eventbus.TopicConversationPrompt)
	defer promptSub.Close()

	// Add context first
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: eventbus.PipelineMessageEvent{SessionID: "", Origin: eventbus.OriginTool, Text: "context message"},
	})

	time.Sleep(20 * time.Millisecond)

	// Now send user message which should trigger prompt
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: eventbus.PipelineMessageEvent{SessionID: "", Origin: eventbus.OriginUser, Text: "list sessions"},
	})

	select {
	case env := <-promptSub.C():
		prompt, ok := env.Payload.(eventbus.ConversationPromptEvent)
		if !ok {
			t.Fatalf("unexpected payload: %T", env.Payload)
		}
		if prompt.SessionID != "" {
			t.Fatalf("expected empty session id for sessionless prompt, got %q", prompt.SessionID)
		}
		if len(prompt.Context) != 1 || prompt.Context[0].Text != "context message" {
			t.Fatalf("unexpected context: %+v", prompt.Context)
		}
		if prompt.NewMessage.Text != "list sessions" {
			t.Fatalf("unexpected new message: %+v", prompt.NewMessage)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for sessionless prompt")
	}
}

func TestConversationSessionlessReply(t *testing.T) {
	bus := eventbus.New()
	globalStore := NewGlobalStore()
	svc := NewService(bus, WithGlobalStore(globalStore))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	// Send user message
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: eventbus.PipelineMessageEvent{SessionID: "", Origin: eventbus.OriginUser, Text: "what sessions?"},
	})

	// Send AI reply
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicConversationReply,
		Source:  eventbus.SourceConversation,
		Payload: eventbus.ConversationReplyEvent{SessionID: "", Text: "You have no active sessions."},
	})

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		ctx2 := globalStore.GetContext()
		if len(ctx2) >= 2 {
			if ctx2[1].Origin != eventbus.OriginAI {
				t.Fatalf("expected AI origin, got %s", ctx2[1].Origin)
			}
			if ctx2[1].Text != "You have no active sessions." {
				t.Fatalf("unexpected reply text: %s", ctx2[1].Text)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for sessionless reply in GlobalStore, got %d turns", len(ctx2))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConversationGlobalContext(t *testing.T) {
	bus := eventbus.New()
	globalStore := NewGlobalStore()
	svc := NewService(bus, WithGlobalStore(globalStore))

	// Add turns directly to globalStore
	globalStore.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginUser, Text: "one"})
	globalStore.AddTurn(eventbus.ConversationTurn{Origin: eventbus.OriginAI, Text: "two"})

	// GlobalContext should return the same
	ctx := svc.GlobalContext()
	if len(ctx) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(ctx))
	}
	if ctx[0].Text != "one" || ctx[1].Text != "two" {
		t.Fatalf("unexpected context: %+v", ctx)
	}
}

func TestConversationGlobalSlice(t *testing.T) {
	bus := eventbus.New()
	globalStore := NewGlobalStore()
	svc := NewService(bus, WithGlobalStore(globalStore))

	// Add turns
	for i := 0; i < 5; i++ {
		globalStore.AddTurn(eventbus.ConversationTurn{
			Origin: eventbus.OriginUser,
			Text:   strconv.Itoa(i),
		})
	}

	// Test GlobalSlice
	total, slice := svc.GlobalSlice(1, 2)
	if total != 5 {
		t.Fatalf("expected total=5, got %d", total)
	}
	if len(slice) != 2 {
		t.Fatalf("expected 2 items, got %d", len(slice))
	}
	if slice[0].Text != "1" || slice[1].Text != "2" {
		t.Fatalf("unexpected slice: %+v", slice)
	}
}

func TestConversationGlobalContextNilStore(t *testing.T) {
	bus := eventbus.New()
	// No globalStore configured
	svc := NewService(bus)

	ctx := svc.GlobalContext()
	if ctx != nil {
		t.Fatalf("expected nil context when no globalStore, got %v", ctx)
	}

	total, slice := svc.GlobalSlice(0, 10)
	if total != 0 || slice != nil {
		t.Fatalf("expected 0/nil when no globalStore, got %d/%v", total, slice)
	}
}

func TestConversationSessionlessIgnoredWithoutGlobalStore(t *testing.T) {
	bus := eventbus.New()
	// No globalStore configured
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	promptSub := bus.Subscribe(eventbus.TopicConversationPrompt)
	defer promptSub.Close()

	// Send sessionless message - should be ignored without globalStore
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicPipelineCleaned,
		Source:  eventbus.SourceContentPipeline,
		Payload: eventbus.PipelineMessageEvent{SessionID: "", Origin: eventbus.OriginUser, Text: "hello"},
	})

	select {
	case <-promptSub.C():
		t.Fatal("should not publish prompt when no globalStore configured")
	case <-time.After(100 * time.Millisecond):
		// Expected: no prompt published
	}
}
