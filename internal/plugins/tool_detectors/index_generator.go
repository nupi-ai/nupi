package tooldetectors

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/nupi-ai/nupi/internal/jsruntime"
	"github.com/nupi-ai/nupi/internal/plugins/manifest"
)

// IndexGenerator creates detectors_index.json from detector plugin manifests.
type IndexGenerator struct {
	pluginDir string
	manifests []*manifest.Manifest
	jsRuntime *jsruntime.Runtime
}

// NewIndexGenerator creates a new index generator.
func NewIndexGenerator(pluginDir string, manifests []*manifest.Manifest) *IndexGenerator {
	return &IndexGenerator{
		pluginDir: pluginDir,
		manifests: manifests,
	}
}

// NewIndexGeneratorWithRuntime creates a new index generator with jsruntime support.
func NewIndexGeneratorWithRuntime(pluginDir string, manifests []*manifest.Manifest, rt *jsruntime.Runtime) *IndexGenerator {
	return &IndexGenerator{
		pluginDir: pluginDir,
		manifests: manifests,
		jsRuntime: rt,
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
		var warnings []manifest.DiscoveryWarning
		manifests, warnings = manifest.DiscoverWithWarnings(g.pluginDir)
		for _, w := range warnings {
			log.Printf("[IndexGenerator] skipped plugin in %s: %v", w.Dir, w.Err)
		}
	}

	index := make(PluginIndex)
	successCount := 0

	for _, mf := range manifests {
		if mf.Type != manifest.PluginTypeToolDetector {
			continue
		}

		mainPath, err := mf.MainPath()
		if err != nil {
			log.Printf("[IndexGenerator] Warning: skip detector %s: %v", mf.Dir, err)
			continue
		}

		// Use jsruntime for loading when available (validates detect function)
		// 10s timeout to prevent hanging on malformed plugins
		var plugin *JSPlugin
		if g.jsRuntime != nil {
			loadCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			plugin, err = LoadPluginWithRuntime(loadCtx, g.jsRuntime, mainPath)
			cancel()
		} else {
			plugin, err = LoadPlugin(mainPath)
		}
		if err != nil {
			log.Printf("[IndexGenerator] Warning: failed to load %s: %v", mainPath, err)
			continue
		}

		relativePath, err := mf.RelativeMainPath(g.pluginDir)
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
		var warnings []manifest.DiscoveryWarning
		manifests, warnings = manifest.DiscoverWithWarnings(g.pluginDir)
		for _, w := range warnings {
			log.Printf("[IndexGenerator] skipped plugin in %s: %v", w.Dir, w.Err)
		}
	}

	var plugins []map[string]interface{}

	for _, mf := range manifests {
		if mf.Type != manifest.PluginTypeToolDetector {
			continue
		}

		mainPath, err := mf.MainPath()
		if err != nil {
			plugins = append(plugins, map[string]interface{}{
				"file":     mf.Dir,
				"error":    err.Error(),
				"commands": []string{},
			})
			continue
		}

		rel, err := mf.RelativeMainPath(g.pluginDir)
		if err != nil {
			rel = mainPath
		}

		// Use jsruntime for loading when available
		// 10s timeout to prevent hanging on malformed plugins
		var plugin *JSPlugin
		if g.jsRuntime != nil {
			loadCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			plugin, err = LoadPluginWithRuntime(loadCtx, g.jsRuntime, mainPath)
			cancel()
		} else {
			plugin, err = LoadPlugin(mainPath)
		}
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
