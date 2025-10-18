package modules

import (
	"context"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
)

// EnsureBuiltinAdapters guarantees that builtin adapters are present in the store.
func EnsureBuiltinAdapters(ctx context.Context, store *configstore.Store) error {
	if store == nil {
		return nil
	}

	adapters := []configstore.Adapter{
		{
			ID:      MockSTTAdapterID,
			Source:  "builtin",
			Type:    "stt",
			Name:    "Nupi Mock STT",
			Version: "dev",
		},
		{
			ID:      MockTTSAdapterID,
			Source:  "builtin",
			Type:    "tts",
			Name:    "Nupi Mock TTS",
			Version: "dev",
		},
	}

	for _, adapter := range adapters {
		if err := store.UpsertAdapter(ctx, adapter); err != nil {
			return err
		}
	}
	return nil
}
