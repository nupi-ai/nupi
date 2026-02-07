package pty_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/pty"
)

// asciicastHeader mirrors the header for test parsing.
type asciicastHeader struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Title     string            `json:"title,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

func TestAsciicastRecorderWriteFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.cast")

	recorder, err := pty.NewAsciicastRecorder(path, 24, 80, "test-session")
	if err != nil {
		t.Fatalf("NewAsciicastRecorder: %v", err)
	}

	// Write 3 output events.
	for _, s := range []string{"hello", " ", "world"} {
		if err := recorder.Write([]byte(s)); err != nil {
			t.Fatalf("Write(%q): %v", s, err)
		}
	}

	// Write 1 resize event.
	if err := recorder.WriteResize(40, 120); err != nil {
		t.Fatalf("WriteResize: %v", err)
	}

	// Write 1 marker event.
	if err := recorder.WriteMarker("checkpoint"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Parse resulting file line-by-line.
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open cast file: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)

	// Line 1: header.
	if !scanner.Scan() {
		t.Fatal("expected header line")
	}
	var hdr asciicastHeader
	if err := json.Unmarshal(scanner.Bytes(), &hdr); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if hdr.Version != 2 {
		t.Fatalf("expected version 2, got %d", hdr.Version)
	}
	if hdr.Width != 80 || hdr.Height != 24 {
		t.Fatalf("expected 80x24, got %dx%d", hdr.Width, hdr.Height)
	}
	if hdr.Title != "test-session" {
		t.Fatalf("expected title 'test-session', got %q", hdr.Title)
	}

	// Parse events: 3 output + 1 resize + 1 marker = 5 events.
	type castEvent struct {
		Time float64
		Type string
		Data string
	}
	var events []castEvent
	for scanner.Scan() {
		var raw []json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		if len(raw) != 3 {
			t.Fatalf("expected 3-element array, got %d", len(raw))
		}
		var ev castEvent
		if err := json.Unmarshal(raw[0], &ev.Time); err != nil {
			t.Fatalf("unmarshal event time: %v", err)
		}
		if err := json.Unmarshal(raw[1], &ev.Type); err != nil {
			t.Fatalf("unmarshal event type: %v", err)
		}
		if err := json.Unmarshal(raw[2], &ev.Data); err != nil {
			t.Fatalf("unmarshal event data: %v", err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(events))
	}

	// Verify event types.
	expectedTypes := []string{"o", "o", "o", "r", "m"}
	for i, ev := range events {
		if ev.Type != expectedTypes[i] {
			t.Fatalf("event[%d] type: expected %q, got %q", i, expectedTypes[i], ev.Type)
		}
	}

	// Verify output data.
	expectedData := []string{"hello", " ", "world", "120x40", "checkpoint"}
	for i, ev := range events {
		if ev.Data != expectedData[i] {
			t.Fatalf("event[%d] data: expected %q, got %q", i, expectedData[i], ev.Data)
		}
	}

	// Verify monotonically increasing timestamps.
	for i := 1; i < len(events); i++ {
		if events[i].Time < events[i-1].Time {
			t.Fatalf("event[%d] timestamp %f < event[%d] timestamp %f (not monotonic)",
				i, events[i].Time, i-1, events[i-1].Time)
		}
	}
}

func TestAsciicastRecorderConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.cast")

	recorder, err := pty.NewAsciicastRecorder(path, 24, 80, "concurrent")
	if err != nil {
		t.Fatalf("NewAsciicastRecorder: %v", err)
	}

	const goroutines = 10
	const writesPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < writesPerGoroutine; i++ {
				if err := recorder.Write([]byte("x")); err != nil {
					t.Errorf("goroutine %d write %d: %v", id, i, err)
				}
			}
		}(g)
	}
	wg.Wait()

	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify all events are present and parseable (not just line count).
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)

	// Line 1: header must be valid JSON object.
	if !scanner.Scan() {
		t.Fatal("expected header line")
	}
	var hdr map[string]interface{}
	if err := json.Unmarshal(scanner.Bytes(), &hdr); err != nil {
		t.Fatalf("header is not valid JSON: %v", err)
	}

	// Remaining lines: each must be a valid JSON array [time, type, data].
	eventCount := 0
	for scanner.Scan() {
		var arr []json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &arr); err != nil {
			t.Fatalf("event line %d is not valid JSON: %v (line: %q)", eventCount+1, err, scanner.Text())
		}
		if len(arr) != 3 {
			t.Fatalf("event line %d: expected 3-element array, got %d", eventCount+1, len(arr))
		}
		eventCount++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	expectedEvents := goroutines * writesPerGoroutine
	if eventCount != expectedEvents {
		t.Fatalf("expected %d events, got %d", expectedEvents, eventCount)
	}
}

func TestAsciicastRecorderCloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "close.cast")

	recorder, err := pty.NewAsciicastRecorder(path, 24, 80, "close-test")
	if err != nil {
		t.Fatalf("NewAsciicastRecorder: %v", err)
	}

	// First close should succeed.
	if err := recorder.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// Second close should return nil (idempotent).
	if err := recorder.Close(); err != nil {
		t.Fatalf("second Close should be nil, got: %v", err)
	}

	// Write after close should return error.
	if err := recorder.Write([]byte("data")); err == nil {
		t.Fatal("Write after Close should return error")
	}
}

func TestAsciicastRecorderGetDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "duration.cast")

	recorder, err := pty.NewAsciicastRecorder(path, 24, 80, "duration")
	if err != nil {
		t.Fatalf("NewAsciicastRecorder: %v", err)
	}
	defer recorder.Close()

	time.Sleep(50 * time.Millisecond)
	dur := recorder.GetDuration()
	if dur < 50*time.Millisecond {
		t.Fatalf("expected duration >= 50ms, got %v", dur)
	}
}
