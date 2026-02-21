package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/audio/barge"
	"github.com/nupi-ai/nupi/internal/audio/egress"
	"github.com/nupi-ai/nupi/internal/audio/ingress"
	"github.com/nupi-ai/nupi/internal/audio/stt"
	"github.com/nupi-ai/nupi/internal/audio/vad"

	"github.com/nupi-ai/nupi/internal/awareness"
	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/contentpipeline"
	"github.com/nupi-ai/nupi/internal/conversation"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/intentrouter"
	"github.com/nupi-ai/nupi/internal/observability"
	"github.com/nupi-ai/nupi/internal/plugins"
	adapters "github.com/nupi-ai/nupi/internal/plugins/adapters"
	"github.com/nupi-ai/nupi/internal/plugins/integrity"
	"github.com/nupi-ai/nupi/internal/procutil"
	"github.com/nupi-ai/nupi/internal/prompts"
	daemonruntime "github.com/nupi-ai/nupi/internal/runtime"
	"github.com/nupi-ai/nupi/internal/server"
	"github.com/nupi-ai/nupi/internal/server/runtimebridge"
	"github.com/nupi-ai/nupi/internal/session"
)

// Options groups dependencies required to construct a Daemon.
type Options struct {
	Store *configstore.Store
}

// Daemon represents the main daemon process.
type Daemon struct {
	store             *configstore.Store
	sessionManager    *session.Manager
	apiServer         *server.APIServer
	serviceHost       *daemonruntime.ServiceHost
	runtimeInfo       *RuntimeInfo
	lifecycle         *daemonruntime.Lifecycle
	instancePaths     config.InstancePaths
	eventBus          *eventbus.Bus
	globalStore       *conversation.GlobalStore
	ctx               context.Context
	cancel            context.CancelFunc
	errMu             sync.Mutex
	runErr            error
	configMu          sync.Mutex
	configCancel      context.CancelFunc
	transportSnapshot server.TransportSnapshot
	commandExecutor   intentrouter.CommandExecutor
}

const (
	// storeQueryTimeout bounds context deadlines for store lookups during
	// daemon operation (plugin enabled checks, integrity checksums, config reloads).
	storeQueryTimeout = 5 * time.Second

	// serviceOpTimeout bounds context deadlines for service lifecycle
	// operations (restart, graceful shutdown).
	serviceOpTimeout = 5 * time.Second

	// transportMonitorInterval is the polling period for TLS certificate
	// file-change detection in the transport monitor goroutine.
	transportMonitorInterval = 5 * time.Second
)

