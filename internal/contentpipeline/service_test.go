package contentpipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/jsrunner"
	"github.com/nupi-ai/nupi/internal/plugins"
	toolhandlers "github.com/nupi-ai/nupi/internal/plugins/tool_handlers"
)

// skipIfNoBun skips the test if the JS runtime is not available.
// It first checks if the runtime is available via the standard resolver.
// If not, it checks if bun is in PATH and sets NUPI_JS_RUNTIME to use it.
func skipIfNoBun(t *testing.T) {
	t.Helper()
	if jsrunner.IsAvailable() {
		return
	}
	bunPath, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("JS runtime not available: not bundled and bun not in PATH")
	}
	t.Setenv("NUPI_JS_RUNTIME", bunPath)
}

// waitForCondition polls until cond returns true or timeout expires.
// Uses short polling interval (5ms) to minimize flakiness on slow CI.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for condition after %v", timeout)
}

func setHostScriptEnv(t *testing.T) {
	t.Helper()
	// Get the path to host.js relative to this test file
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to get test file path")
	}
	// Navigate from service_test.go to jsruntime/host.js
	hostScript := filepath.Join(filepath.Dir(filepath.Dir(filename)), "jsruntime", "host.js")
	if _, err := os.Stat(hostScript); err != nil {
		t.Skipf("host.js not found at %s", hostScript)
	}
	t.Setenv("NUPI_JS_HOST_SCRIPT", hostScript)
}

func writeCleanerPlugin(t *testing.T, root, namespace, slug, script string) {
	t.Helper()
	dir := filepath.Join(root, "plugins", namespace, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	manifest := fmt.Sprintf(`apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: pipeline-cleaner
metadata:
  name: %s
  slug: %s
  namespace: %s
  version: 0.0.1
spec:
  main: main.js
`, slug, slug, namespace)
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.js"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
}

func newPluginService(t *testing.T, baseDir string) *plugins.Service {
	t.Helper()
	svc := plugins.NewService(baseDir)
	// Start the service to initialize jsruntime
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start plugin service: %v", err)
	}
	t.Cleanup(func() {
		svc.Shutdown(context.Background())
	})
	return svc
}

func TestContentPipelineTransformsOutput(t *testing.T) {
	skipIfNoBun(t)
	setHostScriptEnv(t)

	tmp := t.TempDir()
	const namespace = "test.namespace"

	writeCleanerPlugin(t, tmp, namespace, "pipeline-tool-x", `module.exports = {
        name: "default",
        commands: ["tool-x"],
        transform: function(input) {
            return { text: input.text.toUpperCase(), annotations: { cleaned: "yes" } };
        }
    };`)

	svc := newPluginService(t, tmp)

	bus := eventbus.New()
	cp := NewService(bus, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cp.Start(ctx); err != nil {
		t.Fatalf("start pipeline: %v", err)
	}
	defer cp.Shutdown(context.Background())

	cleanedSub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned)
	defer cleanedSub.Close()

	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Tool, eventbus.SourcePluginService, eventbus.SessionToolEvent{
		SessionID: "s1",
		ToolID:    "tool-x",
		ToolName:  "Tool X",
	})

	// Wait for tool to be registered (poll instead of sleep for determinism)
	waitForCondition(t, 500*time.Millisecond, func() bool {
		_, ok := cp.toolBySession.Load("s1")
		return ok
	})

	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Output, eventbus.SourceSessionManager, eventbus.SessionOutputEvent{
		SessionID: "s1",
		Sequence:  1,
		Data:      []byte("hello\n"),
	})

	select {
	case env := <-cleanedSub.C():
		msg := env.Payload
		if msg.Text != "HELLO\n" {
			t.Fatalf("expected transformed text, got %q", msg.Text)
		}
		if msg.Annotations[constants.MetadataKeyCleaned] != "yes" {
			t.Fatalf("expected cleaned annotation, got %v", msg.Annotations)
		}
		if msg.Annotations[constants.MetadataKeyToolID] != "tool-x" {
			t.Fatalf("expected tool_id annotation, got %v", msg.Annotations)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for cleaned event")
	}
}

