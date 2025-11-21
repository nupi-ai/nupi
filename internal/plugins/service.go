package plugins

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nupi-ai/nupi/internal/plugins/manifest"
	pipelinecleaners "github.com/nupi-ai/nupi/internal/plugins/pipeline_cleaners"
	tooldetectors "github.com/nupi-ai/nupi/internal/plugins/tool_detectors"
)

// Service manages plugin assets and metadata for the daemon.
type Service struct {
	pluginDir   string
	pipelineIdx map[string]*pipelinecleaners.PipelinePlugin
	pipelineMu  sync.RWMutex
}

// NewService constructs a plugin service rooted in the given instance directory.
func NewService(instanceDir string) *Service {
	pluginDir := filepath.Join(instanceDir, "plugins")
	return &Service{
		pluginDir:   pluginDir,
		pipelineIdx: make(map[string]*pipelinecleaners.PipelinePlugin),
	}
}

// PluginDir returns the directory where plugins are stored.
func (s *Service) PluginDir() string {
	return s.pluginDir
}

// LoadPipelinePlugins rebuilds the in-memory cleaner registry.
func (s *Service) LoadPipelinePlugins() error {
	manifests, warnings := manifest.DiscoverWithWarnings(s.pluginDir)
	for _, w := range warnings {
		log.Printf("[Plugins] skipped plugin in %s: %v", w.Dir, w.Err)
	}
	return s.loadPipelinePlugins(manifests)
}

func (s *Service) loadPipelinePlugins(manifests []*manifest.Manifest) error {
	index := make(map[string]*pipelinecleaners.PipelinePlugin)
	for _, mf := range manifests {
		if mf.Type != manifest.PluginTypePipelineCleaner {
			continue
		}

		mainPath, err := mf.MainPath()
		if err != nil {
			log.Printf("[Plugins] skip pipeline cleaner %s: %v", mf.Dir, err)
			continue
		}

		plugin, err := pipelinecleaners.LoadPipelinePlugin(mainPath)
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

// GenerateIndex rebuilds the plugin detection index.
func (s *Service) GenerateIndex() error {
	manifests, warnings := manifest.DiscoverWithWarnings(s.pluginDir)
	for _, w := range warnings {
		log.Printf("[Plugins] skipped plugin in %s: %v", w.Dir, w.Err)
	}

	generator := tooldetectors.NewIndexGenerator(s.pluginDir, manifests)
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

	manifests, warnings := manifest.DiscoverWithWarnings(s.pluginDir)
	for _, w := range warnings {
		log.Printf("[Plugins] skipped plugin in %s: %v", w.Dir, w.Err)
	}

	if err := s.loadPipelinePlugins(manifests); err != nil {
		log.Printf("[Plugins] pipeline load error: %v", err)
	}

	generator := tooldetectors.NewIndexGenerator(s.pluginDir, manifests)
	if err := generator.Generate(); err != nil {
		return err
	}

	log.Printf("[Plugins] Plugin index generated successfully")
	return nil
}

// Shutdown is a no-op for plugin management.
func (s *Service) Shutdown(ctx context.Context) error {
	return nil
}
