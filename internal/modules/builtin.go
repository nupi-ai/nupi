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

	adapter := configstore.Adapter{
		ID:      MockSTTAdapterID,
		Source:  "builtin",
		Type:    "stt",
		Name:    "Nupi Mock STT",
		Version: "dev",
	}

	return store.UpsertAdapter(ctx, adapter)
}
