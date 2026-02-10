package plugins

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nupi-ai/nupi/internal/jsrunner"
	"github.com/nupi-ai/nupi/internal/jsruntime"
	"github.com/nupi-ai/nupi/internal/plugins/manifest"
	pipelinecleaners "github.com/nupi-ai/nupi/internal/plugins/pipeline_cleaners"
	toolhandlers "github.com/nupi-ai/nupi/internal/plugins/tool_handlers"
)

// Service manages plugin assets and metadata for the daemon.
type Service struct {
	pluginDir   string
	pipelineIdx map[string]*pipelinecleaners.PipelinePlugin
	pipelineMu  sync.RWMutex

	toolHandlerIdx map[string]*toolhandlers.JSPlugin // tool name -> plugin
	toolHandlerMu  sync.RWMutex

	supervisedRT   *jsruntime.SupervisedRuntime
	supervisedRTMu sync.RWMutex

	lastWarnings   []manifest.DiscoveryWarning
	lastWarningsMu sync.RWMutex
}

// NewService constructs a plugin service rooted in the given instance directory.
func NewService(instanceDir string) *Service {
	pluginDir := filepath.Join(instanceDir, "plugins")
	return &Service{
		pluginDir:       pluginDir,
		pipelineIdx:     make(map[string]*pipelinecleaners.PipelinePlugin),
		toolHandlerIdx: make(map[string]*toolhandlers.JSPlugin),
	}
}

// PluginDir returns the directory where plugins are stored.
func (s *Service) PluginDir() string {
	return s.pluginDir
}

// LoadPipelinePlugins rebuilds the in-memory cleaner registry.
func (s *Service) LoadPipelinePlugins() error {
	manifests, warnings := manifest.DiscoverWithWarnings(s.pluginDir)
	s.setWarnings(warnings)
	if len(warnings) > 0 {
		log.Printf("[Plugins] WARNING: %d plugin(s) skipped due to manifest errors:", len(warnings))
		for _, w := range warnings {
			log.Printf("[Plugins]   - %s: %v", w.Dir, w.Err)
		}
	}
	return s.loadPipelinePlugins(manifests)
}

