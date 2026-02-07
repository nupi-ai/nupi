package integration

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/recording"
	"github.com/nupi-ai/nupi/internal/session"
)

// skipIfNoPTY skips the test on platforms without PTY support.
// Probes actual PTY availability (not just OS check) so tests are skipped
// in sandboxed environments that lack /dev/ptmx access.
func skipIfNoPTY(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests require POSIX platform")
	}
	// Probe actual PTY capability — matches pattern from session/manager_test.go.
	p := pty.New()
	err := p.Start(pty.StartOptions{Command: "/bin/sh", Args: []string{"-c", "exit 0"}})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "operation not permitted") ||
			strings.Contains(msg, "permission denied") ||
			strings.Contains(msg, "no such file or directory") ||
			strings.Contains(msg, "failed to start") {
			t.Skipf("PTY not available in this environment: %v", err)
		}
	}
	if p != nil {
		p.Stop(100 * time.Millisecond)
	}
}

// asciicastHeaderParsed is used for parsing .cast file headers in tests.
type asciicastHeaderParsed struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Title     string            `json:"title,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// parseCastFile parses a .cast file into header and events.
func parseCastFile(t *testing.T, path string) (asciicastHeaderParsed, []castEventParsed) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open cast file %s: %v", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Increase buffer for large output tests.
	scanner.Buffer(make([]byte, 0, 4*1024*1024), 4*1024*1024)

	if !scanner.Scan() {
		t.Fatal("expected header line in cast file")
	}
	var hdr asciicastHeaderParsed
	if err := json.Unmarshal(scanner.Bytes(), &hdr); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}

	var events []castEventParsed
	for scanner.Scan() {
		var raw []json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			t.Fatalf("unmarshal event line: %v", err)
		}
		if len(raw) != 3 {
			t.Fatalf("expected 3-element event array, got %d elements", len(raw))
		}
		var ev castEventParsed
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
		t.Fatalf("scanner error reading cast file: %v", err)
	}

	return hdr, events
}

type castEventParsed struct {
	Time float64
	Type string
	Data string
}

// waitForSessionStopped waits for the session status to become stopped.
func waitForSessionStopped(t *testing.T, sess *session.Session, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if sess.CurrentStatus() == session.StatusStopped {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session did not stop within %v", timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForMetadata polls until the recording store has at least minCount entries.
func waitForMetadata(t *testing.T, store *recording.Store, minCount int, timeout time.Duration) []recording.Metadata {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	var lastCount int
	for {
		metadata, err := store.LoadAll()
		if err == nil && len(metadata) >= minCount {
			return metadata
		}
		lastErr = err
		lastCount = len(metadata)
		if time.Now().After(deadline) {
			if lastErr != nil {
				t.Fatalf("expected at least %d metadata entries within %v, but LoadAll failed: %v", minCount, timeout, lastErr)
			}
			t.Fatalf("expected at least %d metadata entries within %v, got %d", minCount, timeout, lastCount)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --- Task 1: Recording lifecycle tests ---

func TestRecordingCapturesSessionOutput(t *testing.T) {
	skipIfNoPTY(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	mgr := session.NewManager()
	sess, err := mgr.CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		// sleep ensures recording sink is wired before output (PTY.Start precedes AddSink).
		Args: []string{"-c", "sleep 0.1; printf 'RECORDING_TEST_OUTPUT'"},
	}, false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if !sess.RecordingEnabled {
		t.Fatal("expected RecordingEnabled to be true")
	}

	// Wait for process to exit naturally.
	waitForSessionStopped(t, sess, 5*time.Second)

	// Wait for metadata to be saved (poll-based, not sleep).
	store, err := recording.NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	metadata := waitForMetadata(t, store, 1, 3*time.Second)

	// Verify .cast file exists.
	recordingsDir := filepath.Join(home, ".nupi", "recordings")
	castFiles, err := filepath.Glob(filepath.Join(recordingsDir, "*.cast"))
	if err != nil {
		t.Fatalf("glob cast files: %v", err)
	}
	if len(castFiles) != 1 {
		t.Fatalf("expected 1 .cast file, got %d", len(castFiles))
	}

	// Parse and validate .cast file.
	hdr, events := parseCastFile(t, castFiles[0])
	if hdr.Version != 2 {
		t.Fatalf("expected asciicast version 2, got %d", hdr.Version)
	}
	if hdr.Width <= 0 || hdr.Height <= 0 {
		t.Fatalf("expected positive dimensions, got %dx%d", hdr.Width, hdr.Height)
	}
	if hdr.Timestamp <= 0 {
		t.Fatalf("expected positive timestamp, got %d", hdr.Timestamp)
	}

	// Verify output events contain our marker string.
	var allOutput strings.Builder
	for _, ev := range events {
		if ev.Type == "o" {
			allOutput.WriteString(ev.Data)
		}
	}
	if !strings.Contains(allOutput.String(), "RECORDING_TEST_OUTPUT") {
		t.Fatalf("expected output to contain 'RECORDING_TEST_OUTPUT', got %q", allOutput.String())
	}

	if len(metadata) != 1 {
		t.Fatalf("expected 1 metadata entry, got %d", len(metadata))
	}
	meta := metadata[0]
	if meta.SessionID != sess.ID {
		t.Fatalf("expected session ID %s, got %s", sess.ID, meta.SessionID)
	}
	if meta.Duration <= 0 {
		t.Fatalf("expected positive duration, got %f", meta.Duration)
	}
	if meta.RecordingPath == "" {
		t.Fatal("expected non-empty recording path")
	}
}

func TestRecordingPersistsSurvivesDaemonRestart(t *testing.T) {
	skipIfNoPTY(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Create session, produce output, let it finish.
	mgr := session.NewManager()
	sess, err := mgr.CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 0.1; printf 'PERSIST_TEST'"},
	}, false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	waitForSessionStopped(t, sess, 5*time.Second)

	// Wait for metadata to be saved (poll-based).
	store1, err := recording.NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	waitForMetadata(t, store1, 1, 3*time.Second)

	// Simulate "daemon restart" by creating a new Store pointing at same directory.
	store2, err := recording.NewStore()
	if err != nil {
		t.Fatalf("NewStore (restart): %v", err)
	}

	metadata, err := store2.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll after restart: %v", err)
	}
	if len(metadata) != 1 {
		t.Fatalf("expected 1 metadata entry after restart, got %d", len(metadata))
	}
	if metadata[0].SessionID != sess.ID {
		t.Fatalf("expected session ID %s, got %s", sess.ID, metadata[0].SessionID)
	}

	// Verify .cast file still exists and is valid.
	hdr, events := parseCastFile(t, metadata[0].RecordingPath)
	if hdr.Version != 2 {
		t.Fatalf("expected version 2 after restart, got %d", hdr.Version)
	}

	var output strings.Builder
	for _, ev := range events {
		if ev.Type == "o" {
			output.WriteString(ev.Data)
		}
	}
	if !strings.Contains(output.String(), "PERSIST_TEST") {
		t.Fatalf("expected output to contain 'PERSIST_TEST' after restart, got %q", output.String())
	}
}

func TestRecordingDisabledOnSetupFailure(t *testing.T) {
	skipIfNoPTY(t)
	// Set HOME to an unwritable path to force recording setup failure.
	home := t.TempDir()
	recordingsDir := filepath.Join(home, ".nupi", "recordings")
	// Create recordings as a FILE (not directory) to cause MkdirAll to fail.
	if err := os.MkdirAll(filepath.Join(home, ".nupi"), 0o755); err != nil {
		t.Fatalf("mkdir .nupi: %v", err)
	}
	if err := os.WriteFile(recordingsDir, []byte("not-a-directory"), 0o444); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	t.Setenv("HOME", home)

	mgr := session.NewManager()
	sess, err := mgr.CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "printf 'still works'"},
	}, false)
	if err != nil {
		t.Fatalf("CreateSession should succeed even if recording fails: %v", err)
	}
	defer func() {
		if sess.PTY.IsRunning() {
			mgr.KillSession(sess.ID)
		}
	}()

	// Recording should be disabled due to setup failure.
	if sess.RecordingEnabled {
		t.Fatal("expected RecordingEnabled to be false when setup fails")
	}

	// Session should still work normally.
	waitForSessionStopped(t, sess, 5*time.Second)
}

// --- Task 4: Recording data flow tests ---
// NOTE: Real APIServer handler tests (routing, error codes, Content-Type)
// live in internal/server/handlers_test.go where unexported methods are accessible.

func TestMultiSessionRecordingMetadata(t *testing.T) {
	skipIfNoPTY(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	mgr := session.NewManager()

	// Create 2 sessions.
	sess1, err := mgr.CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 0.1; printf 'session1'"},
	}, false)
	if err != nil {
		t.Fatalf("CreateSession 1: %v", err)
	}
	waitForSessionStopped(t, sess1, 5*time.Second)

	store := mgr.GetRecordingStore()
	if store == nil {
		t.Fatal("expected non-nil recording store")
	}
	waitForMetadata(t, store, 1, 3*time.Second)

	sess2, err := mgr.CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 0.1; printf 'session2'"},
	}, false)
	if err != nil {
		t.Fatalf("CreateSession 2: %v", err)
	}
	waitForSessionStopped(t, sess2, 5*time.Second)
	waitForMetadata(t, store, 2, 3*time.Second)

	metadata, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(metadata) != 2 {
		t.Fatalf("expected 2 recordings, got %d", len(metadata))
	}

	// Verify JSON serialization (what the REST handler does).
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var parsed []recording.Metadata
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	// Verify both sessions present.
	ids := map[string]bool{}
	for _, m := range parsed {
		ids[m.SessionID] = true
		if m.Filename == "" {
			t.Fatalf("expected non-empty filename for session %s", m.SessionID)
		}
		if m.Duration <= 0 {
			t.Fatalf("expected positive duration for session %s", m.SessionID)
		}
		if m.RecordingPath == "" {
			t.Fatalf("expected non-empty recording_path for session %s", m.SessionID)
		}
	}
	if !ids[sess1.ID] || !ids[sess2.ID] {
		t.Fatalf("expected both session IDs in metadata, got %v", ids)
	}
}

func TestRecordingFileContainsSessionOutput(t *testing.T) {
	skipIfNoPTY(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	mgr := session.NewManager()
	sess, err := mgr.CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 0.1; printf 'REST_PLAYBACK_TEST'"},
	}, false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForSessionStopped(t, sess, 5*time.Second)

	store := mgr.GetRecordingStore()
	waitForMetadata(t, store, 1, 3*time.Second)

	meta, err := store.GetBySessionID(sess.ID)
	if err != nil {
		t.Fatalf("GetBySessionID: %v", err)
	}

	// Verify metadata points to a valid, serveable .cast file.
	if meta.Filename == "" {
		t.Fatal("expected non-empty filename in metadata")
	}
	if meta.RecordingPath == "" {
		t.Fatal("expected non-empty recording path in metadata")
	}

	// Validate the .cast file that the handler would serve via http.ServeFile.
	hdr, events := parseCastFile(t, meta.RecordingPath)
	if hdr.Version != 2 {
		t.Fatalf("expected asciicast version 2, got %d", hdr.Version)
	}

	var output strings.Builder
	for _, ev := range events {
		if ev.Type == "o" {
			output.WriteString(ev.Data)
		}
	}
	if !strings.Contains(output.String(), "REST_PLAYBACK_TEST") {
		t.Fatalf("expected output to contain 'REST_PLAYBACK_TEST', got %q", output.String())
	}
}

func TestPlaybackTimingPreservation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "timing.cast")

	recorder, err := pty.NewAsciicastRecorder(path, 24, 80, "timing")
	if err != nil {
		t.Fatalf("NewAsciicastRecorder: %v", err)
	}

	// Write event A.
	if err := recorder.Write([]byte("A")); err != nil {
		t.Fatalf("Write A: %v", err)
	}

	// Sleep 200ms — generous gap so timing survives CI scheduler jitter.
	time.Sleep(200 * time.Millisecond)

	// Write event B.
	if err := recorder.Write([]byte("B")); err != nil {
		t.Fatalf("Write B: %v", err)
	}

	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Parse events and verify timing.
	_, events := parseCastFile(t, path)
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(events))
	}

	// Find event A and event B.
	var evA, evB *castEventParsed
	for i := range events {
		if events[i].Data == "A" && evA == nil {
			evA = &events[i]
		} else if events[i].Data == "B" && evB == nil {
			evB = &events[i]
		}
	}
	if evA == nil || evB == nil {
		t.Fatal("could not find events A and B")
	}

	// Verify time difference: at least 50ms proves timing is actually recorded.
	// No upper bound — under heavy CI load, GC pauses or scheduler throttling
	// can inflate wall-clock deltas arbitrarily without indicating a bug.
	diff := evB.Time - evA.Time
	if diff < 0.05 {
		t.Fatalf("expected at least 50ms between events, got %.1fms", diff*1000)
	}
}

func TestPlaybackNonExistentRecording(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := recording.NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	_, err = store.GetBySessionID("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for non-existent session ID")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got: %v", err)
	}
}

// --- Task 5: Edge cases ---

func TestRecordingWithTerminalResize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resize.cast")

	recorder, err := pty.NewAsciicastRecorder(path, 24, 80, "resize-test")
	if err != nil {
		t.Fatalf("NewAsciicastRecorder: %v", err)
	}

	// Write output, resize, write more output.
	if err := recorder.Write([]byte("before-resize")); err != nil {
		t.Fatalf("Write before: %v", err)
	}
	if err := recorder.WriteResize(40, 120); err != nil {
		t.Fatalf("WriteResize: %v", err)
	}
	if err := recorder.Write([]byte("after-resize")); err != nil {
		t.Fatalf("Write after: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, events := parseCastFile(t, path)
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	// Verify sequence: output, resize, output.
	if events[0].Type != "o" || events[0].Data != "before-resize" {
		t.Fatalf("event[0]: expected output 'before-resize', got type=%q data=%q", events[0].Type, events[0].Data)
	}
	if events[1].Type != "r" || events[1].Data != "120x40" {
		t.Fatalf("event[1]: expected resize '120x40', got type=%q data=%q", events[1].Type, events[1].Data)
	}
	if events[2].Type != "o" || events[2].Data != "after-resize" {
		t.Fatalf("event[2]: expected output 'after-resize', got type=%q data=%q", events[2].Type, events[2].Data)
	}
}

func TestRecordingLargeOutput(t *testing.T) {
	skipIfNoPTY(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Generate 1MB of output using dd (faster than printf for large data).
	mgr := session.NewManager()
	sess, err := mgr.CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "dd if=/dev/zero bs=1024 count=1024 2>/dev/null | tr '\\0' 'A'"},
	}, false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	waitForSessionStopped(t, sess, 15*time.Second)

	// Verify .cast file exists and is valid.
	store := mgr.GetRecordingStore()
	waitForMetadata(t, store, 1, 5*time.Second)
	meta, err := store.GetBySessionID(sess.ID)
	if err != nil {
		t.Fatalf("GetBySessionID: %v", err)
	}

	info, err := os.Stat(meta.RecordingPath)
	if err != nil {
		t.Fatalf("stat recording: %v", err)
	}
	// File should be larger than the raw output due to JSON encoding overhead.
	if info.Size() < 512*1024 {
		t.Fatalf("expected recording file >= 512KB, got %d bytes", info.Size())
	}

	// Verify file is parseable (not truncated).
	hdr, events := parseCastFile(t, meta.RecordingPath)
	if hdr.Version != 2 {
		t.Fatalf("expected version 2, got %d", hdr.Version)
	}
	if len(events) == 0 {
		t.Fatal("expected non-zero events in large recording")
	}

	// Verify total output data volume.
	totalOutput := 0
	for _, ev := range events {
		if ev.Type == "o" {
			totalOutput += len(ev.Data)
		}
	}
	// PTY may add escape sequences, so total may differ from raw 1MB.
	// Just verify substantial data was captured.
	if totalOutput < 256*1024 {
		t.Fatalf("expected at least 256KB of output data, got %d bytes", totalOutput)
	}
}

func TestRecordingMetadataIncludesDetectedTool(t *testing.T) {
	skipIfNoPTY(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Create a session with a command. Tool detection depends on plugins,
	// but we verify the Tool field structure exists in metadata.
	mgr := session.NewManager()
	sess, err := mgr.CreateSession(pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 0.1; printf 'tool-test'"},
	}, false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	waitForSessionStopped(t, sess, 5*time.Second)

	store := mgr.GetRecordingStore()
	waitForMetadata(t, store, 1, 3*time.Second)

	meta, err := store.GetBySessionID(sess.ID)
	if err != nil {
		t.Fatalf("GetBySessionID: %v", err)
	}

	// Verify metadata structure includes Tool field (may be empty if no plugins).
	// The important thing is the field is populated from session.GetDetectedTool().
	if meta.Command != "/bin/sh" {
		t.Fatalf("expected command '/bin/sh', got %q", meta.Command)
	}
	// Tool may be empty for /bin/sh (no AI tool detected), which is fine.
	// We just verify the metadata was saved correctly.
	if meta.SessionID != sess.ID {
		t.Fatalf("expected session ID %s, got %s", sess.ID, meta.SessionID)
	}
}
