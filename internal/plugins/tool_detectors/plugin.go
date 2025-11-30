package tooldetectors

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nupi-ai/nupi/internal/jsruntime"
)

// JSPlugin represents a JavaScript plugin for tool detection.
type JSPlugin struct {
	Name     string   `json:"name"`
	Commands []string `json:"commands"`
	Icon     string   `json:"icon,omitempty"`
	FilePath string   `json:"-"`
	Source   string   `json:"-"`
}

// LoadPlugin loads a JavaScript plugin from file.
// This only reads the file and stores metadata. Validation happens when loaded into jsruntime.
func LoadPlugin(filePath string) (*JSPlugin, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read plugin %s: %w", filePath, err)
	}

	plugin := &JSPlugin{
		FilePath: filePath,
		Source:   string(content),
		Name:     filepath.Base(filePath),
	}

	return plugin, nil
}

// LoadPluginWithRuntime loads a tool detector plugin and validates it via jsruntime.
// Returns plugin metadata extracted by the JS runtime.
// Fails if the plugin doesn't export a detect function.
func LoadPluginWithRuntime(ctx context.Context, rt *jsruntime.Runtime, filePath string) (*JSPlugin, error) {
	if rt == nil {
		// Fallback to basic loading without validation
		return LoadPlugin(filePath)
	}

	meta, err := rt.LoadPluginWithOptions(ctx, filePath, jsruntime.LoadPluginOptions{
		RequireFunctions: []string{"detect"},
	})
	if err != nil {
		return nil, fmt.Errorf("tool detector: load %s: %w", filePath, err)
	}

	// Double-check the detect function exists
	if !meta.HasDetect {
		return nil, fmt.Errorf("tool detector: %s missing detect function", filePath)
	}

	plugin := &JSPlugin{
		FilePath: filePath,
		Name:     meta.Name,
		Commands: meta.Commands,
		Icon:     meta.Icon,
	}

	// Read source for reference (may be needed for logging/debugging)
	data, err := os.ReadFile(filePath)
	if err == nil {
		plugin.Source = string(data)
	}

	return plugin, nil
}

// Detect runs the plugin's detect function with the given output via jsruntime.
// If the plugin is not loaded (e.g., after runtime restart), it will reload and retry once.
func (p *JSPlugin) Detect(ctx context.Context, rt *jsruntime.Runtime, output string) (bool, error) {
	if rt == nil {
		return false, fmt.Errorf("tool detector %s: jsruntime not available", p.Name)
	}

	result, err := p.callDetect(ctx, rt, output)
	if err != nil {
		// If plugin not loaded, try to reload and retry once
		if jsruntime.IsPluginNotLoadedError(err) {
			if reloadErr := p.reload(ctx, rt); reloadErr != nil {
				return false, fmt.Errorf("tool detector %s: reload failed: %w", p.Name, reloadErr)
			}
			// Retry after reload
			result, err = p.callDetect(ctx, rt, output)
			if err != nil {
				return false, fmt.Errorf("tool detector %s (after reload): %w", p.Name, err)
			}
		} else {
			return false, fmt.Errorf("tool detector %s: %w", p.Name, err)
		}
	}

	// Parse result - should be a boolean
	switch v := result.(type) {
	case bool:
		return v, nil
	case nil:
		return false, nil
	default:
		return false, fmt.Errorf("tool detector %s: unexpected result type %T", p.Name, result)
	}
}

// callDetect calls the detect function with timeout.
func (p *JSPlugin) callDetect(ctx context.Context, rt *jsruntime.Runtime, output string) (any, error) {
	callCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	return rt.Call(callCtx, p.FilePath, "detect", output)
}

// reload reloads the plugin into the runtime.
func (p *JSPlugin) reload(ctx context.Context, rt *jsruntime.Runtime) error {
	reloadCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := rt.LoadPluginWithOptions(reloadCtx, p.FilePath, jsruntime.LoadPluginOptions{
		RequireFunctions: []string{"detect"},
	})
	return err
}

// GetInfo returns plugin information as JSON.
func (p *JSPlugin) GetInfo() string {
	info := map[string]interface{}{
		"name":     p.Name,
		"commands": p.Commands,
		"file":     filepath.Base(p.FilePath),
	}

	data, _ := json.MarshalIndent(info, "", "  ")
	return string(data)
}

// PluginIndex represents the auto-generated detectors_index.json.
type PluginIndex map[string][]string

// LoadIndex loads the plugin index from file.
func LoadIndex(indexPath string) (PluginIndex, error) {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return make(PluginIndex), nil
		}
		return nil, err
	}

	var index PluginIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("failed to parse index: %w", err)
	}

	return index, nil
}

// SaveIndex saves the plugin index to file.
func SaveIndex(indexPath string, index PluginIndex) error {
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(indexPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	return os.WriteFile(indexPath, data, 0o644)
}
