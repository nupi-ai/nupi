package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Kind string
type PluginType string

const (
	KindPlugin Kind = "Plugin"

	PluginTypeAdapter         PluginType = "adapter"
	PluginTypeToolDetector    PluginType = "tool-detector"
	PluginTypePipelineCleaner PluginType = "pipeline-cleaner"

	manifestYAML = "plugin.yaml"
	manifestYML  = "plugin.yml"
	manifestJSON = "plugin.json"

	fallbackCatalog = "others"
)

type Metadata struct {
	Name        string `yaml:"name"`
	Slug        string `yaml:"slug"`
	Catalog     string `yaml:"catalog"`
	Description string `yaml:"description"`
	Version     string `yaml:"version"`
}

type DetectorSpec struct {
	Main string `yaml:"main"`
}

type PipelineCleanerSpec struct {
	Main string `yaml:"main"`
}

type AdapterSpec struct {
	Slot       string                   `yaml:"slot"`
	Mode       string                   `yaml:"mode"`
	Entrypoint AdapterEntrypoint        `yaml:"entrypoint"`
	Assets     AdapterAssets            `yaml:"assets"`
	Telemetry  AdapterTelemetry         `yaml:"telemetry"`
	Options    map[string]AdapterOption `yaml:"options"`
}

type AdapterEntrypoint struct {
	Runtime      string   `yaml:"runtime"`
	Command      string   `yaml:"command"`
	Args         []string `yaml:"args"`
	Transport    string   `yaml:"transport"`
	ListenEnv    string   `yaml:"listenEnv"`
	WorkingDir   string   `yaml:"workingDir"`
	ReadyTimeout string   `yaml:"readyTimeout"`
}

type AdapterAssets struct {
	Models AdapterModelAssets `yaml:"models"`
}

type AdapterModelAssets struct {
	CacheDirEnv string `yaml:"cacheDirEnv"`
}

type AdapterTelemetry struct {
	Stdout *bool `yaml:"stdout"`
	Stderr *bool `yaml:"stderr"`
}

