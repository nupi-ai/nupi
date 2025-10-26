package vad

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
)

type adapterFactory struct {
	store *configstore.Store
}

// NewAdapterFactory creates a factory driven by adapter bindings in the config store.
func NewAdapterFactory(store *configstore.Store) Factory {
	if store == nil {
		return FactoryFunc(func(context.Context, SessionParams) (Analyzer, error) {
			return nil, ErrFactoryUnavailable
		})
	}
	return adapterFactory{store: store}
}

func (m adapterFactory) Create(ctx context.Context, params SessionParams) (Analyzer, error) {
	bindings, err := m.store.ListAdapterBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("vad: list adapter bindings: %w", err)
	}

	var (
		adapterID string
		config    map[string]any
	)

	for _, binding := range bindings {
		if binding.Slot != string(adapters.SlotVAD) {
			continue
		}
		if !strings.EqualFold(binding.Status, configstore.BindingStatusActive) {
			continue
		}
		if binding.AdapterID == nil {
			continue
		}
		id := strings.TrimSpace(*binding.AdapterID)
		if id == "" {
			continue
		}
		adapterID = id
		if cfg := strings.TrimSpace(binding.Config); cfg != "" {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(cfg), &parsed); err != nil {
				return nil, fmt.Errorf("vad: parse config for %s: %w", binding.Slot, err)
			}
			config = parsed
		}
		break
	}

	if adapterID == "" {
		return nil, ErrAdapterUnavailable
	}

	params.AdapterID = adapterID
	params.Config = config

	switch adapterID {
	case adapters.MockVADAdapterID:
		return newMockAnalyzer(params)
	}

	return nil, ErrAdapterUnavailable
}
