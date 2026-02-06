package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nupi-ai/nupi/internal/audio/adapterutil"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
)

// AdapterRuntimeSource exposes runtime status for adapters.
type AdapterRuntimeSource = adapterutil.AdapterRuntimeSource

type adapterFactory struct {
	store   *configstore.Store
	runtime AdapterRuntimeSource
}

// NewAdapterFactory creates a factory that resolves synthesizers based on adapter bindings.
func NewAdapterFactory(store *configstore.Store, runtime AdapterRuntimeSource) Factory {
	if store == nil {
		return FactoryFunc(func(context.Context, SessionParams) (Synthesizer, error) {
			return nil, ErrFactoryUnavailable
		})
	}
	return adapterFactory{store: store, runtime: runtime}
}

func (m adapterFactory) Create(ctx context.Context, params SessionParams) (Synthesizer, error) {
	bindings, err := m.store.ListAdapterBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("tts: list adapter bindings: %w", err)
	}

	var (
		adapterID string
		config    map[string]any
	)

	for _, binding := range bindings {
		if binding.Slot != string(adapters.SlotTTS) {
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
				return nil, fmt.Errorf("tts: parse config for %s: %w", binding.Slot, err)
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
	case adapters.MockTTSAdapterID:
		return newMockSynthesizer(params)
	}

	endpoint, err := m.store.GetAdapterEndpoint(ctx, adapterID)
	if err != nil {
		if configstore.IsNotFound(err) {
			return nil, ErrAdapterUnavailable
		}
		return nil, fmt.Errorf("tts: fetch adapter endpoint for %s: %w", adapterID, err)
	}

	transport := strings.TrimSpace(endpoint.Transport)
	if transport == "" {
		transport = "process"
	}
	if strings.EqualFold(transport, "process") {
		address := strings.TrimSpace(endpoint.Address)
		if address == "" {
			addr, err := m.lookupRuntimeAddress(ctx, adapterID)
			if err != nil {
				return nil, err
			}
			address = addr
		}
		endpoint.Address = address
	}

	return newNAPSynthesizer(ctx, params, endpoint)
}

func (m adapterFactory) lookupRuntimeAddress(ctx context.Context, adapterID string) (string, error) {
	return adapterutil.LookupRuntimeAddress(ctx, m.runtime, adapterID, "tts", ErrAdapterUnavailable)
}
