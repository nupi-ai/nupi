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
)

// Service manages plugin assets and metadata for the daemon.
type Service struct {
	pluginDir   string
	pipelineDir string
	pipelineIdx map[string]*PipelinePlugin
	pipelineMu  sync.RWMutex
	extract     func(string) error
}

// Option configures optional behaviour on the Service.
type Option func(*Service)

// WithExtractor overrides the function used to materialise embedded plugins.
func WithExtractor(extractor func(string) error) Option {
	return func(s *Service) {
		s.extract = extractor
	}
}

// NewService constructs a plugin service rooted in the given instance directory.
func NewService(instanceDir string, opts ...Option) *Service {
	pluginDir := filepath.Join(instanceDir, "plugins")
	pipelineDir := filepath.Join(instanceDir, "pipeline")
	svc := &Service{
		pluginDir:   pluginDir,
		pipelineDir: pipelineDir,
		pipelineIdx: make(map[string]*PipelinePlugin),
		extract:     ExtractEmbedded,
	}

	for _, opt := range opts {
		opt(svc)
	}

	return svc
}

// PluginDir returns the directory where plugins are stored.
func (s *Service) PluginDir() string {
	return s.pluginDir
}

// PipelineDir returns the directory containing JS cleaners.
func (s *Service) PipelineDir() string {
	return s.pipelineDir
}

// LoadPipelinePlugins rebuilds the in-memory cleaner registry.
func (s *Service) LoadPipelinePlugins() error {
	entries, err := os.ReadDir(s.pipelineDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	index := make(map[string]*PipelinePlugin)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".js" {
			continue
		}
		path := filepath.Join(s.pipelineDir, entry.Name())
		plugin, err := LoadPipelinePlugin(path)
		if err != nil {
			log.Printf("[Plugins] skip pipeline plugin %s: %v", path, err)
			continue
		}

		keys := []string{plugin.Name}
		keys = append(keys, plugin.Commands...)
		if len(keys) == 0 {
			keys = append(keys, strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())))
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
		manifest[key] = filepath.Base(plugin.FilePath)
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("pipeline index marshal: %w", err)
	}

	path := filepath.Join(s.pipelineDir, "index.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("pipeline index write: %w", err)
	}
	return nil
}

// SyncEmbedded updates the embedded plugin set on disk.
func (s *Service) SyncEmbedded() error {
	if s.extract == nil {
		return fmt.Errorf("plugin extractor is not configured")
	}

	log.Printf("[Plugins] Updating embedded plugins...")
	return s.extract(s.pluginDir)
}

// GenerateIndex rebuilds the plugin detection index.
func (s *Service) GenerateIndex() error {
	generator := detector.NewIndexGenerator(s.pluginDir)
	if err := generator.Generate(); err != nil {
		return err
	}

	log.Printf("[Plugins] Plugin index generated successfully")
	return nil
}

// Start implements runtime.Service to integrate with the daemon lifecycle.
func (s *Service) Start(ctx context.Context) error {
	if err := os.MkdirAll(s.pipelineDir, 0o755); err != nil {
		return fmt.Errorf("plugin service: ensure pipeline dir: %w", err)
	}
	if err := s.SyncEmbedded(); err != nil {
		return err
	}

	if err := s.LoadPipelinePlugins(); err != nil {
		log.Printf("[Plugins] pipeline load error: %v", err)
	}

	return s.GenerateIndex()
}

// Shutdown is a no-op for plugin management.
func (s *Service) Shutdown(ctx context.Context) error {
	return nil
}
