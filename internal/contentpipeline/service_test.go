package contentpipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/jsrunner"
	"github.com/nupi-ai/nupi/internal/plugins"
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

func writeCleanerPlugin(t *testing.T, root, catalog, slug, script string) {
	t.Helper()
	dir := filepath.Join(root, "plugins", catalog, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	manifest := fmt.Sprintf(`apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: pipeline-cleaner
metadata:
  name: %s
  slug: %s
  catalog: %s
  version: 0.0.1
spec:
  main: main.js
`, slug, slug, catalog)
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
	const catalog = "test.catalog"

	writeCleanerPlugin(t, tmp, catalog, "pipeline-tool-x", `module.exports = {
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

	cleanedSub := bus.Subscribe(eventbus.TopicPipelineCleaned)
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
		msg, ok := env.Payload.(eventbus.PipelineMessageEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", env.Payload)
		}
		if msg.Text != "HELLO\n" {
			t.Fatalf("expected transformed text, got %q", msg.Text)
		}
		if msg.Annotations["cleaned"] != "yes" {
			t.Fatalf("expected cleaned annotation, got %v", msg.Annotations)
		}
		if msg.Annotations["tool_id"] != "tool-x" {
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
	const catalog = "test.catalog"

	writeCleanerPlugin(t, tmp, catalog, "pipeline-tool-err", `module.exports = {
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

	errSub := bus.Subscribe(eventbus.TopicPipelineError)
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
		msg, ok := env.Payload.(eventbus.PipelineErrorEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", env.Payload)
		}
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

	cleanedSub := bus.Subscribe(eventbus.TopicPipelineCleaned)
	defer cleanedSub.Close()

	eventbus.Publish(context.Background(), bus, eventbus.Speech.TranscriptFinal, eventbus.SourceAudioSTT, eventbus.SpeechTranscriptEvent{
		SessionID:  "voice-session",
		StreamID:   "mic",
		Sequence:   7,
		Text:       "turn lights on",
		Confidence: 0.87,
		Final:      true,
		Metadata: map[string]string{
			"locale": "en-US",
		},
	})

	select {
	case env := <-cleanedSub.C():
		msg, ok := env.Payload.(eventbus.PipelineMessageEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", env.Payload)
		}
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
		if msg.Annotations["input_source"] != "voice" {
			t.Fatalf("missing input_source annotation: %+v", msg.Annotations)
		}
		if msg.Annotations["stream_id"] != "mic" {
			t.Fatalf("missing stream_id annotation: %+v", msg.Annotations)
		}
		if msg.Annotations["locale"] != "en-US" {
			t.Fatalf("metadata not propagated: %+v", msg.Annotations)
		}
		if _, ok := msg.Annotations["confidence"]; !ok {
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

	cleanedSub := bus.Subscribe(eventbus.TopicPipelineCleaned)
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
		msg, ok := env.Payload.(eventbus.PipelineMessageEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", env.Payload)
		}
		if msg.Annotations["buffer_truncated"] != "true" {
			t.Errorf("expected buffer_truncated=true, got %v", msg.Annotations["buffer_truncated"])
		}
		// buffer_max_size should reflect the actual buffer's configured maxSize
		if msg.Annotations["buffer_max_size"] != "100" {
			t.Errorf("expected buffer_max_size=100, got %v", msg.Annotations["buffer_max_size"])
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

	cleanedSub := bus.Subscribe(eventbus.TopicPipelineCleaned)
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
		msg, ok := env.Payload.(eventbus.PipelineMessageEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", env.Payload)
		}
		// Verify Origin is set correctly - tool output must have OriginTool
		if msg.Origin != eventbus.OriginTool {
			t.Errorf("expected Origin=OriginTool, got %q", msg.Origin)
		}
		if msg.Annotations["tool_changed"] != "true" {
			t.Errorf("expected tool_changed=true, got %v", msg.Annotations["tool_changed"])
		}
		if msg.Annotations["idle_state"] != "tool_change" {
			t.Errorf("expected idle_state=tool_change, got %v", msg.Annotations["idle_state"])
		}
		if msg.Annotations["tool_id"] != "tool-a" {
			t.Errorf("expected tool_id=tool-a, got %v", msg.Annotations["tool_id"])
		}
		if msg.Annotations["tool"] != "Tool A" {
			t.Errorf("expected tool=Tool A, got %v", msg.Annotations["tool"])
		}
		if msg.Text != "output from tool A" {
			t.Errorf("expected 'output from tool A', got %q", msg.Text)
		}
		// Verify Sequence propagation from last SessionOutputEvent
		if msg.Sequence != 42 {
			t.Errorf("expected Sequence=42 (from last SessionOutputEvent), got %d", msg.Sequence)
		}
		// Verify Mode propagation from last SessionOutputEvent (stored in annotations)
		if msg.Annotations["mode"] != "test-mode" {
			t.Errorf("expected mode=test-mode in annotations, got %q", msg.Annotations["mode"])
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

	cleanedSub := bus.Subscribe(eventbus.TopicPipelineCleaned)
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
		msg, ok := env.Payload.(eventbus.PipelineMessageEvent)
		if !ok {
			t.Fatalf("unexpected payload type %T", env.Payload)
		}
		if msg.Annotations["idle_state"] != "timeout" {
			t.Errorf("expected idle_state=timeout, got %v", msg.Annotations["idle_state"])
		}
		if msg.Text != "some output" {
			t.Errorf("expected 'some output', got %q", msg.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for idle flush event")
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