// New creates a new daemon instance bound to the provided configuration store.
func New(opts Options) (*Daemon, error) {
	if opts.Store == nil {
		return nil, errors.New("daemon: configuration store is required")
	}

	instanceName := opts.Store.InstanceName()
	paths := config.GetInstancePaths(instanceName)

	bus := eventbus.New()

	sessionManager := session.NewManager()
	sessionManager.UseEventBus(bus)
	runtimeInfo := &RuntimeInfo{}

	transportCfg, err := opts.Store.GetTransportConfig(context.Background())
	if err != nil {
		return nil, fmt.Errorf("daemon: load transport config: %w", err)
	}

	if err := adapters.EnsureBuiltinAdapters(context.Background(), opts.Store); err != nil {
		return nil, fmt.Errorf("daemon: ensure builtin adapters: %w", err)
	}

	apiServer, err := server.NewAPIServer(sessionManager, opts.Store, runtimeInfo)
	if err != nil {
		return nil, fmt.Errorf("daemon: create API server: %w", err)
	}

	host := daemonruntime.NewServiceHost()

	pluginService := plugins.NewService(paths.Home)
	pluginService.SetEnabledChecker(func(namespace, slug string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), storeQueryTimeout)
		defer cancel()
		p, err := opts.Store.GetInstalledPlugin(ctx, namespace, slug)
		if err != nil {
			if configstore.IsNotFound(err) {
				return true // not tracked in DB → manual install, load it
			}
			log.Printf("[Plugins] %s/%s: enabled check failed (DB error), refusing to load: %v", namespace, slug, err)
			return false
		}
		return p.Enabled
	})
	pluginService.SetIntegrityChecker(func(namespace, slug, pluginDir string) error {
		ctx, cancel := context.WithTimeout(context.Background(), storeQueryTimeout)
		defer cancel()
		checksums, err := opts.Store.GetPluginChecksumsByPlugin(ctx, namespace, slug)
		if err != nil {
			return fmt.Errorf("read checksums: %w", err)
		}
		if len(checksums) == 0 {
			log.Printf("[Plugins] %s/%s: no integrity checksums (manual install), skipping verification", namespace, slug)
			return nil
		}
		expected := make([]integrity.FileChecksum, len(checksums))
		for i, c := range checksums {
			expected[i] = integrity.FileChecksum{Path: c.FilePath, SHA256: c.SHA256}
		}
		result := integrity.VerifyPlugin(pluginDir, expected)
		if !result.Verified {
			return errors.New(strings.Join(result.Mismatches, "; "))
		}
		return nil
	})
	sessionManager.SetPluginDir(pluginService.PluginDir())
	sessionManager.SetJSRuntimeFunc(pluginService.JSRuntime)

	// plugins service
	if err := host.Register("plugins", func(ctx context.Context) (daemonruntime.Service, error) {
		return pluginService, nil
	}); err != nil {
		return nil, err
	}

	adapterManager := adapters.NewManager(adapters.ManagerOptions{
		Store:     opts.Store,
		Adapters:  opts.Store,
		PluginDir: pluginService.PluginDir(),
		Bus:       bus,
	})
	adaptersService := adapters.NewService(adapterManager, opts.Store, bus)

	pipelineService := contentpipeline.NewService(bus, pluginService, contentpipeline.WithMetricsInterval(30*time.Second))
	audioIngressService := ingress.New(bus)
	audioSTTService := stt.New(bus, stt.WithFactory(stt.NewAdapterFactory(opts.Store, adaptersService)))
	audioVADService := vad.New(bus, vad.WithFactory(vad.NewAdapterFactory(opts.Store, adaptersService)))
	audioBargeService := barge.New(bus)
	audioEgressService := egress.New(bus, egress.WithFactory(egress.NewAdapterFactory(opts.Store, adaptersService)))

	// Global conversation store for sessionless ("bezpańskie") messages
	globalConversationStore := conversation.NewGlobalStore()
	conversationService := conversation.NewService(bus, conversation.WithGlobalStore(globalConversationStore))
	eventCounter := observability.NewEventCounter()
	bus.AddObserver(eventCounter)
	metricsExporter := observability.NewPrometheusExporter(bus, eventCounter)
	metricsExporter.WithPipeline(pipelineService)
	metricsExporter.WithPluginWarnings(pluginService)
	apiServer.SetMetricsExporter(metricsExporter)
	apiServer.SetConversationStore(conversationService)
	apiServer.SetAdaptersController(adaptersService)
	apiServer.SetAudioIngress(runtimebridge.AudioIngressProvider(audioIngressService))
	apiServer.SetAudioEgress(runtimebridge.AudioEgressController(audioEgressService))
	apiServer.SetEventBus(bus)
	apiServer.SetPluginWarningsProvider(runtimebridge.PluginWarningsProvider(pluginService))
	apiServer.SetPluginReloader(runtimebridge.PluginReloaderProvider(pluginService))

	if err := host.Register("audio_ingress", func(ctx context.Context) (daemonruntime.Service, error) {
		return audioIngressService, nil
	}); err != nil {
		return nil, err
	}

	if err := host.Register("audio_stt", func(ctx context.Context) (daemonruntime.Service, error) {
		return audioSTTService, nil
	}); err != nil {
		return nil, err
	}

	if err := host.Register("audio_vad", func(ctx context.Context) (daemonruntime.Service, error) {
		return audioVADService, nil
	}); err != nil {
		return nil, err
	}

	if err := host.Register("audio_barge", func(ctx context.Context) (daemonruntime.Service, error) {
		return audioBargeService, nil
	}); err != nil {
		return nil, err
	}

	if err := host.Register("audio_egress", func(ctx context.Context) (daemonruntime.Service, error) {
		return audioEgressService, nil
	}); err != nil {
		return nil, err
	}

	if err := host.Register("adapter_runtime", func(ctx context.Context) (daemonruntime.Service, error) {
		return adaptersService, nil
	}); err != nil {
		return nil, err
	}

	if err := host.Register("content_pipeline", func(ctx context.Context) (daemonruntime.Service, error) {
		return pipelineService, nil
	}); err != nil {
		return nil, err
	}

	if err := host.Register("conversation", func(ctx context.Context) (daemonruntime.Service, error) {
		return conversationService, nil
	}); err != nil {
		return nil, err
	}

	// Awareness service - loads core memory (SOUL.md, IDENTITY.md, etc.)
	// and provides it for system prompt injection.
	awarenessService := awareness.NewService(paths.Home)
	awarenessService.SetEventBus(bus)

	if err := host.Register("awareness", func(ctx context.Context) (daemonruntime.Service, error) {
		return awarenessService, nil
	}); err != nil {
		return nil, err
	}

	// Prompts engine for building AI prompts - loads from SQLite store
	// Default templates are seeded in store.Open, so they're already available
	promptsEngine := prompts.New(opts.Store)
	if err := promptsEngine.LoadTemplates(context.Background()); err != nil {
		return nil, fmt.Errorf("daemon: load prompt templates: %w", err)
	}
	promptEngineAdapter := intentrouter.NewPromptEngineAdapter(promptsEngine)

	// Command executor with priority queue (user > tool > ai > system).
	commandExecutor := runtimebridge.CommandExecutor(sessionManager)

	// Tool registry — awareness tools are registered before intent router creation.
	// Handlers are closures that call Service methods; since they're only invoked
	// when the intent router processes a prompt (after all services start), it's
	// safe to register them at construction time.
	toolRegistry := intentrouter.NewToolRegistry()
	for _, spec := range awarenessService.ToolSpecs() {
		toolRegistry.Register(&awarenessToolWrapper{
			name:    spec.Name,
			desc:    spec.Description,
			params:  spec.ParametersJSON,
			handler: spec.Handler,
		})
	}

	// Intent router service - bridges conversation to AI adapters
	// Starts in passive mode (no adapter) - real AI adapter will be set via
	// SetAdapter() when configured through adapter manager/config store
	intentRouterService := intentrouter.NewService(bus,
		intentrouter.WithSessionProvider(runtimebridge.SessionProvider(sessionManager)),
		intentrouter.WithCommandExecutor(commandExecutor),
		intentrouter.WithPromptEngine(promptEngineAdapter),
		intentrouter.WithCoreMemoryProvider(awarenessService),
		intentrouter.WithToolRegistry(toolRegistry),
	)

	if err := host.Register("intent_router", func(ctx context.Context) (daemonruntime.Service, error) {
		return intentRouterService, nil
	}); err != nil {
		return nil, err
	}

	// Wire intent router metrics to Prometheus exporter
	metricsExporter.WithIntentRouter(func() observability.IntentRouterMetricsSnapshot {
		m := intentRouterService.Metrics()
		return observability.IntentRouterMetricsSnapshot{
			RequestsTotal:  m.RequestsTotal,
			RequestsFailed: m.RequestsFailed,
			CommandsQueued: m.CommandsQueued,
			Clarifications: m.Clarifications,
			SpeakEvents:    m.SpeakEvents,
			AdapterName:    m.AdapterName,
			AdapterReady:   m.AdapterReady,
		}
	})

	// Intent router adapter bridge - watches for AI adapter status changes
	// and updates the intent router with the appropriate adapter.
	// Uses adaptersService for initial sync with current binding state.
	intentRouterBridge := intentrouter.NewAdapterBridge(bus, intentRouterService, adaptersService)

	// Wire embedding provider from adapter bridge to awareness service.
	// The embedding bridge tracks the NAP adapter lifecycle automatically.
	awarenessService.SetEmbeddingProvider(intentRouterBridge.EmbeddingBridge())
	if err := host.Register("intent_router_bridge", func(ctx context.Context) (daemonruntime.Service, error) {
		return intentRouterBridge, nil
	}); err != nil {
		return nil, err
	}

	// transport gateway service (gRPC on TCP + Unix socket)
	grpcSocketPath := paths.Socket
	if !filepath.IsAbs(grpcSocketPath) {
		grpcSocketPath = filepath.Clean(grpcSocketPath)
	}
	if err := host.Register("transport_gateway", func(ctx context.Context) (daemonruntime.Service, error) {
		return newGatewayService(apiServer, runtimeInfo, grpcSocketPath), nil
	}); err != nil {
		return nil, err
	}

	runtimeInfo.SetGRPCPort(transportCfg.GRPCPort)

	d := &Daemon{
		store:           opts.Store,
		sessionManager:  sessionManager,
		apiServer:       apiServer,
		serviceHost:     host,
		runtimeInfo:     runtimeInfo,
		lifecycle:       daemonruntime.NewLifecycle(),
		instancePaths:   paths,
		eventBus:        bus,
		globalStore:     globalConversationStore,
		commandExecutor: commandExecutor,
	}

	metricsExporter.WithAudioMetrics(func() observability.AudioMetricsSnapshot {
		select {
		case <-d.lifecycle.Done():
			return observability.AudioMetricsSnapshot{}
		default:
		}
		ingressStats := audioIngressService.Metrics()
		sttStats := audioSTTService.Metrics()
		ttsStats := audioEgressService.Metrics()
		bargeStats := audioBargeService.Metrics()
		vadStats := audioVADService.Metrics()
		return observability.AudioMetricsSnapshot{
			AudioIngressBytes:  ingressStats.BytesTotal,
			STTSegments:        sttStats.SegmentsTotal,
			TTSActiveStreams:   ttsStats.ActiveStreams,
			SpeechBargeInTotal: bargeStats.BargeInTotal,
			VADDetections:      vadStats.DetectionsTotal,
			VADRetryAttempts:   vadStats.RetryAttemptsTotal,
			VADRetryAbandoned:  vadStats.RetryAbandonedTotal,
		}
	})

	apiServer.AddTransportListener(func() {
		select {
		case <-d.lifecycle.Done():
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), serviceOpTimeout)
		defer cancel()
		if err := host.Restart(ctx, "transport_gateway"); err != nil {
			log.Printf("[Daemon] Failed to restart transport_gateway after transport change: %v", err)
		}
	})

	apiServer.SetShutdownFunc(func(ctx context.Context) error {
		go func() {
			if err := d.Shutdown(); err != nil {
				log.Printf("[Daemon] shutdown via API returned error: %v", err)
			}
		}()
		return nil
	})

	return d, nil
}

