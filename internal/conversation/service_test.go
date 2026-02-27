package conversation

import (
	"context"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

func TestConversationPublishesPromptOnUserInput(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt)
	defer promptSub.Close()

	turnSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Turn)
	defer turnSub.Close()

	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "chat", Origin: eventbus.OriginTool, Text: "previous"})

	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "chat", Origin: eventbus.OriginUser, Text: "hello"})

	select {
	case env := <-promptSub.C():
		prompt := env.Payload
		if prompt.SessionID != "chat" {
			t.Fatalf("unexpected session id: %s", prompt.SessionID)
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

	// Verify turn events are published (should have at least the user turn)
	var userTurnFound bool
	deadline := time.After(time.Second)
	for !userTurnFound {
		select {
		case env := <-turnSub.C():
			if env.Payload.Turn.Origin == eventbus.OriginUser && env.Payload.Turn.Text == "hello" {
				userTurnFound = true
			}
		case <-deadline:
			t.Fatal("timeout waiting for user turn event")
		}
	}
}

func TestConversationStoresReplies_PublishesTurnEvent(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	turnSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Turn)
	defer turnSub.Close()

	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "reply", Origin: eventbus.OriginUser, Text: "hi"})

	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{SessionID: "reply", Text: "hello there"})

	// Verify turn events are published for both user and AI turns
	var foundUser, foundAI bool
	deadline := time.After(2 * time.Second)
	for !foundUser || !foundAI {
		select {
		case env := <-turnSub.C():
			if env.Payload.SessionID != "reply" {
				continue
			}
			if env.Payload.Turn.Origin == eventbus.OriginUser && env.Payload.Turn.Text == "hi" {
				foundUser = true
			}
			if env.Payload.Turn.Origin == eventbus.OriginAI && env.Payload.Turn.Text == "hello there" {
				foundAI = true
			}
		case <-deadline:
			t.Fatalf("timeout: foundUser=%v foundAI=%v", foundUser, foundAI)
		}
	}

	// Context() now returns nil (RAM history removed)
	if ctx := svc.Context("reply"); ctx != nil {
		t.Fatalf("expected nil context, got %v", ctx)
	}
}

func TestConversationContextReturnsNil(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	svc := NewService(bus)

	if ctx := svc.Context("anything"); ctx != nil {
		t.Fatalf("expected nil, got %v", ctx)
	}
}

