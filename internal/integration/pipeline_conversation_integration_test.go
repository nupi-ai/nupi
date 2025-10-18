package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/contentpipeline"
	"github.com/nupi-ai/nupi/internal/conversation"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/plugins"
)

func TestPipelineToConversationIntegration(t *testing.T) {
	tmp := t.TempDir()
	pipelineDir := filepath.Join(tmp, "pipeline")
	if err := os.MkdirAll(pipelineDir, 0o755); err != nil {
		t.Fatalf("mkdir pipeline dir: %v", err)
	}

	writePipelinePlugin(t, pipelineDir, "default.js", `module.exports = {
        name: "default",
        transform: function(input) {
            return { text: input.text.toUpperCase(), annotations: { cleaned: "true" } };
        }
    };`)

	pluginSvc := newTestPluginService(t, tmp)

	bus := eventbus.New()
	pipelineSvc := contentpipeline.NewService(bus, pluginSvc)
	conversationSvc := conversation.NewService(bus, conversation.WithHistoryLimit(10), conversation.WithDetachTTL(200*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := pipelineSvc.Start(ctx); err != nil {
		t.Fatalf("start content pipeline: %v", err)
	}
	defer pipelineSvc.Shutdown(context.Background())

	if err := conversationSvc.Start(ctx); err != nil {
		t.Fatalf("start conversation service: %v", err)
	}
	defer conversationSvc.Shutdown(context.Background())

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:  eventbus.TopicSessionsOutput,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionOutputEvent{
			SessionID: "integration-session",
			Sequence:  1,
			Data:      []byte("pipeline -> conversation test"),
			Origin:    eventbus.OriginUser,
		},
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		turns := conversationSvc.Context("integration-session")
		if len(turns) == 1 {
			turn := turns[0]
			if turn.Text != "PIPELINE -> CONVERSATION TEST" {
				t.Fatalf("unexpected conversation text: %q", turn.Text)
			}
			if turn.Origin != eventbus.OriginUser {
				t.Fatalf("unexpected origin: %s", turn.Origin)
			}
			if turn.Meta["cleaned"] != "true" {
				t.Fatalf("expected cleaned annotation, got %+v", turn.Meta)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("conversation history not updated, turns=%d", len(turns))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writePipelinePlugin(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write pipeline plugin: %v", err)
	}
}

func newTestPluginService(t *testing.T, baseDir string) *plugins.Service {
	t.Helper()
	svc := plugins.NewService(baseDir, plugins.WithExtractor(func(string) error { return nil }))
	if err := os.MkdirAll(svc.PipelineDir(), 0o755); err != nil {
		t.Fatalf("ensure pipeline dir: %v", err)
	}
	if err := svc.LoadPipelinePlugins(); err != nil {
		t.Fatalf("load pipeline plugins: %v", err)
	}
	return svc
}