// Start starts the daemon services.
func (d *Daemon) Start() error {
	if err := daemonruntime.WritePIDFile(d.instancePaths.Lock, os.Getpid()); err != nil {
		return fmt.Errorf("daemon: write pid file: %w", err)
	}
	defer daemonruntime.RemovePIDFile(d.instancePaths.Lock)

	d.runtimeInfo.SetStartTime(time.Now())
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.eventBus.StartMetricsReporter(d.ctx, 30*time.Second, nil)

	// Start global conversation store periodic pruning
	if d.globalStore != nil {
		d.globalStore.Start()
	}

	if err := d.serviceHost.Start(d.ctx); err != nil {
		if d.cancel != nil {
			d.cancel()
		}
		return fmt.Errorf("daemon: start services: %w", err)
	}
	d.watchHostErrors()
	d.configMu.Lock()
	d.transportSnapshot = d.apiServer.CurrentTransportSnapshot()
	d.configMu.Unlock()
	if err := d.startConfigWatcher(); err != nil {
		log.Printf("[Daemon] config watcher error: %v", err)
	}
	if err := d.startTransportMonitor(); err != nil {
		log.Printf("[Daemon] transport monitor error: %v", err)
	}

	<-d.lifecycle.Done()

	if d.cancel != nil {
		d.cancel()
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), serviceOpTimeout)
	if err := d.serviceHost.Stop(stopCtx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "daemon: service shutdown error: %v\n", err)
		if d.runErr == nil {
			d.setRunError(err)
		}
	}
	cancel()

	for _, sess := range d.sessionManager.ListSessions() {
		d.sessionManager.KillSession(sess.ID)
	}

	if err := d.store.Close(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "daemon: store close error: %v\n", err)
	}

	return d.getRunError()
}

