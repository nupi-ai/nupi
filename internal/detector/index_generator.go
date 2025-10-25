package detector

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/nupi-ai/nupi/internal/pluginmanifest"
)

// IndexGenerator creates detectors_index.json from detector plugin manifests.
type IndexGenerator struct {
	pluginDir string
	manifests []*pluginmanifest.Manifest
}

// NewIndexGenerator creates a new index generator.
func NewIndexGenerator(pluginDir string, manifests []*pluginmanifest.Manifest) *IndexGenerator {
	return &IndexGenerator{
		pluginDir: pluginDir,
		manifests: manifests,
	}
}

// Generate scans detector manifests and creates detectors_index.json.
func (g *IndexGenerator) Generate() error {
	indexPath := filepath.Join(g.pluginDir, "detectors_index.json")
	log.Printf("[IndexGenerator] Generating index at: %s", indexPath)

	if err := os.MkdirAll(g.pluginDir, 0o755); err != nil {
		return fmt.Errorf("failed to create plugin directory: %w", err)
	}

	manifests := g.manifests
	if manifests == nil {
		var err error
		manifests, err = pluginmanifest.Discover(g.pluginDir)
		if err != nil {
			return err
		}
	}

	index := make(PluginIndex)
	successCount := 0

	for _, manifest := range manifests {
		if manifest.Type != pluginmanifest.PluginTypeDetector {
			continue
		}

		mainPath, err := manifest.MainPath()
		if err != nil {
			log.Printf("[IndexGenerator] Warning: skip detector %s: %v", manifest.Dir, err)
			continue
		}

		plugin, err := LoadPlugin(mainPath)
		if err != nil {
			log.Printf("[IndexGenerator] Warning: failed to load %s: %v", mainPath, err)
			continue
		}

		relativePath, err := manifest.RelativeMainPath(g.pluginDir)
		if err != nil {
			log.Printf("[IndexGenerator] Warning: relative path error for %s: %v", mainPath, err)
			relativePath = mainPath
		}

		for _, cmd := range plugin.Commands {
			index[cmd] = append(index[cmd], relativePath)
		}

		successCount++
		log.Printf("[IndexGenerator] Detector %s registered with %d commands: %v",
			relativePath, len(plugin.Commands), plugin.Commands)
	}

	if err := SaveIndex(indexPath, index); err != nil {
		return fmt.Errorf("failed to save index: %w", err)
	}

	log.Printf("[IndexGenerator] Index generated successfully: %d plugins, %d commands",
		successCount, len(index))

	return nil
}

// ListPlugins returns information about all detector plugins.
func (g *IndexGenerator) ListPlugins() ([]map[string]interface{}, error) {
	manifests := g.manifests
	if manifests == nil {
		var err error
		manifests, err = pluginmanifest.Discover(g.pluginDir)
		if err != nil {
			return nil, err
		}
	}

	var plugins []map[string]interface{}

	for _, manifest := range manifests {
		if manifest.Type != pluginmanifest.PluginTypeDetector {
			continue
		}

		mainPath, err := manifest.MainPath()
		if err != nil {
			plugins = append(plugins, map[string]interface{}{
				"file":     manifest.Dir,
				"error":    err.Error(),
				"commands": []string{},
			})
			continue
		}

		rel, err := manifest.RelativeMainPath(g.pluginDir)
		if err != nil {
			rel = mainPath
		}

		plugin, err := LoadPlugin(mainPath)
		if err != nil {
			plugins = append(plugins, map[string]interface{}{
				"file":     rel,
				"error":    err.Error(),
				"commands": []string{},
			})
			continue
		}

		plugins = append(plugins, map[string]interface{}{
			"name":     plugin.Name,
			"file":     rel,
			"commands": plugin.Commands,
		})
	}

	return plugins, nil
}