func TestContentPipelineEmitsErrors(t *testing.T) {
	skipIfNoBun(t)
	setHostScriptEnv(t)

	tmp := t.TempDir()
	const namespace = "test.namespace"

	writeCleanerPlugin(t, tmp, namespace, "pipeline-tool-err", `module.exports = {
        name: "tool-err",
        commands: ["tool-err"],
        transform: function(input) {
            throw new Error("boom");
        }
    };`)

	svc := newPluginService(t, tmp)

	bus := eventbus.New()
	cp := NewService(bus, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cp.Start(ctx); err != nil {
		t.Fatalf("start pipeline: %v", err)
	}
	defer cp.Shutdown(context.Background())

	errSub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Error)
	defer errSub.Close()

	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Tool, eventbus.SourcePluginService, eventbus.SessionToolEvent{
		SessionID: "s2",
		ToolID:    "tool-err",
		ToolName:  "Tool Err",
	})

	// Wait for tool to be registered (poll instead of sleep for determinism)
	waitForCondition(t, 500*time.Millisecond, func() bool {
		_, ok := cp.toolBySession.Load("s2")
		return ok
	})

	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Output, eventbus.SourceSessionManager, eventbus.SessionOutputEvent{
		SessionID: "s2",
		Sequence:  1,
		Data:      []byte("boom"),
	})

	select {
	case env := <-errSub.C():
		msg := env.Payload
		if msg.SessionID != "s2" {
			t.Fatalf("expected session s2, got %s", msg.SessionID)
		}
		if msg.Stage != "tool-err" {
			t.Fatalf("expected stage tool-err, got %s", msg.Stage)
		}
		if msg.Message == "" {
			t.Fatalf("expected error message")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for pipeline error")
	}
}

