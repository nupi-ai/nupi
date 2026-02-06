package stt

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nupi-ai/nupi/internal/audio/adapterutil"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	adapters "github.com/nupi-ai/nupi/internal/plugins/adapters"
)

// AdapterRuntimeSource exposes runtime status for adapters.
type AdapterRuntimeSource = adapterutil.AdapterRuntimeSource

type adapterFactory struct {
	store   *configstore.Store
	runtime AdapterRuntimeSource
}

// NewAdapterFactory creates a factory based on adapter bindings stored in config.Store.
func NewAdapterFactory(store *configstore.Store, runtime AdapterRuntimeSource) Factory {
	if store == nil {
		return FactoryFunc(func(context.Context, SessionParams) (Transcriber, error) {
			return nil, ErrFactoryUnavailable
		})
	}
	return adapterFactory{
		store:   store,
		runtime: runtime,
	}
}

func (m adapterFactory) Create(ctx context.Context, params SessionParams) (Transcriber, error) {
	bindings, err := m.store.ListAdapterBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("stt: list adapter bindings: %w", err)
	}

	var (
		activeAdapter string
		activeConfig  map[string]any
	)

	for _, binding := range bindings {
		if binding.Slot != string(adapters.SlotSTT) {
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
			if cfg := strings.TrimSpace(binding.Config); cfg != "" {
				var parsed map[string]any
				if err := json.Unmarshal([]byte(cfg), &parsed); err != nil {
					return nil, fmt.Errorf("stt: parse config for %s: %w", binding.Slot, err)
				}
				activeConfig = parsed
			}
			break
		}
	}

	if activeAdapter == "" {
		return nil, ErrAdapterUnavailable
	}

	params.AdapterID = activeAdapter
	params.Config = activeConfig

	switch activeAdapter {
	case adapters.MockSTTAdapterID:
		return newMockTranscriber(params)
	}

	endpoint, err := m.store.GetAdapterEndpoint(ctx, activeAdapter)
	if err != nil {
		if configstore.IsNotFound(err) {
			return nil, ErrAdapterUnavailable
		}
		return nil, fmt.Errorf("stt: fetch adapter endpoint for %s: %w", activeAdapter, err)
	}

	transport := strings.TrimSpace(endpoint.Transport)
	if transport == "" {
		transport = "process"
	}
	if strings.EqualFold(transport, "process") {
		address := strings.TrimSpace(endpoint.Address)
		if address == "" {
			addr, err := m.lookupRuntimeAddress(ctx, activeAdapter)
			if err != nil {
				return nil, err
			}
			address = addr
		}
		endpoint.Address = address
	}

	return newNAPTranscriber(ctx, params, endpoint)
}

func (m adapterFactory) lookupRuntimeAddress(ctx context.Context, adapterID string) (string, error) {
	return adapterutil.LookupRuntimeAddress(ctx, m.runtime, adapterID, "stt", ErrAdapterUnavailable)
}
