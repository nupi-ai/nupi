package modules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nupi-ai/nupi/internal/adapterrunner"
	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/voice/slots"
)

// Slot represents a logical module slot configured in adapter_bindings.
type Slot string

const (
	SlotAI           Slot = "ai.primary"
	SlotSTTPrimary   Slot = slots.STTPrimary
	SlotSTTSecondary Slot = slots.STTSecondary
	SlotTTS          Slot = slots.TTSPrimary
	SlotVAD          Slot = "vad.primary"
	SlotTunnel       Slot = "tunnel.primary"
)

var defaultSlots = []Slot{SlotAI, SlotSTTPrimary, SlotSTTSecondary, SlotTTS, SlotVAD, SlotTunnel}

// Binding describes a configured adapter bound to a slot.
type Binding struct {
	Slot        Slot
	AdapterID   string
	Config      map[string]any
	RawConfig   string
	Fingerprint string
}

// BindingSource exposes adapter binding metadata.
type BindingSource interface {
	ListAdapterBindings(ctx context.Context) ([]configstore.AdapterBinding, error)
}

// AdapterDetailSource exposes adapter metadata and endpoints.
type AdapterDetailSource interface {
	GetAdapter(ctx context.Context, adapterID string) (configstore.Adapter, error)
	GetModuleEndpoint(ctx context.Context, adapterID string) (configstore.ModuleEndpoint, error)
}

// ProcessLauncher abstracts process creation for adapter-runner.
type ProcessLauncher interface {
	Launch(ctx context.Context, binary string, args []string, env []string, stdout io.Writer, stderr io.Writer) (ProcessHandle, error)
}

// ProcessHandle represents a running adapter-runner process.
type ProcessHandle interface {
	Stop(ctx context.Context) error
	PID() int
}

// ManagerOptions configures the modules manager.
type ManagerOptions struct {
	Store     BindingSource
	Runner    *adapterrunner.Manager
	Launcher  ProcessLauncher
	Slots     []Slot
	Adapters  AdapterDetailSource
	PluginDir string
	Bus       *eventbus.Bus
}

// Manager orchestrates adapter modules for the daemon.
type Manager struct {
	store         BindingSource
	adapterSource AdapterDetailSource
	runner        *adapterrunner.Manager
	launcher      ProcessLauncher
	pluginDir     string
	bus           *eventbus.Bus

	slots []Slot

	mu      sync.Mutex
	modules map[Slot]*moduleInstance
}

type moduleInstance struct {
	binding     Binding
	handle      ProcessHandle
	stdout      *moduleLogWriter
	stderr      *moduleLogWriter
	fingerprint string
}

type bindingPlan struct {
	binding     Binding
	adapter     configstore.Adapter
	manifest    *ModuleManifest
	endpoint    configstore.ModuleEndpoint
	fingerprint string
}

var (
	// ErrBindingSourceNotConfigured indicates the manager was created without a store.
	ErrBindingSourceNotConfigured = errors.New("modules: binding source not configured")
	// ErrRunnerManagerNotConfigured indicates adapter-runner manager is missing.
	ErrRunnerManagerNotConfigured = errors.New("modules: adapter-runner manager not configured")
	errAdapterDetailsUnavailable  = errors.New("modules: adapter details unavailable")
)

