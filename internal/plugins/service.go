package plugins

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/jsrunner"
	"github.com/nupi-ai/nupi/internal/jsruntime"
	"github.com/nupi-ai/nupi/internal/plugins/manifest"
	pipelinecleaners "github.com/nupi-ai/nupi/internal/plugins/pipeline_cleaners"
	toolhandlers "github.com/nupi-ai/nupi/internal/plugins/tool_handlers"
	"github.com/nupi-ai/nupi/internal/sanitize"
)

// EnabledChecker returns whether a plugin identified by namespace/slug should be loaded.
// Return true to load, false to skip. If the plugin is not tracked (e.g., pre-marketplace
// plugin on disk), it should return true for backward compatibility.
type EnabledChecker func(namespace, slug string) bool

// IntegrityChecker verifies that plugin files have not been tampered with since
// installation. It receives the plugin's namespace, slug, and directory path.
// Return nil to allow loading, or an error to refuse the plugin.
type IntegrityChecker func(namespace, slug string, pluginDir string) error

// Service manages plugin assets and metadata for the daemon.
type Service struct {
	pluginDir        string
	runDir           string     // directory for runtime sockets (IPC)
	reloadMu         sync.Mutex // serializes Start/Reload/GenerateIndex to prevent concurrent file writes
	enabledChecker   EnabledChecker
	integrityChecker IntegrityChecker
	pipelineIdx      map[string]*pipelinecleaners.PipelinePlugin
	pipelineMu       sync.RWMutex

	toolHandlerIdx map[string]*toolhandlers.JSPlugin // tool name -> plugin
	toolHandlerMu  sync.RWMutex

	supervisedRT   *jsruntime.SupervisedRuntime
	supervisedRTMu sync.RWMutex
	jsStdout       *os.File
	jsStderr       *os.File
	jsPluginLogsMu sync.Mutex
	jsPluginLogs   map[string]*os.File
}

// NewService constructs a plugin service rooted in the given instance directory.
func NewService(instanceDir string) *Service {
	pluginDir := filepath.Join(instanceDir, "plugins")
	runDir := filepath.Join(instanceDir, "run")
	return &Service{
		pluginDir:      pluginDir,
		runDir:         runDir,
		pipelineIdx:    make(map[string]*pipelinecleaners.PipelinePlugin),
		toolHandlerIdx: make(map[string]*toolhandlers.JSPlugin),
		jsPluginLogs:   make(map[string]*os.File),
	}
}

// PluginDir returns the directory where plugins are stored.
func (s *Service) PluginDir() string {
	return s.pluginDir
}

// SetEnabledChecker configures a function that determines whether a plugin
// should be loaded during discovery. When set, only plugins for which the
// checker returns true are loaded into pipeline cleaners and tool handlers.
// Plugins not tracked in the database (checker returns true by default) are
// loaded for backward compatibility with pre-marketplace plugins.
//
// Must not be called concurrently with Start, Reload, or GenerateIndex.
func (s *Service) SetEnabledChecker(fn EnabledChecker) {
	s.enabledChecker = fn
}

// SetIntegrityChecker configures a function that verifies plugin file integrity
// before loading. When set, plugins that fail the integrity check are refused
// with a warning log. Plugins without checksums (manual installs) are loaded
// normally — the checker itself decides the policy via its return value.
//
// Must not be called concurrently with Start, Reload, or GenerateIndex.
func (s *Service) SetIntegrityChecker(fn IntegrityChecker) {
	s.integrityChecker = fn
}

// cachedChecker returns an IntegrityChecker that wraps s.integrityChecker with
// a per-reload cache. The cache ensures each (namespace, slug, pluginDir) tuple
// is verified at most once during a single reload cycle. Returns nil when no
// integrity checker is configured (preserving nil-means-skip semantics).
//
// Not safe for concurrent use — callers must ensure sequential invocation
// (as is the case in Reload and Start).
func (s *Service) cachedChecker() IntegrityChecker {
	checker := s.integrityChecker // capture current value to avoid inconsistency if SetIntegrityChecker is called later
	if checker == nil {
		return nil
	}
	cache := make(map[string]error)
	return func(namespace, slug, pluginDir string) error {
		key := namespace + "\x00" + slug + "\x00" + pluginDir
		if err, ok := cache[key]; ok {
			return err
		}
		err := checker(namespace, slug, pluginDir)
		cache[key] = err
		return err
	}
}

