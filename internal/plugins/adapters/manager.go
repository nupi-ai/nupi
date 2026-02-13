package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/jsrunner"
	"github.com/nupi-ai/nupi/internal/plugins/manifest"
	"github.com/nupi-ai/nupi/internal/voice/slots"
)

// Slot represents a logical adapter slot configured in adapter_bindings.
type Slot string

const (
	SlotAI     Slot = "ai"
	SlotSTT    Slot = slots.STT
	SlotTTS    Slot = slots.TTS
	SlotVAD    Slot = slots.VAD
	SlotTunnel Slot = "tunnel"
)

const processReadyTimeout = 30 * time.Second
const adapterStartTimeout = 2 * time.Minute
const processLaunchMaxAttempts = 3
const processLaunchRetryDelay = 50 * time.Millisecond

var defaultSlots = []Slot{SlotAI, SlotSTT, SlotTTS, SlotVAD, SlotTunnel}

// Binding describes a configured adapter bound to a slot.
type Binding struct {
	Slot        Slot
	AdapterID   string
	Config      map[string]any
	RawConfig   string
	Fingerprint string
	Runtime     map[string]string
}

// BindingSource exposes adapter binding metadata.
type BindingSource interface {
	ListAdapterBindings(ctx context.Context) ([]configstore.AdapterBinding, error)
}

// AdapterDetailSource exposes adapter metadata and endpoints.
type AdapterDetailSource interface {
	GetAdapter(ctx context.Context, adapterID string) (configstore.Adapter, error)
	GetAdapterEndpoint(ctx context.Context, adapterID string) (configstore.AdapterEndpoint, error)
}

// ProcessLauncher abstracts process creation for adapters.
// The binary parameter must be an absolute path; relative paths are not
// resolved against workingDir (os.Stat and exec run in the caller's cwd).
type ProcessLauncher interface {
	Launch(ctx context.Context, binary string, args []string, env []string, stdout io.Writer, stderr io.Writer, workingDir string) (ProcessHandle, error)
}

// ProcessHandle represents a running adapter process.
type ProcessHandle interface {
	Stop(ctx context.Context) error
	PID() int
}

// ManagerOptions configures the adapters manager.
type ManagerOptions struct {
	Store     BindingSource
	Launcher  ProcessLauncher
	Slots     []Slot
	Adapters  AdapterDetailSource
	PluginDir string
	Bus       *eventbus.Bus
}

// Manager orchestrates adapter plugins for the daemon.
type Manager struct {
	store         BindingSource
	adapterSource AdapterDetailSource
	launcher      ProcessLauncher
	pluginDir     string
	bus           *eventbus.Bus

	slots []Slot

	mu        sync.Mutex
	instances map[Slot]*adapterInstance
}

type adapterInstance struct {
	binding     Binding
	handle      ProcessHandle
	stdout          *adapterLogWriter
	stderr          *adapterLogWriter
	fingerprint     string
	shutdownTimeout time.Duration
}

type bindingPlan struct {
	binding     Binding
	adapter     configstore.Adapter
	manifest    *manifest.Manifest
	endpoint    configstore.AdapterEndpoint
	fingerprint string
}

const adapterReadyTimeoutEnv = "NUPI_ADAPTER_READY_TIMEOUT"

var allocateProcessAddressFn = allocateProcessAddress
var waitForAdapterReadyFn = waitForAdapterReady
var readinessCheckerMu sync.Mutex

// getReadinessChecker safely retrieves the current readiness checker function.
func getReadinessChecker() func(ctx context.Context, addr string) error {
	readinessCheckerMu.Lock()
	defer readinessCheckerMu.Unlock()
	return waitForAdapterReadyFn
}

var (
	// ErrBindingSourceNotConfigured indicates the manager was created without a store.
	ErrBindingSourceNotConfigured = errors.New("adapters: binding source not configured")
	errAdapterDetailsUnavailable  = errors.New("adapters: adapter details unavailable")
)