// Shutdown signals the daemon to stop.
func (d *Daemon) Shutdown() error {
	d.lifecycle.Shutdown()
	d.configMu.Lock()
	cancelConfig := d.configCancel
	d.configCancel = nil
	d.configMu.Unlock()
	if cancelConfig != nil {
		cancelConfig()
	}
	if d.cancel != nil {
		d.cancel()
	}
	if d.commandExecutor != nil {
		d.commandExecutor.Stop()
	}
	if d.globalStore != nil {
		d.globalStore.Stop()
	}
	if d.eventBus != nil {
		d.eventBus.Shutdown()
	}
	return nil
}

func (d *Daemon) watchHostErrors() {
	go func() {
		for err := range d.serviceHost.Errors() {
			if err == nil {
				continue
			}
			d.setRunError(err)
			fmt.Fprintf(os.Stderr, "%v\n", err)
			d.lifecycle.Shutdown()
			if d.cancel != nil {
				d.cancel()
			}
		}
	}()
}

func (d *Daemon) startConfigWatcher() error {
	if d.store == nil || d.serviceHost == nil {
		return nil
	}

	cancel, err := d.serviceHost.WatchConfig(d.ctx, d.store, time.Second, d.handleConfigEvent)
	if err != nil {
		return err
	}
	d.configMu.Lock()
	d.configCancel = cancel
	d.configMu.Unlock()
	return nil
}

