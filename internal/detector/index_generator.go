package detector

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// IndexGenerator creates index.json from plugin files.
type IndexGenerator struct {
	pluginDir string
}

// NewIndexGenerator creates a new index generator.
func NewIndexGenerator(pluginDir string) *IndexGenerator {
	return &IndexGenerator{
		pluginDir: pluginDir,
	}
}

// Generate scans all .js files and creates index.json.
func (g *IndexGenerator) Generate() error {
	indexPath := filepath.Join(g.pluginDir, "index.json")
	log.Printf("[IndexGenerator] Generating index at: %s", indexPath)

	if err := os.MkdirAll(g.pluginDir, 0o755); err != nil {
		return fmt.Errorf("failed to create plugin directory: %w", err)
	}

	files, err := filepath.Glob(filepath.Join(g.pluginDir, "*.js"))
	if err != nil {
		return fmt.Errorf("failed to scan plugins: %w", err)
	}

	index := make(PluginIndex)
	successCount := 0

	for _, file := range files {
		if strings.HasSuffix(file, "index.json") {
			continue
		}

		filename := filepath.Base(file)
		log.Printf("[IndexGenerator] Processing plugin: %s", filename)

		plugin, err := LoadPlugin(file)
		if err != nil {
			log.Printf("[IndexGenerator] Warning: failed to load %s: %v", filename, err)
			continue
		}

		for _, cmd := range plugin.Commands {
			if existing, exists := index[cmd]; exists {
				index[cmd] = append(existing, filename)
			} else {
				index[cmd] = []string{filename}
			}
		}

		successCount++
		log.Printf("[IndexGenerator] Plugin %s registered with %d commands: %v",
			filename, len(plugin.Commands), plugin.Commands)
	}

	if err := SaveIndex(indexPath, index); err != nil {
		return fmt.Errorf("failed to save index: %w", err)
	}

	log.Printf("[IndexGenerator] Index generated successfully: %d plugins, %d commands",
		successCount, len(index))

	return nil
}

// ListPlugins returns information about all plugins.
func (g *IndexGenerator) ListPlugins() ([]map[string]interface{}, error) {
	files, err := filepath.Glob(filepath.Join(g.pluginDir, "*.js"))
	if err != nil {
		return nil, err
	}

	var plugins []map[string]interface{}

	for _, file := range files {
		filename := filepath.Base(file)

		plugin, err := LoadPlugin(file)
		if err != nil {
			plugins = append(plugins, map[string]interface{}{
				"file":     filename,
				"error":    err.Error(),
				"commands": []string{},
			})
			continue
		}

		plugins = append(plugins, map[string]interface{}{
			"name":     plugin.Name,
			"file":     filename,
			"commands": plugin.Commands,
		})
	}

	return plugins, nil
}
