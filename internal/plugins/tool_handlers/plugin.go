package toolhandlers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/jsruntime"
)

// JSPlugin represents a JavaScript plugin for tool detection and processing.
type JSPlugin struct {
	Name     string   `json:"name"`
	Commands []string `json:"commands"`
	Icon     string   `json:"icon,omitempty"`
	FilePath string   `json:"-"`
	Source   string   `json:"-"`
	// Capability flags - set during load based on exported functions
	HasDetectIdleState bool `json:"-"`
	HasClean           bool `json:"-"`
	HasExtractEvents   bool `json:"-"`
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

// LoadPluginWithRuntime loads a tool handler plugin and validates it via jsruntime.
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
		return nil, fmt.Errorf("tool handler: load %s: %w", filePath, err)
	}

	// Double-check the detect function exists
	if !meta.HasDetect {
		return nil, fmt.Errorf("tool handler: %s missing detect function", filePath)
	}

	plugin := &JSPlugin{
		FilePath:           filePath,
		Name:               meta.Name,
		Commands:           meta.Commands,
		Icon:               meta.Icon,
		HasDetectIdleState: meta.HasDetectIdleState,
		HasClean:           meta.HasClean,
		HasExtractEvents:   meta.HasExtractEvents,
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
		return false, fmt.Errorf("tool handler %s: jsruntime not available", p.Name)
	}

	result, err := p.callDetect(ctx, rt, output)
	if err != nil {
		// If plugin not loaded, try to reload and retry once
		if jsruntime.IsPluginNotLoadedError(err) {
			if reloadErr := p.reload(ctx, rt); reloadErr != nil {
				return false, fmt.Errorf("tool handler %s: reload failed: %w", p.Name, reloadErr)
			}
			// Retry after reload
			result, err = p.callDetect(ctx, rt, output)
			if err != nil {
				return false, fmt.Errorf("tool handler %s (after reload): %w", p.Name, err)
			}
		} else {
			return false, fmt.Errorf("tool handler %s: %w", p.Name, err)
		}
	}

	// Parse result - should be a boolean
	switch v := result.(type) {
	case bool:
		return v, nil
	case nil:
		return false, nil
	default:
		return false, fmt.Errorf("tool handler %s: unexpected result type %T", p.Name, result)
	}
}

// callDetect calls the detect function with timeout.
func (p *JSPlugin) callDetect(ctx context.Context, rt *jsruntime.Runtime, output string) (any, error) {
	callCtx, cancel := context.WithTimeout(ctx, constants.Duration1Second)
	defer cancel()
	return rt.Call(callCtx, p.FilePath, "detect", output)
}

// DetectIdleState calls the plugin's detectIdleState function.
// Returns nil if function not implemented or tool is not idle.
func (p *JSPlugin) DetectIdleState(ctx context.Context, rt *jsruntime.Runtime, buffer string) (*IdleState, error) {
	if !p.HasDetectIdleState {
		return nil, nil
	}

	if rt == nil {
		return nil, fmt.Errorf("tool handler %s: jsruntime not available", p.Name)
	}

	const timeout = constants.Duration100Milliseconds // fast check

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := rt.Call(callCtx, p.FilePath, "detectIdleState", buffer)
	if err != nil {
		// If plugin not loaded, try to reload and retry once
		if jsruntime.IsPluginNotLoadedError(err) {
			if reloadErr := p.reload(ctx, rt); reloadErr != nil {
				return nil, fmt.Errorf("tool handler %s: reload failed: %w", p.Name, reloadErr)
			}
			// Create fresh context for retry (original may be expired)
			retryCtx, retryCancel := context.WithTimeout(ctx, timeout)
			defer retryCancel()
			result, err = rt.Call(retryCtx, p.FilePath, "detectIdleState", buffer)
			if err != nil {
				return nil, fmt.Errorf("tool handler %s (after reload): %w", p.Name, err)
			}
		} else {
			return nil, fmt.Errorf("tool handler %s: %w", p.Name, err)
		}
	}

	return parseIdleState(result)
}

