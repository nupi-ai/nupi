package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
)

// AdapterRuntimeSource exposes runtime status for adapters.
type AdapterRuntimeSource interface {
	Overview(ctx context.Context) ([]adapters.BindingStatus, error)
}

type adapterFactory struct {
	store   *configstore.Store
	runtime AdapterRuntimeSource
}

const runtimeLookupTimeout = time.Second

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
	if m.runtime == nil {
		return "", fmt.Errorf("tts: process adapter %s missing runtime metadata", adapterID)
	}
	ctxLookup, cancel := context.WithTimeout(ctx, runtimeLookupTimeout)
	defer cancel()

	statuses, err := m.runtime.Overview(ctxLookup)
	if err != nil {
		return "", fmt.Errorf("tts: fetch adapter runtime for %s (%v): %w", adapterID, err, ErrAdapterUnavailable)
	}

	var (
		match    bool
		multiple bool
	)
	for _, status := range statuses {
		if status.AdapterID == nil || strings.TrimSpace(*status.AdapterID) != adapterID {
			continue
		}
		if match {
			multiple = true
		}
		match = true
		if status.Runtime == nil || len(status.Runtime.Extra) == 0 {
			continue
		}
		if addr := strings.TrimSpace(status.Runtime.Extra[adapters.RuntimeExtraAddress]); addr != "" {
			return addr, nil
		}
	}
	if multiple {
		return "", fmt.Errorf("tts: process adapter %s has duplicate runtime entries", adapterID)
	}
	if match {
		return "", fmt.Errorf("tts: process adapter %s awaiting runtime address: %w", adapterID, ErrAdapterUnavailable)
	}
	return "", fmt.Errorf("tts: process adapter %s not running: %w", adapterID, ErrAdapterUnavailable)
}
