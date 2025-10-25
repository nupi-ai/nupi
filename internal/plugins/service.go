package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nupi-ai/nupi/internal/detector"
	"github.com/nupi-ai/nupi/internal/pluginmanifest"
)

// Service manages plugin assets and metadata for the daemon.
type Service struct {
	pluginDir   string
	pipelineIdx map[string]*PipelinePlugin
	pipelineMu  sync.RWMutex
}

// NewService constructs a plugin service rooted in the given instance directory.
func NewService(instanceDir string) *Service {
	pluginDir := filepath.Join(instanceDir, "plugins")
	return &Service{
		pluginDir:   pluginDir,
		pipelineIdx: make(map[string]*PipelinePlugin),
	}
}

// PluginDir returns the directory where plugins are stored.
func (s *Service) PluginDir() string {
	return s.pluginDir
}

// LoadPipelinePlugins rebuilds the in-memory cleaner registry.
func (s *Service) LoadPipelinePlugins() error {
	manifests, err := pluginmanifest.Discover(s.pluginDir)
	if err != nil {
		return err
	}
	return s.loadPipelinePlugins(manifests)
}

func (s *Service) loadPipelinePlugins(manifests []*pluginmanifest.Manifest) error {
	index := make(map[string]*PipelinePlugin)
	for _, manifest := range manifests {
		if manifest.Type != pluginmanifest.PluginTypePipelineCleaner {
			continue
		}

		mainPath, err := manifest.MainPath()
		if err != nil {
			log.Printf("[Plugins] skip pipeline cleaner %s: %v", manifest.Dir, err)
			continue
		}

		plugin, err := LoadPipelinePlugin(mainPath)
		if err != nil {
			log.Printf("[Plugins] skip pipeline cleaner %s: %v", mainPath, err)
			continue
		}

		if plugin.Name == "" && strings.TrimSpace(manifest.Metadata.Name) != "" {
			plugin.Name = manifest.Metadata.Name
		}

		keys := []string{plugin.Name}
		keys = append(keys, plugin.Commands...)
		if len(keys) == 0 {
			keys = append(keys, filepath.Base(manifest.Dir))
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

	if err := s.writePipelineIndex(index); err != nil {
		log.Printf("[Plugins] pipeline index write error: %v", err)
	}
	return nil
}

// PipelinePluginFor returns a cleaner matching the supplied name.
func (s *Service) PipelinePluginFor(name string) (*PipelinePlugin, bool) {
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

func (s *Service) writePipelineIndex(index map[string]*PipelinePlugin) error {
	manifest := make(map[string]string, len(index))
	for key, plugin := range index {
		if plugin == nil {
			continue
		}
		rel, err := filepath.Rel(s.pluginDir, plugin.FilePath)
		if err != nil {
			rel = plugin.FilePath
		}
		manifest[key] = rel
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("pipeline index marshal: %w", err)
	}

	path := filepath.Join(s.pluginDir, "pipeline_cleaners_index.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("pipeline index ensure dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("pipeline index write: %w", err)
	}

	return nil
}

// GenerateIndex rebuilds the plugin detection index.
func (s *Service) GenerateIndex() error {
	manifests, err := pluginmanifest.Discover(s.pluginDir)
	if err != nil {
		return err
	}

	generator := detector.NewIndexGenerator(s.pluginDir, manifests)
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

	manifests, err := pluginmanifest.Discover(s.pluginDir)
	if err != nil {
		return err
	}

	if err := s.loadPipelinePlugins(manifests); err != nil {
		log.Printf("[Plugins] pipeline load error: %v", err)
	}

	generator := detector.NewIndexGenerator(s.pluginDir, manifests)
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