// NewManager constructs a new adapters manager with the supplied dependencies.
func NewManager(opts ManagerOptions) *Manager {
	manager := &Manager{
		store:         opts.Store,
		adapterSource: opts.Adapters,
		launcher:      opts.Launcher,
		pluginDir:     strings.TrimSpace(opts.PluginDir),
		bus:           opts.Bus,
		slots:         opts.Slots,
		instances:     make(map[Slot]*adapterInstance),
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

// Ensure reconciles running adapters with the current bindings.
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
			planErrs = append(planErrs, fmt.Errorf("adapters: prepare %s: %w", slot, err))
			continue
		}
		plans[slot] = plan
	}
	if len(planErrs) > 0 && len(plans) == 0 {
		return errors.Join(planErrs...)
	}
	if len(planErrs) > 0 {
		for _, err := range planErrs {
			if err != nil {
				logError(m.bus, err)
			}
		}
	}

	var ensureErrs []error

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.instances == nil {
		m.instances = make(map[Slot]*adapterInstance)
	}

	for slot, plan := range plans {
		current, running := m.instances[slot]
		if !running {
			instance, err := m.startAdapter(ctx, plan)
			if err != nil {
				wrapped := fmt.Errorf("adapters: start %s: %w", slot, err)
				logError(m.bus, wrapped)
				ensureErrs = append(ensureErrs, wrapped)
				continue
			}
			m.instances[slot] = instance
			continue
		}

		if current.fingerprint == plan.fingerprint {
			continue
		}

		if err := m.stopAdapter(ctx, slot, current); err != nil {
			wrapped := fmt.Errorf("adapters: stop %s: %w", slot, err)
			logError(m.bus, wrapped)
			ensureErrs = append(ensureErrs, wrapped)
		}
		instance, err := m.startAdapter(ctx, plan)
		if err != nil {
			wrapped := fmt.Errorf("adapters: restart %s: %w", slot, err)
			logError(m.bus, wrapped)
			ensureErrs = append(ensureErrs, wrapped)
			continue
		}
		m.instances[slot] = instance
	}

	for slot, instance := range m.instances {
		if _, keep := plans[slot]; keep {
			continue
		}
		if err := m.stopAdapter(ctx, slot, instance); err != nil {
			wrapped := fmt.Errorf("adapters: stop %s: %w", slot, err)
			logError(m.bus, wrapped)
			ensureErrs = append(ensureErrs, wrapped)
		}
		delete(m.instances, slot)
	}

	if len(planErrs) > 0 {
		ensureErrs = append(ensureErrs, planErrs...)
	}
	if len(ensureErrs) > 0 {
		return errors.Join(ensureErrs...)
	}
	return nil
}

// Stop gracefully stops all running adapter instances.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	for slot, instance := range m.instances {
		if err := m.stopAdapter(ctx, slot, instance); err != nil {
			errs = append(errs, fmt.Errorf("adapters: stop %s: %w", slot, err))
		}
		delete(m.instances, slot)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (m *Manager) loadActiveBindings(ctx context.Context) (map[Slot]Binding, error) {
	if m.store == nil {
		return nil, ErrBindingSourceNotConfigured
	}

	records, err := m.store.ListAdapterBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("adapters: list adapter bindings: %w", err)
	}

	slotFilter := make(map[string]struct{}, len(m.slots))
	for _, slot := range m.slots {
		slotFilter[string(slot)] = struct{}{}
	}

	active := make(map[Slot]Binding)
	for _, record := range records {
		if _, tracked := slotFilter[record.Slot]; len(slotFilter) > 0 && !tracked {
			continue
		}
		if !strings.EqualFold(record.Status, configstore.BindingStatusActive) {
			continue
		}
		if record.AdapterID == nil {
			continue
		}
		id := strings.TrimSpace(*record.AdapterID)
		if id == "" {
			continue
		}

		slot := Slot(record.Slot)
		binding := Binding{
			Slot:      slot,
			AdapterID: id,
			RawConfig: record.Config,
		}
		if strings.TrimSpace(record.Config) != "" {
			var cfg map[string]any
			if err := json.Unmarshal([]byte(record.Config), &cfg); err != nil {
				return nil, fmt.Errorf("adapters: decode config for %s: %w", record.Slot, err)
			}
			binding.Config = cfg
		}
		active[slot] = binding
	}
	return active, nil
}

// Running returns a snapshot of adapters currently managed by the manager.
func (m *Manager) Running() []Binding {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Binding, 0, len(m.instances))
	for _, inst := range m.instances {
		binding := inst.binding
		binding.Fingerprint = inst.fingerprint
		if len(binding.Runtime) > 0 {
			binding.Runtime = cloneStringMap(binding.Runtime)
		}
		out = append(out, binding)
	}
	return out
}

// StopSlot stops the adapter instance assigned to the given slot.
func (m *Manager) StopSlot(ctx context.Context, slot Slot) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.instances[slot]
	if !ok {
		return nil
	}
	if err := m.stopAdapter(ctx, slot, inst); err != nil {
		return err
	}
	delete(m.instances, slot)
	return nil
}

