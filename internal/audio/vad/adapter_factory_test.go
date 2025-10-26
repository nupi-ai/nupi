package vad

import (
	"context"
	"errors"
	"testing"

	"github.com/nupi-ai/nupi/internal/plugins/adapters"
	testutil "github.com/nupi-ai/nupi/internal/testutil"
)

func TestAdapterFactoryReturnsMockAnalyzer(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, map[string]any{
		"threshold":  0.05,
		"min_frames": 2,
	}); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	factory := NewAdapterFactory(store)
	analyzer, err := factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  "mic",
	})
	if err != nil {
		t.Fatalf("expected analyzer, got error: %v", err)
	}
	mock, ok := analyzer.(*mockAnalyzer)
	if !ok {
		t.Fatalf("expected *mockAnalyzer, got %T", analyzer)
	}
	if mock.threshold != 0.05 {
		t.Fatalf("expected threshold 0.05, got %f", mock.threshold)
	}
	if mock.minFrames != 2 {
		t.Fatalf("expected minFrames 2, got %d", mock.minFrames)
	}
}

func TestAdapterFactoryReturnsErrorOnConfigParseFailure(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := adapters.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotVAD), adapters.MockVADAdapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	_, err := store.DB().ExecContext(ctx, `
        UPDATE adapter_bindings
        SET config = '{invalid'
        WHERE slot = ? AND instance_name = ? AND profile_name = ?
    `, string(adapters.SlotVAD), store.InstanceName(), store.ProfileName())
	if err != nil {
		t.Fatalf("corrupt config: %v", err)
	}

	factory := NewAdapterFactory(store)
	_, err = factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  "mic",
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
		StreamID:  "mic",
	})
	if !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("expected ErrAdapterUnavailable, got %v", err)
	}
}
