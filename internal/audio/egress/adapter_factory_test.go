package egress

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/plugins/adapters"
	testutil "github.com/nupi-ai/nupi/internal/testutil"
	"github.com/nupi-ai/nupi/internal/voice/slots"
)

func TestAdapterFactoryReturnsMockSynthesizer(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, map[string]any{
		"frequency":   880.0,
		"duration_ms": 250,
		"metadata":    map[string]any{"voice": "test"},
	}); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	factory := NewAdapterFactory(store)
	synth, err := factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
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

func TestAdapterFactoryReturnsErrorOnConfigParseFailure(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotTTS), adapters.MockTTSAdapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	_, err := store.DB().ExecContext(ctx, `
        UPDATE adapter_bindings
        SET config = '{invalid'
        WHERE slot = ? AND instance_name = ? AND profile_name = ?
    `, string(adapters.SlotTTS), store.InstanceName(), store.ProfileName())
	if err != nil {
		t.Fatalf("corrupt config: %v", err)
	}

	factory := NewAdapterFactory(store)
	_, err = factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
	})
	if err == nil {
		t.Fatalf("expected error due to invalid config")
	}
}

func TestAdapterFactoryReturnsUnavailableWhenAdapterMissing(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	factory := NewAdapterFactory(store)
	_, err := factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  slots.TTS,
	})
	if !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("expected ErrAdapterUnavailable, got %v", err)
	}
}