// NewManager constructs a new modules manager with the supplied dependencies.
func NewManager(opts ManagerOptions) *Manager {
	manager := &Manager{
		store:         opts.Store,
		adapterSource: opts.Adapters,
		runner:        opts.Runner,
		launcher:      opts.Launcher,
		pluginDir:     strings.TrimSpace(opts.PluginDir),
		bus:           opts.Bus,
		slots:         opts.Slots,
		modules:       make(map[Slot]*moduleInstance),
	}
	if len(manager.slots) == 0 {
		manager.slots = defaultSlots
	}
	if manager.launcher == nil {
		manager.launcher = execLauncher{}
	}
	if manager.adapterSource == nil {
		if src, ok := opts.Store.(AdapterDetailSource); ok {
			manager.adapterSource = src
		}
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

	plans := make(map[Slot]bindingPlan, len(active))
	var planErrs []error
	for slot, binding := range active {
		plan, err := m.prepareBinding(ctx, binding)
		if err != nil {
			planErrs = append(planErrs, fmt.Errorf("modules: prepare %s: %w", slot, err))
			continue
		}
		plans[slot] = plan
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	if len(planErrs) > 0 {
		errs = append(errs, planErrs...)
	}

	for slot, inst := range m.modules {
		plan, ok := plans[slot]
		shouldStop := !ok
		if !shouldStop {
			shouldStop = plan.binding.AdapterID == "" ||
				plan.binding.AdapterID != inst.binding.AdapterID ||
				plan.fingerprint != inst.fingerprint
		}
		if shouldStop {
			if err := inst.handle.Stop(ctx); err != nil {
				errs = append(errs, fmt.Errorf("modules: stop %s: %w", slot, err))
			}
			if inst.stdout != nil {
				inst.stdout.Close()
			}
			if inst.stderr != nil {
				inst.stderr.Close()
			}
			delete(m.modules, slot)
			continue
		}
		inst.binding = plan.binding
		inst.fingerprint = plan.fingerprint
	}

	for slot, plan := range plans {
		if plan.binding.AdapterID == "" {
			continue
		}
		if existing, exists := m.modules[slot]; exists {
			if existing.fingerprint == plan.fingerprint && existing.binding.AdapterID == plan.binding.AdapterID {
				continue
			}
			continue
		}
		inst, err := m.startModule(ctx, plan)
		if err != nil {
			errs = append(errs, fmt.Errorf("modules: start %s: %w", slot, err))
			continue
		}
		m.modules[slot] = inst
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
	if inst.stdout != nil {
		inst.stdout.Close()
	}
	if inst.stderr != nil {
		inst.stderr.Close()
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
		binding := inst.binding
		binding.Fingerprint = inst.fingerprint
		out = append(out, binding)
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

func (m *Manager) startModule(ctx context.Context, plan bindingPlan) (*moduleInstance, error) {
	binding := plan.binding
	if m.runner == nil {
		return nil, ErrRunnerManagerNotConfigured
	}

	binary := strings.TrimSpace(m.runner.BinaryPath())
	args := []string{"--slot", string(binding.Slot), "--adapter", binding.AdapterID}

	env := append(os.Environ(),
		"NUPI_MODULE_SLOT="+string(binding.Slot),
		"NUPI_MODULE_ADAPTER="+binding.AdapterID,
	)

	var (
		moduleHome     string
		dataDir        string
		cleanupHome    bool
		cleanupDataDir bool
	)
	defer func() {
		if cleanupDataDir && dataDir != "" {
			_ = os.RemoveAll(dataDir)
		}
		if cleanupHome && moduleHome != "" {
			_ = os.RemoveAll(moduleHome)
		}
	}()

	if len(binding.Config) > 0 {
		payload, err := json.Marshal(binding.Config)
		if err != nil {
			return nil, fmt.Errorf("modules: marshal config for %s: %w", binding.Slot, err)
		}
		env = append(env, "NUPI_MODULE_CONFIG="+string(payload))
	}

	adapter := plan.adapter
	manifest := plan.manifest
	endpoint := plan.endpoint
	manifestRaw := strings.TrimSpace(adapter.Manifest)

	pluginRoot := m.pluginRoot()
	if pluginRoot != "" {
		slug := sanitizeModuleSlug(manifest, binding.AdapterID)
		moduleHome = filepath.Join(pluginRoot, slug)
		createdHome, err := ensureDir(moduleHome, 0o755)
		if err != nil {
			return nil, fmt.Errorf("modules: ensure module home for %s: %w", binding.AdapterID, err)
		}
		cleanupHome = createdHome
		env = append(env, "NUPI_MODULE_HOME="+moduleHome)
		if manifestRaw != "" {
			manifestPath := filepath.Join(moduleHome, "module.yaml")
			if err := os.WriteFile(manifestPath, []byte(manifestRaw), 0o644); err != nil {
				return nil, fmt.Errorf("modules: write manifest for %s: %w", binding.AdapterID, err)
			}
			env = append(env, "NUPI_MODULE_MANIFEST_PATH="+manifestPath)
		}
	}

	if moduleHome != "" {
		dataDir = filepath.Join(moduleHome, "data")
		createdDataDir, err := ensureDir(dataDir, 0o755)
		if err != nil {
			return nil, fmt.Errorf("modules: ensure data dir for %s: %w", binding.AdapterID, err)
		}
		if createdDataDir && !cleanupHome {
			cleanupDataDir = true
		}
		env = append(env, "NUPI_MODULE_DATA_DIR="+dataDir)
		if manifest != nil {
			cacheEnv := strings.TrimSpace(manifest.Spec.Assets.Models.CacheDirEnv)
			if cacheEnv != "" {
				env = append(env, fmt.Sprintf("%s=%s", cacheEnv, dataDir))
			}
		}
	}

	if endpoint.Transport != "" {
		env = append(env, "NUPI_MODULE_TRANSPORT="+endpoint.Transport)
	}
	if endpoint.Address != "" {
		env = append(env, "NUPI_MODULE_ENDPOINT="+endpoint.Address)
		listenEnv := "NUPI_ADAPTER_LISTEN_ADDR"
		if manifest != nil {
			if v := strings.TrimSpace(manifest.Spec.Entrypoint.ListenEnv); v != "" {
				listenEnv = v
			}
		}
		env = append(env, fmt.Sprintf("%s=%s", listenEnv, endpoint.Address))
	}
	if endpoint.Command != "" {
		env = append(env, "NUPI_MODULE_COMMAND="+endpoint.Command)
	}
	if len(endpoint.Args) > 0 {
		payload, err := json.Marshal(endpoint.Args)
		if err != nil {
			return nil, fmt.Errorf("modules: marshal endpoint args for %s: %w", binding.AdapterID, err)
		}
		env = append(env, "NUPI_MODULE_ARGS="+string(payload))
	}
	if len(endpoint.Env) > 0 {
		for k, v := range endpoint.Env {
			key := strings.TrimSpace(k)
			if key == "" {
				continue
			}
			env = append(env, fmt.Sprintf("%s=%s", key, v))
		}
	}

	stdoutWriter := io.Writer(io.Discard)
	stderrWriter := io.Writer(io.Discard)
	var stdoutLogger, stderrLogger *moduleLogWriter

	// Default telemetry to enabled when manifest metadata is missing for easier debugging.
	telemetryStdout := manifest == nil || manifest.Spec.Telemetry.Stdout == nil || *manifest.Spec.Telemetry.Stdout
	telemetryStderr := manifest == nil || manifest.Spec.Telemetry.Stderr == nil || *manifest.Spec.Telemetry.Stderr
	if m.bus != nil {
		if telemetryStdout {
			stdoutLogger = newModuleLogWriter(m.bus, binding.AdapterID, binding.Slot, eventbus.LogLevelInfo)
			stdoutWriter = stdoutLogger
		}
		if telemetryStderr {
			stderrLogger = newModuleLogWriter(m.bus, binding.AdapterID, binding.Slot, eventbus.LogLevelError)
			stderrWriter = stderrLogger
		}
	}

	handle, err := m.launcher.Launch(ctx, binary, args, env, stdoutWriter, stderrWriter)
	if err != nil {
		if stdoutLogger != nil {
			stdoutLogger.Close()
		}
		if stderrLogger != nil {
			stderrLogger.Close()
		}
		return nil, err
	}

	cleanupDataDir = false
	cleanupHome = false

	binding.Fingerprint = plan.fingerprint

	return &moduleInstance{
		binding:     binding,
		handle:      handle,
		stdout:      stdoutLogger,
		stderr:      stderrLogger,
		fingerprint: plan.fingerprint,
	}, nil
}

func (m *Manager) lookupAdapter(ctx context.Context, adapterID string) (configstore.Adapter, *ModuleManifest, error) {
	if m.adapterSource == nil {
		return configstore.Adapter{}, nil, errAdapterDetailsUnavailable
	}
	adapter, err := m.adapterSource.GetAdapter(ctx, adapterID)
	if err != nil {
		if configstore.IsNotFound(err) {
			return configstore.Adapter{}, nil, nil
		}
		return configstore.Adapter{}, nil, fmt.Errorf("modules: get adapter %s: %w", adapterID, err)
	}

	if strings.TrimSpace(adapter.Manifest) == "" {
		return adapter, nil, nil
	}
	manifest, err := ParseModuleManifest(adapter.Manifest)
	if err != nil {
		return adapter, nil, fmt.Errorf("modules: parse manifest for %s: %w", adapterID, err)
	}
	return adapter, manifest, nil
}

func (m *Manager) lookupEndpoint(ctx context.Context, adapterID string) (configstore.ModuleEndpoint, error) {
	if m.adapterSource == nil {
		return configstore.ModuleEndpoint{}, errAdapterDetailsUnavailable
	}
	endpoint, err := m.adapterSource.GetModuleEndpoint(ctx, adapterID)
	if err != nil {
		if configstore.IsNotFound(err) {
			return configstore.ModuleEndpoint{}, nil
		}
		return configstore.ModuleEndpoint{}, fmt.Errorf("modules: get module endpoint %s: %w", adapterID, err)
	}
	return endpoint, nil
}

func (m *Manager) pluginRoot() string {
	if strings.TrimSpace(m.pluginDir) != "" {
		return m.pluginDir
	}
	return filepath.Join(config.GetNupiHome(), "plugins")
}

func ensureDir(path string, perm os.FileMode) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("modules: path %s exists but is not a directory", path)
		}
		return false, nil
	}
	if !os.IsNotExist(err) {
		return false, fmt.Errorf("modules: stat %s: %w", path, err)
	}
	if err := os.MkdirAll(path, perm); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) prepareBinding(ctx context.Context, binding Binding) (bindingPlan, error) {
	plan := bindingPlan{
		binding: binding,
	}
	if binding.AdapterID == "" {
		return plan, nil
	}

	if m.adapterSource != nil {
		adapter, manifest, err := m.lookupAdapter(ctx, binding.AdapterID)
		if errors.Is(err, errAdapterDetailsUnavailable) {
			err = nil
		}
		if err != nil {
			return bindingPlan{}, err
		}
		plan.adapter = adapter
		plan.manifest = manifest

		endpoint, err := m.lookupEndpoint(ctx, binding.AdapterID)
		if errors.Is(err, errAdapterDetailsUnavailable) {
			err = nil
		}
		if err != nil {
			return bindingPlan{}, err
		}
		plan.endpoint = endpoint
	}

	plan.fingerprint = computePlanFingerprint(binding, plan.adapter.Manifest, plan.endpoint)
	plan.binding.Fingerprint = plan.fingerprint
	return plan, nil
}

func computePlanFingerprint(binding Binding, manifest string, endpoint configstore.ModuleEndpoint) string {
	h := sha256.New()
	write := func(value string) {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}

	write(binding.AdapterID)
	write(strings.TrimSpace(binding.RawConfig))
	write(strings.TrimSpace(manifest))
	write(endpoint.Transport)
	write(endpoint.Address)
	write(endpoint.Command)
	for _, arg := range endpoint.Args {
		write(arg)
	}
	if len(endpoint.Env) > 0 {
		keys := make([]string, 0, len(endpoint.Env))
		for k := range endpoint.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			write(k)
			write(endpoint.Env[k])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sanitizeModuleSlug(manifest *ModuleManifest, fallback string) string {
	if manifest != nil {
		if slug := strings.TrimSpace(manifest.Metadata.Slug); slug != "" {
			if sanitized := sanitizeIdentifier(slug); sanitized != "" {
				return sanitized
			}
		}
		if name := strings.TrimSpace(manifest.Metadata.Name); name != "" {
			if sanitized := sanitizeIdentifier(name); sanitized != "" {
				return sanitized
			}
		}
	}
	if fallback = strings.TrimSpace(fallback); fallback != "" {
		if sanitized := sanitizeIdentifier(fallback); sanitized != "" {
			return sanitized
		}
	}
	return "module"
}

func sanitizeIdentifier(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.' || r == '/':
			b.WriteRune('-')
		}
	}
	res := strings.Trim(b.String(), "-_")
	if res == "" {
		return "module"
	}
	if len(res) > 64 {
		return res[:64]
	}
	return res
}
