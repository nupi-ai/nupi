package server

import (
	"context"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	adapters "github.com/nupi-ai/nupi/internal/plugins/adapters"
	"github.com/nupi-ai/nupi/internal/session"
)

type runtimeStub struct{}

func (runtimeStub) GRPCPort() int        { return 0 }
func (runtimeStub) ConnectPort() int     { return 0 }
func (runtimeStub) StartTime() time.Time { return time.Unix(0, 0) }

func openTestStore(t *testing.T) *configstore.Store {
	t.Helper()

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if _, err := config.EnsureInstanceDirs(config.DefaultInstance); err != nil {
		t.Fatalf("ensure instance dirs: %v", err)
	}
	if _, err := config.EnsureProfileDirs(config.DefaultInstance, config.DefaultProfile); err != nil {
		t.Fatalf("ensure profile dirs: %v", err)
	}
	store, err := configstore.Open(configstore.Options{
		InstanceName: config.DefaultInstance,
		ProfileName:  config.DefaultProfile,
	})
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newTestAPIServer(t *testing.T) (*APIServer, *session.Manager) {
	t.Helper()

	sm := session.NewManager()
	store := openTestStore(t)
	apiServer, err := NewAPIServer(sm, store, runtimeStub{})
	if err != nil {
		t.Fatalf("failed to create test API server: %v", err)
	}
	return apiServer, sm
}

func newTestAPIServerWithStore(t *testing.T) (*APIServer, *session.Manager, *configstore.Store) {
	t.Helper()

	sm := session.NewManager()
	store := openTestStore(t)
	apiServer, err := NewAPIServer(sm, store, runtimeStub{})
	if err != nil {
		t.Fatalf("failed to create test API server: %v", err)
	}
	return apiServer, sm, store
}

func newTestAdaptersService(t *testing.T, store *configstore.Store) AdaptersController {
	t.Helper()

	bus := eventbus.New()
	mgr := adapters.NewManager(adapters.ManagerOptions{
		Store:    store,
		Adapters: store,
		Bus:      bus,
	})
	return adapters.NewService(mgr, store, bus)
}

func enableVoiceAdapters(t *testing.T, store ConfigStore) {
	t.Helper()

	ctx := context.Background()

	sttAdapter := configstore.Adapter{ID: "test-stt", Source: "test", Type: "stt", Name: "Test STT"}
	if err := store.UpsertAdapter(ctx, sttAdapter); err != nil {
		t.Fatalf("upsert stt adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: sttAdapter.ID,
		Transport: "grpc",
		Address:   "127.0.0.1:0",
	}); err != nil {
		t.Fatalf("upsert stt endpoint: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, "stt", sttAdapter.ID, nil); err != nil {
		t.Fatalf("set active stt adapter: %v", err)
	}

	ttsAdapter := configstore.Adapter{ID: "test-tts", Source: "test", Type: "tts", Name: "Test TTS"}
	if err := store.UpsertAdapter(ctx, ttsAdapter); err != nil {
		t.Fatalf("upsert tts adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: ttsAdapter.ID,
		Transport: "grpc",
		Address:   "127.0.0.1:0",
	}); err != nil {
		t.Fatalf("upsert tts endpoint: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, "tts", ttsAdapter.ID, nil); err != nil {
		t.Fatalf("set active tts adapter: %v", err)
	}

	vadAdapter := configstore.Adapter{ID: "test-vad", Source: "test", Type: "vad", Name: "Test VAD"}
	if err := store.UpsertAdapter(ctx, vadAdapter); err != nil {
		t.Fatalf("upsert vad adapter: %v", err)
	}
	if err := store.UpsertAdapterEndpoint(ctx, configstore.AdapterEndpoint{
		AdapterID: vadAdapter.ID,
		Transport: "grpc",
		Address:   "127.0.0.1:0",
	}); err != nil {
		t.Fatalf("upsert vad endpoint: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, "vad", vadAdapter.ID, nil); err != nil {
		t.Fatalf("set active vad adapter: %v", err)
	}
}

type mockConversationStore struct {
	turns       map[string][]eventbus.ConversationTurn
	globalTurns []eventbus.ConversationTurn
}

func (m *mockConversationStore) Context(sessionID string) []eventbus.ConversationTurn {
	if m.turns == nil {
		return nil
	}
	return m.turns[sessionID]
}

func (m *mockConversationStore) Slice(sessionID string, offset, limit int) (int, []eventbus.ConversationTurn) {
	turns := m.Context(sessionID)
	total := len(turns)
	if offset >= total {
		return total, nil
	}
	end := offset + limit
	if limit <= 0 || end > total {
		end = total
	}
	return total, turns[offset:end]
}

func (m *mockConversationStore) GlobalContext() []eventbus.ConversationTurn {
	return m.globalTurns
}

func (m *mockConversationStore) GlobalSlice(offset, limit int) (int, []eventbus.ConversationTurn) {
	total := len(m.globalTurns)
	if offset >= total {
		return total, nil
	}
	end := offset + limit
	if limit <= 0 || end > total {
		end = total
	}
	return total, m.globalTurns[offset:end]
}