func TestContentPipelineHandlesTranscripts(t *testing.T) {
	bus := eventbus.New()
	cp := NewService(bus, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cp.Start(ctx); err != nil {
		t.Fatalf("start pipeline: %v", err)
	}
	defer cp.Shutdown(context.Background())

	cleanedSub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned)
	defer cleanedSub.Close()

	eventbus.Publish(context.Background(), bus, eventbus.Speech.TranscriptFinal, eventbus.SourceAudioSTT, eventbus.SpeechTranscriptEvent{
		SessionID:  "voice-session",
		StreamID:   "mic",
		Sequence:   7,
		Text:       "turn lights on",
		Confidence: 0.87,
		Final:      true,
		Metadata: map[string]string{
			constants.MetadataKeyLocale: "en-US",
		},
	})

	select {
	case env := <-cleanedSub.C():
		msg := env.Payload
		if msg.SessionID != "voice-session" {
			t.Fatalf("unexpected session id %q", msg.SessionID)
		}
		if msg.Origin != eventbus.OriginUser {
			t.Fatalf("expected OriginUser, got %s", msg.Origin)
		}
		if msg.Text != "turn lights on" {
			t.Fatalf("unexpected text: %q", msg.Text)
		}
		if msg.Sequence != 7 {
			t.Fatalf("unexpected sequence: %d", msg.Sequence)
		}
		if msg.Annotations[constants.MetadataKeyInputSource] != "voice" {
			t.Fatalf("missing input_source annotation: %+v", msg.Annotations)
		}
		if msg.Annotations[constants.MetadataKeyStreamID] != "mic" {
			t.Fatalf("missing stream_id annotation: %+v", msg.Annotations)
		}
		if msg.Annotations[constants.MetadataKeyLocale] != "en-US" {
			t.Fatalf("metadata not propagated: %+v", msg.Annotations)
		}
		if _, ok := msg.Annotations[constants.MetadataKeyConfidence]; !ok {
			t.Fatalf("expected confidence annotation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for cleaned transcript event")
	}
}

func TestBufferOverflowAnnotation(t *testing.T) {
	bus := eventbus.New()
	cp := NewService(bus, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cp.Start(ctx); err != nil {
		t.Fatalf("start pipeline: %v", err)
	}
	defer cp.Shutdown(context.Background())

	// Override the buffer with a small max size
	smallBuf := NewOutputBufferWithOptions(WithMaxSize(100))
	cp.buffers.Store("overflow-session", smallBuf)

	cleanedSub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned)
	defer cleanedSub.Close()

	// Write more data than the buffer can hold
	largeData := make([]byte, 200)
	for i := range largeData {
		largeData[i] = 'x'
	}

	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Output, eventbus.SourceSessionManager, eventbus.SessionOutputEvent{
		SessionID: "overflow-session",
		Sequence:  1,
		Data:      largeData,
	})

	// Wait for idle timeout to trigger flush
	select {
	case env := <-cleanedSub.C():
		msg := env.Payload
		if msg.Annotations[constants.MetadataKeyBufferTruncated] != "true" {
			t.Errorf("expected buffer_truncated=true, got %v", msg.Annotations[constants.MetadataKeyBufferTruncated])
		}
		// buffer_max_size should reflect the actual buffer's configured maxSize
		if msg.Annotations[constants.MetadataKeyBufferMaxSize] != "100" {
			t.Errorf("expected buffer_max_size=100, got %v", msg.Annotations[constants.MetadataKeyBufferMaxSize])
		}
		// Content should be truncated to last 100 bytes
		if len(msg.Text) != 100 {
			t.Errorf("expected text length 100, got %d", len(msg.Text))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for cleaned event with overflow annotations")
	}
}

func TestToolChangeFlushesBuffer(t *testing.T) {
	bus := eventbus.New()
	cp := NewService(bus, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cp.Start(ctx); err != nil {
		t.Fatalf("start pipeline: %v", err)
	}
	defer cp.Shutdown(context.Background())

	cleanedSub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned)
	defer cleanedSub.Close()

	// Set initial tool
	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Tool, eventbus.SourcePluginService, eventbus.SessionToolEvent{
		SessionID: "tool-change-session",
		ToolID:    "tool-a",
		ToolName:  "Tool A",
	})

	// Wait for tool to be registered (poll instead of sleep for determinism)
	waitForCondition(t, 500*time.Millisecond, func() bool {
		_, ok := cp.toolBySession.Load("tool-change-session")
		return ok
	})

	// Write some output with Sequence and Mode
	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Output, eventbus.SourceSessionManager, eventbus.SessionOutputEvent{
		SessionID: "tool-change-session",
		Sequence:  42,
		Mode:      "test-mode",
		Data:      []byte("output from tool A"),
	})

	// Wait for output to be buffered (poll instead of sleep for determinism)
	waitForCondition(t, 500*time.Millisecond, func() bool {
		if val, ok := cp.buffers.Load("tool-change-session"); ok {
			buf := val.(*OutputBuffer)
			return !buf.IsEmpty()
		}
		return false
	})

	// Change tool - should trigger flush of buffered content
	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Tool, eventbus.SourcePluginService, eventbus.SessionToolEvent{
		SessionID: "tool-change-session",
		ToolID:    "tool-b",
		ToolName:  "Tool B",
	})

	// Should get the flushed content from tool A
	select {
	case env := <-cleanedSub.C():
		msg := env.Payload
		// Verify Origin is set correctly - tool output must have OriginTool
		if msg.Origin != eventbus.OriginTool {
			t.Errorf("expected Origin=OriginTool, got %q", msg.Origin)
		}
		if msg.Annotations[constants.MetadataKeyToolChanged] != "true" {
			t.Errorf("expected tool_changed=true, got %v", msg.Annotations[constants.MetadataKeyToolChanged])
		}
		if msg.Annotations[constants.MetadataKeyIdleState] != "tool_change" {
			t.Errorf("expected idle_state=tool_change, got %v", msg.Annotations[constants.MetadataKeyIdleState])
		}
		if msg.Annotations[constants.MetadataKeyToolID] != "tool-a" {
			t.Errorf("expected tool_id=tool-a, got %v", msg.Annotations[constants.MetadataKeyToolID])
		}
		if msg.Annotations[constants.MetadataKeyTool] != "Tool A" {
			t.Errorf("expected tool=Tool A, got %v", msg.Annotations[constants.MetadataKeyTool])
		}
		if msg.Text != "output from tool A" {
			t.Errorf("expected 'output from tool A', got %q", msg.Text)
		}
		// Verify Sequence propagation from last SessionOutputEvent
		if msg.Sequence != 42 {
			t.Errorf("expected Sequence=42 (from last SessionOutputEvent), got %d", msg.Sequence)
		}
		// Verify Mode propagation from last SessionOutputEvent (stored in annotations)
		if msg.Annotations[constants.MetadataKeyMode] != "test-mode" {
			t.Errorf("expected mode=test-mode in annotations, got %q", msg.Annotations[constants.MetadataKeyMode])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for tool change flush event")
	}
}