// AdapterOption describes a configurable option exposed by an adapter plugin.
// It supports a small set of primitive types (string, enum, boolean, integer,
// number). Values are validated when the manifest is parsed so that defaults
// remain consistent with declared types.
//
// Required vs Optional behavior:
//   - If Required=true, the option MUST be present in config (validation fails if missing)
//   - If Required=false (default), the option is optional and adapter uses Default value
//   - Options with Required=true should NOT have a Default (enforced at manifest load time)
type AdapterOption struct {
	Type        string `yaml:"type" json:"type"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Default     any    `yaml:"default,omitempty" json:"default,omitempty"`
	Values      []any  `yaml:"values,omitempty" json:"values,omitempty"`
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
}

type Manifest struct {
	Dir        string
	File       string
	Raw        string
	APIVersion string
	Kind       Kind
	Type       PluginType
	Metadata   Metadata

	Adapter         *AdapterSpec
	Detector        *DetectorSpec
	PipelineCleaner *PipelineCleanerSpec
}

// DiscoveryWarning represents a skipped plugin during discovery.
type DiscoveryWarning struct {
	Dir string
	Err error
}

// Parse decodes a manifest from the provided raw bytes without requiring a backing directory.
func Parse(data []byte) (*Manifest, error) {
	return decodeManifest(data, "", "")
}

func (m *Manifest) MainPath() (string, error) {
	switch m.Type {
	case PluginTypeToolDetector:
		if m.Detector == nil {
			return "", fmt.Errorf("detector manifest missing spec")
		}
		return filepath.Join(m.Dir, m.Detector.Main), nil
	case PluginTypePipelineCleaner:
		if m.PipelineCleaner == nil {
			return "", fmt.Errorf("pipeline cleaner manifest missing spec")
		}
		return filepath.Join(m.Dir, m.PipelineCleaner.Main), nil
	default:
		return "", fmt.Errorf("plugin type %s does not define a main script", m.Type)
	}
}

func (m *Manifest) RelativeMainPath(root string) (string, error) {
	mainPath, err := m.MainPath()
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, mainPath)
	if err != nil {
		return "", err
	}
	return rel, nil
}

// Discover scans the plugin directory and returns valid manifests.
// Invalid manifests are logged and skipped. For detailed error reporting,
// use DiscoverWithWarnings instead.
func Discover(root string) ([]*Manifest, error) {
	manifests, _ := DiscoverWithWarnings(root)
	return manifests, nil
}

// DiscoverWithWarnings scans the plugin directory and returns both valid manifests
// and warnings about skipped plugins.
func DiscoverWithWarnings(root string) ([]*Manifest, []DiscoveryWarning) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []DiscoveryWarning{{Dir: root, Err: fmt.Errorf("read plugin root: %w", err)}}
	}

	var manifests []*Manifest
	var warnings []DiscoveryWarning

	for _, catalogEntry := range entries {
		if !catalogEntry.IsDir() {
			continue
		}
		catalogName := catalogEntry.Name()
		catalogDir := filepath.Join(root, catalogName)

		slugEntries, err := os.ReadDir(catalogDir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			warnings = append(warnings, DiscoveryWarning{
				Dir: catalogDir,
				Err: fmt.Errorf("read catalog dir: %w", err),
			})
			log.Printf("[PluginManifest] skipping catalog %s: %v", catalogDir, err)
			continue
		}

		for _, slugEntry := range slugEntries {
			if !slugEntry.IsDir() {
				continue
			}
			slugDir := filepath.Join(catalogDir, slugEntry.Name())

			manifest, err := LoadFromDir(slugDir)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				warnings = append(warnings, DiscoveryWarning{Dir: slugDir, Err: err})
				log.Printf("[PluginManifest] skipping %s: %v", slugDir, err)
				continue
			}

			if manifest.Metadata.Catalog == "" {
				manifest.Metadata.Catalog = fallbackCatalog
				log.Printf("[PluginManifest] directory %s missing catalog metadata, using fallback %q", slugDir, fallbackCatalog)
			} else if trimmed := strings.TrimSpace(catalogName); trimmed != "" && manifest.Metadata.Catalog != trimmed {
				log.Printf("[PluginManifest] directory %s catalog mismatch: manifest=%q dir=%q", slugDir, manifest.Metadata.Catalog, trimmed)
			}

			if manifest.Metadata.Slug == "" {
				warnings = append(warnings, DiscoveryWarning{
					Dir: slugDir,
					Err: fmt.Errorf("slug metadata is required"),
				})
				log.Printf("[PluginManifest] skipping %s: slug metadata is required", slugDir)
				continue
			}

			manifests = append(manifests, manifest)
		}
	}

	sort.Slice(manifests, func(i, j int) bool {
		return manifests[i].Dir < manifests[j].Dir
	})
	return manifests, warnings
}

func LoadFromDir(dir string) (*Manifest, error) {
	file, err := locateManifestFile(dir)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", file, err)
	}

	return decodeManifest(data, dir, file)
}

func decodeManifest(data []byte, dir, file string) (*Manifest, error) {
	var doc struct {
		APIVersion string    `yaml:"apiVersion"`
		Kind       string    `yaml:"kind"`
		Type       string    `yaml:"type"`
		Metadata   Metadata  `yaml:"metadata"`
		Spec       yaml.Node `yaml:"spec"`
	}

	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", file, err)
	}

	rawKind := strings.TrimSpace(doc.Kind)
	if rawKind == "" {
		rawKind = string(KindPlugin)
	}

	kind := Kind(rawKind)
	if kind != KindPlugin {
		return nil, fmt.Errorf("unsupported manifest kind %q in %s", rawKind, file)
	}

	pluginType := PluginType(strings.TrimSpace(doc.Type))
	if pluginType == "" {
		return nil, fmt.Errorf("manifest %s missing type", file)
	}
	switch pluginType {
	case PluginTypeAdapter, PluginTypeToolDetector, PluginTypePipelineCleaner:
	default:
		return nil, fmt.Errorf("unsupported plugin type %q in %s", pluginType, file)
	}

	manifest := &Manifest{
		Dir:        dir,
		File:       file,
		Raw:        string(data),
		APIVersion: strings.TrimSpace(doc.APIVersion),
		Kind:       kind,
		Type:       pluginType,
		Metadata:   doc.Metadata,
	}

	manifest.Metadata.Catalog = strings.TrimSpace(manifest.Metadata.Catalog)
	manifest.Metadata.Slug = strings.TrimSpace(manifest.Metadata.Slug)

	if doc.Spec.IsZero() {
		return manifest, nil
	}

	switch pluginType {
	case PluginTypeAdapter:
		var spec AdapterSpec
		if err := doc.Spec.Decode(&spec); err != nil {
			return nil, fmt.Errorf("decode adapter spec %s: %w", file, err)
		}
		if len(spec.Options) > 0 {
			validated := make(map[string]AdapterOption, len(spec.Options))
			for key, opt := range spec.Options {
				normalizedKey := strings.TrimSpace(key)
				if normalizedKey == "" {
					return nil, fmt.Errorf("adapter option key is empty in %s", file)
				}
				normalizedOpt, err := normalizeAdapterOption(normalizedKey, opt)
				if err != nil {
					return nil, fmt.Errorf("adapter option %q: %w", normalizedKey, err)
				}
				validated[normalizedKey] = normalizedOpt
			}
			spec.Options = validated
		}
		if err := validateAdapterSpec(&spec, file); err != nil {
			return nil, err
		}
		manifest.Adapter = &spec
	case PluginTypeToolDetector:
		var spec DetectorSpec
		if err := doc.Spec.Decode(&spec); err != nil {
			return nil, fmt.Errorf("decode detector spec %s: %w", file, err)
		}
		if strings.TrimSpace(spec.Main) == "" {
			spec.Main = "main.js"
		}
		manifest.Detector = &spec
	case PluginTypePipelineCleaner:
		var spec PipelineCleanerSpec
		if err := doc.Spec.Decode(&spec); err != nil {
			return nil, fmt.Errorf("decode pipeline cleaner spec %s: %w", file, err)
		}
		if strings.TrimSpace(spec.Main) == "" {
			spec.Main = "main.js"
		}
		manifest.PipelineCleaner = &spec
	default:
		return nil, fmt.Errorf("unsupported plugin type %q in %s", pluginType, file)
	}

	return manifest, nil
}

var allowedOptionTypes = map[string]struct{}{
	"string":  {},
	"enum":    {},
	"boolean": {},
	"integer": {},
	"number":  {},
}

func normalizeAdapterOption(key string, opt AdapterOption) (AdapterOption, error) {
	opt.Description = strings.TrimSpace(opt.Description)
	opt.Type = strings.ToLower(strings.TrimSpace(opt.Type))

	if opt.Type == "" {
		if len(opt.Values) > 0 {
			opt.Type = "enum"
		} else if inferred := inferOptionType(opt.Default); inferred != "" {
			opt.Type = inferred
		} else {
			opt.Type = "string"
		}
	}

	if _, ok := allowedOptionTypes[opt.Type]; !ok {
		return AdapterOption{}, fmt.Errorf("unsupported type %q", opt.Type)
	}

	normalizeValues := func(fn func(any) (any, error)) ([]any, error) {
		if len(opt.Values) == 0 {
			return nil, nil
		}
		out := make([]any, len(opt.Values))
		for i, raw := range opt.Values {
			val, err := fn(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid value at index %d: %w", i, err)
			}
			out[i] = val
		}
		return out, nil
	}

	switch opt.Type {
	case "boolean":
		values, err := normalizeValues(coerceBool)
		if err != nil {
			return AdapterOption{}, err
		}
		opt.Values = values
		if opt.Default != nil {
			val, err := coerceBool(opt.Default)
			if err != nil {
				return AdapterOption{}, fmt.Errorf("default: %w", err)
			}
			if len(opt.Values) > 0 && !containsValue(opt.Values, val) {
				return AdapterOption{}, fmt.Errorf("default %v not present in values", val)
			}
			opt.Default = val
		}
	case "integer":
		values, err := normalizeValues(coerceInt)
		if err != nil {
			return AdapterOption{}, err
		}
		opt.Values = values
		if opt.Default != nil {
			val, err := coerceInt(opt.Default)
			if err != nil {
				return AdapterOption{}, fmt.Errorf("default: %w", err)
			}
			if len(opt.Values) > 0 && !containsValue(opt.Values, val) {
				return AdapterOption{}, fmt.Errorf("default %v not present in values", val)
			}
			opt.Default = val
		}
	case "number":
		values, err := normalizeValues(coerceNumber)
		if err != nil {
			return AdapterOption{}, err
		}
		opt.Values = values
		if opt.Default != nil {
			val, err := coerceNumber(opt.Default)
			if err != nil {
				return AdapterOption{}, fmt.Errorf("default: %w", err)
			}
			if len(opt.Values) > 0 && !containsValue(opt.Values, val) {
				return AdapterOption{}, fmt.Errorf("default %v not present in values", val)
			}
			opt.Default = val
		}
	case "string":
		values, err := normalizeValues(coerceString)
		if err != nil {
			return AdapterOption{}, err
		}
		opt.Values = values
		if opt.Default != nil {
			val, err := coerceString(opt.Default)
			if err != nil {
				return AdapterOption{}, fmt.Errorf("default: %w", err)
			}
			if len(opt.Values) > 0 && !containsValue(opt.Values, val) {
				return AdapterOption{}, fmt.Errorf("default %q not present in values", val)
			}
			opt.Default = val
		}
	case "enum":
		values, err := normalizeValues(coerceString)
		if err != nil {
			return AdapterOption{}, err
		}
		if len(values) == 0 {
			return AdapterOption{}, fmt.Errorf("enum requires non-empty values")
		}
		opt.Values = values
		if opt.Default != nil {
			val, err := coerceString(opt.Default)
			if err != nil {
				return AdapterOption{}, fmt.Errorf("default: %w", err)
			}
			if !containsValue(opt.Values, val) {
				return AdapterOption{}, fmt.Errorf("default %q not present in values", val)
			}
			opt.Default = val
		}
	default:
		return AdapterOption{}, fmt.Errorf("unsupported option type %q", opt.Type)
	}

	return opt, nil
}

// NormalizeAdapterOptionValue coerces an arbitrary value to the shape required
// by the supplied adapter option. The returned value is suitable for inclusion
// in adapter configuration payloads. Nil values are passed through unchanged.
func NormalizeAdapterOptionValue(opt AdapterOption, value any) (any, error) {
	if value == nil {
		return nil, nil
	}

	switch opt.Type {
	case "boolean":
		out, err := coerceBool(value)
		if err != nil {
			return nil, err
		}
		if len(opt.Values) > 0 && !containsValue(opt.Values, out) {
			return nil, fmt.Errorf("expected one of %v, got %v", opt.Values, out)
		}
		return out.(bool), nil
	case "integer":
		out, err := coerceInt(value)
		if err != nil {
			return nil, err
		}
		if len(opt.Values) > 0 && !containsValue(opt.Values, out) {
			return nil, fmt.Errorf("expected one of %v, got %v", opt.Values, out)
		}
		return out.(int), nil
	case "number":
		out, err := coerceNumber(value)
		if err != nil {
			return nil, err
		}
		if len(opt.Values) > 0 && !containsValue(opt.Values, out) {
			return nil, fmt.Errorf("expected one of %v, got %v", opt.Values, out)
		}
		return out.(float64), nil
	case "string":
		out, err := coerceString(value)
		if err != nil {
			return nil, err
		}
		if len(opt.Values) > 0 && !containsValue(opt.Values, out) {
			return nil, fmt.Errorf("expected one of %v, got %q", opt.Values, out)
		}
		return out.(string), nil
	case "enum":
		out, err := coerceString(value)
		if err != nil {
			return nil, err
		}
		if !containsValue(opt.Values, out) {
			return nil, fmt.Errorf("expected one of %v, got %q", opt.Values, out)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported option type %q", opt.Type)
	}
}

func inferOptionType(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case bool:
		return "boolean"
	case int, int32, int64:
		return "integer"
	case uint, uint32, uint64:
		return "integer"
	case float32:
		if isFloatIntegral(float64(v)) {
			return "integer"
		}
		return "number"
	case float64:
		if isFloatIntegral(v) {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	default:
		return ""
	}
}

func coerceBool(value any) (any, error) {
	switch v := value.(type) {
	case bool:
		return v, nil
	case string:
		trimmed := strings.TrimSpace(strings.ToLower(v))
		if trimmed == "true" || trimmed == "1" {
			return true, nil
		}
		if trimmed == "false" || trimmed == "0" {
			return false, nil
		}
		return nil, fmt.Errorf("expected boolean, got %q", v)
	default:
		return nil, fmt.Errorf("expected boolean, got %T", value)
	}
}

func coerceInt(value any) (any, error) {
	switch v := value.(type) {
	case int:
		return v, nil
	case int32:
		return int(v), nil
	case int64:
		return intFromInt64(v)
	case uint:
		return intFromInt64(int64(v))
	case uint32:
		return intFromInt64(int64(v))
	case uint64:
		if v > uint64(math.MaxInt64) {
			return nil, fmt.Errorf("integer value out of range: %d", v)
		}
		return intFromInt64(int64(v))
	case float32:
		if !isFloatIntegral(float64(v)) {
			return nil, fmt.Errorf("expected integer, got %v", v)
		}
		return intFromInt64(int64(v))
	case float64:
		if !isFloatIntegral(v) {
			return nil, fmt.Errorf("expected integer, got %v", v)
		}
		return intFromInt64(int64(v))
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil, fmt.Errorf("expected integer, got empty string")
		}
		parsed, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expected integer, got %q", v)
		}
		return intFromInt64(parsed)
	default:
		return nil, fmt.Errorf("expected integer, got %T", value)
	}
}

func coerceNumber(value any) (any, error) {
	switch v := value.(type) {
	case float32:
		return float64(v), nil
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case uint:
		return float64(v), nil
	case uint32:
		return float64(v), nil
	case uint64:
		return float64(v), nil
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil, fmt.Errorf("expected number, got empty string")
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return nil, fmt.Errorf("expected number, got %q", v)
		}
		return parsed, nil
	default:
		return nil, fmt.Errorf("expected number, got %T", value)
	}
}

func coerceString(value any) (any, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case fmt.Stringer:
		return v.String(), nil
	default:
		return nil, fmt.Errorf("expected string, got %T", value)
	}
}

func containsValue(values []any, expected any) bool {
	for _, v := range values {
		if v == expected {
			return true
		}
	}
	return false
}

func isFloatIntegral(value float64) bool {
	return math.Mod(value, 1.0) == 0
}

func intFromInt64(v int64) (int, error) {
	if int64(int(v)) != v {
		return 0, fmt.Errorf("integer value out of range: %d", v)
	}
	return int(v), nil
}

func locateManifestFile(dir string) (string, error) {
	candidates := []string{
		filepath.Join(dir, manifestYAML),
		filepath.Join(dir, manifestYML),
		filepath.Join(dir, manifestJSON),
	}

	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", fmt.Errorf("stat manifest %s: %w", candidate, err)
		}
		if info.IsDir() {
			continue
		}
		return candidate, nil
	}

	return "", fs.ErrNotExist
}

var allowedTransports = map[string]struct{}{
	"process": {},
	"grpc":    {},
	"http":    {},
}

var allowedRuntimes = map[string]struct{}{
	"":       {}, // empty = default (binary)
	"binary": {},
	"js":     {}, // JavaScript/TypeScript - Nupi provides the runtime
}

func validateAdapterSpec(spec *AdapterSpec, file string) error {
	if strings.TrimSpace(spec.Slot) == "" {
		return fmt.Errorf("adapter manifest %s missing required field: slot", file)
	}

	// Mode is required per architecture section 5.2 (adapter contract)
	if strings.TrimSpace(spec.Mode) == "" {
		return fmt.Errorf("adapter manifest %s missing required field: mode", file)
	}

	runtime := strings.TrimSpace(spec.Entrypoint.Runtime)
	transport := strings.TrimSpace(spec.Entrypoint.Transport)
	command := strings.TrimSpace(spec.Entrypoint.Command)

	// Validate runtime value (optional, defaults to binary)
	if _, ok := allowedRuntimes[runtime]; !ok {
		return fmt.Errorf("adapter manifest %s has invalid runtime %q (allowed: binary, js)", file, runtime)
	}

	// Transport is required (adapter-runner needs NUPI_ADAPTER_TRANSPORT)
	if transport == "" {
		return fmt.Errorf("adapter manifest %s missing required field: entrypoint.transport", file)
	}

	// Validate transport value
	if _, ok := allowedTransports[transport]; !ok {
		return fmt.Errorf("adapter manifest %s has invalid transport %q (allowed: process, grpc, http)", file, transport)
	}

	// Transport-specific validation
	switch transport {
	case "process":
		if command == "" {
			return fmt.Errorf("adapter manifest %s with transport=process requires entrypoint.command", file)
		}
	case "grpc", "http":
		// For network transports, command is optional but should be validated if present
		// The actual network address will be provided via endpoint configuration
	}

	return nil
}