func (d *Daemon) handleConfigEvent(event configstore.ChangeEvent) {
	if !event.Changed() {
		return
	}

	if event.SettingsChanged {
		d.handleTransportSettings()
	}

	if event.AdaptersChanged || event.AdapterBindingsChanged {
		d.restartService("plugins")
	}
}

func (d *Daemon) handleTransportSettings() {
	d.configMu.Lock()
	defer d.configMu.Unlock()

	select {
	case <-d.ctx.Done():
		return
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), storeQueryTimeout)
	defer cancel()

	cfg, err := d.store.GetTransportConfig(ctx)
	if err != nil {
		log.Printf("[Daemon] failed to load transport config: %v", err)
		return
	}

	current := d.apiServer.CurrentTransportSnapshot()
	if current.EqualConfig(cfg) {
		d.transportSnapshot = current
		return
	}

	restartCtx, restartCancel := context.WithTimeout(context.Background(), serviceOpTimeout)
	defer restartCancel()
	if err := d.serviceHost.Restart(restartCtx, "transport_gateway"); err != nil {
		log.Printf("[Daemon] restart transport_gateway failed: %v", err)
	} else {
		log.Printf("[Daemon] transport_gateway restarted after transport config change")
	}
	d.transportSnapshot = d.apiServer.CurrentTransportSnapshot()
}

func (d *Daemon) startTransportMonitor() error {
	if d.apiServer == nil {
		return nil
	}

	go func() {
		ticker := time.NewTicker(transportMonitorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-ticker.C:
				d.checkTransportFiles()
			}
		}
	}()

	return nil
}

func (d *Daemon) restartService(name string) {
	d.configMu.Lock()
	defer d.configMu.Unlock()

	select {
	case <-d.ctx.Done():
		return
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), serviceOpTimeout)
	defer cancel()

	if err := d.serviceHost.Restart(ctx, name); err != nil {
		log.Printf("[Daemon] restart %s failed: %v", name, err)
	}
}