// filterManifests returns only manifests that pass the enabled and integrity
// checks. It skips disabled plugins (via enabledChecker) and tampered plugins
// (via the provided checker). When neither checker is set, all manifests are
// returned unchanged.
func (s *Service) filterManifests(manifests []*manifest.Manifest, checker IntegrityChecker) []*manifest.Manifest {
	if s.enabledChecker == nil && checker == nil {
		return manifests
	}
	filtered := make([]*manifest.Manifest, 0, len(manifests))
	for _, mf := range manifests {
		if s.enabledChecker != nil && !s.enabledChecker(mf.Metadata.Namespace, mf.Metadata.Slug) {
			log.Printf("[Plugins] filtered out %s/%s from index: plugin disabled", mf.Metadata.Namespace, mf.Metadata.Slug)
			continue
		}
		if checker != nil {
			if err := checker(mf.Metadata.Namespace, mf.Metadata.Slug, mf.Dir); err != nil {
				log.Printf("[Plugins] filtered out %s/%s from index: integrity check failed: %v", mf.Metadata.Namespace, mf.Metadata.Slug, err)
				continue
			}
		}
		filtered = append(filtered, mf)
	}
	return filtered
}

// LoadPipelinePlugins rebuilds the in-memory cleaner registry.
// Serialized by reloadMu to prevent races with Start, Reload, or GenerateIndex.
func (s *Service) LoadPipelinePlugins() error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	manifests, warnings := manifest.DiscoverWithWarnings(s.pluginDir)
	if len(warnings) > 0 {
		log.Printf("[Plugins] WARNING: %d plugin(s) skipped due to manifest errors:", len(warnings))
		for _, w := range warnings {
			log.Printf("[Plugins]   - %s: %v", w.Dir, w.Err)
		}
	}
	_, err := s.loadPipelinePlugins(context.Background(), manifests, s.integrityChecker)
	return err
}

func (s *Service) loadPipelinePlugins(ctx context.Context, manifests []*manifest.Manifest, checker IntegrityChecker) (int, error) {
	index := make(map[string]*pipelinecleaners.PipelinePlugin)
	pluginCount := 0

	// Get jsruntime for loading plugins with validation
	rt := s.runtime()

	for _, mf := range manifests {
		if mf.Type != manifest.PluginTypePipelineCleaner {
			continue
		}

		// Respect enabled/disabled state from database
		if s.enabledChecker != nil && !s.enabledChecker(mf.Metadata.Namespace, mf.Metadata.Slug) {
			log.Printf("[Plugins] skip disabled pipeline cleaner %s/%s", mf.Metadata.Namespace, mf.Metadata.Slug)
			continue
		}

		// Verify plugin file integrity before loading
		if checker != nil {
			if err := checker(mf.Metadata.Namespace, mf.Metadata.Slug, mf.Dir); err != nil {
				log.Printf("[Plugins] REFUSED pipeline cleaner %s/%s: integrity check failed: %v", mf.Metadata.Namespace, mf.Metadata.Slug, err)
				continue
			}
		}

		mainPath, err := mf.MainPath()
		if err != nil {
			log.Printf("[Plugins] skip pipeline cleaner %s: %v", mf.Dir, err)
			continue
		}

		// Use jsruntime for loading when available (validates transform function)
		var plugin *pipelinecleaners.PipelinePlugin
		if rt != nil {
			plugin, err = pipelinecleaners.LoadPipelinePluginWithRuntime(ctx, rt, mainPath)
		} else {
			plugin, err = pipelinecleaners.LoadPipelinePlugin(mainPath)
		}
		if err != nil {
			log.Printf("[Plugins] skip pipeline cleaner %s: %v", mainPath, err)
			continue
		}

		pluginCount++

		if plugin.Name == "" && strings.TrimSpace(mf.Metadata.Name) != "" {
			plugin.Name = mf.Metadata.Name
		}

		keys := []string{plugin.Name}
		keys = append(keys, plugin.Commands...)

		hasNonEmpty := false
		for _, k := range keys {
			if strings.TrimSpace(k) != "" {
				hasNonEmpty = true
				break
			}
		}
		if !hasNonEmpty {
			keys = append(keys, filepath.Base(mf.Dir))
		}

		for _, key := range keys {
			key = strings.TrimSpace(strings.ToLower(key))
			if key == "" {
				continue
			}
			index[key] = plugin
		}
	}

	s.pipelineMu.Lock()
	s.pipelineIdx = index
	s.pipelineMu.Unlock()

	log.Printf("[Plugins] Loaded %d pipeline cleaner plugins (%d index entries)", pluginCount, len(index))
	return pluginCount, nil
}

