package awareness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adhocore/gronx"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

// mockHeartbeatStore implements InsertHeartbeatStore for testing.
type mockHeartbeatStore struct {
	mu         sync.Mutex
	heartbeats []Heartbeat
	lastRunIDs []int64
	insertErr  error
	deleteErr  error
	listErr    error
}

func (m *mockHeartbeatStore) ListHeartbeats(_ context.Context) ([]Heartbeat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	result := make([]Heartbeat, len(m.heartbeats))
	copy(result, m.heartbeats)
	return result, nil
}

func (m *mockHeartbeatStore) UpdateHeartbeatLastRun(_ context.Context, id int64, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastRunIDs = append(m.lastRunIDs, id)
	return nil
}

func (m *mockHeartbeatStore) InsertHeartbeat(_ context.Context, name, cronExpr, prompt string, maxCount int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.insertErr != nil {
		return m.insertErr
	}
	if maxCount > 0 && len(m.heartbeats) >= maxCount {
		return fmt.Errorf("maximum number of heartbeats (%d) reached", maxCount)
	}
	m.heartbeats = append(m.heartbeats, Heartbeat{
		ID:        int64(len(m.heartbeats) + 1),
		Name:      name,
		CronExpr:  cronExpr,
		Prompt:    prompt,
		CreatedAt: time.Now(),
	})
	return nil
}

func (m *mockHeartbeatStore) DeleteHeartbeat(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	for i, h := range m.heartbeats {
		if h.Name == name {
			m.heartbeats = append(m.heartbeats[:i], m.heartbeats[i+1:]...)
			return nil
		}
	}
	return &notFoundErr{name: name}
}

func (m *mockHeartbeatStore) getLastRunIDs() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]int64, len(m.lastRunIDs))
	copy(result, m.lastRunIDs)
	return result
}

type notFoundErr struct{ name string }

func (e *notFoundErr) Error() string  { return "not found: " + e.name }
func (e *notFoundErr) NotFound() bool { return true }

// duplicateErr simulates a store DuplicateError with the Duplicate() marker method.
type duplicateErr struct{ name string }

func (e *duplicateErr) Error() string   { return e.name + " already exists" }
func (e *duplicateErr) Duplicate() bool { return true }

// newGronx returns a shared gronx instance for tests.
func newGronx() *gronx.Gronx { return gronx.New() }

func TestCheckHeartbeatsSkipsNotDue(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	store := &mockHeartbeatStore{
		heartbeats: []Heartbeat{
			{ID: 1, Name: "future", CronExpr: "0 0 31 2 *", Prompt: "never runs"},
		},
	}

	svc := NewService(t.TempDir())
	svc.SetEventBus(bus)
	svc.SetHeartbeatStore(store)

	svc.checkHeartbeats(context.Background(), newGronx())

	// No last_run_at updates means no events were published
	ids := store.getLastRunIDs()
	if len(ids) != 0 {
		t.Fatalf("expected no last_run_at updates, got %v", ids)
	}
}

func TestCheckHeartbeatsHandlesInvalidCron(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	store := &mockHeartbeatStore{
		heartbeats: []Heartbeat{
			{ID: 1, Name: "invalid", CronExpr: "not-a-cron", Prompt: "bad"},
			{ID: 2, Name: "every-min", CronExpr: "* * * * *", Prompt: "valid"},
		},
	}

	svc := NewService(t.TempDir())
	svc.SetEventBus(bus)
	svc.SetHeartbeatStore(store)

	svc.checkHeartbeats(context.Background(), newGronx())

	// Only the valid heartbeat should have last_run_at updated (invalid cron is skipped)
	ids := store.getLastRunIDs()
	if len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("expected last_run_at update for id=2 only, got %v", ids)
	}
}

func TestCheckHeartbeatsEmptyList(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	store := &mockHeartbeatStore{}

	svc := NewService(t.TempDir())
	svc.SetEventBus(bus)
	svc.SetHeartbeatStore(store)

	// Should not panic or error
	svc.checkHeartbeats(context.Background(), newGronx())
}

func TestCheckHeartbeatsRespectsShutdown(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	store := &mockHeartbeatStore{
		heartbeats: []Heartbeat{
			{ID: 1, Name: "always-due", CronExpr: "* * * * *", Prompt: "run me"},
		},
	}

	svc := NewService(t.TempDir())
	svc.SetEventBus(bus)
	svc.SetHeartbeatStore(store)
	svc.shuttingDown.Store(true)

	svc.checkHeartbeats(context.Background(), newGronx())

	// No last_run_at updates means no events were published
	ids := store.getLastRunIDs()
	if len(ids) != 0 {
		t.Fatalf("expected no last_run_at updates when shutting down, got %v", ids)
	}
}

