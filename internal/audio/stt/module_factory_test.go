package stt

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/modules"
)

func TestModuleFactoryCreatesMockTranscriber(t *testing.T) {
	ctx := context.Background()

	store, err := configstore.Open(configstore.Options{DBPath: filepath.Join(t.TempDir(), "config.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
	})

	if err := modules.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}

	if err := store.SetActiveAdapter(ctx, string(modules.SlotSTTPrimary), modules.MockSTTAdapterID, map[string]any{
		"text": "factory",
	}); err != nil {
		t.Fatalf("activate mock adapter: %v", err)
	}

	factory := NewModuleFactory(store)

	params := SessionParams{
		SessionID: "sess",
		StreamID:  "mic",
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
	}

	transcriber, err := factory.Create(ctx, params)
	if err != nil {
		t.Fatalf("create transcriber: %v", err)
	}

	segment := eventbus.AudioIngressSegmentEvent{
		SessionID: "sess",
		StreamID:  "mic",
		Sequence:  1,
		Format:    params.Format,
		Data:      make([]byte, 640),
		Duration:  20 * time.Millisecond,
		First:     true,
		Last:      true,
		StartedAt: time.Now().UTC(),
		EndedAt:   time.Now().UTC().Add(20 * time.Millisecond),
	}

	transcripts, err := transcriber.OnSegment(ctx, segment)
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if len(transcripts) == 0 || transcripts[0].Text != "factory" {
		t.Fatalf("unexpected transcript: %+v", transcripts)
	}
}