func (d *Daemon) checkTransportFiles() {
	d.configMu.Lock()
	defer d.configMu.Unlock()

	if d.ctx.Err() != nil {
		return
	}

	current := d.apiServer.CurrentTransportSnapshot()
	if !tlsFilesChanged(d.transportSnapshot, current) {
		d.transportSnapshot = current
		return
	}

	if err := validateTLSAssets(current); err != nil {
		log.Printf("[Daemon] TLS assets change detected but validation failed (keeping existing listeners): %v", err)
		d.transportSnapshot = current
		return
	}

	log.Printf("[Daemon] TLS assets changed, restarting transport_gateway")
	ctx, cancel := context.WithTimeout(context.Background(), serviceOpTimeout)
	defer cancel()
	if err := d.serviceHost.Restart(ctx, "transport_gateway"); err != nil {
		log.Printf("[Daemon] restart transport_gateway after TLS change failed: %v", err)
	}
	d.transportSnapshot = d.apiServer.CurrentTransportSnapshot()
}

func tlsFilesChanged(prev, curr server.TransportSnapshot) bool {
	if strings.TrimSpace(prev.TLSCertPath) != strings.TrimSpace(curr.TLSCertPath) {
		return true
	}
	if strings.TrimSpace(prev.TLSKeyPath) != strings.TrimSpace(curr.TLSKeyPath) {
		return true
	}
	if !prev.TLSCertModTime.Equal(curr.TLSCertModTime) {
		return true
	}
	if !prev.TLSKeyModTime.Equal(curr.TLSKeyModTime) {
		return true
	}
	return false
}

func validateTLSAssets(snapshot server.TransportSnapshot) error {
	certPath := strings.TrimSpace(snapshot.TLSCertPath)
	keyPath := strings.TrimSpace(snapshot.TLSKeyPath)

	if certPath == "" && keyPath == "" {
		return nil
	}
	if certPath == "" || keyPath == "" {
		return fmt.Errorf("tls asset paths must include both certificate and key (cert=%q key=%q)", certPath, keyPath)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		return fmt.Errorf("load tls certificate/key pair: %w", err)
	}
	return nil
}

func (d *Daemon) setRunError(err error) {
	if err == nil {
		return
	}

	d.errMu.Lock()
	defer d.errMu.Unlock()
	if d.runErr == nil {
		d.runErr = err
	}
}

func (d *Daemon) getRunError() error {
	d.errMu.Lock()
	defer d.errMu.Unlock()
	return d.runErr
}

// RuntimeInfo exposes runtime metadata to protocols.
func (d *Daemon) RuntimeInfo() *RuntimeInfo {
	return d.runtimeInfo
}

// SessionManager returns the session manager.
func (d *Daemon) SessionManager() *session.Manager {
	return d.sessionManager
}

// APIServer returns the API server.
func (d *Daemon) APIServer() *server.APIServer {
	return d.apiServer
}

// ServiceHost returns the runtime service host.
func (d *Daemon) ServiceHost() *daemonruntime.ServiceHost {
	return d.serviceHost
}

// IsRunning checks if daemon is already running for the default instance.
func IsRunning() bool {
	paths := config.GetInstancePaths("")

	if conn, err := net.Dial("unix", paths.Socket); err == nil {
		conn.Close()
		return true
	}

	data, err := os.ReadFile(paths.Lock)
	if err != nil {
		return false
	}

	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		os.Remove(paths.Lock)
		return false
	}

	if !procutil.IsProcessAlive(pid) {
		os.Remove(paths.Lock)
		return false
	}

	return true
}

// awarenessToolWrapper bridges awareness.ToolSpec to intentrouter.ToolHandler.
// This adapter maintains service boundary isolation: the awareness package
// does not import intentrouter types.
type awarenessToolWrapper struct {
	name, desc, params string
	handler            func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

func (w *awarenessToolWrapper) Definition() intentrouter.ToolDefinition {
	return intentrouter.ToolDefinition{
		Name:           w.name,
		Description:    w.desc,
		ParametersJSON: w.params,
	}
}

func (w *awarenessToolWrapper) Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	return w.handler(ctx, args)
}