func TestConversationSliceReturnsEmpty(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	svc := NewService(bus)

	total, slice := svc.Slice("anything", 0, 10)
	if total != 0 {
		t.Fatalf("expected total=0, got %d", total)
	}
	if slice != nil {
		t.Fatalf("expected nil slice, got %v", slice)
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

	turnSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Turn)
	defer turnSub.Close()

	annotations := make(map[string]string)
	for i := 0; i < maxMetadataEntries+10; i++ {
		key := "key-" + string(rune('a'+i%26))
		value := "value"
		annotations[key] = value
	}

	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "metadata", Origin: eventbus.OriginUser, Text: "payload", Annotations: annotations})

	select {
	case env := <-turnSub.C():
		turn := env.Payload.Turn
		if len(turn.Meta) > maxMetadataEntries {
			t.Fatalf("expected metadata entries <= %d, got %d", maxMetadataEntries, len(turn.Meta))
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for turn event")
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

	turnSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Turn)
	defer turnSub.Close()

	// Send a sessionless message (empty SessionID)
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "", Origin: eventbus.OriginUser, Text: "hello world"})

	// Verify turn event is published for sessionless message
	select {
	case env := <-turnSub.C():
		if env.Payload.SessionID != "" {
			t.Fatalf("expected empty sessionID for sessionless turn, got %q", env.Payload.SessionID)
		}
		if env.Payload.Turn.Text != "hello world" {
			t.Fatalf("expected turn text=hello world, got %q", env.Payload.Turn.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for sessionless turn event")
	}

	// Should also be stored in GlobalStore
	if globalStore.Len() != 1 {
		t.Fatalf("expected 1 turn in GlobalStore, got %d", globalStore.Len())
	}

	ctx2 := globalStore.GetContext()
	if ctx2[0].Text != "hello world" {
		t.Fatalf("unexpected text in GlobalStore: %s", ctx2[0].Text)
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

	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt)
	defer promptSub.Close()

	turnSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Turn)
	defer turnSub.Close()

	// Add context first
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "", Origin: eventbus.OriginTool, Text: "context message"})

	// Wait for tool turn event — proves context message was processed and added to GlobalStore
	select {
	case <-turnSub.C():
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for tool turn event")
	}

	// Now send user message which should trigger prompt
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "", Origin: eventbus.OriginUser, Text: "list sessions"})

	select {
	case env := <-promptSub.C():
		prompt := env.Payload
		if prompt.SessionID != "" {
			t.Fatalf("expected empty session id for sessionless prompt, got %q", prompt.SessionID)
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

	turnSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Turn)
	defer turnSub.Close()

	// Send user message
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{SessionID: "", Origin: eventbus.OriginUser, Text: "what sessions?"})

	// Wait for user turn event — proves conversation service processed the message
	select {
	case env := <-turnSub.C():
		if env.Payload.Turn.Origin != eventbus.OriginUser || env.Payload.Turn.Text != "what sessions?" {
			t.Fatalf("expected user turn, got %+v", env.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for user turn event")
	}

	// Send AI reply (user message guaranteed in GlobalStore)
	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{SessionID: "", Text: "You have no active sessions."})

	// Wait for AI turn event
	select {
	case env := <-turnSub.C():
		if env.Payload.SessionID != "" {
			t.Fatalf("expected empty sessionID, got %q", env.Payload.SessionID)
		}
		if env.Payload.Turn.Origin != eventbus.OriginAI || env.Payload.Turn.Text != "You have no active sessions." {
			t.Fatalf("expected AI turn, got %+v", env.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for AI turn event")
	}

	// Verify GlobalStore also has both turns
	ctx2 := globalStore.GetContext()
	if len(ctx2) < 2 {
		t.Fatalf("expected at least 2 turns in GlobalStore, got %d", len(ctx2))
	}
	if ctx2[1].Origin != eventbus.OriginAI {
		t.Fatalf("expected AI origin, got %s", ctx2[1].Origin)
	}
	if ctx2[1].Text != "You have no active sessions." {
		t.Fatalf("unexpected reply text: %s", ctx2[1].Text)
	}
}

func TestConversationGlobalContext(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	bus := eventbus.New()
	globalStore := NewGlobalStore()
	svc := NewService(bus, WithGlobalStore(globalStore))

	// Add turns
	for i := 0; i < 5; i++ {
		globalStore.AddTurn(eventbus.ConversationTurn{
			Origin: eventbus.OriginUser,
			Text:   string(rune('0' + i)),
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
	t.Parallel()
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

	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt)
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

	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt)
	defer promptSub.Close()

	// First notable event should trigger prompt
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID:   "rate-limit-test",
		Origin:      eventbus.OriginTool,
		Text:        "first notable output",
		Annotations: map[string]string{constants.MetadataKeyNotable: constants.MetadataValueTrue},
	})

	select {
	case env := <-promptSub.C():
		prompt := env.Payload
		if prompt.Metadata[constants.MetadataKeyEventType] != constants.PromptEventSessionOutput {
			t.Fatalf("expected event_type=session_output, got %q", prompt.Metadata[constants.MetadataKeyEventType])
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for first prompt (should trigger)")
	}

	// Second notable event immediately after should be blocked (within 2s)
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID:   "rate-limit-test",
		Origin:      eventbus.OriginTool,
		Text:        "second notable output",
		Annotations: map[string]string{constants.MetadataKeyNotable: constants.MetadataValueTrue},
	})

	select {
	case <-promptSub.C():
		t.Fatal("second prompt should be blocked by rate limiting")
	case <-time.After(200 * time.Millisecond):
		// Expected: no prompt due to rate limiting
	}

	// Reset rate limit timestamp to simulate >2s elapsed
	svc.ResetRateLimitForTesting("rate-limit-test", time.Now().Add(-3*time.Second))

	// Third notable event after rate limit should trigger
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID:   "rate-limit-test",
		Origin:      eventbus.OriginTool,
		Text:        "third notable output",
		Annotations: map[string]string{constants.MetadataKeyNotable: constants.MetadataValueTrue},
	})

	select {
	case env := <-promptSub.C():
		prompt := env.Payload
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

	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt)
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
		Annotations: map[string]string{constants.MetadataKeyNotable: constants.MetadataValueTrue, constants.MetadataKeySeverity: "error"},
	})

	select {
	case env := <-promptSub.C():
		prompt := env.Payload
		if prompt.Metadata[constants.MetadataKeyEventType] != constants.PromptEventSessionOutput {
			t.Fatalf("expected event_type=session_output, got %q", prompt.Metadata[constants.MetadataKeyEventType])
		}
		if prompt.Metadata[constants.MetadataKeyNotable] != "true" {
			t.Fatalf("expected notable=true in metadata")
		}
		if prompt.Metadata[constants.MetadataKeySeverity] != "error" {
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

	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt)
	defer promptSub.Close()

	// Send notable message with full annotations
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "metadata-test",
		Origin:    eventbus.OriginTool,
		Text:      "tool output text",
		Annotations: map[string]string{
			constants.MetadataKeyNotable:     constants.MetadataValueTrue,
			constants.MetadataKeyTool:        "TestTool",
			constants.MetadataKeyToolID:      "test-tool",
			constants.MetadataKeyToolChanged: constants.MetadataValueTrue,
			constants.MetadataKeyIdleState:   "prompt",
			constants.MetadataKeyWaitingFor:  constants.PipelineWaitingForUserInput,
		},
	})

	select {
	case env := <-promptSub.C():
		prompt := env.Payload

		// Verify event_type
		if prompt.Metadata[constants.MetadataKeyEventType] != constants.PromptEventSessionOutput {
			t.Errorf("expected event_type=session_output, got %q", prompt.Metadata[constants.MetadataKeyEventType])
		}

		// Verify session_output field contains the turn text
		if prompt.Metadata[constants.MetadataKeySessionOutput] != "tool output text" {
			t.Errorf("expected session_output=%q, got %q", "tool output text", prompt.Metadata[constants.MetadataKeySessionOutput])
		}

		// Verify tool_changed propagation
		if prompt.Metadata[constants.MetadataKeyToolChanged] != "true" {
			t.Errorf("expected tool_changed=true, got %q", prompt.Metadata[constants.MetadataKeyToolChanged])
		}

		// Verify other metadata propagation
		if prompt.Metadata[constants.MetadataKeyTool] != "TestTool" {
			t.Errorf("expected tool=TestTool, got %q", prompt.Metadata[constants.MetadataKeyTool])
		}
		if prompt.Metadata[constants.MetadataKeyToolID] != "test-tool" {
			t.Errorf("expected tool_id=test-tool, got %q", prompt.Metadata[constants.MetadataKeyToolID])
		}
		if prompt.Metadata[constants.MetadataKeyIdleState] != "prompt" {
			t.Errorf("expected idle_state=prompt, got %q", prompt.Metadata[constants.MetadataKeyIdleState])
		}
		if prompt.Metadata[constants.MetadataKeyWaitingFor] != constants.PipelineWaitingForUserInput {
			t.Errorf("expected waiting_for=user_input, got %q", prompt.Metadata[constants.MetadataKeyWaitingFor])
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

	// Add a rate limiting entry
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

// --- New Turn Publishing Tests ---

func TestPublishTurnOnUserMessage(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	turnSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Turn)
	defer turnSub.Close()

	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "user-turn-test",
		Origin:    eventbus.OriginUser,
		Text:      "hello world",
	})

	select {
	case env := <-turnSub.C():
		te := env.Payload
		if te.SessionID != "user-turn-test" {
			t.Fatalf("expected sessionID=user-turn-test, got %q", te.SessionID)
		}
		if te.Turn.Origin != eventbus.OriginUser {
			t.Fatalf("expected origin=user, got %q", te.Turn.Origin)
		}
		if te.Turn.Text != "hello world" {
			t.Fatalf("expected text=hello world, got %q", te.Turn.Text)
		}
		if te.Turn.At.IsZero() {
			t.Fatal("expected non-zero timestamp")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for turn event")
	}
}

