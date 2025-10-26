package tooldetectors

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/dop251/goja"
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
func LoadPlugin(filePath string) (*JSPlugin, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read plugin %s: %w", filePath, err)
	}

	vm := goja.New()

	exports := vm.NewObject()
	vm.Set("module", vm.NewObject())
	vm.Set("exports", exports)

	_, err = vm.RunString(string(content))
	if err != nil {
		return nil, fmt.Errorf("failed to execute plugin %s: %w", filePath, err)
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

	plugin := &JSPlugin{
		FilePath: filePath,
		Source:   string(content),
	}

	if name := exports.Get("name"); name != nil {
		plugin.Name = name.String()
	} else {
		plugin.Name = filepath.Base(filePath)
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

	if icon := exports.Get("icon"); icon != nil {
		plugin.Icon = icon.String()
	}

	if detectProp := exports.Get("detect"); detectProp != nil {
		if _, ok := goja.AssertFunction(detectProp); !ok {
			return nil, fmt.Errorf("plugin %s: detect must be a function", filePath)
		}
	} else {
		return nil, fmt.Errorf("plugin %s: missing detect function", filePath)
	}

	return plugin, nil
}

// Detect runs the plugin's detect function with the given output.
func (p *JSPlugin) Detect(output string) (bool, error) {
	type detectResult struct {
		result bool
		err    error
	}
	resultChan := make(chan detectResult, 1)

	vm := goja.New()

	console := vm.NewObject()
	console.Set("log", func(call goja.FunctionCall) goja.Value {
		var args []interface{}
		for _, arg := range call.Arguments {
			args = append(args, arg.Export())
		}
		log.Println(args...)
		return goja.Undefined()
	})
	vm.Set("console", console)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(goja.InterruptedError); ok {
					return
				}
				resultChan <- detectResult{false, fmt.Errorf("plugin panicked: %v", r)}
			}
		}()

		exports := vm.NewObject()
		vm.Set("module", vm.NewObject())
		vm.Set("exports", exports)

		_, err := vm.RunString(p.Source)
		if err != nil {
			resultChan <- detectResult{false, fmt.Errorf("failed to load plugin: %w", err)}
			return
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

		detectProp := exports.Get("detect")
		if detectProp == nil {
			resultChan <- detectResult{false, fmt.Errorf("detect function not found")}
			return
		}

		detectFn, ok := goja.AssertFunction(detectProp)
		if !ok {
			resultChan <- detectResult{false, fmt.Errorf("detect is not a function")}
			return
		}

		result, err := detectFn(goja.Undefined(), vm.ToValue(output))
		if err != nil {
			resultChan <- detectResult{false, fmt.Errorf("detect function error: %w", err)}
			return
		}

		resultChan <- detectResult{result.ToBoolean(), nil}
	}()

	select {
	case res := <-resultChan:
		return res.result, res.err
	case <-time.After(1 * time.Second):
		vm.Interrupt("timeout")
		return false, fmt.Errorf("plugin timeout after 1s")
	}
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
