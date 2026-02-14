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

	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "s", Origin: eventbus.OriginTool, Text: "first"})
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "s", Origin: eventbus.OriginTool, Text: "second"})
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "s", Origin: eventbus.OriginTool, Text: "third"})

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

	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "chat", Origin: eventbus.OriginTool, Text: "previous"})

	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "chat", Origin: eventbus.OriginUser, Text: "hello"})

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

	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "reply", Origin: eventbus.OriginUser, Text: "hi"})

	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{SessionID: "reply", Text: "hello there"})

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
		eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "window", Origin: eventbus.OriginUser, Text: strconv.Itoa(i)})
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

	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "metadata", Origin: eventbus.OriginUser, Text: "payload", Annotations: annotations})

	replyMeta := map[string]string{
		" prompt ": strings.Repeat("z", maxMetadataValueRunes+50),
	}
	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
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
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "", Origin: eventbus.OriginUser, Text: "hello world"})

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
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "", Origin: eventbus.OriginTool, Text: "context message"})

	time.Sleep(20 * time.Millisecond)

	// Now send user message which should trigger prompt
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "", Origin: eventbus.OriginUser, Text: "list sessions"})

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
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "", Origin: eventbus.OriginUser, Text: "what sessions?"})

	// Send AI reply
	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{SessionID: "", Text: "You have no active sessions."})

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
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "", Origin: eventbus.OriginUser, Text: "hello"})

	select {
	case <-promptSub.C():
		t.Fatal("should not publish prompt when no globalStore configured")
	case <-time.After(100 * time.Millisecond):
		// Expected: no prompt published
	}
}

func TestConversation_SessionOutputRateLimiting(t *testing.T) {
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

	// First notable event should trigger prompt
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID:   "rate-limit-test",
		Origin:      eventbus.OriginTool,
		Text:        "first notable output",
		Annotations: map[string]string{"notable": "true"},
	})

	select {
	case env := <-promptSub.C():
		prompt, ok := env.Payload.(eventbus.ConversationPromptEvent)
		if !ok {
			t.Fatalf("expected ConversationPromptEvent, got %T", env.Payload)
		}
		if prompt.Metadata["event_type"] != "session_output" {
			t.Fatalf("expected event_type=session_output, got %q", prompt.Metadata["event_type"])
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for first prompt (should trigger)")
	}

	// Second notable event immediately after should be blocked (within 2s)
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID:   "rate-limit-test",
		Origin:      eventbus.OriginTool,
		Text:        "second notable output",
		Annotations: map[string]string{"notable": "true"},
	})

	select {
	case <-promptSub.C():
		t.Fatal("second prompt should be blocked by rate limiting")
	case <-time.After(200 * time.Millisecond):
		// Expected: no prompt due to rate limiting
	}

	// Manually set last output time to past to simulate >2s elapsed
	svc.lastSessionOutput.Store("rate-limit-test", time.Now().Add(-3*time.Second))

	// Third notable event after rate limit should trigger
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID:   "rate-limit-test",
		Origin:      eventbus.OriginTool,
		Text:        "third notable output",
		Annotations: map[string]string{"notable": "true"},
	})

	select {
	case env := <-promptSub.C():
		prompt, ok := env.Payload.(eventbus.ConversationPromptEvent)
		if !ok {
			t.Fatalf("expected ConversationPromptEvent, got %T", env.Payload)
		}
		if prompt.NewMessage.Text != "third notable output" {
			t.Fatalf("expected third output text, got %q", prompt.NewMessage.Text)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for third prompt (should trigger after rate limit)")
	}
}

func TestConversation_NotableTriggersAI(t *testing.T) {
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

	// Message without notable=true should NOT trigger prompt (unless from user)
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "notable-test",
		Origin:    eventbus.OriginTool,
		Text:      "regular output without notable",
	})

	select {
	case <-promptSub.C():
		t.Fatal("should not publish prompt for non-notable tool output")
	case <-time.After(100 * time.Millisecond):
		// Expected: no prompt
	}

	// Message with notable=true SHOULD trigger prompt
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID:   "notable-test",
		Origin:      eventbus.OriginTool,
		Text:        "error: compilation failed",
		Annotations: map[string]string{"notable": "true", "severity": "error"},
	})

	select {
	case env := <-promptSub.C():
		prompt, ok := env.Payload.(eventbus.ConversationPromptEvent)
		if !ok {
			t.Fatalf("expected ConversationPromptEvent, got %T", env.Payload)
		}
		if prompt.Metadata["event_type"] != "session_output" {
			t.Fatalf("expected event_type=session_output, got %q", prompt.Metadata["event_type"])
		}
		if prompt.Metadata["notable"] != "true" {
			t.Fatalf("expected notable=true in metadata")
		}
		if prompt.Metadata["severity"] != "error" {
			t.Fatalf("expected severity=error in metadata")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for notable prompt")
	}
}