func TestIdleTimeoutAnnotation(t *testing.T) {
	bus := eventbus.New()
	cp := NewService(bus, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cp.Start(ctx); err != nil {
		t.Fatalf("start pipeline: %v", err)
	}
	defer cp.Shutdown(context.Background())

	cleanedSub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned)
	defer cleanedSub.Close()

	// Set tool
	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Tool, eventbus.SourcePluginService, eventbus.SessionToolEvent{
		SessionID: "idle-session",
		ToolID:    "test-tool",
		ToolName:  "Test Tool",
	})

	// Wait for tool to be registered (poll instead of sleep for determinism)
	waitForCondition(t, 500*time.Millisecond, func() bool {
		_, ok := cp.toolBySession.Load("idle-session")
		return ok
	})

	// Write output
	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Output, eventbus.SourceSessionManager, eventbus.SessionOutputEvent{
		SessionID: "idle-session",
		Sequence:  1,
		Data:      []byte("some output"),
	})

	// Wait for idle timeout (DefaultIdleTimeout is 500ms)
	select {
	case env := <-cleanedSub.C():
		msg := env.Payload
		if msg.Annotations[constants.MetadataKeyIdleState] != "timeout" {
			t.Errorf("expected idle_state=timeout, got %v", msg.Annotations[constants.MetadataKeyIdleState])
		}
		if msg.Text != "some output" {
			t.Errorf("expected 'some output', got %q", msg.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for idle flush event")
	}
}

func TestGetIdleStateUnknownSession(t *testing.T) {
	svc := NewService(eventbus.New(), nil)

	isIdle, waitingFor, since := svc.GetIdleState("unknown-session")
	if isIdle {
		t.Fatal("expected isIdle=false for unknown session")
	}
	if waitingFor != "" {
		t.Fatalf("expected empty waitingFor, got %q", waitingFor)
	}
	if !since.IsZero() {
		t.Fatalf("expected zero time, got %v", since)
	}
}

func TestIdleStateSetOnFlush(t *testing.T) {
	bus := eventbus.New()
	cp := NewService(bus, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cp.Start(ctx); err != nil {
		t.Fatalf("start pipeline: %v", err)
	}
	defer cp.Shutdown(context.Background())

	cleanedSub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned)
	defer cleanedSub.Close()

	// Write output — idle timer will fire and call flushAndProcess with idle state
	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Output, eventbus.SourceSessionManager, eventbus.SessionOutputEvent{
		SessionID: "idle-track-session",
		Sequence:  1,
		Data:      []byte("waiting for input"),
	})

	// Wait for flush (idle timeout triggers flushAndProcess with IsIdle=true)
	select {
	case <-cleanedSub.C():
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for flush")
	}

	// After flush with idle state, GetIdleState should return true.
	// Timer-based flush sets waitingFor="" (only plugin-detected idle has waitingFor).
	isIdle, waitingFor, since := cp.GetIdleState("idle-track-session")
	if !isIdle {
		t.Fatal("expected isIdle=true after idle flush")
	}
	if waitingFor != "" {
		t.Fatalf("expected empty waitingFor for timer-based idle, got %q", waitingFor)
	}
	if since.IsZero() {
		t.Fatal("expected non-zero Since time")
	}
	if elapsed := time.Since(since); elapsed > 5*time.Second {
		t.Fatalf("expected Since within last 5s, got %v ago", elapsed)
	}
}

