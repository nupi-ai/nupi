package adapterutil

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
)

// testProduct is a trivial product type used to exercise Factory[T].
type testProduct struct {
	Name    string
	Address string
}

var (
	errUnavailable = errors.New("test: adapter unavailable")
	errFactory     = errors.New("test: factory unavailable")
)

func testConfig() FactoryConfig[testProduct] {
	return FactoryConfig[testProduct]{
		Prefix:        "test",
		Slot:          adapters.SlotSTT,
		MockAdapterID: adapters.MockSTTAdapterID,
		NewMock: func(p SessionParams) (testProduct, error) {
			return testProduct{Name: "mock", Address: p.AdapterID}, nil
		},
		NewNAP: func(_ context.Context, p SessionParams, ep configstore.AdapterEndpoint) (testProduct, error) {
			return testProduct{Name: "nap", Address: ep.Address}, nil
		},
		ErrAdapterUnavailable: errUnavailable,
		ErrFactoryUnavailable: errFactory,
	}
}

func openTestStore(t *testing.T) *configstore.Store {
	t.Helper()
	store, err := configstore.Open(configstore.Options{
		DBPath: filepath.Join(t.TempDir(), "config.db"),
	})
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// registerAndBind registers an adapter in the store and binds it to the STT slot.
func registerAndBind(t *testing.T, store *configstore.Store, adapterID string) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertAdapter(ctx, configstore.Adapter{
		ID: adapterID, Source: "builtin", Type: "stt", Name: adapterID,
	}); err != nil {
		t.Fatalf("upsert adapter %s: %v", adapterID, err)
	}
	if err := store.SetActiveAdapter(ctx, string(adapters.SlotSTT), adapterID, nil); err != nil {
		t.Fatalf("set active adapter %s: %v", adapterID, err)
	}
}

func TestFactoryNilStoreReturnsFactoryUnavailable(t *testing.T) {
	f := NewFactory[testProduct](nil, nil, testConfig())
	_, err := f.Create(context.Background(), SessionParams{})
	if !errors.Is(err, errFactory) {
		t.Fatalf("expected ErrFactoryUnavailable, got %v", err)
	}
}

func TestFactoryNoActiveBindingReturnsAdapterUnavailable(t *testing.T) {
	store := openTestStore(t)
	f := NewFactory(store, nil, testConfig())
	_, err := f.Create(context.Background(), SessionParams{})
	if !errors.Is(err, errUnavailable) {
		t.Fatalf("expected ErrAdapterUnavailable, got %v", err)
	}
}

func TestFactoryMockDispatch(t *testing.T) {
	store := openTestStore(t)
	registerAndBind(t, store, adapters.MockSTTAdapterID)

	f := NewFactory(store, nil, testConfig())
	prod, err := f.Create(context.Background(), SessionParams{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if prod.Name != "mock" || prod.Address != adapters.MockSTTAdapterID {
		t.Fatalf("expected mock product with AdapterID=%q, got %+v", adapters.MockSTTAdapterID, prod)
	}
}

func TestFactoryNAPDispatch(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	adapterID := "adapter.stt.test-nap"
	registerAndBind(t, store, adapterID)
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapterID,
		Transport: "grpc",
		Address:   "localhost:9090",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	f := NewFactory(store, nil, testConfig())
	prod, err := f.Create(ctx, SessionParams{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if prod.Name != "nap" || prod.Address != "localhost:9090" {
		t.Fatalf("expected nap product at localhost:9090, got %+v", prod)
	}
}

func TestFactoryInvalidConfigJSON(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	adapterID := "adapter.stt.bad-json"
	registerAndBind(t, store, adapterID)
	// Overwrite config with invalid JSON via raw SQL.
	db := store.DB()
	if _, err := db.ExecContext(ctx, `UPDATE adapter_bindings SET config = '{bad' WHERE slot = ?`, string(adapters.SlotSTT)); err != nil {
		t.Fatalf("raw update: %v", err)
	}

	f := NewFactory(store, nil, testConfig())
	_, err := f.Create(ctx, SessionParams{})
	if err == nil {
		t.Fatal("expected error for invalid config JSON")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("expected parse config error, got: %v", err)
	}
}

func TestNewFactoryPanicsOnNilNewMock(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil NewMock")
		}
	}()
	cfg := testConfig()
	cfg.NewMock = nil
	NewFactory[testProduct](nil, nil, cfg)
}

func TestNewFactoryPanicsOnNilNewNAP(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil NewNAP")
		}
	}()
	cfg := testConfig()
	cfg.NewNAP = nil
	NewFactory[testProduct](nil, nil, cfg)
}