func TestConversation_SessionOutputMetadataPropagation(t *testing.T) {
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

	// Send notable message with full annotations
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "metadata-test",
		Origin:    eventbus.OriginTool,
		Text:      "tool output text",
		Annotations: map[string]string{
			"notable":      "true",
			"tool":         "TestTool",
			"tool_id":      "test-tool",
			"tool_changed": "true",
			"idle_state":   "prompt",
			"waiting_for":  "user_input",
		},
	})

	select {
	case env := <-promptSub.C():
		prompt, ok := env.Payload.(eventbus.ConversationPromptEvent)
		if !ok {
			t.Fatalf("expected ConversationPromptEvent, got %T", env.Payload)
		}

		// Verify event_type
		if prompt.Metadata["event_type"] != "session_output" {
			t.Errorf("expected event_type=session_output, got %q", prompt.Metadata["event_type"])
		}

		// Verify session_output field contains the turn text
		if prompt.Metadata["session_output"] != "tool output text" {
			t.Errorf("expected session_output=%q, got %q", "tool output text", prompt.Metadata["session_output"])
		}

		// Verify tool_changed propagation
		if prompt.Metadata["tool_changed"] != "true" {
			t.Errorf("expected tool_changed=true, got %q", prompt.Metadata["tool_changed"])
		}

		// Verify other metadata propagation
		if prompt.Metadata["tool"] != "TestTool" {
			t.Errorf("expected tool=TestTool, got %q", prompt.Metadata["tool"])
		}
		if prompt.Metadata["tool_id"] != "test-tool" {
			t.Errorf("expected tool_id=test-tool, got %q", prompt.Metadata["tool_id"])
		}
		if prompt.Metadata["idle_state"] != "prompt" {
			t.Errorf("expected idle_state=prompt, got %q", prompt.Metadata["idle_state"])
		}
		if prompt.Metadata["waiting_for"] != "user_input" {
			t.Errorf("expected waiting_for=user_input, got %q", prompt.Metadata["waiting_for"])
		}

	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for prompt")
	}
}

func TestConversation_RateLimitingCleanup(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	sessionID := "cleanup-test"

	// Add a turn and trigger rate limiting
	svc.lastSessionOutput.Store(sessionID, time.Now())

	// Verify the entry exists
	if _, ok := svc.lastSessionOutput.Load(sessionID); !ok {
		t.Fatal("expected lastSessionOutput entry to exist")
	}

	// Clear session should remove the rate limiting entry
	svc.clearSession(sessionID)

	// Verify the entry is removed
	if _, ok := svc.lastSessionOutput.Load(sessionID); ok {
		t.Fatal("expected lastSessionOutput entry to be deleted after clearSession")
	}
}

// --- History Summary Tests ---

func TestSummaryTrigger(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus, WithHistoryLimit(50))
	// Lower threshold for testing
	svc.summaryThreshold = 5
	svc.summaryBatchSize = 3

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	promptSub := bus.Subscribe(eventbus.TopicConversationPrompt)
	defer promptSub.Close()

	now := time.Now().UTC()

	// Add 5 turns to reach threshold (use OriginUser for first 4, OriginTool for last to avoid rapid user_intent prompts)
	for i := 0; i < 5; i++ {
		svc.handlePipelineMessage(now.Add(time.Duration(i)*time.Millisecond), eventbus.PipelineMessageEvent{
			SessionID: "summary-trigger",
			Origin:    eventbus.OriginUser,
			Text:      "message " + strconv.Itoa(i),
		})
	}

	// Expect the user_intent prompts (5 of them) plus one history_summary prompt
	var summaryPrompt *eventbus.ConversationPromptEvent
	deadline := time.After(2 * time.Second)
	for {
		select {
		case env := <-promptSub.C():
			prompt, ok := env.Payload.(eventbus.ConversationPromptEvent)
			if !ok {
				continue
			}
			if prompt.Metadata["event_type"] == "history_summary" {
				summaryPrompt = &prompt
			}
		case <-deadline:
			if summaryPrompt == nil {
				t.Fatal("timeout waiting for history_summary prompt")
			}
		}
		if summaryPrompt != nil {
			break
		}
	}

	if summaryPrompt.SessionID != "summary-trigger" {
		t.Fatalf("expected session summary-trigger, got %s", summaryPrompt.SessionID)
	}
	if summaryPrompt.PromptID == "" {
		t.Fatal("expected non-empty PromptID")
	}
	// The new message text should contain serialized turns (fallback for adapters without prompt engine)
	if !strings.Contains(summaryPrompt.NewMessage.Text, "[user] message 0") {
		t.Fatalf("expected serialized turns in prompt text, got: %s", summaryPrompt.NewMessage.Text)
	}
	// Context should contain structured turns for prompt engine (populates {{.history}})
	if len(summaryPrompt.Context) != 3 {
		t.Fatalf("expected 3 turns in Context (summaryBatchSize=3), got %d", len(summaryPrompt.Context))
	}
	if summaryPrompt.Context[0].Text != "message 0" {
		t.Fatalf("expected first context turn text 'message 0', got %q", summaryPrompt.Context[0].Text)
	}
}

