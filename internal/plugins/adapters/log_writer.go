package adapters

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

const (
	rateLimitPerSecond = 50
	rateLimitBurstSize = 100
	maxLogLineLength   = 2000
)

const (
	defaultDropReportInterval = 2 * time.Second
	truncatedSuffix           = "…[truncated]"
)

type adapterLogWriter struct {
	mu sync.Mutex

	bus       *eventbus.Bus
	adapterID string
	slot      Slot
	level     eventbus.LogLevel

	buffer string

	tokens     int
	lastRefill time.Time

	dropped            int
	lastDropReport     time.Time
	dropReportInterval time.Duration
}

func newAdapterLogWriter(bus *eventbus.Bus, adapterID string, slot Slot, level eventbus.LogLevel) *adapterLogWriter {
	now := time.Now()
	return &adapterLogWriter{
		bus:                bus,
		adapterID:          adapterID,
		slot:               slot,
		level:              level,
		tokens:             rateLimitBurstSize,
		lastRefill:         now,
		lastDropReport:     now,
		dropReportInterval: defaultDropReportInterval,
	}
}

func (w *adapterLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buffer += string(p)
	lines := strings.Split(w.buffer, "\n")
	w.buffer = lines[len(lines)-1]

	for _, line := range lines[:len(lines)-1] {
		w.publishLineLocked(line)
	}

	return len(p), nil
}

func (w *adapterLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.buffer == "" {
		return nil
	}

	w.publishLineLocked(w.buffer)
	w.buffer = ""
	return nil
}

func (w *adapterLogWriter) publishLineLocked(line string) {
	w.maybeReportDropsLocked()

	line = strings.TrimSuffix(line, "\r")
	if line == "" {
		return
	}

	fields := map[string]string{
		"slot": string(w.slot),
	}

	message, truncated := truncateLogLine(line, maxLogLineLength)
	if truncated {
		fields["truncated"] = "true"
	}

	if !w.allowPublishLocked() {
		w.dropped++
		return
	}

	eventbus.Publish(context.Background(), w.bus, eventbus.Adapters.Log, eventbus.SourceAdapterProcess, eventbus.AdapterLogEvent{
		AdapterID: w.adapterID,
		Level:     w.level,
		Message:   message,
		Fields:    fields,
		Timestamp: time.Now().UTC(),
	})
}

func (w *adapterLogWriter) maybeReportDropsLocked() {
	if w.dropped == 0 {
		return
	}
	if time.Since(w.lastDropReport) < w.dropReportInterval {
		return
	}

	message := "Rate limit exceeded: " + itoa(w.dropped) + " messages dropped"
	eventbus.Publish(context.Background(), w.bus, eventbus.Adapters.Log, eventbus.SourceAdapterProcess, eventbus.AdapterLogEvent{
		AdapterID: w.adapterID,
		Level:     eventbus.LogLevelWarn,
		Message:   message,
		Fields: map[string]string{
			"slot":         string(w.slot),
			"rate_limited": "true",
		},
		Timestamp: time.Now().UTC(),
	})

	w.dropped = 0
	w.lastDropReport = time.Now()
}

func (w *adapterLogWriter) allowPublishLocked() bool {
	now := time.Now()
	if w.lastRefill.IsZero() {
		w.lastRefill = now
	}

	interval := time.Second / rateLimitPerSecond
	if interval > 0 {
		elapsed := now.Sub(w.lastRefill)
		if elapsed >= interval {
			add := int(elapsed / interval)
			if add > 0 {
				w.tokens += add
				if w.tokens > rateLimitBurstSize {
					w.tokens = rateLimitBurstSize
				}
				w.lastRefill = w.lastRefill.Add(time.Duration(add) * interval)
			}
		}
	}

	if w.tokens <= 0 {
		return false
	}

	w.tokens--
	return true
}

func truncateLogLine(line string, max int) (string, bool) {
	if max <= 0 {
		return line, false
	}
	runes := []rune(line)
	if len(runes) <= max {
		return line, false
	}

	limit := max - len([]rune(truncatedSuffix))
	if limit < 0 {
		limit = 0
	}
	return string(runes[:limit]) + truncatedSuffix, true
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