// PipelinePluginFor returns a cleaner matching the supplied name.
func (s *Service) PipelinePluginFor(name string) (*pipelinecleaners.PipelinePlugin, bool) {
	s.pipelineMu.RLock()
	defer s.pipelineMu.RUnlock()

	if len(s.pipelineIdx) == 0 {
		return nil, false
	}

	key := strings.TrimSpace(strings.ToLower(name))
	if key != "" {
		if plugin, ok := s.pipelineIdx[key]; ok {
			return plugin, true
		}
	}

	plugin, ok := s.pipelineIdx["default"]
	return plugin, ok
}

// LoadToolHandlerPlugins loads all tool handler plugins into memory.
// This enables the content pipeline to use tool processor methods like
// DetectIdleState, Clean, ExtractEvents, and Summarize.
// Serialized by reloadMu to prevent races with Start, Reload, or GenerateIndex.
func (s *Service) LoadToolHandlerPlugins() error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	manifests, warnings := manifest.DiscoverWithWarnings(s.pluginDir)
	if len(warnings) > 0 {
		log.Printf("[Plugins] WARNING: %d plugin(s) skipped due to manifest errors:", len(warnings))
		for _, w := range warnings {
			log.Printf("[Plugins]   - %s: %v", w.Dir, w.Err)
		}
	}
	_, err := s.loadToolHandlerPlugins(context.Background(), manifests, s.integrityChecker)
	return err
}

func (s *Service) loadToolHandlerPlugins(ctx context.Context, manifests []*manifest.Manifest, checker IntegrityChecker) (int, error) {
	index := make(map[string]*toolhandlers.JSPlugin)
	pluginCount := 0

	rt := s.runtime()

	for _, mf := range manifests {
		if mf.Type != manifest.PluginTypeToolHandler {
			continue
		}

		// Respect enabled/disabled state from database
		if s.enabledChecker != nil && !s.enabledChecker(mf.Metadata.Namespace, mf.Metadata.Slug) {
			log.Printf("[Plugins] skip disabled tool handler %s/%s", mf.Metadata.Namespace, mf.Metadata.Slug)
			continue
		}

		// Verify plugin file integrity before loading
		if checker != nil {
			if err := checker(mf.Metadata.Namespace, mf.Metadata.Slug, mf.Dir); err != nil {
				log.Printf("[Plugins] REFUSED tool handler %s/%s: integrity check failed: %v", mf.Metadata.Namespace, mf.Metadata.Slug, err)
				continue
			}
		}

		mainPath, err := mf.MainPath()
		if err != nil {
			log.Printf("[Plugins] skip tool handler %s: %v", mf.Dir, err)
			continue
		}

		var plugin *toolhandlers.JSPlugin
		if rt != nil {
			plugin, err = toolhandlers.LoadPluginWithRuntime(ctx, rt, mainPath)
		} else {
			plugin, err = toolhandlers.LoadPlugin(mainPath)
		}
		if err != nil {
			log.Printf("[Plugins] skip tool handler %s: %v", mainPath, err)
			continue
		}

		pluginCount++

		if plugin.Name == "" && strings.TrimSpace(mf.Metadata.Name) != "" {
			plugin.Name = mf.Metadata.Name
		}

		// Index by plugin name and commands
		keys := []string{plugin.Name}
		keys = append(keys, plugin.Commands...)

		hasNonEmpty := false
		for _, k := range keys {
			if strings.TrimSpace(k) != "" {
				hasNonEmpty = true
				break
			}
		}
		if !hasNonEmpty {
			keys = append(keys, filepath.Base(mf.Dir))
		}

		for _, key := range keys {
			key = strings.TrimSpace(strings.ToLower(key))
			if key == "" {
				continue
			}
			index[key] = plugin
		}
	}

	s.toolHandlerMu.Lock()
	s.toolHandlerIdx = index
	s.toolHandlerMu.Unlock()

	log.Printf("[Plugins] Loaded %d tool handler plugins (%d index entries)", pluginCount, len(index))
	return pluginCount, nil
}

