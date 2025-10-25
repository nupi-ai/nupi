package pluginmanifest

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
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
	ModuleType string            `yaml:"moduleType"`
	Mode       string            `yaml:"mode"`
	Entrypoint AdapterEntrypoint `yaml:"entrypoint"`
	Assets     AdapterAssets     `yaml:"assets"`
	Telemetry  AdapterTelemetry  `yaml:"telemetry"`
}

type AdapterEntrypoint struct {
	Command    string   `yaml:"command"`
	Args       []string `yaml:"args"`
	Transport  string   `yaml:"transport"`
	ListenEnv  string   `yaml:"listenEnv"`
	WorkingDir string   `yaml:"workingDir"`
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

func Discover(root string) ([]*Manifest, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read plugin root: %w", err)
	}

	var manifests []*Manifest

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
			return nil, fmt.Errorf("read catalog dir %s: %w", catalogDir, err)
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
				return nil, fmt.Errorf("load manifest in %s: %w", slugDir, err)
			}

			if manifest.Metadata.Catalog == "" {
				manifest.Metadata.Catalog = fallbackCatalog
				log.Printf("[PluginManifest] directory %s missing catalog metadata, using fallback %q", slugDir, fallbackCatalog)
			} else if trimmed := strings.TrimSpace(catalogName); trimmed != "" && manifest.Metadata.Catalog != trimmed {
				log.Printf("[PluginManifest] directory %s catalog mismatch: manifest=%q dir=%q", slugDir, manifest.Metadata.Catalog, trimmed)
			}

			if manifest.Metadata.Slug == "" {
				log.Printf("[PluginManifest] skipping %s: slug metadata is required", slugDir)
				continue
			}

			manifests = append(manifests, manifest)
		}
	}

	sort.Slice(manifests, func(i, j int) bool {
		return manifests[i].Dir < manifests[j].Dir
	})
	return manifests, nil
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