// StopAll stops all running adapter instances.
func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	for slot, inst := range m.instances {
		if err := m.stopAdapter(ctx, slot, inst); err != nil {
			errs = append(errs, fmt.Errorf("adapters: stop %s: %w", slot, err))
		}
		delete(m.instances, slot)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (m *Manager) startAdapter(ctx context.Context, plan bindingPlan) (*adapterInstance, error) {
	// Handle builtin mock adapters without launching a process.
	// These adapters are implemented in-process.
	if IsBuiltinMockAdapter(plan.binding.AdapterID) {
		log.Printf("[Adapters] configuring builtin mock adapter: %s", plan.binding.AdapterID)
		plan.binding.Runtime = map[string]string{
			RuntimeExtraTransport: "builtin",
		}
		plan.fingerprint = computePlanFingerprint(plan.binding, plan.manifest, plan.adapter.Manifest, plan.endpoint)
		plan.binding.Fingerprint = plan.fingerprint
		return &adapterInstance{
			binding:     plan.binding,
			fingerprint: plan.fingerprint,
			// No handle - builtin mock runs in-process
		}, nil
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, adapterStartTimeout)
		defer cancel()
	}
	checkCtx := func() error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("adapters: start %s context: %w", plan.binding.AdapterID, err)
		}
		return nil
	}

	adapterID := strings.TrimSpace(plan.adapter.ID)
	if adapterID == "" {
		adapterID = strings.TrimSpace(plan.binding.AdapterID)
	}
	if adapterID == "" {
		return nil, fmt.Errorf("adapter ID missing in plan")
	}

	homeEnv := os.Environ()
	env := append([]string(nil), homeEnv...)
	if err := checkCtx(); err != nil {
		return nil, err
	}

	env = append(env,
		"NUPI_ADAPTER_SLOT="+string(plan.binding.Slot),
		"NUPI_ADAPTER_ID="+plan.binding.AdapterID,
	)

	var (
		adapterHome string
		dataDir     string
		cleanups    []func()
		binary      string
		args        []string
		err         error
	)
	defer func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}()

	if len(plan.binding.Config) > 0 {
		payload, err := json.Marshal(plan.binding.Config)
		if err != nil {
			return nil, fmt.Errorf("adapters: marshal config for %s: %w", plan.binding.Slot, err)
		}
		env = append(env, "NUPI_ADAPTER_CONFIG="+string(payload))
	}

	adapter := plan.adapter
	manifest := plan.manifest
	endpoint := plan.endpoint
	manifestRaw := strings.TrimSpace(adapter.Manifest)

	transport := strings.TrimSpace(endpoint.Transport)
	if transport == "" {
		transport = "process"
	}
	endpoint.Transport = transport

	runtimeExtra := map[string]string{
		RuntimeExtraTransport: transport,
	}

	readyTimeout := processReadyTimeout
	shutdownTimeout := DefaultGracefulShutdownTimeout
	if manifest != nil && manifest.Adapter != nil {
		if v := strings.TrimSpace(manifest.Adapter.Entrypoint.ReadyTimeout); v != "" {
			if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
				readyTimeout = parsed
			} else {
				log.Printf("[Adapters] invalid ready timeout %q for %s: %v", v, plan.binding.AdapterID, err)
			}
		}
		if v := strings.TrimSpace(manifest.Adapter.Entrypoint.ShutdownTimeout); v != "" {
			if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
				shutdownTimeout = parsed
			} else {
				log.Printf("[Adapters] invalid shutdown timeout %q for %s: %v", v, plan.binding.AdapterID, err)
			}
		}
	}
	if endpoint.Env != nil {
		if v := strings.TrimSpace(endpoint.Env[adapterReadyTimeoutEnv]); v != "" {
			if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
				readyTimeout = parsed
			} else {
				log.Printf("[Adapters] invalid ready timeout env %q for %s: %v", v, plan.binding.AdapterID, err)
			}
		}
	}

	if err := checkCtx(); err != nil {
		return nil, err
	}

	// For non-process transports (grpc, http), the adapter is already running
	// at a remote address. Short-circuit before creating any directories.
	if transport != "process" {
		addr := strings.TrimSpace(endpoint.Address)
		if addr == "" {
			return nil, fmt.Errorf("adapters: %s transport requires address for %s", transport, plan.binding.AdapterID)
		}

		// Verify remote adapter is reachable before marking as ready
		readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
		readyErr := getReadinessChecker()(readyCtx, addr)
		cancel()
		if readyErr != nil {
			return nil, fmt.Errorf("adapters: remote adapter %s at %s not reachable: %w", plan.binding.AdapterID, addr, readyErr)
		}

		plan.binding.Runtime = map[string]string{
			RuntimeExtraTransport: transport,
			RuntimeExtraAddress:   addr,
		}
		if endpoint.TLSCertPath != "" {
			plan.binding.Runtime[RuntimeExtraTLSCertPath] = endpoint.TLSCertPath
		}
		if endpoint.TLSKeyPath != "" {
			plan.binding.Runtime[RuntimeExtraTLSKeyPath] = endpoint.TLSKeyPath
		}
		if endpoint.TLSCACertPath != "" {
			plan.binding.Runtime[RuntimeExtraTLSCACertPath] = endpoint.TLSCACertPath
		}
		if endpoint.TLSInsecure {
			plan.binding.Runtime[RuntimeExtraTLSInsecure] = "true"
		}
		plan.fingerprint = computePlanFingerprint(plan.binding, plan.manifest, plan.adapter.Manifest, plan.endpoint)
		plan.binding.Fingerprint = plan.fingerprint

		cleanups = nil // Don't clean up adapter home on success

		return &adapterInstance{
			binding:     plan.binding,
			fingerprint: plan.fingerprint,
			// No handle - remote adapter, not managed by us
		}, nil
	}

	// Process transport - we launch the adapter locally
	// Prepare adapter home and data directories
	pluginRoot := m.pluginRoot()
	if pluginRoot != "" {
		slug := sanitizeAdapterSlug(manifest, plan.binding.AdapterID)
		adapterHome = filepath.Join(pluginRoot, slug)
		createdHome, err := ensureDir(adapterHome, 0o755)
		if err != nil {
			return nil, fmt.Errorf("adapters: ensure adapter home for %s: %w", plan.binding.AdapterID, err)
		}
		if createdHome {
			cleanups = append(cleanups, func() {
				_ = os.RemoveAll(adapterHome)
			})
		}
		if err := checkCtx(); err != nil {
			return nil, err
		}
		env = append(env, "NUPI_ADAPTER_HOME="+adapterHome)
		if manifestRaw != "" {
			manifestPath := filepath.Join(adapterHome, "plugin.yaml")
			if err := os.WriteFile(manifestPath, []byte(manifestRaw), 0o644); err != nil {
				return nil, fmt.Errorf("adapters: write manifest for %s: %w", plan.binding.AdapterID, err)
			}
			if err := checkCtx(); err != nil {
				return nil, err
			}
			env = append(env, "NUPI_ADAPTER_MANIFEST_PATH="+manifestPath)
		}
	}

	if adapterHome != "" {
		dataDir = filepath.Join(adapterHome, "data")
		createdDataDir, err := ensureDir(dataDir, 0o755)
		if err != nil {
			return nil, fmt.Errorf("adapters: ensure data dir for %s: %w", plan.binding.AdapterID, err)
		}
		if err := checkCtx(); err != nil {
			return nil, err
		}
		if createdDataDir {
			cleanups = append(cleanups, func() {
				_ = os.RemoveAll(dataDir)
			})
		}
		env = append(env, "NUPI_ADAPTER_DATA_DIR="+dataDir)
		if manifest != nil && manifest.Adapter != nil {
			cacheEnv := strings.TrimSpace(manifest.Adapter.Assets.Models.CacheDirEnv)
			if cacheEnv != "" {
				env = append(env, fmt.Sprintf("%s=%s", cacheEnv, dataDir))
			}
		}
	}

	baseEnv := append([]string(nil), env...)

	// Resolve the adapter command and args based on runtime type (binary vs js)
	binary, args, err = m.resolveAdapterCommand(plan, adapterHome)
	if err != nil {
		return nil, err
	}

	if len(endpoint.Args) > 0 {
		for _, arg := range endpoint.Args {
			if strings.ContainsAny(arg, "\x00") {
				return nil, fmt.Errorf("adapters: invalid character in args for %s", plan.binding.AdapterID)
			}
		}
		payload, err := json.Marshal(endpoint.Args)
		if err != nil {
			return nil, fmt.Errorf("adapters: marshal endpoint args for %s: %w", plan.binding.AdapterID, err)
		}
		baseEnv = append(baseEnv, "NUPI_ADAPTER_ARGS="+string(payload))
	}
	if len(endpoint.Env) > 0 {
		for k, v := range endpoint.Env {
			key := strings.TrimSpace(k)
			if key == "" {
				continue
			}
			if strings.ContainsAny(key, "=\n\r\x00") {
				return nil, fmt.Errorf("adapters: invalid character in env key %q for %s", key, plan.binding.AdapterID)
			}
			if strings.ContainsAny(v, "\n\r\x00") {
				return nil, fmt.Errorf("adapters: invalid character in env value for key %s in %s", key, plan.binding.AdapterID)
			}
			baseEnv = append(baseEnv, fmt.Sprintf("%s=%s", key, v))
		}
	}

	// Resolve working directory for adapter process.
	// Priority: explicit workingDir from manifest > plugin root directory > inherit daemon cwd.
	var workingDir string
	if manifest != nil && manifest.Adapter != nil {
		if wd := strings.TrimSpace(manifest.Adapter.Entrypoint.WorkingDir); wd != "" {
			if !filepath.IsAbs(wd) && manifest.Dir != "" {
				wd = filepath.Join(manifest.Dir, wd)
			}
			workingDir = wd
		} else if manifest.Dir != "" {
			workingDir = manifest.Dir
		}
	}

	appendAddressEnv := func(envIn []string, addr string) []string {
		out := append([]string(nil), envIn...)
		if endpoint.Transport != "" {
			out = append(out, "NUPI_ADAPTER_TRANSPORT="+endpoint.Transport)
		}
		if endpoint.Command != "" {
			out = append(out, "NUPI_ADAPTER_COMMAND="+endpoint.Command)
		}
		// Pass runtime type to adapter (js adapters use bundled Bun runtime)
		if manifest != nil && manifest.Adapter != nil {
			runtime := strings.TrimSpace(manifest.Adapter.Entrypoint.Runtime)
			if runtime == "" {
				runtime = "binary" // default
			}
			out = append(out, "NUPI_ADAPTER_RUNTIME="+runtime)
		}
		listenEnv := "NUPI_ADAPTER_LISTEN_ADDR"
		if manifest != nil && manifest.Adapter != nil {
			if v := strings.TrimSpace(manifest.Adapter.Entrypoint.ListenEnv); v != "" {
				listenEnv = v
			}
		}
		if strings.TrimSpace(addr) != "" {
			out = append(out, "NUPI_ADAPTER_ENDPOINT="+addr)
			out = append(out, fmt.Sprintf("%s=%s", listenEnv, addr))
		}
		return out
	}

	launchAttempt := func(addr string) (*adapterInstance, error) {
		if err := checkCtx(); err != nil {
			return nil, err
		}
		runtimeExtra[RuntimeExtraAddress] = strings.TrimSpace(addr)
		envAttempt := appendAddressEnv(baseEnv, addr)

		stdoutWriter := io.Writer(io.Discard)
		stderrWriter := io.Writer(io.Discard)
		var stdoutLogger, stderrLogger *adapterLogWriter

		telemetryStdout := manifest == nil || manifest.Adapter == nil || manifest.Adapter.Telemetry.Stdout == nil || *manifest.Adapter.Telemetry.Stdout
		telemetryStderr := manifest == nil || manifest.Adapter == nil || manifest.Adapter.Telemetry.Stderr == nil || *manifest.Adapter.Telemetry.Stderr
		if m.bus != nil {
			if telemetryStdout {
				stdoutLogger = newAdapterLogWriter(m.bus, plan.binding.AdapterID, plan.binding.Slot, eventbus.LogLevelInfo)
				stdoutWriter = stdoutLogger
			}
			if telemetryStderr {
				stderrLogger = newAdapterLogWriter(m.bus, plan.binding.AdapterID, plan.binding.Slot, eventbus.LogLevelError)
				stderrWriter = stderrLogger
			}
		}

		handle, err := m.launcher.Launch(ctx, binary, args, envAttempt, stdoutWriter, stderrWriter, workingDir)
		if err != nil {
			if stdoutLogger != nil {
				stdoutLogger.Close()
			}
			if stderrLogger != nil {
				stderrLogger.Close()
			}
			return nil, err
		}

		stopHandle := func() {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
			_ = handle.Stop(stopCtx)
			stopCancel()
		}

		if transport == "process" {
			readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
			readyErr := getReadinessChecker()(readyCtx, addr)
			cancel()
			if readyErr != nil {
				if stdoutLogger != nil {
					stdoutLogger.Close()
				}
				if stderrLogger != nil {
					stderrLogger.Close()
				}
				stopHandle()
				return nil, fmt.Errorf("adapters: process adapter %s readiness: %w", plan.binding.AdapterID, readyErr)
			}
		}

		if len(runtimeExtra) > 0 {
			plan.binding.Runtime = cloneStringMap(runtimeExtra)
		}
		plan.endpoint = endpoint

		cleanups = nil

		plan.fingerprint = computePlanFingerprint(plan.binding, plan.manifest, plan.adapter.Manifest, plan.endpoint)
		plan.binding.Fingerprint = plan.fingerprint

		return &adapterInstance{
			binding:         plan.binding,
			handle:          handle,
			stdout:          stdoutLogger,
			stderr:          stderrLogger,
			fingerprint:     plan.fingerprint,
			shutdownTimeout: shutdownTimeout,
		}, nil
	}

	var lastErr error
	for attempt := 0; attempt < processLaunchMaxAttempts; attempt++ {
		if err := checkCtx(); err != nil {
			return nil, err
		}
		addr, err := allocateProcessAddressFn()
		if err != nil {
			return nil, fmt.Errorf("adapters: allocate process address: %w", err)
		}
		endpoint.Address = addr
		inst, err := launchAttempt(addr)
		if err == nil {
			return inst, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("adapters: process adapter %s readiness after %d attempts: %w", plan.binding.AdapterID, processLaunchMaxAttempts, lastErr)
}

func (m *Manager) stopAdapter(ctx context.Context, slot Slot, instance *adapterInstance) error {
	if instance == nil {
		return nil
	}
	if instance.stdout != nil {
		instance.stdout.Close()
	}
	if instance.stderr != nil {
		instance.stderr.Close()
	}
	if instance.handle == nil {
		return nil
	}

	// Use instance-specific shutdown timeout if available and handle supports it
	var err error
	if instance.shutdownTimeout > 0 {
		if h, ok := instance.handle.(*execHandle); ok {
			err = h.StopWithTimeout(ctx, instance.shutdownTimeout)
		} else {
			err = instance.handle.Stop(ctx)
		}
	} else {
		err = instance.handle.Stop(ctx)
	}
	if err != nil {
		// ErrAdapterKilled means we intentionally killed the adapter after graceful timeout
		// This is not an error for deliberate Stop operations - the adapter was stopped as requested
		if errors.Is(err, ErrAdapterKilled) {
			log.Printf("[Adapters] adapter %s did not exit gracefully, was killed after timeout", slot)
			return nil
		}
		return fmt.Errorf("stop adapter %s: %w", slot, err)
	}
	return nil
}

func ensureDir(path string, perm os.FileMode) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("adapters: path %s exists but is not a directory", path)
		}
		return false, nil
	}
	if !os.IsNotExist(err) {
		return false, fmt.Errorf("adapters: stat %s: %w", path, err)
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
		if manifest != nil && manifest.Adapter != nil {
			resolved, err := resolveAdapterConfig(manifest.Adapter.Options, plan.binding.Config)
			if err != nil {
				return bindingPlan{}, fmt.Errorf("adapters: invalid config for %s: %w", binding.AdapterID, err)
			}
			plan.binding.Config = resolved
		}

		endpoint, err := m.lookupEndpoint(ctx, binding.AdapterID)
		if errors.Is(err, errAdapterDetailsUnavailable) {
			err = nil
		}
		if err != nil {
			return bindingPlan{}, err
		}
		mergedEndpoint, err := mergeManifestEndpoint(manifest, binding.AdapterID, endpoint)
		if err != nil {
			return bindingPlan{}, err
		}
		plan.endpoint = mergedEndpoint
	}

	plan.fingerprint = computePlanFingerprint(plan.binding, plan.manifest, plan.adapter.Manifest, plan.endpoint)
	plan.binding.Fingerprint = plan.fingerprint
	return plan, nil
}

