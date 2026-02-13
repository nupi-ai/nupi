package stt

import (
	"context"

	"github.com/nupi-ai/nupi/internal/audio/adapterutil"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
)

// AdapterRuntimeSource exposes runtime status for adapters.
type AdapterRuntimeSource = adapterutil.AdapterRuntimeSource

// NewAdapterFactory creates a factory based on adapter bindings stored in config.Store.
func NewAdapterFactory(store *configstore.Store, runtime AdapterRuntimeSource) Factory {
	f := adapterutil.NewFactory(store, runtime, adapterutil.FactoryConfig[Transcriber]{
		Prefix:                "stt",
		Slot:                  adapters.SlotSTT,
		MockAdapterID:         adapters.MockSTTAdapterID,
		NewMock:               newMockTranscriber,
		NewNAP:                newNAPTranscriber,
		ErrAdapterUnavailable: ErrAdapterUnavailable,
		ErrFactoryUnavailable: ErrFactoryUnavailable,
	})
	return FactoryFunc(func(ctx context.Context, params SessionParams) (Transcriber, error) {
		return f.Create(ctx, params)
	})
}