// ToolHandlerPluginFor returns a tool handler plugin matching the supplied tool name.
// The plugin can be used for idle detection, cleaning, event extraction, and summarization.
func (s *Service) ToolHandlerPluginFor(name string) (*toolhandlers.JSPlugin, bool) {
	s.toolHandlerMu.RLock()
	defer s.toolHandlerMu.RUnlock()

	if len(s.toolHandlerIdx) == 0 {
		return nil, false
	}

	key := strings.TrimSpace(strings.ToLower(name))
	if key != "" {
		if plugin, ok := s.toolHandlerIdx[key]; ok {
			return plugin, true
		}
	}

	return nil, false
}

// GenerateIndex rebuilds the plugin detection index.
// Uses s.integrityChecker directly (not cachedChecker) because this is a
// standalone public method — each call is an independent operation with its
// own manifest discovery, so there is no duplicate hashing to avoid within
// a single invocation. Caching is only valuable in Reload/Start where the
// same checker result is consumed by three sequential load paths.
func (s *Service) GenerateIndex() error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	manifests, warnings := manifest.DiscoverWithWarnings(s.pluginDir)
	if len(warnings) > 0 {
		log.Printf("[Plugins] WARNING: %d plugin(s) skipped due to manifest errors:", len(warnings))
		for _, w := range warnings {
			log.Printf("[Plugins]   - %s: %v", w.Dir, w.Err)
		}
	}

	filtered := s.filterManifests(manifests, s.integrityChecker)
	generator := toolhandlers.NewIndexGeneratorWithRuntime(s.pluginDir, filtered, s.runtime())
	if err := generator.Generate(); err != nil {
		return err
	}
	log.Printf("[Plugins] Index generated: %d manifests included (%d discovered, %d parse errors)", len(filtered), len(manifests), len(warnings))
	return nil
}

