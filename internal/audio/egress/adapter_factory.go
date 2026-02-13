package egress

import (
	"context"

	"github.com/nupi-ai/nupi/internal/audio/adapterutil"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
)

// AdapterRuntimeSource exposes runtime status for adapters.
type AdapterRuntimeSource = adapterutil.AdapterRuntimeSource

// NewAdapterFactory creates a factory that resolves synthesizers based on adapter bindings.
func NewAdapterFactory(store *configstore.Store, runtime AdapterRuntimeSource) Factory {
	f := adapterutil.NewFactory(store, runtime, adapterutil.FactoryConfig[Synthesizer]{
		Prefix:                "tts",
		Slot:                  adapters.SlotTTS,
		MockAdapterID:         adapters.MockTTSAdapterID,
		NewMock:               newMockSynthesizer,
		NewNAP:                newNAPSynthesizer,
		ErrAdapterUnavailable: ErrAdapterUnavailable,
		ErrFactoryUnavailable: ErrFactoryUnavailable,
	})
	return FactoryFunc(func(ctx context.Context, params SessionParams) (Synthesizer, error) {
		return f.Create(ctx, params)
	})
}