func TestPublishTurnOnAIReply(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	turnSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Turn)
	defer turnSub.Close()

	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
		SessionID: "ai-turn-test",
		PromptID:  "prompt-1",
		Text:      "I am an AI",
	})

	select {
	case env := <-turnSub.C():
		te := env.Payload
		if te.SessionID != "ai-turn-test" {
			t.Fatalf("expected sessionID=ai-turn-test, got %q", te.SessionID)
		}
		if te.Turn.Origin != eventbus.OriginAI {
			t.Fatalf("expected origin=ai, got %q", te.Turn.Origin)
		}
		if te.Turn.Text != "I am an AI" {
			t.Fatalf("expected text=I am an AI, got %q", te.Turn.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for AI turn event")
	}
}

func TestPublishTurnOnSessionOutput(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	turnSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Turn)
	defer turnSub.Close()

	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID:   "output-turn-test",
		Origin:      eventbus.OriginTool,
		Text:        "tool output",
		Annotations: map[string]string{constants.MetadataKeyNotable: constants.MetadataValueTrue},
	})

	select {
	case env := <-turnSub.C():
		te := env.Payload
		if te.SessionID != "output-turn-test" {
			t.Fatalf("expected sessionID=output-turn-test, got %q", te.SessionID)
		}
		if te.Turn.Origin != eventbus.OriginTool {
			t.Fatalf("expected origin=tool, got %q", te.Turn.Origin)
		}
		if te.Turn.Text != "tool output" {
			t.Fatalf("expected text=tool output, got %q", te.Turn.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for session output turn event")
	}
}

func TestNonNotableToolOutputPublishesTurnButNotPrompt(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	turnSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Turn)
	defer turnSub.Close()

	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt)
	defer promptSub.Close()

	// Non-notable tool output — should publish turn but NOT prompt
	eventbus.Publish(context.Background(), bus, eventbus.Pipeline.Cleaned, eventbus.SourceContentPipeline, eventbus.PipelineMessageEvent{
		SessionID: "tool-turn-test",
		Origin:    eventbus.OriginTool,
		Text:      "regular output",
	})

	// Verify turn event IS published
	select {
	case env := <-turnSub.C():
		if env.Payload.Turn.Origin != eventbus.OriginTool {
			t.Fatalf("expected origin=tool, got %q", env.Payload.Turn.Origin)
		}
		if env.Payload.Turn.Text != "regular output" {
			t.Fatalf("expected text=regular output, got %q", env.Payload.Turn.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for turn event")
	}

	// Verify prompt is NOT published — use the turn event as barrier
	// (same consumer goroutine processed the pipeline message, so if the
	// turn arrived, any prompt would have been published already)
	select {
	case <-promptSub.C():
		t.Fatal("non-notable tool output should not trigger prompt")
	case <-time.After(50 * time.Millisecond):
		// Expected: no prompt
	}
}

func TestInterceptJournalCompaction(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	turnSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Turn)
	defer turnSub.Close()

	// Send a journal_compaction reply — should be intercepted
	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
		SessionID: "compaction-test",
		PromptID:  "prompt-journal",
		Text:      "compacted journal summary",
		Metadata:  map[string]string{constants.MetadataKeyEventType: constants.PromptEventJournalCompaction},
	})

	// Publish a normal reply as barrier — its turn event proves the compaction was already processed
	// (same topic, same consumer goroutine, sequential delivery)
	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
		SessionID: "compaction-test",
		PromptID:  "prompt-barrier",
		Text:      "barrier reply",
	})

	select {
	case env := <-turnSub.C():
		if env.Payload.Turn.Text == "compacted journal summary" {
			t.Fatal("journal_compaction should be intercepted, but got its turn event")
		}
		if env.Payload.Turn.Text != "barrier reply" {
			t.Fatalf("expected barrier turn, got %q", env.Payload.Turn.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for barrier turn event")
	}
}

func TestInterceptConversationCompaction(t *testing.T) {
	bus := eventbus.New()
	svc := NewService(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	defer svc.Shutdown(context.Background())

	turnSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Turn)
	defer turnSub.Close()

	// Send a conversation_compaction reply — should be intercepted
	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
		SessionID: "compaction-test",
		PromptID:  "prompt-conv",
		Text:      "compacted conversation summary",
		Metadata:  map[string]string{constants.MetadataKeyEventType: constants.PromptEventConversationCompaction},
	})

	// Publish a normal reply as barrier — its turn event proves the compaction was already processed
	eventbus.Publish(context.Background(), bus, eventbus.Conversation.Reply, eventbus.SourceConversation, eventbus.ConversationReplyEvent{
		SessionID: "compaction-test",
		PromptID:  "prompt-barrier",
		Text:      "barrier reply",
	})

	select {
	case env := <-turnSub.C():
		if env.Payload.Turn.Text == "compacted conversation summary" {
			t.Fatal("conversation_compaction should be intercepted, but got its turn event")
		}
		if env.Payload.Turn.Text != "barrier reply" {
			t.Fatalf("expected barrier turn, got %q", env.Payload.Turn.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for barrier turn event")
	}
}

func TestSerializeTurns(t *testing.T) {
	t.Parallel()
	turns := []eventbus.ConversationTurn{
		{Origin: eventbus.OriginUser, Text: "hello"},
		{Origin: eventbus.OriginAI, Text: "hi there"},
		{Origin: eventbus.OriginTool, Text: "tool output"},
		{Origin: eventbus.OriginSystem, Text: "system note"},
	}

	result := eventbus.SerializeTurns(turns)
	expected := "[user] hello\n[assistant] hi there\n[tool] tool output\n[system] system note"
	if result != expected {
		t.Fatalf("unexpected serialization:\ngot:  %q\nwant: %q", result, expected)
	}
}

func TestSerializeTurns_Empty(t *testing.T) {
	t.Parallel()
	result := eventbus.SerializeTurns(nil)
	if result != "" {
		t.Fatalf("expected empty string for nil turns, got %q", result)
	}
}

func TestValidEventTypesSyncWithPromptTemplates(t *testing.T) {
	templates := store.DefaultPromptTemplates()

	for key := range templates {
		if !validEventTypes[key] {
			t.Errorf("template key %q missing from validEventTypes", key)
		}
	}
	for key := range validEventTypes {
		if _, ok := templates[key]; !ok {
			t.Errorf("validEventTypes key %q missing from DefaultPromptTemplates", key)
		}
	}
}