func TestSummaryReplyReplacesTurns(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus, WithHistoryLimit(50))
	svc.summaryThreshold = 5
	svc.summaryBatchSize = 3

	now := time.Now().UTC()

	// Pre-populate 6 turns directly
	svc.mu.Lock()
	for i := 0; i < 6; i++ {
		svc.sessions["summary-replace"] = append(svc.sessions["summary-replace"], eventbus.ConversationTurn{
			Origin: eventbus.OriginUser,
			Text:   "turn " + strconv.Itoa(i),
			At:     now.Add(time.Duration(i) * time.Millisecond),
		})
	}
	svc.mu.Unlock()

	// Manually trigger summary
	svc.requestSummary("summary-replace")

	// Get the pending request to find the promptID
	val, ok := svc.pendingSummary.Load("summary-replace")
	if !ok {
		t.Fatal("expected pending summary request")
	}
	req := val.(*summaryRequest)
	promptID := req.promptID

	// Simulate AI reply
	svc.handleSummaryReply("summary-replace", eventbus.ConversationReplyEvent{
		SessionID: "summary-replace",
		PromptID:  promptID,
		Text:      "Summary: user discussed turns 0-2",
		Metadata:  map[string]string{"event_type": "history_summary"},
	})

	// Verify history was replaced
	history := svc.Context("summary-replace")
	// Should be 1 summary + 3 remaining = 4 turns
	if len(history) != 4 {
		t.Fatalf("expected 4 turns after summary, got %d", len(history))
	}

	// First turn should be the summary
	if history[0].Origin != eventbus.OriginSystem {
		t.Fatalf("expected system origin for summary turn, got %s", history[0].Origin)
	}
	if history[0].Text != "Summary: user discussed turns 0-2" {
		t.Fatalf("unexpected summary text: %s", history[0].Text)
	}
	if history[0].Meta["summarized"] != "true" {
		t.Fatalf("expected summarized=true, got %+v", history[0].Meta)
	}
	if history[0].Meta["original_length"] != "3" {
		t.Fatalf("expected original_length=3, got %s", history[0].Meta["original_length"])
	}

	// Summary turn should preserve the timestamp of the first summarized turn
	// so that sort.SliceStable keeps it in chronological order
	if !history[0].At.Equal(now) {
		t.Fatalf("expected summary timestamp to equal first summarized turn (%v), got %v", now, history[0].At)
	}

	// Verify chronological ordering is maintained (summary before remaining turns)
	for i := 1; i < len(history); i++ {
		if history[i].At.Before(history[i-1].At) {
			t.Fatalf("history not in chronological order at index %d: %v before %v",
				i, history[i].At, history[i-1].At)
		}
	}

	// Remaining turns should be the original turns 3-5
	if history[1].Text != "turn 3" {
		t.Fatalf("expected turn 3, got %s", history[1].Text)
	}

	// Pending summary should be cleared
	if _, ok := svc.pendingSummary.Load("summary-replace"); ok {
		t.Fatal("expected pending summary to be cleared after reply")
	}
}