func TestNewFactoryPanicsOnNilErrAdapterUnavailable(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil ErrAdapterUnavailable")
		}
	}()
	cfg := testConfig()
	cfg.ErrAdapterUnavailable = nil
	NewFactory[testProduct](nil, nil, cfg)
}

func TestNewFactoryPanicsOnNilErrFactoryUnavailable(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil ErrFactoryUnavailable")
		}
	}()
	cfg := testConfig()
	cfg.ErrFactoryUnavailable = nil
	NewFactory[testProduct](nil, nil, cfg)
}

func TestNewFactoryPanicsOnEmptyPrefix(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for empty Prefix")
		}
	}()
	cfg := testConfig()
	cfg.Prefix = ""
	NewFactory[testProduct](nil, nil, cfg)
}

func TestNewFactoryPanicsOnEmptySlot(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for empty Slot")
		}
	}()
	cfg := testConfig()
	cfg.Slot = ""
	NewFactory[testProduct](nil, nil, cfg)
}

func TestFactoryEndpointNotFoundReturnsAdapterUnavailable(t *testing.T) {
	store := openTestStore(t)
	adapterID := "adapter.stt.no-endpoint"
	registerAndBind(t, store, adapterID)
	// Do NOT register an endpoint for this adapter.

	f := NewFactory(store, nil, testConfig())
	_, err := f.Create(context.Background(), SessionParams{})
	if !errors.Is(err, errUnavailable) {
		t.Fatalf("expected ErrAdapterUnavailable for missing endpoint, got %v", err)
	}
}

// testRuntimeSource provides a fake runtime source for process-transport tests.
type testRuntimeSource struct {
	statuses []adapters.BindingStatus
	err      error
}

func (t testRuntimeSource) Overview(_ context.Context) ([]adapters.BindingStatus, error) {
	return t.statuses, t.err
}

func TestFactoryProcessTransportUsesRuntimeLookup(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	adapterID := "adapter.stt.process-test"
	registerAndBind(t, store, adapterID)
	// Endpoint with process transport and no address — triggers runtime lookup.
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapterID,
		Transport: "process",
		Address:   "",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	runtime := testRuntimeSource{
		statuses: []adapters.BindingStatus{
			{
				AdapterID: &adapterID,
				Runtime: &adapters.RuntimeStatus{
					Extra: map[string]string{
						adapters.RuntimeExtraAddress: "127.0.0.1:50051",
					},
				},
			},
		},
	}

	f := NewFactory(store, runtime, testConfig())
	prod, err := f.Create(ctx, SessionParams{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if prod.Name != "nap" || prod.Address != "127.0.0.1:50051" {
		t.Fatalf("expected nap product at 127.0.0.1:50051, got %+v", prod)
	}
}

func TestFactoryEmptyTransportDefaultsToProcess(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	adapterID := "adapter.stt.default-transport"
	registerAndBind(t, store, adapterID)
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: adapterID,
		Transport: "", // empty — should default to "process"
		Address:   "",
	}); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}

	runtime := testRuntimeSource{
		statuses: []adapters.BindingStatus{
			{
				AdapterID: &adapterID,
				Runtime: &adapters.RuntimeStatus{
					Extra: map[string]string{
						adapters.RuntimeExtraAddress: "127.0.0.1:50052",
					},
				},
			},
		},
	}

	f := NewFactory(store, runtime, testConfig())
	prod, err := f.Create(ctx, SessionParams{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if prod.Name != "nap" || prod.Address != "127.0.0.1:50052" {
		t.Fatalf("expected nap product at 127.0.0.1:50052, got %+v", prod)
	}
}