// Reload re-discovers and reloads all plugins (pipeline cleaners, tool handlers, index).
// This is the hot-reload entry point called by the gRPC ReloadPlugins handler.
// Serialized by reloadMu to prevent concurrent file writes to handlers_index.json.
// The provided ctx controls cancellation of JS runtime calls during plugin loading.
func (s *Service) Reload(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	manifests, warnings := manifest.DiscoverWithWarnings(s.pluginDir)
	if len(warnings) > 0 {
		log.Printf("[Plugins] WARNING: %d plugin(s) skipped during reload:", len(warnings))
		for _, w := range warnings {
			log.Printf("[Plugins]   - %s: %v", w.Dir, w.Err)
		}
	}

	// Per-reload integrity cache: each plugin is verified at most once per
	// Reload() cycle, even though multiple load paths reference it.
	// Note: enabledChecker is intentionally NOT cached — it is a simple
	// boolean lookup (no I/O), whereas integrityChecker performs SHA-256
	// hashing over plugin files. The duplicate enabledChecker calls in
	// loadPipelinePlugins/loadToolHandlerPlugins + filterManifests are
	// negligible compared to disk I/O savings from integrity caching.
	checker := s.cachedChecker()

	var errs []string

	pipelineCount, err := s.loadPipelinePlugins(ctx, manifests, checker)
	if err != nil {
		log.Printf("[Plugins] pipeline reload error: %v", err)
		errs = append(errs, fmt.Sprintf("pipeline cleaners: %v", err))
	}

	handlerCount, err := s.loadToolHandlerPlugins(ctx, manifests, checker)
	if err != nil {
		log.Printf("[Plugins] tool handler reload error: %v", err)
		errs = append(errs, fmt.Sprintf("tool handlers: %v", err))
	}

	generator := toolhandlers.NewIndexGeneratorWithRuntime(s.pluginDir, s.filterManifests(manifests, checker), s.runtime())
	if err := generator.Generate(); err != nil {
		errs = append(errs, fmt.Sprintf("index generation: %v", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("reload partially failed (%d pipeline, %d handler loaded; %d discovered, %d parse errors): %s", pipelineCount, handlerCount, len(manifests), len(warnings), strings.Join(errs, "; "))
	}

	log.Printf("[Plugins] Hot reload completed: %d pipeline, %d handler loaded (%d discovered, %d parse errors)", pipelineCount, handlerCount, len(manifests), len(warnings))
	return nil
}

// Start implements runtime.Service to integrate with the daemon lifecycle.
// The provided ctx must remain valid for the service lifetime (not just the
// Start call), as the JS runtime's OnRestart callback uses it for retry
// delay cancellation.
func (s *Service) Start(ctx context.Context) error {
	if err := os.MkdirAll(s.pluginDir, 0o755); err != nil {
		return fmt.Errorf("ensure plugin dir: %w", err)
	}

	// Start supervised JS runtime for plugin execution with auto-restart.
	// If Bun is not installed, degrade gracefully — JS plugins are entirely
	// skipped to avoid loading metadata for plugins that cannot execute.
	jsRuntimeAvailable := true
	if err := s.startJSRuntime(ctx); err != nil {
		if jsrunner.IsRuntimeNotFound(err) {
			jsRuntimeAvailable = false
			log.Printf("[Plugins] WARNING: JS runtime not available — JS plugins disabled. %v", err)
		} else {
			return fmt.Errorf("JS runtime failed to start: %w", err)
		}
	}

	// Serialize with Reload()/GenerateIndex() to prevent concurrent
	// file writes to handlers_index.json during JS runtime restarts.
	// OnRestart callback is registered AFTER this section completes,
	// eliminating the race window where a crash could trigger a
	// redundant reload cycle before Start finishes loading.
	s.reloadMu.Lock()

	manifests, warnings := manifest.DiscoverWithWarnings(s.pluginDir)
	if len(warnings) > 0 {
		log.Printf("[Plugins] WARNING: %d plugin(s) skipped due to manifest errors:", len(warnings))
		for _, w := range warnings {
			log.Printf("[Plugins]   - %s: %v", w.Dir, w.Err)
		}
	}

	// Per-reload integrity cache: each plugin is verified at most once per
	// Start() cycle, even though multiple load paths reference it.
	checker := s.cachedChecker()

	var errs []string
	var pipelineCount, handlerCount int

	if jsRuntimeAvailable {
		var err error
		pipelineCount, err = s.loadPipelinePlugins(ctx, manifests, checker)
		if err != nil {
			log.Printf("[Plugins] pipeline load error: %v", err)
			errs = append(errs, fmt.Sprintf("pipeline cleaners: %v", err))
		}

		handlerCount, err = s.loadToolHandlerPlugins(ctx, manifests, checker)
		if err != nil {
			log.Printf("[Plugins] tool handler load error: %v", err)
			errs = append(errs, fmt.Sprintf("tool handlers: %v", err))
		}
	} else {
		// Clear any stale plugin entries from a previous Start() that had
		// a working runtime. Without this, a hot-restart (Shutdown→Start)
		// where Bun disappears would leave orphan entries that reference
		// a nil runtime, causing panics in the content pipeline.
		s.pipelineMu.Lock()
		s.pipelineIdx = make(map[string]*pipelinecleaners.PipelinePlugin)
		s.pipelineMu.Unlock()

		s.toolHandlerMu.Lock()
		s.toolHandlerIdx = make(map[string]*toolhandlers.JSPlugin)
		s.toolHandlerMu.Unlock()

		log.Printf("[Plugins] Skipping JS plugin loading — pipeline cleaners and tool handlers unavailable")
	}

	// Index generation still runs — it handles nil runtime and is needed
	// for adapter plugin manifest discovery regardless of JS availability.
	// Filter by integrity to prevent tampered plugins from executing JS during index generation.
	generator := toolhandlers.NewIndexGeneratorWithRuntime(s.pluginDir, s.filterManifests(manifests, checker), s.runtime())
	if err := generator.Generate(); err != nil {
		errs = append(errs, fmt.Sprintf("index generation: %v", err))
	}

	s.reloadMu.Unlock()

	if len(errs) > 0 {
		return fmt.Errorf("start partially failed (%d pipeline, %d handler loaded; %d discovered, %d parse errors): %s", pipelineCount, handlerCount, len(manifests), len(warnings), strings.Join(errs, "; "))
	}

	// Register OnRestart callback AFTER the reloadMu-protected section.
	// This ensures a JS runtime crash during initial loading doesn't
	// trigger a redundant reload — the callback only fires on subsequent
	// restarts after Start completes.
	s.registerOnRestart(ctx)

	log.Printf("[Plugins] Plugin service started: %d pipeline, %d handler loaded (%d discovered, %d parse errors)", pipelineCount, handlerCount, len(manifests), len(warnings))
	return nil
}

// Shutdown stops the plugin service and JS runtime.
// Waits for any in-progress Reload/Start/GenerateIndex to finish before
// killing the JS runtime, preventing spurious errors from dead-runtime calls.
func (s *Service) Shutdown(ctx context.Context) error {
	// Wait for any in-progress reload to finish before killing the runtime.
	// This prevents load methods from calling methods on a dead JS runtime.
	s.reloadMu.Lock()

	// Clear plugin indices while holding reloadMu — after shutdown, callers
	// should not receive plugin handles referencing a dead runtime.
	s.pipelineMu.Lock()
	s.pipelineIdx = make(map[string]*pipelinecleaners.PipelinePlugin)
	s.pipelineMu.Unlock()

	s.toolHandlerMu.Lock()
	s.toolHandlerIdx = make(map[string]*toolhandlers.JSPlugin)
	s.toolHandlerMu.Unlock()

	s.reloadMu.Unlock()

	s.supervisedRTMu.Lock()
	srt := s.supervisedRT
	s.supervisedRT = nil
	s.supervisedRTMu.Unlock()

	if srt != nil {
		if err := srt.Shutdown(ctx); err != nil {
			log.Printf("[Plugins] JS runtime shutdown error: %v", err)
			return err
		}
		log.Printf("[Plugins] JS runtime stopped")
	}
	if s.jsStdout != nil {
		_ = s.jsStdout.Close()
		s.jsStdout = nil
	}
	if s.jsStderr != nil {
		_ = s.jsStderr.Close()
		s.jsStderr = nil
	}
	s.closeJSPluginLogs()
	return nil
}

func (s *Service) pluginLogDir() (string, error) {
	if s.pluginDir == "" {
		return "", fmt.Errorf("plugin dir not configured")
	}
	instanceDir := filepath.Dir(s.pluginDir)
	dir := filepath.Join(instanceDir, "logs", "plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func (s *Service) openPluginLogFiles() {
	if s.jsStdout != nil {
		_ = s.jsStdout.Close()
		s.jsStdout = nil
	}
	if s.jsStderr != nil {
		_ = s.jsStderr.Close()
		s.jsStderr = nil
	}
	dir, err := s.pluginLogDir()
	if err != nil {
		log.Printf("[Plugins] failed to prepare plugin log dir: %v", err)
		return
	}
	stdoutPath := filepath.Join(dir, "plugin.jsruntime.runtime.stdout.log")
	stderrPath := filepath.Join(dir, "plugin.jsruntime.runtime.stderr.log")
	if f, err := os.OpenFile(stdoutPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
		log.Printf("[Plugins] failed to open runtime stdout log: %v", err)
	} else {
		s.jsStdout = f
	}
	if f, err := os.OpenFile(stderrPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
		log.Printf("[Plugins] failed to open runtime stderr log: %v", err)
	} else {
		s.jsStderr = f
	}
}

func (s *Service) closeJSPluginLogs() {
	s.jsPluginLogsMu.Lock()
	defer s.jsPluginLogsMu.Unlock()
	for _, f := range s.jsPluginLogs {
		_ = f.Close()
	}
	s.jsPluginLogs = make(map[string]*os.File)
}

func (s *Service) jsPluginLogFile(slug, stream string) (*os.File, error) {
	dir, err := s.pluginLogDir()
	if err != nil {
		return nil, err
	}
	safeSlug := sanitize.SafeSlug(slug)
	if safeSlug == "" {
		safeSlug = "unknown"
	}
	safeStream := sanitize.SafeSlug(stream)
	if safeStream == "" {
		safeStream = "stdout"
	}
	filename := fmt.Sprintf("plugin.%s.runtime.%s.log", safeSlug, safeStream)
	return os.OpenFile(filepath.Join(dir, filename), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

func (s *Service) handleJSRuntimeLog(entry jsruntime.LogEntry) {
	slug := strings.TrimSpace(entry.Plugin)
	if slug == "" {
		slug = "unknown"
	}
	stream := strings.TrimSpace(entry.Stream)
	if stream == "" {
		level := strings.ToLower(strings.TrimSpace(entry.Level))
		if level == "warn" || level == "error" {
			stream = "stderr"
		} else {
			stream = "stdout"
		}
	}
	key := slug + "|" + stream

	s.jsPluginLogsMu.Lock()
	f := s.jsPluginLogs[key]
	if f == nil {
		var err error
		f, err = s.jsPluginLogFile(slug, stream)
		if err == nil {
			s.jsPluginLogs[key] = f
		}
	}
	s.jsPluginLogsMu.Unlock()

	if f == nil {
		return
	}

	if msg := strings.TrimRight(entry.Message, "\n"); msg != "" {
		_, _ = f.WriteString(msg + "\n")
	}
}

// reloadWithRetry calls fn up to maxAttempts times with linear backoff
// (500ms * attempt). ctx controls retry delay cancellation — callers should
// pass a context that remains valid for the service lifetime.
func reloadWithRetry(ctx context.Context, fn func() error, maxAttempts int) error {
	if maxAttempts < 1 {
		return fmt.Errorf("reloadWithRetry: maxAttempts must be >= 1, got %d", maxAttempts)
	}
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if attempt < maxAttempts {
			log.Printf("[Plugins] Reload attempt %d/%d failed, retrying: %v", attempt, maxAttempts, err)
			delay := time.NewTimer(time.Duration(attempt) * 500 * time.Millisecond)
			select {
			case <-ctx.Done():
				delay.Stop()
				return fmt.Errorf("reload retry cancelled: %w", ctx.Err())
			case <-delay.C:
			}
		}
	}
	return fmt.Errorf("all %d reload attempts failed: %w", maxAttempts, err)
}

// startJSRuntime initializes the supervised persistent Bun subprocess.
// Uses embedded host.js script - no external file needed.
// The supervised runtime will automatically restart if the process crashes.
func (s *Service) startJSRuntime(ctx context.Context) error {
	s.openPluginLogFiles()
	// Pass empty strings - NewSupervised() will use embedded host.js and resolve bun.
	// WithRunDir tells the runtime where to place the IPC socket.
	srt, err := jsruntime.NewSupervised(
		ctx,
		"",
		"",
		jsruntime.WithRunDir(s.runDir),
		jsruntime.WithStdoutWriter(s.jsStdout),
		jsruntime.WithStderrWriter(s.jsStderr),
		jsruntime.WithLogHandler(s.handleJSRuntimeLog),
	)
	if err != nil {
		return err
	}

	s.supervisedRTMu.Lock()
	s.supervisedRT = srt
	s.supervisedRTMu.Unlock()

	log.Printf("[Plugins] JS runtime started (supervised, embedded host.js)")
	return nil
}

// registerOnRestart registers the OnRestart callback on the supervised runtime.
// Must be called after the initial plugin loading (reloadMu-protected section)
// completes, to prevent a race where a crash between startJSRuntime and
// reloadMu acquisition triggers a redundant reload cycle.
func (s *Service) registerOnRestart(ctx context.Context) {
	s.supervisedRTMu.RLock()
	srt := s.supervisedRT
	s.supervisedRTMu.RUnlock()

	if srt == nil {
		return
	}

	srt.OnRestart(func() {
		log.Printf("[Plugins] JS runtime restarted, reloading plugins...")
		reloadFn := func() error { return s.Reload(ctx) }
		if err := reloadWithRetry(ctx, reloadFn, 3); err != nil {
			log.Printf("[Plugins] WARNING: %v", err)
		} else {
			log.Printf("[Plugins] Plugin reload after restart completed")
		}
	})
}

// JSRuntime returns the active JS runtime, or nil if not available.
func (s *Service) JSRuntime() *jsruntime.Runtime {
	return s.runtime()
}

// runtime returns the underlying Runtime from the supervised runtime.
func (s *Service) runtime() *jsruntime.Runtime {
	s.supervisedRTMu.RLock()
	defer s.supervisedRTMu.RUnlock()
	if s.supervisedRT == nil {
		return nil
	}
	return s.supervisedRT.Runtime()
}