func TestCheckHeartbeatsSkipsAlreadyFiredThisMinute(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	now := time.Now().Truncate(time.Minute)
	store := &mockHeartbeatStore{
		heartbeats: []Heartbeat{
			{ID: 1, Name: "already-ran", CronExpr: "* * * * *", Prompt: "skip me", LastRunAt: &now},
			{ID: 2, Name: "fresh", CronExpr: "* * * * *", Prompt: "run me"},
		},
	}

	svc := NewService(t.TempDir())
	svc.SetEventBus(bus)
	svc.SetHeartbeatStore(store)

	svc.checkHeartbeats(context.Background(), newGronx())

	// Only the fresh heartbeat (ID=2) should fire; already-ran (ID=1) is skipped.
	ids := store.getLastRunIDs()
	if len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("expected last_run_at update for id=2 only, got %v", ids)
	}
}

func TestRunHeartbeatLoopImmediateCheck(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	store := &mockHeartbeatStore{
		heartbeats: []Heartbeat{
			{ID: 1, Name: "due-now", CronExpr: "* * * * *", Prompt: "fire immediately"},
		},
	}

	svc := NewService(t.TempDir())
	svc.SetEventBus(bus)
	svc.SetHeartbeatStore(store)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		svc.runHeartbeatLoop(ctx)
		close(done)
	}()

	// Poll for the immediate check to complete (well under 60s ticker).
	deadline := time.After(5 * time.Second)
	for {
		ids := store.getLastRunIDs()
		if len(ids) > 0 {
			if ids[0] != 1 {
				t.Fatalf("expected last_run_at update for id=1, got %v", ids)
			}
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("timeout waiting for immediate heartbeat check to fire on startup")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	<-done
}

func TestHeartbeatAddToolValidatesCron(t *testing.T) {
	svc := NewService(t.TempDir())
	store := &mockHeartbeatStore{}
	svc.SetHeartbeatStore(store)

	spec := heartbeatAddSpec(svc)
	args := json.RawMessage(`{"name":"test","cron_expr":"invalid","prompt":"hello"}`)
	_, err := spec.Handler(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for invalid cron expression")
	}
}

func TestHeartbeatAddToolSuccess(t *testing.T) {
	svc := NewService(t.TempDir())
	store := &mockHeartbeatStore{}
	svc.SetHeartbeatStore(store)

	spec := heartbeatAddSpec(svc)
	args := json.RawMessage(`{"name":"test","cron_expr":"0 9 * * *","prompt":"hello"}`)
	result, err := spec.Handler(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["status"] != "ok" {
		t.Fatalf("expected status 'ok', got %q", parsed["status"])
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.heartbeats) != 1 {
		t.Fatalf("expected 1 heartbeat in store, got %d", len(store.heartbeats))
	}
	if store.heartbeats[0].Name != "test" {
		t.Fatalf("expected name 'test', got %q", store.heartbeats[0].Name)
	}
}

func TestHeartbeatAddToolDuplicateName(t *testing.T) {
	svc := NewService(t.TempDir())
	store := &mockHeartbeatStore{}
	svc.SetHeartbeatStore(store)

	spec := heartbeatAddSpec(svc)
	args := json.RawMessage(`{"name":"dup","cron_expr":"0 9 * * *","prompt":"first"}`)
	if _, err := spec.Handler(context.Background(), args); err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	store.insertErr = &duplicateErr{name: "dup"}
	store.mu.Unlock()

	_, err := spec.Handler(context.Background(), args)
	if err == nil {
		t.Fatal("expected error on duplicate name")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected user-friendly duplicate error, got: %v", err)
	}
}

func TestHeartbeatRemoveToolSuccess(t *testing.T) {
	svc := NewService(t.TempDir())
	store := &mockHeartbeatStore{
		heartbeats: []Heartbeat{
			{ID: 1, Name: "to-remove", CronExpr: "* * * * *", Prompt: "bye"},
		},
	}
	svc.SetHeartbeatStore(store)

	spec := heartbeatRemoveSpec(svc)
	args := json.RawMessage(`{"name":"to-remove"}`)
	result, err := spec.Handler(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["status"] != "ok" {
		t.Fatalf("expected status 'ok', got %q", parsed["status"])
	}
}

func TestHeartbeatRemoveToolNotFound(t *testing.T) {
	svc := NewService(t.TempDir())
	store := &mockHeartbeatStore{}
	svc.SetHeartbeatStore(store)

	spec := heartbeatRemoveSpec(svc)
	args := json.RawMessage(`{"name":"nonexistent"}`)
	_, err := spec.Handler(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for nonexistent heartbeat")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected user-friendly not-found error, got: %v", err)
	}
	// Should NOT contain redundant "remove heartbeat" wrapper
	if strings.Contains(err.Error(), "remove heartbeat") {
		t.Fatalf("expected clean not-found message without wrapper, got: %v", err)
	}
}

func TestToolSpecsIncludesHeartbeatTools(t *testing.T) {
	svc := NewService(t.TempDir())
	store := &mockHeartbeatStore{}
	svc.SetHeartbeatStore(store)

	specs := svc.ToolSpecs()
	names := make(map[string]bool)
	for _, s := range specs {
		names[s.Name] = true
	}
	if !names["heartbeat_add"] {
		t.Fatal("expected heartbeat_add in ToolSpecs")
	}
	if !names["heartbeat_remove"] {
		t.Fatal("expected heartbeat_remove in ToolSpecs")
	}
	if !names["heartbeat_list"] {
		t.Fatal("expected heartbeat_list in ToolSpecs")
	}
}

func TestHeartbeatAddToolRejectsLongName(t *testing.T) {
	svc := NewService(t.TempDir())
	store := &mockHeartbeatStore{}
	svc.SetHeartbeatStore(store)

	spec := heartbeatAddSpec(svc)
	longName := strings.Repeat("x", 256)
	args := json.RawMessage(`{"name":"` + longName + `","cron_expr":"0 9 * * *","prompt":"hello"}`)
	_, err := spec.Handler(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for name exceeding max length")
	}
	if !strings.Contains(err.Error(), "maximum length") {
		t.Fatalf("expected max length error, got: %v", err)
	}
}

func TestHeartbeatAddToolRejectsLongPrompt(t *testing.T) {
	svc := NewService(t.TempDir())
	store := &mockHeartbeatStore{}
	svc.SetHeartbeatStore(store)

	spec := heartbeatAddSpec(svc)
	longPrompt := strings.Repeat("x", 10001)
	args := json.RawMessage(`{"name":"test","cron_expr":"0 9 * * *","prompt":"` + longPrompt + `"}`)
	_, err := spec.Handler(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for prompt exceeding max length")
	}
	if !strings.Contains(err.Error(), "maximum length") {
		t.Fatalf("expected max length error, got: %v", err)
	}
}

func TestHeartbeatAddToolRejectsLongCronExpr(t *testing.T) {
	svc := NewService(t.TempDir())
	store := &mockHeartbeatStore{}
	svc.SetHeartbeatStore(store)

	spec := heartbeatAddSpec(svc)
	longCron := strings.Repeat("* ", 200)
	args := json.RawMessage(`{"name":"test","cron_expr":"` + longCron + `","prompt":"hello"}`)
	_, err := spec.Handler(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for cron_expr exceeding max length")
	}
	if !strings.Contains(err.Error(), "maximum length") {
		t.Fatalf("expected max length error, got: %v", err)
	}
}

func TestHeartbeatAddToolRejectsControlCharsInName(t *testing.T) {
	svc := NewService(t.TempDir())
	store := &mockHeartbeatStore{}
	svc.SetHeartbeatStore(store)

	spec := heartbeatAddSpec(svc)
	args := json.RawMessage(`{"name":"bad\nname","cron_expr":"0 9 * * *","prompt":"hello"}`)
	_, err := spec.Handler(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for name with control characters")
	}
	if !strings.Contains(err.Error(), "control characters") {
		t.Fatalf("expected control characters error, got: %v", err)
	}
}

func TestHeartbeatAddToolRejectsDoubleQuoteInName(t *testing.T) {
	svc := NewService(t.TempDir())
	store := &mockHeartbeatStore{}
	svc.SetHeartbeatStore(store)

	spec := heartbeatAddSpec(svc)
	args := json.RawMessage(`{"name":"has\"quote","cron_expr":"0 9 * * *","prompt":"hello"}`)
	_, err := spec.Handler(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for name with double quotes")
	}
	if !strings.Contains(err.Error(), "double quotes") {
		t.Fatalf("expected double quotes error, got: %v", err)
	}
}

func TestCheckHeartbeatsNilGronx(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	store := &mockHeartbeatStore{
		heartbeats: []Heartbeat{
			{ID: 1, Name: "test", CronExpr: "* * * * *", Prompt: "run"},
		},
	}

	svc := NewService(t.TempDir())
	svc.SetEventBus(bus)
	svc.SetHeartbeatStore(store)

	// Should not panic with nil gronx
	svc.checkHeartbeats(context.Background(), nil)

	// No heartbeats should have fired
	ids := store.getLastRunIDs()
	if len(ids) != 0 {
		t.Fatalf("expected no last_run_at updates with nil gronx, got %v", ids)
	}
}

func TestToolSpecsExcludesHeartbeatToolsWhenNoStore(t *testing.T) {
	svc := NewService(t.TempDir())

	specs := svc.ToolSpecs()
	names := make(map[string]bool)
	for _, s := range specs {
		if s.Name == "heartbeat_add" || s.Name == "heartbeat_remove" || s.Name == "heartbeat_list" {
			t.Fatalf("expected no heartbeat tool when store is nil, found %q", s.Name)
		}
		names[s.Name] = true
	}
	// Verify core tools are still present when heartbeat store is nil.
	for _, expected := range []string{"memory_search", "memory_get", "memory_write", "core_memory_update", "onboarding_complete"} {
		if !names[expected] {
			t.Fatalf("expected %q in ToolSpecs when heartbeat store is nil, got %v", expected, names)
		}
	}
}

func TestCheckHeartbeatsHandlesListError(t *testing.T) {
	bus := eventbus.New()
	defer bus.Shutdown()

	store := &mockHeartbeatStore{
		listErr: fmt.Errorf("database is locked"),
	}

	svc := NewService(t.TempDir())
	svc.SetEventBus(bus)
	svc.SetHeartbeatStore(store)

	// Should not panic; should log error and return.
	svc.checkHeartbeats(context.Background(), newGronx())

	ids := store.getLastRunIDs()
	if len(ids) != 0 {
		t.Fatalf("expected no last_run_at updates on list error, got %v", ids)
	}
}

func TestHeartbeatListToolSuccess(t *testing.T) {
	svc := NewService(t.TempDir())
	now := time.Now()
	store := &mockHeartbeatStore{
		heartbeats: []Heartbeat{
			{ID: 1, Name: "daily", CronExpr: "0 9 * * *", Prompt: "summarize", LastRunAt: &now},
			{ID: 2, Name: "hourly", CronExpr: "0 * * * *", Prompt: "check"},
		},
	}
	svc.SetHeartbeatStore(store)

	spec := heartbeatListSpec(svc)
	result, err := spec.Handler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatal(err)
	}
	count, ok := parsed["count"].(float64)
	if !ok || int(count) != 2 {
		t.Fatalf("expected count=2, got %v", parsed["count"])
	}
	heartbeats, ok := parsed["heartbeats"].([]interface{})
	if !ok || len(heartbeats) != 2 {
		t.Fatalf("expected 2 heartbeats, got %v", parsed["heartbeats"])
	}
}

func TestHeartbeatListToolEmpty(t *testing.T) {
	svc := NewService(t.TempDir())
	store := &mockHeartbeatStore{}
	svc.SetHeartbeatStore(store)

	spec := heartbeatListSpec(svc)
	result, err := spec.Handler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatal(err)
	}
	count, ok := parsed["count"].(float64)
	if !ok || int(count) != 0 {
		t.Fatalf("expected count=0, got %v", parsed["count"])
	}
}

func TestHeartbeatAddToolRejectsWhenMaxCountReached(t *testing.T) {
	svc := NewService(t.TempDir())
	store := &mockHeartbeatStore{}
	svc.SetHeartbeatStore(store)

	// Pre-fill store to max capacity.
	for i := 0; i < constants.MaxHeartbeatCount; i++ {
		store.heartbeats = append(store.heartbeats, Heartbeat{
			ID:       int64(i + 1),
			Name:     fmt.Sprintf("hb-%d", i),
			CronExpr: "* * * * *",
			Prompt:   "test",
		})
	}

	spec := heartbeatAddSpec(svc)
	args := json.RawMessage(`{"name":"one-too-many","cron_expr":"0 9 * * *","prompt":"hello"}`)
	_, err := spec.Handler(context.Background(), args)
	if err == nil {
		t.Fatal("expected error when max heartbeat count reached")
	}
	if !strings.Contains(err.Error(), "maximum number") {
		t.Fatalf("expected max count error, got: %v", err)
	}
}