func TestIdleStateSetOnFlushWithWaitingFor(t *testing.T) {
	cp := NewService(eventbus.New(), nil)

	buf := NewOutputBuffer()
	buf.Write([]byte("$ "))
	cp.buffers.Store("plugin-idle-session", buf) // register so flushAndProcess guard passes
	cp.flushAndProcess(context.Background(), "plugin-idle-session", buf, nil,
		&toolhandlers.IdleState{IsIdle: true, WaitingFor: "user_input"},
		eventbus.SessionOutputEvent{SessionID: "plugin-idle-session"}, "", "")

	isIdle, waitingFor, since := cp.GetIdleState("plugin-idle-session")
	if !isIdle {
		t.Fatal("expected isIdle=true after flush with plugin-detected idle")
	}
	if waitingFor != "user_input" {
		t.Fatalf("expected waitingFor=user_input, got %q", waitingFor)
	}
	if since.IsZero() {
		t.Fatal("expected non-zero Since time")
	}
}

func TestIdleStateClearedOnNonIdleFlush(t *testing.T) {
	cp := NewService(eventbus.New(), nil)

	// Manually store an idle state
	cp.idleStates.Store("clear-session", sessionIdleState{
		waitingFor: "user_input",
		since:      time.Now(),
	})

	// Verify it's stored
	isIdle, _, _ := cp.GetIdleState("clear-session")
	if !isIdle {
		t.Fatal("expected idle state to be stored")
	}

	// Create a buffer and call flushAndProcess with nil idleState (non-idle)
	buf := NewOutputBuffer()
	buf.Write([]byte("some output"))
	cp.buffers.Store("clear-session", buf) // register so flushAndProcess guard passes
	cp.flushAndProcess(context.Background(), "clear-session", buf, nil, nil, eventbus.SessionOutputEvent{
		SessionID: "clear-session",
	}, "", "")

	// idle state should be cleared
	isIdle, _, _ = cp.GetIdleState("clear-session")
	if isIdle {
		t.Fatal("expected idle state to be cleared after non-idle flush")
	}
}

func TestIdleStateCleanedOnSessionStop(t *testing.T) {
	bus := eventbus.New()
	cp := NewService(bus, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cp.Start(ctx); err != nil {
		t.Fatalf("start pipeline: %v", err)
	}
	defer cp.Shutdown(context.Background())

	// Manually store idle state
	cp.idleStates.Store("stop-session", sessionIdleState{
		waitingFor: "user_input",
		since:      time.Now(),
	})

	// Publish SessionStateStopped lifecycle event
	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "stop-session",
		State:     eventbus.SessionStateStopped,
	})

	// Wait for cleanup to happen
	waitForCondition(t, time.Second, func() bool {
		isIdle, _, _ := cp.GetIdleState("stop-session")
		return !isIdle
	})
}

func TestIdleStateCleanedOnSessionCreated(t *testing.T) {
	bus := eventbus.New()
	cp := NewService(bus, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cp.Start(ctx); err != nil {
		t.Fatalf("start pipeline: %v", err)
	}
	defer cp.Shutdown(context.Background())

	// Manually store idle state for a session being recreated
	cp.idleStates.Store("recreate-session", sessionIdleState{
		waitingFor: "user_input",
		since:      time.Now(),
	})

	// Publish SessionStateCreated lifecycle event (session recreated)
	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Lifecycle, eventbus.SourceSessionManager, eventbus.SessionLifecycleEvent{
		SessionID: "recreate-session",
		State:     eventbus.SessionStateCreated,
	})

	// Wait for cleanup to happen
	waitForCondition(t, time.Second, func() bool {
		isIdle, _, _ := cp.GetIdleState("recreate-session")
		return !isIdle
	})
}

