package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/nupi-ai/nupi/internal/adapterrunner"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
)

// Slot represents a logical module slot configured in adapter_bindings.
type Slot string

const (
	SlotAI           Slot = "ai.primary"
	SlotSTTPrimary   Slot = "stt.primary"
	SlotSTTSecondary Slot = "stt.secondary"
	SlotTTS          Slot = "tts.primary"
	SlotVAD          Slot = "vad.primary"
	SlotTunnel       Slot = "tunnel.primary"
)

var defaultSlots = []Slot{SlotAI, SlotSTTPrimary, SlotSTTSecondary, SlotTTS, SlotVAD, SlotTunnel}

// Binding describes a configured adapter bound to a slot.
type Binding struct {
	Slot      Slot
	AdapterID string
	Config    map[string]any
	RawConfig string
}

// BindingSource exposes adapter binding metadata.
type BindingSource interface {
	ListAdapterBindings(ctx context.Context) ([]configstore.AdapterBinding, error)
}

// ProcessLauncher abstracts process creation for adapter-runner.
type ProcessLauncher interface {
	Launch(ctx context.Context, binary string, args []string, env []string) (ProcessHandle, error)
}

// ProcessHandle represents a running adapter-runner process.
type ProcessHandle interface {
	Stop(ctx context.Context) error
	PID() int
}

// ManagerOptions configures the modules manager.
type ManagerOptions struct {
	Store    BindingSource
	Runner   *adapterrunner.Manager
	Launcher ProcessLauncher
	Slots    []Slot
}

// Manager orchestrates adapter modules for the daemon.
type Manager struct {
	store    BindingSource
	runner   *adapterrunner.Manager
	launcher ProcessLauncher

	slots []Slot

	mu      sync.Mutex
	modules map[Slot]*moduleInstance
}

type moduleInstance struct {
	binding Binding
	handle  ProcessHandle
}

var (
	// ErrBindingSourceNotConfigured indicates the manager was created without a store.
	ErrBindingSourceNotConfigured = errors.New("modules: binding source not configured")
	// ErrRunnerManagerNotConfigured indicates adapter-runner manager is missing.
	ErrRunnerManagerNotConfigured = errors.New("modules: adapter-runner manager not configured")
)

// NewManager constructs a new modules manager with the supplied dependencies.
func NewManager(opts ManagerOptions) *Manager {
	manager := &Manager{
		store:    opts.Store,
		runner:   opts.Runner,
		launcher: opts.Launcher,
		slots:    opts.Slots,
		modules:  make(map[Slot]*moduleInstance),
	}
	if len(manager.slots) == 0 {
		manager.slots = defaultSlots
	}
	if manager.launcher == nil {
		manager.launcher = execLauncher{}
	}
	return manager
}

// Ensure reconciles running modules with the current adapter bindings.
func (m *Manager) Ensure(ctx context.Context) error {
	if m.store == nil {
		return ErrBindingSourceNotConfigured
	}
	active, err := m.loadActiveBindings(ctx)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error

	for slot, inst := range m.modules {
		desired, ok := active[slot]
		shouldStop := !ok
		if !shouldStop {
			shouldStop = desired.AdapterID == "" || desired.AdapterID != inst.binding.AdapterID || !configEquivalent(desired, inst.binding)
		}
		if shouldStop {
			if err := inst.handle.Stop(ctx); err != nil {
				errs = append(errs, fmt.Errorf("modules: stop %s: %w", slot, err))
			}
			delete(m.modules, slot)
		}
	}

	for slot, binding := range active {
		if binding.AdapterID == "" {
			continue
		}
		if existing, exists := m.modules[slot]; exists {
			if configEquivalent(binding, existing.binding) {
				continue
			}
			if err := existing.handle.Stop(ctx); err != nil {
				errs = append(errs, fmt.Errorf("modules: stop %s: %w", slot, err))
			}
			delete(m.modules, slot)
		}
		handle, err := m.startModule(ctx, binding)
		if err != nil {
			errs = append(errs, fmt.Errorf("modules: start %s: %w", slot, err))
			continue
		}
		m.modules[slot] = &moduleInstance{
			binding: binding,
			handle:  handle,
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// StopAll terminates every running module process managed by the manager.
func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	for slot := range m.modules {
		if err := m.stopSlotLocked(ctx, slot); err != nil {
			errs = append(errs, fmt.Errorf("modules: stop %s: %w", slot, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// StopSlot terminates a running module for the specified slot without altering bindings.
func (m *Manager) StopSlot(ctx context.Context, slot Slot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopSlotLocked(ctx, slot)
}

func (m *Manager) stopSlotLocked(ctx context.Context, slot Slot) error {
	inst, ok := m.modules[slot]
	if !ok {
		return nil
	}
	if err := inst.handle.Stop(ctx); err != nil {
		return err
	}
	delete(m.modules, slot)
	return nil
}

// Running returns a snapshot of bindings for currently running modules.
func (m *Manager) Running() []Binding {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Binding, 0, len(m.modules))
	for _, inst := range m.modules {
		out = append(out, inst.binding)
	}
	return out
}

func (m *Manager) loadActiveBindings(ctx context.Context) (map[Slot]Binding, error) {
	records, err := m.store.ListAdapterBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("modules: list adapter bindings: %w", err)
	}
	slotFilter := make(map[string]struct{}, len(m.slots))
	for _, slot := range m.slots {
		slotFilter[string(slot)] = struct{}{}
	}

	active := make(map[Slot]Binding)
	for _, record := range records {
		if _, tracked := slotFilter[record.Slot]; !tracked {
			continue
		}
		if !strings.EqualFold(record.Status, "active") {
			continue
		}
		if record.AdapterID == nil || strings.TrimSpace(*record.AdapterID) == "" {
			continue
		}
		binding := Binding{
			Slot:      Slot(record.Slot),
			AdapterID: strings.TrimSpace(*record.AdapterID),
			RawConfig: record.Config,
		}
		if record.Config != "" {
			var cfg map[string]any
			if err := json.Unmarshal([]byte(record.Config), &cfg); err != nil {
				return nil, fmt.Errorf("modules: decode config for %s: %w", record.Slot, err)
			}
			binding.Config = cfg
		}
		active[binding.Slot] = binding
	}
	return active, nil
}

func (m *Manager) startModule(ctx context.Context, binding Binding) (ProcessHandle, error) {
	if m.runner == nil {
		return nil, ErrRunnerManagerNotConfigured
	}
	binary := strings.TrimSpace(m.runner.BinaryPath())
	args := []string{
		"--slot", string(binding.Slot),
		"--adapter", binding.AdapterID,
	}

	env := []string{
		"NUPI_MODULE_SLOT=" + string(binding.Slot),
		"NUPI_MODULE_ADAPTER=" + binding.AdapterID,
	}

	if len(binding.Config) > 0 {
		payload, err := json.Marshal(binding.Config)
		if err != nil {
			return nil, fmt.Errorf("modules: marshal config for %s: %w", binding.Slot, err)
		}
		env = append(env, "NUPI_MODULE_CONFIG="+string(payload))
	}

	handle, err := m.launcher.Launch(ctx, binary, args, env)
	if err != nil {
		return nil, err
	}
	return handle, nil
}

func configEquivalent(a, b Binding) bool {
	if strings.TrimSpace(a.RawConfig) == strings.TrimSpace(b.RawConfig) {
		return true
	}
	if len(a.Config) == 0 && len(b.Config) == 0 {
		return true
	}
	return reflect.DeepEqual(a.Config, b.Config)
}
