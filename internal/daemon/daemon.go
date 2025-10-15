package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nupi-ai/nupi/internal/adapterrunner"
	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/plugins"
	daemonruntime "github.com/nupi-ai/nupi/internal/runtime"
	"github.com/nupi-ai/nupi/internal/server"
	"github.com/nupi-ai/nupi/internal/session"
)

// Options groups dependencies required to construct a Daemon.
type Options struct {
	Store         *configstore.Store
	RunnerManager *adapterrunner.Manager
}

// Daemon represents the main daemon process.
type Daemon struct {
	store                *configstore.Store
	sessionManager       *session.Manager
	apiServer            *server.APIServer
	serviceHost          *daemonruntime.ServiceHost
	runtimeInfo          *RuntimeInfo
	lifecycle            *daemonruntime.Lifecycle
	instancePaths        config.InstancePaths
	runnerManager        *adapterrunner.Manager
	ctx                  context.Context
	cancel               context.CancelFunc
	errMu                sync.Mutex
	runErr               error
	configMu             sync.Mutex
	configCancel         context.CancelFunc
	transportMonitorStop context.CancelFunc
	transportSnapshot    server.TransportSnapshot
}

// New creates a new daemon instance bound to the provided configuration store.
func New(opts Options) (*Daemon, error) {
	if opts.Store == nil {
		return nil, errors.New("daemon: configuration store is required")
	}

	instanceName := opts.Store.InstanceName()
	paths := config.GetInstancePaths(instanceName)

	sessionManager := session.NewManager()
	runtimeInfo := &RuntimeInfo{}

	runnerManager := opts.RunnerManager
	if runnerManager == nil {
		runnerManager = adapterrunner.NewManager("")
	}
	if err := runnerManager.EnsureLayout(); err != nil {
		log.Printf("[Daemon] adapter-runner layout error: %v", err)
	}

	transportCfg, err := opts.Store.GetTransportConfig(context.Background())
	if err != nil {
		return nil, fmt.Errorf("daemon: load transport config: %w", err)
	}

	apiServer, err := server.NewAPIServer(sessionManager, opts.Store, runtimeInfo, transportCfg.Port)
	if err != nil {
		return nil, fmt.Errorf("daemon: create API server: %w", err)
	}

	host := daemonruntime.NewServiceHost()

	// plugins service
	if err := host.Register("plugins", func(ctx context.Context) (daemonruntime.Service, error) {
		svc := plugins.NewService(paths.Home)
		sessionManager.SetPluginDir(svc.PluginDir())
		return svc, nil
	}); err != nil {
		return nil, err
	}

	// unix socket service
	if err := host.Register("unix_socket", func(ctx context.Context) (daemonruntime.Service, error) {
		socket := paths.Socket
		if !filepath.IsAbs(socket) {
			socket = filepath.Clean(socket)
		}
		return newUnixSocketService(socket, sessionManager, apiServer, runtimeInfo), nil
	}); err != nil {
		return nil, err
	}

	// transport gateway service
	if err := host.Register("transport_gateway", func(ctx context.Context) (daemonruntime.Service, error) {
		return newGatewayService(apiServer, runtimeInfo), nil
	}); err != nil {
		return nil, err
	}

	apiServer.AddTransportListener(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := host.Restart(ctx, "transport_gateway"); err != nil {
			log.Printf("[Daemon] Failed to restart transport_gateway after transport change: %v", err)
		}
	})

	runtimeInfo.SetPort(transportCfg.Port)
	runtimeInfo.SetGRPCPort(transportCfg.GRPCPort)

	d := &Daemon{
		store:          opts.Store,
		sessionManager: sessionManager,
		apiServer:      apiServer,
		serviceHost:    host,
		runtimeInfo:    runtimeInfo,
		lifecycle:      daemonruntime.NewLifecycle(),
		instancePaths:  paths,
		runnerManager:  runnerManager,
	}

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
	if d.runnerManager != nil {
		syncCtx, cancelSync := context.WithTimeout(context.Background(), 2*time.Second)
		d.syncAdapterRunnerSettings(syncCtx)
		cancelSync()
	}

	d.ctx, d.cancel = context.WithCancel(context.Background())

	if err := d.serviceHost.Start(d.ctx); err != nil {
		if d.cancel != nil {
			d.cancel()
		}
		return fmt.Errorf("daemon: start services: %w", err)
	}
	d.watchHostErrors()
	d.transportSnapshot = d.apiServer.CurrentTransportSnapshot()
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

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	if d.configCancel != nil {
		d.configCancel()
	}
	if d.transportMonitorStop != nil {
		d.transportMonitorStop()
	}
	if d.cancel != nil {
		d.cancel()
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
	d.configCancel = cancel
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

	if err := d.serviceHost.Restart(ctx, "transport_gateway"); err != nil {
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

	ctx, cancel := context.WithCancel(d.ctx)
	d.transportMonitorStop = cancel

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

func (d *Daemon) syncAdapterRunnerSettings(ctx context.Context) {
	if d.store == nil || d.runnerManager == nil {
		return
	}

	if err := adapterrunner.SyncSettings(ctx, d.store, d.runnerManager); err != nil {
		log.Printf("[Daemon] adapter-runner sync failed: %v", err)
	}
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

// APIServer returns the HTTP/WebSocket API server.
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

	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	if err := process.Signal(syscall.Signal(0)); err != nil {
		os.Remove(paths.Lock)
		return false
	}

	return true
}
