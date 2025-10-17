package plugins

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/nupi-ai/nupi/internal/detector"
)

// Service manages plugin assets and metadata for the daemon.
type Service struct {
	pluginDir   string
	pipelineDir string
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

	return s.GenerateIndex()
}

// Shutdown is a no-op for plugin management.
func (s *Service) Shutdown(ctx context.Context) error {
	return nil
}
