package adapters

import (
	"strings"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

func TestAdapterLogWriter_BasicPublish(t *testing.T) {
	bus := eventbus.New()
	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Log)
	defer sub.Close()

	writer := newAdapterLogWriter(bus, "test-adapter", SlotSTT, eventbus.LogLevelInfo)

	// Write a single line.
	_, err := writer.Write([]byte("test message\n"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Should receive the message.
	select {
	case env := <-sub.C():
		evt := env.Payload
		if evt.Message != "test message" {
			t.Errorf("expected 'test message', got %q", evt.Message)
		}
		if evt.AdapterID != "test-adapter" {
			t.Errorf("expected adapter ID 'test-adapter', got %q", evt.AdapterID)
		}
		if evt.Level != eventbus.LogLevelInfo {
			t.Errorf("expected level info, got %q", evt.Level)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for log event")
	}
}

func TestAdapterLogWriter_RateLimiting(t *testing.T) {
	bus := eventbus.New()
	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Log, eventbus.WithSubscriptionBuffer(200))
	defer sub.Close()

	writer := newAdapterLogWriter(bus, "chatty-adapter", SlotSTT, eventbus.LogLevelInfo)

	// Publish burst_size + 50 messages instantly (should trigger rate limiting).
	messagesToSend := rateLimitBurstSize + 50
	for i := 0; i < messagesToSend; i++ {
		_, err := writer.Write([]byte("rapid message\n"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	// Wait a bit for all publishes to complete.
	time.Sleep(50 * time.Millisecond)

	// Count received messages by draining channel.
	receivedCount := 0
	draining := true
	for draining {
		select {
		case <-sub.C():
			receivedCount++
		default:
			draining = false
		}
	}

	// We should have received approximately burst_size messages.
	// Some messages should have been dropped.
	if receivedCount >= messagesToSend {
		t.Errorf("expected rate limiting to drop some messages, but received %d/%d", receivedCount, messagesToSend)
	}

	if receivedCount > rateLimitBurstSize+5 {
		t.Errorf("expected ~%d messages (burst size), got %d", rateLimitBurstSize, receivedCount)
	}

	t.Logf("Sent %d messages, received %d (dropped ~%d)", messagesToSend, receivedCount, messagesToSend-receivedCount)
}

func TestAdapterLogWriter_TokenRefill(t *testing.T) {
	bus := eventbus.New()
	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Log, eventbus.WithSubscriptionBuffer(200))
	defer sub.Close()

	writer := newAdapterLogWriter(bus, "test-adapter", SlotSTT, eventbus.LogLevelInfo)

	// Send burst_size messages instantly (should consume all tokens).
	for i := 0; i < rateLimitBurstSize; i++ {
		_, err := writer.Write([]byte("burst message\n"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	// Send one more (should be dropped).
	_, err := writer.Write([]byte("dropped message\n"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Drain messages received so far.
	droppedCount := 0
	drainTimer := time.NewTimer(200 * time.Millisecond)
drainLoop:
	for {
		select {
		case <-sub.C():
			droppedCount++
		case <-drainTimer.C:
			break drainLoop
		}
	}

	// Wait for tokens to refill (50 msg/s = 20ms per message).
	// Wait 100ms to get ~5 tokens back.
	time.Sleep(100 * time.Millisecond)

	// Now send 5 more messages - they should be published.
	for i := 0; i < 5; i++ {
		_, err := writer.Write([]byte("refilled message\n"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	// Count new messages after refill.
	refilledCount := 0
	refillTimer := time.NewTimer(200 * time.Millisecond)
refillLoop:
	for {
		select {
		case <-sub.C():
			refilledCount++
		case <-refillTimer.C:
			break refillLoop
		}
	}

	// We should have received approximately 5 messages after refill.
	if refilledCount < 3 || refilledCount > 7 {
		t.Errorf("expected ~5 messages after refill, got %d", refilledCount)
	}

	t.Logf("After refill: received %d new messages", refilledCount)
}

func TestAdapterLogWriter_DroppedMessageReport(t *testing.T) {
	bus := eventbus.New()
	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Log, eventbus.WithSubscriptionBuffer(300))
	defer sub.Close()

	writer := newAdapterLogWriter(bus, "test-adapter", SlotSTT, eventbus.LogLevelInfo)
	writer.dropReportInterval = 100 * time.Millisecond // Short interval for test speed.

	// Send enough messages to trigger rate limiting.
	for i := 0; i < rateLimitBurstSize+100; i++ {
		_, err := writer.Write([]byte("message\n"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	// Wait for drop report interval + buffer.
	time.Sleep(200 * time.Millisecond)

	// Send one more message to trigger report check.
	_, err := writer.Write([]byte("trigger\n"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Look for rate limit warning in received messages.
	foundWarning := false
	timeout := time.After(500 * time.Millisecond)
	for !foundWarning {
		select {
		case env := <-sub.C():
			evt := env.Payload
			if evt.Level == eventbus.LogLevelWarn && strings.Contains(evt.Message, "Rate limit exceeded") {
				foundWarning = true
				if !strings.Contains(evt.Message, "messages dropped") {
					t.Errorf("expected drop count in warning message, got: %q", evt.Message)
				}
				if evt.Fields["rate_limited"] != "true" {
					t.Errorf("expected rate_limited=true field")
				}
				t.Logf("Found rate limit warning: %s", evt.Message)
			}
		case <-timeout:
			if !foundWarning {
				t.Error("expected to find rate limit warning, but none received")
			}
			return
		}
	}
}

func TestAdapterLogWriter_Truncation(t *testing.T) {
	bus := eventbus.New()
	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Log)
	defer sub.Close()

	writer := newAdapterLogWriter(bus, "test-adapter", SlotSTT, eventbus.LogLevelInfo)

	// Create a very long line (> maxLogLineLength).
	longMessage := strings.Repeat("x", maxLogLineLength+500) + "\n"
	_, err := writer.Write([]byte(longMessage))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	select {
	case env := <-sub.C():
		evt := env.Payload
		if !strings.HasSuffix(evt.Message, "…[truncated]") {
			t.Errorf("expected truncated suffix, got: %q", evt.Message[len(evt.Message)-20:])
		}
		if evt.Fields["truncated"] != "true" {
			t.Errorf("expected truncated=true field")
		}
		messageLen := len([]rune(evt.Message))
		if messageLen > maxLogLineLength+20 {
			t.Errorf("expected message length <= %d, got %d", maxLogLineLength+20, messageLen)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for truncated log event")
	}
}

// TestLogAggregation_AC1_AdapterIDAndTimestamp explicitly proves AC #1:
// logs are aggregated on adapters.log with adapter ID and timestamp.
func TestLogAggregation_AC1_AdapterIDAndTimestamp(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Log)
	defer sub.Close()

	const adapterID = "adapter.stt.whisper"
	writer := newAdapterLogWriter(bus, adapterID, SlotSTT, eventbus.LogLevelInfo)

	before := time.Now().UTC().Add(-time.Second)
	_, err := writer.Write([]byte("hello from adapter\n"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	select {
	case env := <-sub.C():
		evt := env.Payload
		if evt.AdapterID != adapterID {
			t.Errorf("expected AdapterID %q, got %q", adapterID, evt.AdapterID)
		}
		if evt.Timestamp.IsZero() {
			t.Error("expected non-zero Timestamp")
		}
		if evt.Timestamp.Before(before) || evt.Timestamp.After(after) {
			t.Errorf("Timestamp %v not in expected range [%v, %v]", evt.Timestamp, before, after)
		}
		if evt.Fields["slot"] != string(SlotSTT) {
			t.Errorf("expected slot %q, got %q", SlotSTT, evt.Fields["slot"])
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for log event")
	}
}

// TestLogAggregation_AC1_StdoutAndStderr proves both stdout (info level)
// and stderr (error level) are captured per adapter instance.
func TestLogAggregation_AC1_StdoutAndStderr(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Log, eventbus.WithSubscriptionBuffer(10))
	defer sub.Close()

	const adapterID = "adapter.ai.openai"
	stdout := newAdapterLogWriter(bus, adapterID, SlotAI, eventbus.LogLevelInfo)
	stderr := newAdapterLogWriter(bus, adapterID, SlotAI, eventbus.LogLevelError)

	stdout.Write([]byte("info message\n"))
	stderr.Write([]byte("error message\n"))

	var gotInfo, gotError bool
	timeout := time.After(time.Second)
	for !gotInfo || !gotError {
		select {
		case env := <-sub.C():
			evt := env.Payload
			if evt.AdapterID != adapterID {
				t.Errorf("unexpected AdapterID %q", evt.AdapterID)
			}
			switch evt.Level {
			case eventbus.LogLevelInfo:
				if evt.Message != "info message" {
					t.Errorf("unexpected info message: %q", evt.Message)
				}
				gotInfo = true
			case eventbus.LogLevelError:
				if evt.Message != "error message" {
					t.Errorf("unexpected error message: %q", evt.Message)
				}
				gotError = true
			}
		case <-timeout:
			t.Fatalf("timeout: gotInfo=%v gotError=%v", gotInfo, gotError)
		}
	}
}

// TestLogAggregation_AC1_RateLimitDropReport proves rate limiting (50 msg/s, burst 100):
// emit >100 messages rapidly, verify only ≤100 published + drop report emitted.
func TestLogAggregation_AC1_RateLimitDropReport(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Log, eventbus.WithSubscriptionBuffer(300))
	defer sub.Close()

	writer := newAdapterLogWriter(bus, "chatty", SlotTTS, eventbus.LogLevelInfo)
	writer.dropReportInterval = 100 * time.Millisecond // Short interval for test speed.

	// Send burst_size + 50 messages instantly.
	total := rateLimitBurstSize + 50
	for i := 0; i < total; i++ {
		writer.Write([]byte("msg\n"))
	}

	// Drain burst messages first (timeout-based to handle async delivery).
	burstCount := 0
	burstTimer := time.NewTimer(500 * time.Millisecond)
burstDrain:
	for {
		select {
		case <-sub.C():
			burstCount++
		case <-burstTimer.C:
			break burstDrain
		}
	}

	if burstCount >= total {
		t.Errorf("expected rate limiting: published %d of %d sent", burstCount, total)
	}
	if burstCount > rateLimitBurstSize+5 {
		t.Errorf("expected ≤%d published (burst size + margin), got %d", rateLimitBurstSize+5, burstCount)
	}

	// Wait for drop report interval to elapse, then trigger a publish.
	time.Sleep(200 * time.Millisecond)
	writer.Write([]byte("trigger\n"))

	// Look for the drop report among received messages.
	var foundDropReport bool
	reportTimer := time.After(time.Second)
	for !foundDropReport {
		select {
		case env := <-sub.C():
			if env.Payload.Level == eventbus.LogLevelWarn &&
				strings.Contains(env.Payload.Message, "Rate limit exceeded") &&
				strings.Contains(env.Payload.Message, "messages dropped") {
				foundDropReport = true
			}
		case <-reportTimer:
			t.Error("expected drop report warning but none received")
			return
		}
	}
	t.Logf("Sent %d, burst published %d, drop report=%v", total, burstCount, foundDropReport)
}

func TestAdapterLogWriter_BufferFlush(t *testing.T) {
	bus := eventbus.New()
	sub := eventbus.SubscribeTo(bus, eventbus.Adapters.Log)
	defer sub.Close()

	writer := newAdapterLogWriter(bus, "test-adapter", SlotSTT, eventbus.LogLevelInfo)

	// Write partial line (no newline).
	_, err := writer.Write([]byte("partial"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Should not be published yet.
	select {
	case <-sub.C():
		t.Error("expected no message before Close, but got one")
	case <-time.After(50 * time.Millisecond):
		// Expected - no message yet.
	}

	// Close should flush the buffer.
	writer.Close()

	// Now should receive the partial line.
	select {
	case env := <-sub.C():
		evt := env.Payload
		if evt.Message != "partial" {
			t.Errorf("expected 'partial', got %q", evt.Message)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for flushed log event")
	}
}
