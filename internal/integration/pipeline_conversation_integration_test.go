package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/contentpipeline"
	"github.com/nupi-ai/nupi/internal/conversation"
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

func setHostScriptEnv(t *testing.T) {
	t.Helper()
	// Get the path to host.js relative to this test file
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to get test file path")
	}
	// Navigate from pipeline_conversation_integration_test.go to jsruntime/host.js
	hostScript := filepath.Join(filepath.Dir(filepath.Dir(filename)), "jsruntime", "host.js")
	if _, err := os.Stat(hostScript); err != nil {
		t.Skipf("host.js not found at %s", hostScript)
	}
	t.Setenv("NUPI_JS_HOST_SCRIPT", hostScript)
}

func TestPipelineToConversationIntegration(t *testing.T) {
	skipIfNoBun(t)
	setHostScriptEnv(t)

	tmp := t.TempDir()
	const namespace = "test.namespace"

	writePipelinePlugin(t, tmp, namespace, "pipeline-default", `module.exports = {
        name: "default",
        transform: function(input) {
            return { text: input.text.toUpperCase(), annotations: { cleaned: "true" } };
        }
    };`)

	pluginSvc := newTestPluginService(t, tmp)

	bus := eventbus.New()
	pipelineSvc := contentpipeline.NewService(bus, pluginSvc)
	conversationSvc := conversation.NewService(bus, conversation.WithDetachTTL(200*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Subscribe to conversation.prompt to verify pipeline→conversation flow.
	// Context() no longer returns RAM history (Story 19.2 refactoring),
	// so we verify via the published ConversationPromptEvent instead.
	promptSub := eventbus.SubscribeTo(bus, eventbus.Conversation.Prompt, eventbus.WithSubscriptionName("test_prompt"))
	defer promptSub.Close()

	if err := pipelineSvc.Start(ctx); err != nil {
		t.Fatalf("start content pipeline: %v", err)
	}
	defer pipelineSvc.Shutdown(context.Background())

	if err := conversationSvc.Start(ctx); err != nil {
		t.Fatalf("start conversation service: %v", err)
	}
	defer conversationSvc.Shutdown(context.Background())

	eventbus.Publish(context.Background(), bus, eventbus.Sessions.Output, eventbus.SourceSessionManager, eventbus.SessionOutputEvent{
		SessionID: "integration-session",
		Sequence:  1,
		Data:      []byte("pipeline -> conversation test"),
		Origin:    eventbus.OriginUser,
	})

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case env, ok := <-promptSub.C():
		if !ok {
			t.Fatal("prompt subscription closed")
		}
		prompt := env.Payload
		if prompt.NewMessage.Text != "PIPELINE -> CONVERSATION TEST" {
			t.Fatalf("unexpected conversation text: %q", prompt.NewMessage.Text)
		}
		if prompt.NewMessage.Origin != eventbus.OriginUser {
			t.Fatalf("unexpected origin: %s", prompt.NewMessage.Origin)
		}
		if prompt.NewMessage.Meta[constants.MetadataKeyCleaned] != "true" {
			t.Fatalf("expected cleaned annotation, got %+v", prompt.NewMessage.Meta)
		}
	case <-timer.C:
		t.Fatal("timeout waiting for conversation prompt event")
	}
}

func writePipelinePlugin(t *testing.T, root, namespace, slug, body string) {
	t.Helper()
	plDir := filepath.Join(root, "plugins", namespace, slug)
	if err := os.MkdirAll(plDir, 0o755); err != nil {
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
	if err := os.WriteFile(filepath.Join(plDir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(plDir, "main.js"), []byte(body), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
}

func newTestPluginService(t *testing.T, baseDir string) *plugins.Service {
	t.Helper()
	svc := plugins.NewService(baseDir)
	// Start the service to initialize jsruntime (with timeout to avoid hanging on slow Bun startup)
	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startCancel()
	if err := svc.Start(startCtx); err != nil {
		t.Fatalf("start plugin service: %v", err)
	}
	t.Cleanup(func() {
		svc.Shutdown(context.Background())
	})
	return svc
}
