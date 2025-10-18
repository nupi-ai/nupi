package stt

import (
	"context"
	"fmt"
	"strings"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/modules"
)

type moduleFactory struct {
	store *configstore.Store
}

// NewModuleFactory creates a factory based on module bindings stored in config.Store.
// It currently validates the presence of an active adapter and returns an error until
// transport clients are implemented.
func NewModuleFactory(store *configstore.Store) Factory {
	if store == nil {
		return FactoryFunc(func(context.Context, SessionParams) (Transcriber, error) {
			return nil, ErrFactoryUnavailable
		})
	}
	return moduleFactory{store: store}
}

func (m moduleFactory) Create(ctx context.Context, params SessionParams) (Transcriber, error) {
	bindings, err := m.store.ListAdapterBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("stt: list adapter bindings: %w", err)
	}

	activeAdapter := ""
	for _, binding := range bindings {
		if binding.Slot != string(modules.SlotSTTPrimary) && binding.Slot != string(modules.SlotSTTSecondary) {
			continue
		}
		if !strings.EqualFold(binding.Status, configstore.BindingStatusActive) {
			continue
		}
		if binding.AdapterID == nil {
			continue
		}
		id := strings.TrimSpace(*binding.AdapterID)
		if id != "" {
			activeAdapter = id
			break
		}
	}

	if activeAdapter == "" {
		return nil, ErrAdapterUnavailable
	}

	// TODO(#audio-stt-nap): replace the stub once NAP client integration is available.
	return nil, ErrAdapterUnavailable
}
