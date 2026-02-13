package adapters

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

const maxLogLineLength = 2048

// Rate limiting configuration to prevent chatty adapters from flooding the event bus.
const (
	// Maximum messages per second allowed (refill rate).
	rateLimitMessagesPerSecond = 50
	// Maximum burst size (bucket capacity).
	rateLimitBurstSize = 100
	// Interval for reporting dropped messages.
	droppedMessagesReportInterval = 5 * time.Second
)

// adapterLogWriter publishes adapter stdout/stderr lines on the event bus.
type adapterLogWriter struct {
	bus       *eventbus.Bus
	adapterID string
	slot      Slot
	level     eventbus.LogLevel

	mu  sync.Mutex
	buf bytes.Buffer

	// Rate limiting state (token bucket).
	tokens         float64
	lastRefill     time.Time
	droppedCount   int
	lastDropReport time.Time
}

func newAdapterLogWriter(bus *eventbus.Bus, adapterID string, slot Slot, level eventbus.LogLevel) *adapterLogWriter {
	now := time.Now()
	return &adapterLogWriter{
		bus:            bus,
		adapterID:      adapterID,
		slot:           slot,
		level:          level,
		tokens:         rateLimitBurstSize, // Start with full bucket.
		lastRefill:     now,
		lastDropReport: now,
	}
}

func (w *adapterLogWriter) Write(p []byte) (int, error) {
	if w.bus == nil {
		return len(p), nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.buf.Write(p); err != nil {
		return 0, err
	}

	for {
		data := w.buf.Bytes()
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		line := string(bytes.TrimRight(data[:idx], "\r"))
		w.publish(line)
		w.buf.Next(idx + 1)
	}

	// Flush if buffer grows excessively without newline to avoid unbounded usage.
	if w.buf.Len() > 16*1024 {
		line := strings.TrimSpace(w.buf.String())
		if line != "" {
			w.publish(line)
		}
		w.buf.Reset()
	}

	return len(p), nil
}

func (w *adapterLogWriter) Close() {
	if w.bus == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.publish(strings.TrimSpace(w.buf.String()))
		w.buf.Reset()
	}
}

func (w *adapterLogWriter) publish(message string) {
	if strings.TrimSpace(message) == "" {
		return
	}

	now := time.Now()

	// Refill tokens based on elapsed time.
	w.refillTokens(now)

	// Check if we should publish or drop due to rate limiting.
	if !w.shouldPublish(now) {
		w.droppedCount++
		return
	}

	// Publish the log message.
	truncatedMessage, truncated := limitLogLine(message)
	fields := map[string]string{
		"slot": string(w.slot),
	}
	if truncated {
		fields["truncated"] = "true"
	}
	event := eventbus.AdapterLogEvent{
		AdapterID: w.adapterID,
		Level:     w.level,
		Message:   truncatedMessage,
		Timestamp: now.UTC(),
		Fields:    fields,
	}
	eventbus.PublishWithOpts(context.Background(), w.bus, eventbus.Adapters.Log, eventbus.SourceAdapterProcess, event, eventbus.WithTimestamp(now))

	// Periodically report dropped messages.
	w.reportDroppedMessages(now)
}

// refillTokens adds tokens to the bucket based on elapsed time since last refill.
func (w *adapterLogWriter) refillTokens(now time.Time) {
	elapsed := now.Sub(w.lastRefill).Seconds()
	if elapsed <= 0 {
		return
	}
	tokensToAdd := elapsed * rateLimitMessagesPerSecond
	w.tokens += tokensToAdd
	if w.tokens > rateLimitBurstSize {
		w.tokens = rateLimitBurstSize
	}
	w.lastRefill = now
}

// shouldPublish checks if we have tokens available and consumes one if so.
func (w *adapterLogWriter) shouldPublish(now time.Time) bool {
	if w.tokens >= 1.0 {
		w.tokens -= 1.0
		return true
	}
	return false
}

// reportDroppedMessages publishes a summary of dropped messages periodically.
func (w *adapterLogWriter) reportDroppedMessages(now time.Time) {
	if w.droppedCount == 0 {
		return
	}
	if now.Sub(w.lastDropReport) < droppedMessagesReportInterval {
		return
	}

	// Publish drop report as a warning.
	message := fmt.Sprintf("Rate limit exceeded: %d messages dropped in last %v",
		w.droppedCount, now.Sub(w.lastDropReport).Round(time.Second))
	fields := map[string]string{
		"slot":         string(w.slot),
		"rate_limited": "true",
	}
	event := eventbus.AdapterLogEvent{
		AdapterID: w.adapterID,
		Level:     eventbus.LogLevelWarn,
		Message:   message,
		Timestamp: now.UTC(),
		Fields:    fields,
	}
	eventbus.PublishWithOpts(context.Background(), w.bus, eventbus.Adapters.Log, eventbus.SourceAdapterProcess, event, eventbus.WithTimestamp(now))

	// Reset counters.
	w.droppedCount = 0
	w.lastDropReport = now
}

func limitLogLine(line string) (string, bool) {
	runes := []rune(line)
	if len(runes) <= maxLogLineLength {
		return line, false
	}
	truncated := strings.TrimSpace(string(runes[:maxLogLineLength]))
	if truncated == "" {
		truncated = string(runes[:maxLogLineLength])
	}
	return truncated + " …[truncated]", true
}