func TestSummaryIdempotency(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus, WithHistoryLimit(50))
	svc.summaryThreshold = 5
	svc.summaryBatchSize = 3

	now := time.Now().UTC()

	// Pre-populate enough turns
	svc.mu.Lock()
	for i := 0; i < 6; i++ {
		svc.sessions["idempotent"] = append(svc.sessions["idempotent"], eventbus.ConversationTurn{
			Origin: eventbus.OriginUser,
			Text:   "turn " + strconv.Itoa(i),
			At:     now.Add(time.Duration(i) * time.Millisecond),
		})
	}
	svc.mu.Unlock()

	// First call should succeed
	svc.requestSummary("idempotent")
	val1, ok := svc.pendingSummary.Load("idempotent")
	if !ok {
		t.Fatal("expected pending summary after first call")
	}
	req1 := val1.(*summaryRequest)
	firstPromptID := req1.promptID

	// Second call should be a no-op (already pending)
	svc.requestSummary("idempotent")
	val2, _ := svc.pendingSummary.Load("idempotent")
	req2 := val2.(*summaryRequest)

	// Should be the same request (same promptID)
	if req2.promptID != firstPromptID {
		t.Fatalf("expected same promptID after duplicate call, got %s vs %s", firstPromptID, req2.promptID)
	}

	// Clean up timer
	if req1.timer != nil {
		req1.timer.Stop()
	}
}

func TestSummaryTimeout(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus, WithHistoryLimit(50))
	svc.summaryThreshold = 5
	svc.summaryBatchSize = 3

	now := time.Now().UTC()

	// Pre-populate enough turns
	svc.mu.Lock()
	for i := 0; i < 6; i++ {
		svc.sessions["timeout-sess"] = append(svc.sessions["timeout-sess"], eventbus.ConversationTurn{
			Origin: eventbus.OriginUser,
			Text:   "turn " + strconv.Itoa(i),
			At:     now.Add(time.Duration(i) * time.Millisecond),
		})
	}
	svc.mu.Unlock()

	// Trigger summary
	svc.requestSummary("timeout-sess")
	val, ok := svc.pendingSummary.Load("timeout-sess")
	if !ok {
		t.Fatal("expected pending summary")
	}
	req := val.(*summaryRequest)

	// Replace the timer with a very short one for testing
	req.timer.Stop()
	req.timer = time.AfterFunc(50*time.Millisecond, func() {
		svc.pendingSummary.Delete("timeout-sess")
	})

	// Wait for timeout
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		if _, ok := svc.pendingSummary.Load("timeout-sess"); !ok {
			break // Timeout cleared the pending state
		}
		if time.Now().After(deadline) {
			t.Fatal("expected pending summary to be cleared after timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSummaryPromptIDMismatch(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus, WithHistoryLimit(50))
	svc.summaryThreshold = 5
	svc.summaryBatchSize = 3

	now := time.Now().UTC()

	// Pre-populate turns
	svc.mu.Lock()
	for i := 0; i < 6; i++ {
		svc.sessions["mismatch"] = append(svc.sessions["mismatch"], eventbus.ConversationTurn{
			Origin: eventbus.OriginUser,
			Text:   "turn " + strconv.Itoa(i),
			At:     now.Add(time.Duration(i) * time.Millisecond),
		})
	}
	svc.mu.Unlock()

	// Trigger summary
	svc.requestSummary("mismatch")
	val, ok := svc.pendingSummary.Load("mismatch")
	if !ok {
		t.Fatal("expected pending summary")
	}
	req := val.(*summaryRequest)
	if req.timer != nil {
		defer req.timer.Stop()
	}

	// Send reply with wrong PromptID
	svc.handleSummaryReply("mismatch", eventbus.ConversationReplyEvent{
		SessionID: "mismatch",
		PromptID:  "wrong-prompt-id",
		Text:      "bogus summary",
		Metadata:  map[string]string{"event_type": "history_summary"},
	})

	// History should be unchanged (6 turns)
	history := svc.Context("mismatch")
	if len(history) != 6 {
		t.Fatalf("expected 6 turns (unchanged), got %d", len(history))
	}

	// Pending summary should have been consumed by LoadAndDelete even though
	// the PromptID didn't match, which prevents retries with the same request
	if _, ok := svc.pendingSummary.Load("mismatch"); ok {
		t.Fatal("expected pending summary to be consumed (LoadAndDelete)")
	}
}

func TestSummaryClearSession(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus, WithHistoryLimit(50))
	svc.summaryThreshold = 5
	svc.summaryBatchSize = 3

	now := time.Now().UTC()

	// Pre-populate turns
	svc.mu.Lock()
	for i := 0; i < 6; i++ {
		svc.sessions["clear-sess"] = append(svc.sessions["clear-sess"], eventbus.ConversationTurn{
			Origin: eventbus.OriginUser,
			Text:   "turn " + strconv.Itoa(i),
			At:     now.Add(time.Duration(i) * time.Millisecond),
		})
	}
	svc.mu.Unlock()

	// Trigger summary to create pending state
	svc.requestSummary("clear-sess")
	if _, ok := svc.pendingSummary.Load("clear-sess"); !ok {
		t.Fatal("expected pending summary before clear")
	}

	// Clear the session
	svc.clearSession("clear-sess")

	// Pending summary should be cleaned up
	if _, ok := svc.pendingSummary.Load("clear-sess"); ok {
		t.Fatal("expected pending summary to be cleared after clearSession")
	}

	// History should be empty
	history := svc.Context("clear-sess")
	if len(history) != 0 {
		t.Fatalf("expected empty history after clearSession, got %d", len(history))
	}
}

