package vad

import (
	"context"
	"errors"
	"testing"

	"github.com/nupi-ai/nupi/internal/modules"
	testutil "github.com/nupi-ai/nupi/internal/testutil"
)

func TestModuleFactoryReturnsMockAnalyzer(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := modules.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(modules.SlotVAD), modules.MockVADAdapterID, map[string]any{
		"threshold":  0.05,
		"min_frames": 2,
	}); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	factory := NewModuleFactory(store)
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

func TestModuleFactoryReturnsErrorOnConfigParseFailure(t *testing.T) {
	ctx := context.Background()
	store, cleanup := testutil.OpenStore(t)
	defer cleanup()

	if err := modules.EnsureBuiltinAdapters(ctx, store); err != nil {
		t.Fatalf("ensure builtin adapters: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, string(modules.SlotVAD), modules.MockVADAdapterID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	_, err := store.DB().ExecContext(ctx, `
        UPDATE adapter_bindings
        SET config = '{invalid'
        WHERE slot = ? AND instance_name = ? AND profile_name = ?
    `, string(modules.SlotVAD), store.InstanceName(), store.ProfileName())
	if err != nil {
		t.Fatalf("corrupt config: %v", err)
	}

	factory := NewModuleFactory(store)
	_, err = factory.Create(ctx, SessionParams{
		SessionID: "sess",
		StreamID:  "mic",
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
		StreamID:  "mic",
	})
	if !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("expected ErrAdapterUnavailable, got %v", err)
	}
}
