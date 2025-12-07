package adapters

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
)

// LaunchRecord captures metadata about a launched mock adapter process.
type LaunchRecord struct {
	Binary     string
	Args       []string
	Env        []string
	Slot       string
	Adapter    string
	LaunchedAt time.Time
	StdoutNil  bool
	StderrNil  bool
}

// MockLauncher implements ProcessLauncher for tests, recording launches without spawning processes.
type MockLauncher struct {
	mu      sync.Mutex
	records []LaunchRecord
	stops   map[string]int
	err     error
	nextPID int
}

// NewMockLauncher constructs a launcher stub optionally preconfigured with an error.
func NewMockLauncher() *MockLauncher {
	return &MockLauncher{
		nextPID: 1000,
		stops:   make(map[string]int),
	}
}

// SetError forces subsequent Launch calls to fail with the provided error.
func (m *MockLauncher) SetError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// Launch records adapter metadata and returns a controllable handle.
func (m *MockLauncher) Launch(ctx context.Context, binary string, args []string, env []string, stdout io.Writer, stderr io.Writer) (ProcessHandle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}

	record := LaunchRecord{
		Binary:     binary,
		Args:       append([]string(nil), args...),
		Env:        append([]string(nil), env...),
		Slot:       slotFromEnv(env),
		Adapter:    adapterFromEnv(env),
		LaunchedAt: time.Now().UTC(),
		StdoutNil:  stdout == nil,
		StderrNil:  stderr == nil,
	}
	m.records = append(m.records, record)

	handle := &mockHandle{
		parent: m,
		slot:   record.Slot,
		pid:    m.nextPID,
	}
	m.nextPID++
	return handle, nil
}

// Records returns a copy of launch records for assertions.
func (m *MockLauncher) Records() []LaunchRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]LaunchRecord, len(m.records))
	copy(out, m.records)
	return out
}

// StopCount returns how many times Stop was invoked for the slot.
func (m *MockLauncher) StopCount(slot string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stops[slot]
}

// Reset clears recorded launches and stop counters.
func (m *MockLauncher) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = nil
	m.stops = make(map[string]int)
}

type mockHandle struct {
	parent *MockLauncher
	slot   string
	pid    int
}

func (h *mockHandle) Stop(context.Context) error {
	if h.slot != "" {
		h.parent.mu.Lock()
		h.parent.stops[h.slot]++
		h.parent.mu.Unlock()
	}
	return nil
}

func (h *mockHandle) PID() int {
	return h.pid
}

func slotFromEnv(env []string) string {
	for _, e := range env {
		if strings.HasPrefix(e, "NUPI_ADAPTER_SLOT=") {
			return strings.TrimPrefix(e, "NUPI_ADAPTER_SLOT=")
		}
	}
	return ""
}

func adapterFromEnv(env []string) string {
	for _, e := range env {
		if strings.HasPrefix(e, "NUPI_ADAPTER_ID=") {
			return strings.TrimPrefix(e, "NUPI_ADAPTER_ID=")
		}
	}
	return ""
}

// RegisterMockAdapter inserts an adapter record and binds it to the provided slot for tests.
func RegisterMockAdapter(ctx context.Context, store *configstore.Store, slot string, adapterID string, cfg map[string]any) error {
	if store == nil {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	adapter := configstore.Adapter{
		ID:        adapterID,
		Source:    "test",
		Type:      baseTypeFromSlot(slot),
		Name:      adapterID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		return err
	}
	return store.SetActiveAdapter(ctx, slot, adapterID, cfg)
}

func baseTypeFromSlot(slot string) string {
	base := slot
	if idx := strings.IndexRune(base, '.'); idx > 0 {
		base = base[:idx]
	}
	if base == "" {
		return "test"
	}
	return base
}
