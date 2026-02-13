package vad

import (
	"context"

	"github.com/nupi-ai/nupi/internal/audio/adapterutil"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
)

// AdapterRuntimeSource exposes runtime status for adapters.
type AdapterRuntimeSource = adapterutil.AdapterRuntimeSource

// NewAdapterFactory creates a factory driven by adapter bindings in the config store.
func NewAdapterFactory(store *configstore.Store, runtime AdapterRuntimeSource) Factory {
	f := adapterutil.NewFactory(store, runtime, adapterutil.FactoryConfig[Analyzer]{
		Prefix:                "vad",
		Slot:                  adapters.SlotVAD,
		MockAdapterID:         adapters.MockVADAdapterID,
		NewMock:               newMockAnalyzer,
		NewNAP:                newNAPAnalyzer,
		ErrAdapterUnavailable: ErrAdapterUnavailable,
		ErrFactoryUnavailable: ErrFactoryUnavailable,
	})
	return FactoryFunc(func(ctx context.Context, params SessionParams) (Analyzer, error) {
		return f.Create(ctx, params)
	})
}