func (s *Service) loadPipelinePlugins(manifests []*manifest.Manifest) error {
	index := make(map[string]*pipelinecleaners.PipelinePlugin)

	// Get jsruntime for loading plugins with validation
	rt := s.runtime()

	for _, mf := range manifests {
		if mf.Type != manifest.PluginTypePipelineCleaner {
			continue
		}

		mainPath, err := mf.MainPath()
		if err != nil {
			log.Printf("[Plugins] skip pipeline cleaner %s: %v", mf.Dir, err)
			continue
		}

		// Use jsruntime for loading when available (validates transform function)
		var plugin *pipelinecleaners.PipelinePlugin
		if rt != nil {
			plugin, err = pipelinecleaners.LoadPipelinePluginWithRuntime(context.Background(), rt, mainPath)
		} else {
			plugin, err = pipelinecleaners.LoadPipelinePlugin(mainPath)
		}
		if err != nil {
			log.Printf("[Plugins] skip pipeline cleaner %s: %v", mainPath, err)
			continue
		}

		if plugin.Name == "" && strings.TrimSpace(mf.Metadata.Name) != "" {
			plugin.Name = mf.Metadata.Name
		}

		keys := []string{plugin.Name}
		keys = append(keys, plugin.Commands...)
		if len(keys) == 0 {
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

	return nil
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
func (s *Service) LoadToolHandlerPlugins() error {
	manifests, warnings := manifest.DiscoverWithWarnings(s.pluginDir)
	s.setWarnings(warnings)
	return s.loadToolHandlerPlugins(manifests)
}

func (s *Service) loadToolHandlerPlugins(manifests []*manifest.Manifest) error {
	index := make(map[string]*toolhandlers.JSPlugin)

	rt := s.runtime()

	for _, mf := range manifests {
		if mf.Type != manifest.PluginTypeToolHandler {
			continue
		}

		mainPath, err := mf.MainPath()
		if err != nil {
			log.Printf("[Plugins] skip tool handler %s: %v", mf.Dir, err)
			continue
		}

		var plugin *toolhandlers.JSPlugin
		if rt != nil {
			plugin, err = toolhandlers.LoadPluginWithRuntime(context.Background(), rt, mainPath)
		} else {
			plugin, err = toolhandlers.LoadPlugin(mainPath)
		}
		if err != nil {
			log.Printf("[Plugins] skip tool handler %s: %v", mainPath, err)
			continue
		}

		if plugin.Name == "" && strings.TrimSpace(mf.Metadata.Name) != "" {
			plugin.Name = mf.Metadata.Name
		}

		// Index by plugin name and commands
		keys := []string{plugin.Name}
		keys = append(keys, plugin.Commands...)
		if len(keys) == 0 {
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

	log.Printf("[Plugins] Loaded %d tool handler plugins", len(index))
	return nil
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
func (s *Service) GenerateIndex() error {
	manifests, warnings := manifest.DiscoverWithWarnings(s.pluginDir)
	s.setWarnings(warnings)
	if len(warnings) > 0 {
		log.Printf("[Plugins] WARNING: %d plugin(s) skipped due to manifest errors:", len(warnings))
		for _, w := range warnings {
			log.Printf("[Plugins]   - %s: %v", w.Dir, w.Err)
		}
	}

	// Use jsruntime-aware index generator when available
	generator := toolhandlers.NewIndexGeneratorWithRuntime(s.pluginDir, manifests, s.runtime())
	if err := generator.Generate(); err != nil {
		return err
	}

	return nil
}

// Start implements runtime.Service to integrate with the daemon lifecycle.
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

	manifests, warnings := manifest.DiscoverWithWarnings(s.pluginDir)
	s.setWarnings(warnings)
	if len(warnings) > 0 {
		log.Printf("[Plugins] WARNING: %d plugin(s) skipped due to manifest errors:", len(warnings))
		for _, w := range warnings {
			log.Printf("[Plugins]   - %s: %v", w.Dir, w.Err)
		}
	}

	if jsRuntimeAvailable {
		if err := s.loadPipelinePlugins(manifests); err != nil {
			log.Printf("[Plugins] pipeline load error: %v", err)
		}

		if err := s.loadToolHandlerPlugins(manifests); err != nil {
			log.Printf("[Plugins] tool handler load error: %v", err)
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
	generator := toolhandlers.NewIndexGeneratorWithRuntime(s.pluginDir, manifests, s.runtime())
	if err := generator.Generate(); err != nil {
		return err
	}

	log.Printf("[Plugins] Plugin index generated successfully (%d valid, %d skipped)", len(manifests), len(warnings))
	return nil
}

// Shutdown stops the plugin service and JS runtime.
func (s *Service) Shutdown(ctx context.Context) error {
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
	return nil
}

// startJSRuntime initializes the supervised persistent Bun subprocess.
// Uses embedded host.js script - no external file needed.
// The supervised runtime will automatically restart if the process crashes.
func (s *Service) startJSRuntime(ctx context.Context) error {
	// Pass empty strings - NewSupervised() will use embedded host.js and resolve bun
	srt, err := jsruntime.NewSupervised(ctx, "", "")
	if err != nil {
		return err
	}

	s.supervisedRTMu.Lock()
	s.supervisedRT = srt
	s.supervisedRTMu.Unlock()

	// Register callback to reload plugins after runtime restart
	srt.OnRestart(func() {
		log.Printf("[Plugins] JS runtime restarted, reloading plugins...")
		if err := s.LoadPipelinePlugins(); err != nil {
			log.Printf("[Plugins] Failed to reload pipeline plugins after restart: %v", err)
		}
		if err := s.LoadToolHandlerPlugins(); err != nil {
			log.Printf("[Plugins] Failed to reload tool handler plugins after restart: %v", err)
		}
		if err := s.GenerateIndex(); err != nil {
			log.Printf("[Plugins] Failed to regenerate index after restart: %v", err)
		}
		log.Printf("[Plugins] Plugin reload after restart completed")
	})

	log.Printf("[Plugins] JS runtime started (supervised, embedded host.js)")
	return nil
}

// JSRuntime returns the active JS runtime, or nil if not available.
// Deprecated: Use runtime() internally. This is kept for backward compatibility.
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

// GetDiscoveryWarnings returns the most recent plugin discovery warnings.
// These represent manifests that were skipped during the last discovery operation.
func (s *Service) GetDiscoveryWarnings() []manifest.DiscoveryWarning {
	s.lastWarningsMu.RLock()
	defer s.lastWarningsMu.RUnlock()

	if len(s.lastWarnings) == 0 {
		return nil
	}

	// Return a copy to prevent external modification
	warnings := make([]manifest.DiscoveryWarning, len(s.lastWarnings))
	copy(warnings, s.lastWarnings)
	return warnings
}

func (s *Service) setWarnings(warnings []manifest.DiscoveryWarning) {
	s.lastWarningsMu.Lock()
	defer s.lastWarningsMu.Unlock()
	s.lastWarnings = warnings
}

// WarningsCount returns the count of current discovery warnings for metrics.
func (s *Service) WarningsCount() int {
	s.lastWarningsMu.RLock()
	defer s.lastWarningsMu.RUnlock()
	return len(s.lastWarnings)
}