func TestSerializeTurnsForSummary(t *testing.T) {
	turns := []eventbus.ConversationTurn{
		{Origin: eventbus.OriginUser, Text: "hello"},
		{Origin: eventbus.OriginAI, Text: "hi there"},
		{Origin: eventbus.OriginTool, Text: "tool output"},
		{Origin: eventbus.OriginSystem, Text: "system note"},
	}

	result := serializeTurnsForSummary(turns)
	expected := "[user] hello\n[assistant] hi there\n[tool] tool output\n[system] system note"
	if result != expected {
		t.Fatalf("unexpected serialization:\ngot:  %q\nwant: %q", result, expected)
	}
}

func TestSerializeTurnsForSummary_Empty(t *testing.T) {
	result := serializeTurnsForSummary(nil)
	if result != "" {
		t.Fatalf("expected empty string for nil turns, got %q", result)
	}
}

func TestSummaryBatchSizeValidation(t *testing.T) {
	bus := eventbus.New()

	// If batchSize >= threshold, NewService should clamp batchSize
	svc := NewService(bus, WithSummaryThreshold(5), WithSummaryBatchSize(10))
	if svc.summaryBatchSize >= svc.summaryThreshold {
		t.Fatalf("expected summaryBatchSize < summaryThreshold, got batch=%d threshold=%d",
			svc.summaryBatchSize, svc.summaryThreshold)
	}

	// Equal values should also be clamped
	svc2 := NewService(bus, WithSummaryThreshold(4), WithSummaryBatchSize(4))
	if svc2.summaryBatchSize >= svc2.summaryThreshold {
		t.Fatalf("expected summaryBatchSize < summaryThreshold for equal values, got batch=%d threshold=%d",
			svc2.summaryBatchSize, svc2.summaryThreshold)
	}
}

func TestSummaryShutdownCleansTimers(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus, WithHistoryLimit(50))
	svc.summaryThreshold = 5
	svc.summaryBatchSize = 3

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}

	now := time.Now().UTC()

	// Pre-populate turns and trigger summary
	svc.mu.Lock()
	for i := 0; i < 6; i++ {
		svc.sessions["shutdown-sess"] = append(svc.sessions["shutdown-sess"], eventbus.ConversationTurn{
			Origin: eventbus.OriginUser,
			Text:   "turn " + strconv.Itoa(i),
			At:     now.Add(time.Duration(i) * time.Millisecond),
		})
	}
	svc.mu.Unlock()

	svc.requestSummary("shutdown-sess")
	if _, ok := svc.pendingSummary.Load("shutdown-sess"); !ok {
		t.Fatal("expected pending summary before shutdown")
	}

	// Shutdown should clean up pending timers
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Pending summary should be cleaned up
	if _, ok := svc.pendingSummary.Load("shutdown-sess"); ok {
		t.Fatal("expected pending summary to be cleaned after shutdown")
	}
}

func TestSummaryThresholdClampedToMaxHistory(t *testing.T) {
	bus := eventbus.New()
	// summaryThreshold=35 but maxHistory=10 — should clamp threshold to 10
	svc := NewService(bus, WithHistoryLimit(10), WithSummaryThreshold(35))
	if svc.summaryThreshold > svc.maxHistory {
		t.Fatalf("expected summaryThreshold <= maxHistory, got threshold=%d maxHistory=%d",
			svc.summaryThreshold, svc.maxHistory)
	}
	// batchSize should also be adjusted since it must be < clamped threshold
	if svc.summaryBatchSize >= svc.summaryThreshold {
		t.Fatalf("expected batchSize < threshold after clamping, got batch=%d threshold=%d",
			svc.summaryBatchSize, svc.summaryThreshold)
	}
}

func TestSummaryWithOptionFunctions(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus, WithSummaryThreshold(10), WithSummaryBatchSize(5))

	if svc.summaryThreshold != 10 {
		t.Fatalf("expected threshold 10, got %d", svc.summaryThreshold)
	}
	if svc.summaryBatchSize != 5 {
		t.Fatalf("expected batchSize 5, got %d", svc.summaryBatchSize)
	}
}
