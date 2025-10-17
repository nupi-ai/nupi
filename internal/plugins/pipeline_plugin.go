package plugins

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dop251/goja"
)

// PipelinePlugin describes a JS cleaner used by content pipeline.
type PipelinePlugin struct {
	Name     string   `json:"name"`
	Commands []string `json:"commands"`
	FilePath string   `json:"-"`
	Source   string   `json:"-"`
}

// LoadPipelinePlugin loads a pipeline cleaner from disk.
func LoadPipelinePlugin(path string) (*PipelinePlugin, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pipeline plugin: read %s: %w", path, err)
	}

	vm := goja.New()
	exports := vm.NewObject()
	vm.Set("module", vm.NewObject())
	vm.Set("exports", exports)

	if _, err := vm.RunString(string(data)); err != nil {
		return nil, fmt.Errorf("pipeline plugin: execute %s: %w", path, err)
	}

	moduleObj := vm.Get("module")
	if moduleObj == nil {
		moduleObj = vm.Get("exports")
	} else {
		moduleExports := moduleObj.ToObject(vm).Get("exports")
		if moduleExports != nil {
			exports = moduleExports.ToObject(vm)
		}
	}

	plugin := &PipelinePlugin{
		FilePath: path,
		Source:   string(data),
	}

	if name := exports.Get("name"); name != nil {
		plugin.Name = name.String()
	} else {
		plugin.Name = filepath.Base(path)
	}

	if commands := exports.Get("commands"); commands != nil {
		if arr, ok := commands.Export().([]interface{}); ok {
			for _, cmd := range arr {
				if str, ok := cmd.(string); ok {
					plugin.Commands = append(plugin.Commands, str)
				}
			}
		}
	}

	if transform := exports.Get("transform"); transform != nil {
		if _, ok := goja.AssertFunction(transform); !ok {
			return nil, fmt.Errorf("pipeline plugin %s: transform must be function", path)
		}
	} else {
		return nil, fmt.Errorf("pipeline plugin %s: missing transform function", path)
	}

	return plugin, nil
}
