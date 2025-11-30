package pipelinecleaners

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nupi-ai/nupi/internal/jsruntime"
)

// PipelinePlugin describes a JS cleaner used by content pipeline.
type PipelinePlugin struct {
	Name     string   `json:"name"`
	Commands []string `json:"commands"`
	FilePath string   `json:"-"`
	Source   string   `json:"-"`
}

// LoadPipelinePlugin loads a pipeline cleaner from disk.
// This only reads the file and stores metadata. Validation happens when loaded into jsruntime.
func LoadPipelinePlugin(path string) (*PipelinePlugin, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pipeline plugin: read %s: %w", path, err)
	}

	plugin := &PipelinePlugin{
		FilePath: path,
		Source:   string(data),
		Name:     filepath.Base(path),
	}

	return plugin, nil
}

// LoadPipelinePluginWithRuntime loads a pipeline cleaner and validates it via jsruntime.
// Returns plugin metadata extracted by the JS runtime.
// Fails if the plugin doesn't export a transform function.
// Uses 10s timeout to prevent hanging on malformed plugins.
func LoadPipelinePluginWithRuntime(ctx context.Context, rt *jsruntime.Runtime, path string) (*PipelinePlugin, error) {
	if rt == nil {
		// Fallback to basic loading without validation
		return LoadPipelinePlugin(path)
	}

	// Add 10s deadline to prevent hanging on malformed plugins
	loadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	meta, err := rt.LoadPluginWithOptions(loadCtx, path, jsruntime.LoadPluginOptions{
		RequireFunctions: []string{"transform"},
	})
	if err != nil {
		return nil, fmt.Errorf("pipeline plugin: load %s: %w", path, err)
	}

	// Double-check the transform function exists
	if !meta.HasTransform {
		return nil, fmt.Errorf("pipeline plugin: %s missing transform function", path)
	}

	plugin := &PipelinePlugin{
		FilePath: path,
		Name:     meta.Name,
		Commands: meta.Commands,
	}

	// Read source for reference (may be needed for logging/debugging)
	data, err := os.ReadFile(path)
	if err == nil {
		plugin.Source = string(data)
	}

	return plugin, nil
}

// TransformInput is the input structure passed to transform function.
type TransformInput struct {
	Text        string            `json:"text"`
	Annotations map[string]string `json:"annotations"`
}

// TransformOutput is the output structure from transform function.
type TransformOutput struct {
	Text        string            `json:"text"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Transform runs the plugin's transform function via jsruntime.
// If the plugin is not loaded (e.g., after runtime restart), it will reload and retry once.
func (p *PipelinePlugin) Transform(ctx context.Context, rt *jsruntime.Runtime, input TransformInput) (*TransformOutput, error) {
	if rt == nil {
		return nil, fmt.Errorf("pipeline plugin %s: jsruntime not available", p.Name)
	}

	result, err := p.callTransform(ctx, rt, input)
	if err != nil {
		// If plugin not loaded, try to reload and retry once
		if jsruntime.IsPluginNotLoadedError(err) {
			if reloadErr := p.reload(ctx, rt); reloadErr != nil {
				return nil, fmt.Errorf("pipeline plugin %s: reload failed: %w", p.Name, reloadErr)
			}
			// Retry after reload
			result, err = p.callTransform(ctx, rt, input)
			if err != nil {
				return nil, fmt.Errorf("pipeline plugin %s (after reload): %w", p.Name, err)
			}
		} else {
			return nil, fmt.Errorf("pipeline plugin %s: %w", p.Name, err)
		}
	}

	// Parse result
	switch v := result.(type) {
	case nil:
		return &TransformOutput{Text: input.Text}, nil
	case string:
		return &TransformOutput{Text: v}, nil
	case map[string]any:
		output := &TransformOutput{Text: input.Text}
		if text, ok := v["text"].(string); ok {
			output.Text = text
		}
		if ann, ok := v["annotations"].(map[string]any); ok {
			output.Annotations = make(map[string]string)
			for k, val := range ann {
				if s, ok := val.(string); ok {
					output.Annotations[k] = s
				}
			}
		}
		return output, nil
	default:
		return nil, fmt.Errorf("pipeline plugin %s: unexpected result type %T", p.Name, result)
	}
}

// callTransform calls the transform function with timeout.
func (p *PipelinePlugin) callTransform(ctx context.Context, rt *jsruntime.Runtime, input TransformInput) (any, error) {
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return rt.Call(callCtx, p.FilePath, "transform", map[string]any{
		"text":        input.Text,
		"annotations": input.Annotations,
	})
}

// reload reloads the plugin into the runtime.
func (p *PipelinePlugin) reload(ctx context.Context, rt *jsruntime.Runtime) error {
	reloadCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := rt.LoadPluginWithOptions(reloadCtx, p.FilePath, jsruntime.LoadPluginOptions{
		RequireFunctions: []string{"transform"},
	})
	return err
}
