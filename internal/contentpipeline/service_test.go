package contentpipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/plugins"
)

func writePlugin(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write plugin: %v", err)
	}
}

func newPluginService(t *testing.T, baseDir string) *plugins.Service {
	t.Helper()
	svc := plugins.NewService(baseDir, plugins.WithExtractor(func(string) error { return nil }))
	if err := os.MkdirAll(svc.PipelineDir(), 0o755); err != nil {
		t.Fatalf("mkdir pipeline: %v", err)
	}
	if err := svc.LoadPipelinePlugins(); err != nil {
		t.Fatalf("load pipeline plugins: %v", err)
	}
	return svc
}

func TestContentPipelineTransformsOutput(t *testing.T) {
	tmp := t.TempDir()
	pipelineDir := filepath.Join(tmp, "pipeline")
	if err := os.MkdirAll(pipelineDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writePlugin(t, pipelineDir, "tool-x.js", `module.exports = {
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

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:  eventbus.TopicSessionsTool,
		Source: eventbus.SourcePluginService,
		Payload: eventbus.SessionToolEvent{
			SessionID: "s1",
			ToolID:    "tool-x",
			ToolName:  "Tool X",
		},
	})

	time.Sleep(50 * time.Millisecond)

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:  eventbus.TopicSessionsOutput,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionOutputEvent{
			SessionID: "s1",
			Sequence:  1,
			Data:      []byte("hello\n"),
		},
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
	tmp := t.TempDir()
	pipelineDir := filepath.Join(tmp, "pipeline")
	if err := os.MkdirAll(pipelineDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writePlugin(t, pipelineDir, "tool-err.js", `module.exports = {
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

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:  eventbus.TopicSessionsTool,
		Source: eventbus.SourcePluginService,
		Payload: eventbus.SessionToolEvent{
			SessionID: "s2",
			ToolID:    "tool-err",
			ToolName:  "Tool Err",
		},
	})

	time.Sleep(50 * time.Millisecond)

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:  eventbus.TopicSessionsOutput,
		Source: eventbus.SourceSessionManager,
		Payload: eventbus.SessionOutputEvent{
			SessionID: "s2",
			Sequence:  1,
			Data:      []byte("boom"),
		},
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

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic:  eventbus.TopicSpeechTranscriptFinal,
		Source: eventbus.SourceAudioSTT,
		Payload: eventbus.SpeechTranscriptEvent{
			SessionID:  "voice-session",
			StreamID:   "mic",
			Sequence:   7,
			Text:       "turn lights on",
			Confidence: 0.87,
			Final:      true,
			Metadata: map[string]string{
				"locale": "en-US",
			},
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
