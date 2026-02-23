package adapterutil

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/nupi-ai/nupi/internal/audio/streammanager"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
)

// SessionParams is an alias for streammanager.SessionParams used by all
// adapter factories.
type SessionParams = streammanager.SessionParams

// FactoryConfig parametrises a generic adapter factory.
type FactoryConfig[T any] struct {
	Prefix                string // log/error prefix, e.g. "stt", "tts", "vad"
	Slot                  adapters.Slot
	MockAdapterID         string
	NewMock               func(SessionParams) (T, error)
	NewNAP                func(context.Context, SessionParams, configstore.AdapterEndpoint) (T, error)
	ErrAdapterUnavailable error
	ErrFactoryUnavailable error
}

// Factory resolves adapter bindings from the config store and delegates to
// the appropriate constructor. It is parametrised over the adapter product
// type (Transcriber, Synthesizer, Analyzer).
type Factory[T any] struct {
	store   *configstore.Store
	runtime AdapterRuntimeSource
	cfg     FactoryConfig[T]
}

// NewFactory creates a generic adapter factory. If store is nil the returned
// factory always returns ErrFactoryUnavailable. NewMock and NewNAP must not
// be nil; the function panics if either is missing to catch misconfiguration
// early.
func NewFactory[T any](store *configstore.Store, runtime AdapterRuntimeSource, cfg FactoryConfig[T]) *Factory[T] {
	if cfg.Prefix == "" {
		panic("adapterutil.NewFactory: Prefix must not be empty")
	}
	if cfg.Slot == "" {
		panic(fmt.Sprintf("adapterutil.NewFactory: %s: Slot must not be empty", cfg.Prefix))
	}
	if cfg.NewMock == nil {
		panic(fmt.Sprintf("adapterutil.NewFactory: %s: NewMock must not be nil", cfg.Prefix))
	}
	if cfg.NewNAP == nil {
		panic(fmt.Sprintf("adapterutil.NewFactory: %s: NewNAP must not be nil", cfg.Prefix))
	}
	if cfg.ErrAdapterUnavailable == nil {
		panic(fmt.Sprintf("adapterutil.NewFactory: %s: ErrAdapterUnavailable must not be nil", cfg.Prefix))
	}
	if cfg.ErrFactoryUnavailable == nil {
		panic(fmt.Sprintf("adapterutil.NewFactory: %s: ErrFactoryUnavailable must not be nil", cfg.Prefix))
	}
	return &Factory[T]{store: store, runtime: runtime, cfg: cfg}
}

// Create resolves the active binding for the configured slot and constructs
// the adapter instance.
func (f *Factory[T]) Create(ctx context.Context, params SessionParams) (T, error) {
	var zero T

	if f.store == nil {
		return zero, f.cfg.ErrFactoryUnavailable
	}

	bindings, err := f.store.ListAdapterBindings(ctx)
	if err != nil {
		return zero, fmt.Errorf("%s: list adapter bindings: %w", f.cfg.Prefix, err)
	}

	var (
		adapterID string
		config    map[string]any
	)

	for _, binding := range bindings {
		if binding.Slot != string(f.cfg.Slot) {
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
		parsed, err := configstore.DecodeJSON[map[string]any](sql.NullString{Valid: true, String: binding.Config})
		if err != nil {
			return zero, fmt.Errorf("%s: parse config for %s: %w", f.cfg.Prefix, binding.Slot, err)
		}
		config = parsed
		break
	}

	if adapterID == "" {
		return zero, f.cfg.ErrAdapterUnavailable
	}

	params.AdapterID = adapterID
	params.Config = config

	if adapterID == f.cfg.MockAdapterID {
		return f.cfg.NewMock(params)
	}

	endpoint, err := f.store.GetAdapterEndpoint(ctx, adapterID)
	if err != nil {
		if configstore.IsNotFound(err) {
			return zero, f.cfg.ErrAdapterUnavailable
		}
		return zero, fmt.Errorf("%s: fetch adapter endpoint for %s: %w", f.cfg.Prefix, adapterID, err)
	}

	transport := strings.TrimSpace(endpoint.Transport)
	if transport == "" {
		transport = "process"
	}
	if strings.EqualFold(transport, "process") {
		address := strings.TrimSpace(endpoint.Address)
		if address == "" {
			addr, err := LookupRuntimeAddress(ctx, f.runtime, adapterID, f.cfg.Prefix, f.cfg.ErrAdapterUnavailable)
			if err != nil {
				return zero, err
			}
			address = addr
		}
		endpoint.Address = address
	}

	return f.cfg.NewNAP(ctx, params, endpoint)
}