func TestShutdownClearsIdleStates(t *testing.T) {
	bus := eventbus.New()
	cp := NewService(bus, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cp.Start(ctx); err != nil {
		t.Fatalf("start pipeline: %v", err)
	}

	// Store idle states for multiple sessions
	cp.idleStates.Store("session-a", sessionIdleState{waitingFor: "user_input", since: time.Now()})
	cp.idleStates.Store("session-b", sessionIdleState{waitingFor: "confirmation", since: time.Now()})

	// Shutdown should clear all idle states
	if err := cp.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Verify all idle states are cleared
	isIdle, _, _ := cp.GetIdleState("session-a")
	if isIdle {
		t.Fatal("expected session-a idle state to be cleared after shutdown")
	}
	isIdle, _, _ = cp.GetIdleState("session-b")
	if isIdle {
		t.Fatal("expected session-b idle state to be cleared after shutdown")
	}
}

func TestFlushAndProcessPublishesWhenBufferRemovedByRace(t *testing.T) {
	bus := eventbus.New()
	cp := NewService(bus, nil)

	cleanedSub := eventbus.SubscribeTo(bus, eventbus.Pipeline.Cleaned)
	defer cleanedSub.Close()

	buf := NewOutputBuffer()
	buf.Write([]byte("output before cleanup"))
	// Register then remove — simulates cleanup race between handleSessionOutput
	// acquiring the buffer and flushAndProcess checking the guard.
	cp.buffers.Store("race-session", buf)
	cp.buffers.Delete("race-session")

	cp.flushAndProcess(context.Background(), "race-session", buf, nil,
		&toolhandlers.IdleState{IsIdle: true, WaitingFor: "user_input"},
		eventbus.SessionOutputEvent{SessionID: "race-session"}, "", "")

	// Pipeline message must still be published (text not dropped).
	select {
	case <-cleanedSub.C():
	case <-time.After(time.Second):
		t.Fatal("expected pipeline message to be published even after buffer cleanup race")
	}

	// Idle state must NOT be stored (guard prevented stale entry).
	isIdle, _, _ := cp.GetIdleState("race-session")
	if isIdle {
		t.Fatal("expected idle state NOT to be stored when buffer was cleaned up")
	}
}

func TestToolChangeWithEmptyBufferClearsIdleState(t *testing.T) {
	bus := eventbus.New()
	cp := NewService(bus, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cp.Start(ctx); err != nil {
		t.Fatalf("start pipeline: %v", err)
	}
	defer cp.Shutdown(context.Background())

	// Store idle state from a previous tool (simulating tool A being idle)
	cp.idleStates.Store("tool-empty-session", sessionIdleState{
		waitingFor: "user_input",
		since:      time.Now(),
	})

	// Register old tool — buffer exists but is empty (already flushed by timer)
	cp.toolBySession.Store("tool-empty-session", toolMetadata{ID: "tool-a", Name: "Tool A"})
	cp.buffers.Store("tool-empty-session", NewOutputBuffer()) // empty buffer

	// Trigger tool change via event bus
	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Tool, eventbus.SourcePluginService, eventbus.SessionToolEvent{
		SessionID: "tool-empty-session",
		ToolID:    "tool-b",
		ToolName:  "Tool B",
	})

	// Wait for tool change to be processed
	waitForCondition(t, time.Second, func() bool {
		val, ok := cp.toolBySession.Load("tool-empty-session")
		if !ok {
			return false
		}
		meta := val.(toolMetadata)
		return meta.ID == "tool-b"
	})

	// Idle state from tool A must be cleared — tool changed
	isIdle, _, _ := cp.GetIdleState("tool-empty-session")
	if isIdle {
		t.Fatal("expected idle state to be cleared after tool change with empty buffer")
	}
}

func TestToolChangeWithNoBufferClearsIdleState(t *testing.T) {
	bus := eventbus.New()
	cp := NewService(bus, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cp.Start(ctx); err != nil {
		t.Fatalf("start pipeline: %v", err)
	}
	defer cp.Shutdown(context.Background())

	// Store idle state but NO buffer for this session
	cp.idleStates.Store("no-buf-session", sessionIdleState{
		waitingFor: "confirmation",
		since:      time.Now(),
	})

	// Register old tool without any buffer
	cp.toolBySession.Store("no-buf-session", toolMetadata{ID: "tool-old", Name: "Old Tool"})

	// Trigger tool change
	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Tool, eventbus.SourcePluginService, eventbus.SessionToolEvent{
		SessionID: "no-buf-session",
		ToolID:    "tool-new",
		ToolName:  "New Tool",
	})

	// Wait for tool change to be processed
	waitForCondition(t, time.Second, func() bool {
		val, ok := cp.toolBySession.Load("no-buf-session")
		if !ok {
			return false
		}
		meta := val.(toolMetadata)
		return meta.ID == "tool-new"
	})

	// Idle state must be cleared even without a buffer
	isIdle, _, _ := cp.GetIdleState("no-buf-session")
	if isIdle {
		t.Fatal("expected idle state to be cleared after tool change with no buffer")
	}
}

func TestToolChangeDoesNotStoreIdleState(t *testing.T) {
	cp := NewService(eventbus.New(), nil)

	// Manually store idle state (simulating a previous plugin-detected idle)
	cp.idleStates.Store("tool-change-session", sessionIdleState{
		waitingFor: "user_input",
		since:      time.Now(),
	})

	// flushAndProcess with tool_change should clear (not store) idle state
	buf := NewOutputBuffer()
	buf.Write([]byte("output before tool change"))
	cp.buffers.Store("tool-change-session", buf) // register so flushAndProcess guard passes
	cp.flushAndProcess(context.Background(), "tool-change-session", buf, nil,
		&toolhandlers.IdleState{IsIdle: true, Reason: "tool_change"},
		eventbus.SessionOutputEvent{SessionID: "tool-change-session"}, "", "")

	// tool_change should NOT appear as idle in GetIdleState
	isIdle, _, _ := cp.GetIdleState("tool-change-session")
	if isIdle {
		t.Fatal("expected tool_change to NOT store idle state — tool transitions are transient, not real idle")
	}
}

func TestEmptySessionIDIgnored(t *testing.T) {
	// Test the guard directly without event bus for fully deterministic behavior
	bus := eventbus.New()
	cp := NewService(bus, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cp.Start(ctx); err != nil {
		t.Fatalf("start pipeline: %v", err)
	}
	defer cp.Shutdown(context.Background())

	// Call handleSessionOutput directly with empty SessionID
	// This is deterministic - no async event bus involved
	cp.handleSessionOutput(ctx, eventbus.SessionOutputEvent{
		SessionID: "", // Empty!
		Sequence:  1,
		Data:      []byte("should be ignored"),
	})

	// Verify immediately: no buffer was created for empty session ID
	_, exists := cp.buffers.Load("")
	if exists {
		t.Fatal("buffer should not be created for empty SessionID")
	}

	// Verify no lastSeenMeta was stored for empty session ID
	_, metaExists := cp.lastSeenBySession.Load("")
	if metaExists {
		t.Fatal("lastSeenMeta should not be stored for empty SessionID")
	}
}

func TestGetIdleStateConcurrent(t *testing.T) {
	cp := NewService(eventbus.New(), nil)

	const goroutines = 10
	const iterations = 200
	sessionID := "concurrent-session"

	// Pre-register a buffer so flushAndProcess guard passes
	buf := NewOutputBuffer()
	cp.buffers.Store(sessionID, buf)

	var wg sync.WaitGroup

	// Writers: alternate between storing and deleting idle state
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if j%2 == 0 {
					cp.idleStates.Store(sessionID, sessionIdleState{
						waitingFor: "user_input",
						since:      time.Now(),
					})
				} else {
					cp.idleStates.Delete(sessionID)
				}
			}
		}()
	}

	// Readers: call GetIdleState concurrently with writes
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				isIdle, waitingFor, since := cp.GetIdleState(sessionID)
				if isIdle {
					// When idle, waitingFor must be "user_input" (the only value we store)
					if waitingFor != "user_input" {
						t.Errorf("expected waitingFor=user_input when idle, got %q", waitingFor)
					}
					if since.IsZero() {
						t.Error("expected non-zero since when idle")
					}
				}
			}
		}()
	}

	wg.Wait()
}
