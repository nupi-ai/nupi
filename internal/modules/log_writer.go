package modules

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

const maxLogLineLength = 2048

// moduleLogWriter publishes module stdout/stderr lines on the event bus.
type moduleLogWriter struct {
	bus      *eventbus.Bus
	moduleID string
	slot     Slot
	level    eventbus.LogLevel

	mu  sync.Mutex
	buf bytes.Buffer
}

func newModuleLogWriter(bus *eventbus.Bus, moduleID string, slot Slot, level eventbus.LogLevel) *moduleLogWriter {
	return &moduleLogWriter{
		bus:      bus,
		moduleID: moduleID,
		slot:     slot,
		level:    level,
	}
}

func (w *moduleLogWriter) Write(p []byte) (int, error) {
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

func (w *moduleLogWriter) Close() {
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

func (w *moduleLogWriter) publish(message string) {
	if strings.TrimSpace(message) == "" {
		return
	}
	truncatedMessage, truncated := limitLogLine(message)
	fields := map[string]string{
		"slot": string(w.slot),
	}
	if truncated {
		fields["truncated"] = "true"
	}
	event := eventbus.ModuleLogEvent{
		ModuleID:  w.moduleID,
		Level:     w.level,
		Message:   truncatedMessage,
		Timestamp: time.Now().UTC(),
		Fields:    fields,
	}
	w.bus.Publish(context.Background(), eventbus.Envelope{
		Topic:   eventbus.TopicModulesLog,
		Source:  eventbus.SourceAdapterRunner,
		Payload: event,
	})
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
