package egress

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/modules"
	testutil "github.com/nupi-ai/nupi/internal/testutil"
)

func TestModuleFactoryReturnsMockSynthesizer(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := modules.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(modules.SlotTTS), modules.MockTTSAdapterID, map[string]any{
		"frequency":   880.0,
		"duration_ms": 250,
		"metadata":    map[string]any{"voice": "test"},
	}); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	factory := NewModuleFactory(store)
	synth, err := factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  "tts.primary",
	})
	if err != nil {
		t.Fatalf("expected synthesizer, got error: %v", err)
	}
	mock, ok := synth.(*mockSynthesizer)
	if !ok {
		t.Fatalf("expected *mockSynthesizer, got %T", synth)
	}
	if mock.frequency != 880.0 {
		t.Fatalf("expected frequency 880Hz, got %f", mock.frequency)
	}
	if mock.duration != 250*time.Millisecond {
		t.Fatalf("expected duration 250ms, got %s", mock.duration)
	}
	if mock.metadata["voice"] != "test" {
		t.Fatalf("expected metadata voice=test, got %+v", mock.metadata)
	}
}

func TestModuleFactoryReturnsErrorOnConfigParseFailure(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := modules.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(modules.SlotTTS), modules.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	_, err := store.DB().ExecContext(ctx, `
        UPDATE adapter_bindings
        SET config = '{invalid'
        WHERE slot = ? AND instance_name = ? AND profile_name = ?
    `, string(modules.SlotTTS), store.InstanceName(), store.ProfileName())
	if err != nil {
		t.Fatalf("corrupt config: %v", err)
	}

	factory := NewModuleFactory(store)
	_, err = factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  "tts.primary",
	})
	if err == nil {
		t.Fatalf("expected error due to invalid config")
	}
}

func TestModuleFactoryReturnsUnavailableWhenAdapterMissing(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	factory := NewModuleFactory(store)
	_, err := factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  "tts.primary",
	})
	if !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("expected ErrAdapterUnavailable, got %v", err)
	}
}