func resolveAdapterConfig(options map[string]manifest.AdapterOption, current map[string]any) (map[string]any, error) {
	// Validate config against manifest options before processing
	if err := manifest.ValidateConfigAgainstOptions(options, current); err != nil {
		return nil, err
	}

	if len(options) == 0 {
		if len(current) == 0 {
			return nil, nil
		}
		out := make(map[string]any, len(current))
		for key, value := range current {
			trimmed := strings.TrimSpace(key)
			if trimmed == "" {
				continue
			}
			out[trimmed] = value
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, nil
	}

	out := make(map[string]any, len(options)+len(current))
	for key, opt := range options {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		if opt.Default != nil {
			out[trimmed] = opt.Default
		}
	}

	for key, raw := range current {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		opt, known := options[trimmed]
		if !known {
			return nil, fmt.Errorf("unknown option %q (not declared in manifest)", trimmed)
		}
		if raw == nil {
			delete(out, trimmed)
			continue
		}
		coerced, err := manifest.NormalizeAdapterOptionValue(opt, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", trimmed, err)
		}
		out[trimmed] = coerced
	}

	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// computePlanFingerprint generates a hash used to detect configuration changes
// that require adapter restart. The fingerprint includes:
// - Adapter binding (ID, slot, config)
// - Manifest runtime fields (slot/entrypoint/options/telemetry/assets)
// - Endpoint settings (transport, command, args, env; address only for non-process transports)
// Note: Using parsed manifest removes whitespace sensitivity; adding new runtime-relevant
// fields should extend this function accordingly.
func computePlanFingerprint(binding Binding, manifest *manifest.Manifest, manifestRaw string, endpoint configstore.AdapterEndpoint) string {
	h := sha256.New()
	write := func(value string) {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}

	write(binding.AdapterID)
	rawConfig := strings.TrimSpace(binding.RawConfig)
	if rawConfig == "" && len(binding.Config) > 0 {
		if payload, err := json.Marshal(binding.Config); err == nil {
			rawConfig = string(payload)
		}
	}
	write(strings.TrimSpace(rawConfig))

	if manifest != nil && manifest.Adapter != nil {
		spec := manifest.Adapter
		write(strings.TrimSpace(spec.Slot))
		write(strings.TrimSpace(spec.Entrypoint.Runtime))
		write(strings.TrimSpace(spec.Entrypoint.Command))
		write(strings.TrimSpace(spec.Entrypoint.Transport))
		write(strings.TrimSpace(spec.Entrypoint.ListenEnv))
		write(strings.TrimSpace(spec.Entrypoint.WorkingDir))
		write(strings.TrimSpace(spec.Entrypoint.ReadyTimeout))
		if len(spec.Entrypoint.Args) == 0 {
			write("")
		} else {
			args := append([]string(nil), spec.Entrypoint.Args...)
			for _, arg := range args {
				write(arg)
			}
		}
		if spec.Telemetry.Stdout != nil {
			write(fmt.Sprintf("stdout:%t", *spec.Telemetry.Stdout))
		} else {
			write("stdout:nil")
		}
		if spec.Telemetry.Stderr != nil {
			write(fmt.Sprintf("stderr:%t", *spec.Telemetry.Stderr))
		} else {
			write("stderr:nil")
		}
		write(strings.TrimSpace(spec.Assets.Models.CacheDirEnv))
		if len(spec.Options) == 0 {
			write("")
		} else {
			keys := make([]string, 0, len(spec.Options))
			for k := range spec.Options {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				opt := spec.Options[k]
				write(k)
				write(strings.TrimSpace(opt.Type))
				write(strings.TrimSpace(opt.Description))
				if b, err := json.Marshal(opt.Default); err == nil {
					write(string(b))
				} else {
					write(fmt.Sprint(opt.Default))
				}
				if len(opt.Values) > 0 {
					if b, err := json.Marshal(opt.Values); err == nil {
						write(string(b))
					} else {
						write(fmt.Sprint(opt.Values))
					}
				} else {
					write("")
				}
			}
		}
	} else {
		write(strings.TrimSpace(manifestRaw))
	}

	write(endpoint.Transport)
	// For process transport, address is dynamically allocated per-instance and should NOT affect fingerprint.
	if endpoint.Transport != "process" {
		write(endpoint.Address)
	}
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
	// TLS configuration affects connectivity — changes require restart.
	write(endpoint.TLSCertPath)
	write(endpoint.TLSKeyPath)
	write(endpoint.TLSCACertPath)
	if endpoint.TLSInsecure {
		write("tls_insecure:true")
	} else {
		write("tls_insecure:false")
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (m *Manager) pluginRoot() string {
	if strings.TrimSpace(m.pluginDir) != "" {
		return m.pluginDir
	}
	return filepath.Join(config.GetNupiHome(), "plugins")
}

func (m *Manager) lookupAdapter(ctx context.Context, adapterID string) (configstore.Adapter, *manifest.Manifest, error) {
	if m.adapterSource == nil {
		return configstore.Adapter{}, nil, errAdapterDetailsUnavailable
	}

	adapter, err := m.adapterSource.GetAdapter(ctx, adapterID)
	if err != nil {
		if configstore.IsNotFound(err) {
			return configstore.Adapter{}, nil, nil
		}
		return configstore.Adapter{}, nil, fmt.Errorf("adapters: get adapter %s: %w", adapterID, err)
	}

	raw := strings.TrimSpace(adapter.Manifest)
	if raw == "" {
		return adapter, nil, nil
	}

	mf, err := manifest.Parse([]byte(raw))
	if err != nil {
		return adapter, nil, fmt.Errorf("adapters: parse manifest for %s: %w", adapterID, err)
	}
	mf.Dir = filepath.Join(m.pluginRoot(), sanitizeAdapterSlug(mf, adapterID))
	return adapter, mf, nil
}

func (m *Manager) lookupEndpoint(ctx context.Context, adapterID string) (configstore.AdapterEndpoint, error) {
	if m.adapterSource == nil {
		return configstore.AdapterEndpoint{}, errAdapterDetailsUnavailable
	}

	endpoint, err := m.adapterSource.GetAdapterEndpoint(ctx, adapterID)
	if err != nil {
		if configstore.IsNotFound(err) {
			return configstore.AdapterEndpoint{}, nil
		}
		return configstore.AdapterEndpoint{}, fmt.Errorf("adapters: get adapter endpoint %s: %w", adapterID, err)
	}
	return endpoint, nil
}

func logError(bus *eventbus.Bus, err error) {
	if err == nil {
		return
	}
	log.Printf("[Adapters] %v", err)
	eventbus.Publish(context.Background(), bus, eventbus.Adapters.Status, eventbus.SourceAdaptersService, eventbus.AdapterStatusEvent{
		Status:  eventbus.AdapterHealthError,
		Message: err.Error(),
	})
}

func sanitizeAdapterSlug(manifest *manifest.Manifest, fallback string) string {
	if manifest != nil {
		if slug := strings.TrimSpace(manifest.Metadata.Slug); slug != "" {
			if sanitized := sanitizeIdentifier(slug); sanitized != "" {
				if sanitized != strings.ToLower(strings.ReplaceAll(slug, " ", "-")) {
					log.Printf("[Adapters] sanitized slug %q -> %q", slug, sanitized)
				} else if sanitized != slug {
					log.Printf("[Adapters] sanitized slug %q -> %q", slug, sanitized)
				}
				return sanitized
			}
		}
		if name := strings.TrimSpace(manifest.Metadata.Name); name != "" {
			if sanitized := sanitizeIdentifier(name); sanitized != "" {
				if sanitized != strings.ToLower(strings.ReplaceAll(name, " ", "-")) {
					log.Printf("[Adapters] sanitized name %q -> %q", name, sanitized)
				} else if sanitized != name {
					log.Printf("[Adapters] sanitized name %q -> %q", name, sanitized)
				}
				return sanitized
			}
		}
	}
	if fallback = strings.TrimSpace(fallback); fallback != "" {
		if sanitized := sanitizeIdentifier(fallback); sanitized != "" {
			if sanitized != fallback {
				log.Printf("[Adapters] sanitized fallback %q -> %q", fallback, sanitized)
			}
			return sanitized
		}
	}
	return "adapter"
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
		return "adapter"
	}
	if len(res) > 64 {
		return res[:64]
	}
	return res
}

func mergeManifestEndpoint(manifest *manifest.Manifest, adapterID string, endpoint configstore.AdapterEndpoint) (configstore.AdapterEndpoint, error) {
	if endpoint.AdapterID == "" {
		endpoint.AdapterID = strings.TrimSpace(adapterID)
	}
	if manifest == nil || manifest.Adapter == nil {
		return endpoint, nil
	}

	entry := manifest.Adapter.Entrypoint
	endpointTransport := strings.TrimSpace(endpoint.Transport)
	manifestTransport := strings.TrimSpace(entry.Transport)

	if endpointTransport != "" && manifestTransport != "" {
		endNormalized, err := normalizeAdapterTransport(endpointTransport)
		if err != nil {
			return endpoint, fmt.Errorf("adapters: manifest transport for %s: %w", adapterID, err)
		}
		manifestNormalized, err := normalizeAdapterTransport(manifestTransport)
		if err != nil {
			return endpoint, fmt.Errorf("adapters: manifest transport for %s: %w", adapterID, err)
		}
		if endNormalized != "" && manifestNormalized != "" && endNormalized != manifestNormalized {
			return endpoint, fmt.Errorf("adapters: transport mismatch for %s (manifest=%s endpoint=%s)", adapterID, manifestNormalized, endNormalized)
		}
	}

	transport := endpointTransport
	if transport == "" {
		transport = manifestTransport
	}
	normalizedTransport, err := normalizeAdapterTransport(transport)
	if err != nil {
		return endpoint, fmt.Errorf("adapters: manifest transport for %s: %w", adapterID, err)
	}
	if normalizedTransport == "" {
		normalizedTransport = "process"
	}
	endpoint.Transport = normalizedTransport

	if normalizedTransport == "process" {
		if strings.TrimSpace(endpoint.Command) == "" {
			endpoint.Command = strings.TrimSpace(entry.Command)
		}
		if len(endpoint.Args) == 0 && len(entry.Args) > 0 {
			endpoint.Args = append([]string(nil), entry.Args...)
		}
		if strings.TrimSpace(endpoint.Command) == "" {
			return endpoint, fmt.Errorf("adapters: process transport for %s requires command", adapterID)
		}
	} else {
		if strings.TrimSpace(endpoint.Address) == "" {
			return endpoint, fmt.Errorf("adapters: transport %s for %s requires address", normalizedTransport, adapterID)
		}
	}

	return endpoint, nil
}

func normalizeAdapterTransport(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	return configstore.NormalizeAdapterTransport(trimmed)
}

// allocateProcessAddress reserves an ephemeral localhost TCP port for process adapters.
// NOTE: Closing the listener introduces a narrow race window before the adapter binds to the
// port. This is acceptable because:
//  1. The OS assigns ports from the ephemeral range on 127.0.0.1, keeping collision probability low.
//  2. If the adapter fails to bind (for example due to EADDRINUSE), Ensure() surfaces the error
//     and the reconciliation loop retries with a fresh allocation.
//  3. Ports never leave the local loopback interface, so another user cannot hijack them remotely.
//
// If these assumptions change we should revisit this approach (e.g. add retry logic or active health checks).
func allocateProcessAddress() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		return "", err
	}
	return addr, nil
}

func waitForAdapterReady(ctx context.Context, addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return errors.New("adapters: wait ready: address empty")
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr := err

		select {
		case <-ctx.Done():
			if ctx.Err() != nil {
				return fmt.Errorf("dial %s: %w", addr, ctx.Err())
			}
			return fmt.Errorf("dial %s: %w", addr, lastErr)
		case <-ticker.C:
		}
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// SetReadinessChecker allows tests to override the adapter readiness check function.
// Call the returned function to restore the original behavior.
// This should only be used in tests.
func SetReadinessChecker(fn func(ctx context.Context, addr string) error) func() {
	readinessCheckerMu.Lock()
	original := waitForAdapterReadyFn
	waitForAdapterReadyFn = fn
	readinessCheckerMu.Unlock()
	return func() {
		readinessCheckerMu.Lock()
		waitForAdapterReadyFn = original
		readinessCheckerMu.Unlock()
	}
}

// resolveAdapterCommand determines the binary and args to launch an adapter based on its runtime type.
// For binary adapters, it runs the command directly.
// For JS adapters, it runs the command via the bundled Bun runtime.
func (m *Manager) resolveAdapterCommand(plan bindingPlan, adapterHome string) (string, []string, error) {
	command := strings.TrimSpace(plan.endpoint.Command)
	if command == "" {
		return "", nil, fmt.Errorf("adapters: endpoint command required for %s", plan.binding.AdapterID)
	}

	// Resolve relative paths against adapter home
	if !filepath.IsAbs(command) && adapterHome != "" {
		command = filepath.Join(adapterHome, command)
	}

	// Check runtime type from manifest
	runtime := "binary"
	if plan.manifest != nil && plan.manifest.Adapter != nil {
		if r := strings.TrimSpace(plan.manifest.Adapter.Entrypoint.Runtime); r != "" {
			runtime = r
		}
	}

	if runtime == "js" {
		// JS adapter - run via bundled Bun runtime
		bunPath, err := jsrunner.GetRuntimePath()
		if err != nil {
			return "", nil, fmt.Errorf("adapters: JS runtime not available for %s: %w", plan.binding.AdapterID, err)
		}
		return bunPath, append([]string{"run", command}, plan.endpoint.Args...), nil
	}

	// Binary adapter - run directly
	return command, plan.endpoint.Args, nil
}

// IsBuiltinMockAdapter returns true if the adapter is a builtin mock
// that runs in-process without requiring a process to launch.
// This function is exported for use by other packages (e.g., intentrouter bridge).
func IsBuiltinMockAdapter(adapterID string) bool {
	switch adapterID {
	case MockSTTAdapterID, MockTTSAdapterID, MockVADAdapterID, MockAIAdapterID:
		return true
	default:
		return false
	}
}