// Clean calls the plugin's clean function if available.
// Returns the original output if clean is not implemented.
func (p *JSPlugin) Clean(ctx context.Context, rt *jsruntime.Runtime, output string) (string, error) {
	if !p.HasClean {
		return output, nil // passthrough if not implemented
	}

	if rt == nil {
		return output, fmt.Errorf("tool handler %s: jsruntime not available", p.Name)
	}

	const timeout = constants.Duration1Second

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := rt.Call(callCtx, p.FilePath, "clean", output)
	if err != nil {
		// If plugin not loaded, try to reload and retry once
		if jsruntime.IsPluginNotLoadedError(err) {
			if reloadErr := p.reload(ctx, rt); reloadErr != nil {
				return output, fmt.Errorf("tool handler %s: reload failed: %w", p.Name, reloadErr)
			}
			// Create fresh context for retry (original may be expired)
			retryCtx, retryCancel := context.WithTimeout(ctx, timeout)
			defer retryCancel()
			result, err = rt.Call(retryCtx, p.FilePath, "clean", output)
			if err != nil {
				return output, fmt.Errorf("tool handler %s (after reload): %w", p.Name, err)
			}
		} else {
			return output, fmt.Errorf("tool handler %s: %w", p.Name, err)
		}
	}

	if s, ok := result.(string); ok {
		return s, nil
	}
	return output, nil
}

// ExtractEvents calls the plugin's extractEvents function if available.
// Returns nil if not implemented.
func (p *JSPlugin) ExtractEvents(ctx context.Context, rt *jsruntime.Runtime, output string) ([]Event, error) {
	if !p.HasExtractEvents {
		return nil, nil
	}

	if rt == nil {
		return nil, fmt.Errorf("tool handler %s: jsruntime not available", p.Name)
	}

	const timeout = constants.Duration2Seconds

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := rt.Call(callCtx, p.FilePath, "extractEvents", output)
	if err != nil {
		// If plugin not loaded, try to reload and retry once
		if jsruntime.IsPluginNotLoadedError(err) {
			if reloadErr := p.reload(ctx, rt); reloadErr != nil {
				return nil, fmt.Errorf("tool handler %s: reload failed: %w", p.Name, reloadErr)
			}
			// Create fresh context for retry (original may be expired)
			retryCtx, retryCancel := context.WithTimeout(ctx, timeout)
			defer retryCancel()
			result, err = rt.Call(retryCtx, p.FilePath, "extractEvents", output)
			if err != nil {
				return nil, fmt.Errorf("tool handler %s (after reload): %w", p.Name, err)
			}
		} else {
			return nil, fmt.Errorf("tool handler %s: %w", p.Name, err)
		}
	}

	return parseEvents(result)
}

// Process runs the full tool processing pipeline: detectIdleState, clean, extractEvents.
// Returns ProcessorResult with all gathered information.
// This is a convenience method that combines all processing steps.
func (p *JSPlugin) Process(ctx context.Context, rt *jsruntime.Runtime, buffer string) (*ProcessorResult, error) {
	result := &ProcessorResult{
		Cleaned: buffer,
	}

	// Detect idle state
	idleState, err := p.DetectIdleState(ctx, rt, buffer)
	if err != nil {
		return nil, fmt.Errorf("detectIdleState: %w", err)
	}
	result.IdleState = idleState

	// Clean output
	cleaned, err := p.Clean(ctx, rt, buffer)
	if err != nil {
		return nil, fmt.Errorf("clean: %w", err)
	}
	result.Cleaned = cleaned

	// Extract events
	events, err := p.ExtractEvents(ctx, rt, cleaned)
	if err != nil {
		return nil, fmt.Errorf("extractEvents: %w", err)
	}
	result.Events = events

	return result, nil
}

// parseIdleState converts a JS result to IdleState.
func parseIdleState(result any) (*IdleState, error) {
	if result == nil {
		return nil, nil
	}

	// Convert result to JSON and back to IdleState
	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal idle state: %w", err)
	}

	var state IdleState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal idle state: %w", err)
	}

	// Return nil if not idle
	if !state.IsIdle {
		return nil, nil
	}

	return &state, nil
}

// parseEvents converts a JS result to []Event.
func parseEvents(result any) ([]Event, error) {
	if result == nil {
		return nil, nil
	}

	// Convert result to JSON and back to []Event
	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal events: %w", err)
	}

	var events []Event
	if err := json.Unmarshal(data, &events); err != nil {
		return nil, fmt.Errorf("unmarshal events: %w", err)
	}

	return events, nil
}

// reload reloads the plugin into the runtime.
func (p *JSPlugin) reload(ctx context.Context, rt *jsruntime.Runtime) error {
	reloadCtx, cancel := context.WithTimeout(ctx, constants.Duration5Seconds)
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

// PluginIndex represents the auto-generated handlers_index.json.
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
