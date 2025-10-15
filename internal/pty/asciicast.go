package pty

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// AsciicastRecorder implements OutputSink for recording sessions in asciicast v2 format.
// Format spec: https://docs.asciinema.org/manual/asciicast/v2/
type AsciicastRecorder struct {
	file      *os.File
	startTime time.Time
	mu        sync.Mutex
	closed    bool
	rows      int
	cols      int
}

// AsciicastHeader represents the asciicast v2 header.
type AsciicastHeader struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Title     string            `json:"title,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// NewAsciicastRecorder creates a new asciicast recorder.
// filePath: path to .cast file
// rows, cols: terminal dimensions
// title: optional session title
func NewAsciicastRecorder(filePath string, rows, cols int, title string) (*AsciicastRecorder, error) {
	file, err := os.Create(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create recording file: %w", err)
	}

	recorder := &AsciicastRecorder{
		file:      file,
		startTime: time.Now(),
		rows:      rows,
		cols:      cols,
	}

	// Write header as first line.
	header := AsciicastHeader{
		Version:   2,
		Width:     cols,
		Height:    rows,
		Timestamp: recorder.startTime.Unix(),
		Title:     title,
		Env: map[string]string{
			"TERM":  "xterm-256color",
			"SHELL": os.Getenv("SHELL"),
		},
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to marshal header: %w", err)
	}

	if _, err := file.Write(headerJSON); err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to write header: %w", err)
	}

	if _, err := file.WriteString("\n"); err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to write newline: %w", err)
	}

	return recorder, nil
}

// Write implements OutputSink interface.
// Records output events in asciicast v2 format: [time, "o", data]
func (r *AsciicastRecorder) Write(data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return fmt.Errorf("recorder is closed")
	}

	// Calculate elapsed time since start.
	elapsed := time.Since(r.startTime).Seconds()

	// Create event: [time, "o", data]
	event := []interface{}{
		elapsed,
		"o",
		string(data),
	}

	eventJSON, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	if _, err := r.file.Write(eventJSON); err != nil {
		return fmt.Errorf("failed to write event: %w", err)
	}

	if _, err := r.file.WriteString("\n"); err != nil {
		return fmt.Errorf("failed to write newline: %w", err)
	}

	return r.file.Sync()
}

// WriteResize records a terminal resize event.
// Format: [time, "r", "{COLS}x{ROWS}"]
func (r *AsciicastRecorder) WriteResize(rows, cols int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return fmt.Errorf("recorder is closed")
	}

	// Update stored dimensions.
	r.rows = rows
	r.cols = cols

	elapsed := time.Since(r.startTime).Seconds()
	resizeData := fmt.Sprintf("%dx%d", cols, rows)

	event := []interface{}{
		elapsed,
		"r",
		resizeData,
	}

	eventJSON, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal resize event: %w", err)
	}

	if _, err := r.file.Write(eventJSON); err != nil {
		return fmt.Errorf("failed to write resize event: %w", err)
	}

	if _, err := r.file.WriteString("\n"); err != nil {
		return fmt.Errorf("failed to write newline: %w", err)
	}

	return r.file.Sync()
}

// WriteMarker records a marker event (for pause points).
// Format: [time, "m", label]
func (r *AsciicastRecorder) WriteMarker(label string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return fmt.Errorf("recorder is closed")
	}

	elapsed := time.Since(r.startTime).Seconds()

	event := []interface{}{
		elapsed,
		"m",
		label,
	}

	eventJSON, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal marker event: %w", err)
	}

	if _, err := r.file.Write(eventJSON); err != nil {
		return fmt.Errorf("failed to write marker event: %w", err)
	}

	if _, err := r.file.WriteString("\n"); err != nil {
		return fmt.Errorf("failed to write newline: %w", err)
	}

	return r.file.Sync()
}

// Close closes the recording file.
func (r *AsciicastRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil
	}

	r.closed = true
	return r.file.Close()
}

// GetDuration returns the duration of the recording.
func (r *AsciicastRecorder) GetDuration() time.Duration {
	return time.Since(r.startTime)
}
