package pluginmanifest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind describes the type of plugin represented by the manifest.
type Kind string

const (
	// KindModule identifies process-based adapters executed via adapter-runner.
	KindModule Kind = "ModuleManifest"
	// KindDetector identifies tool-detector plugins executed in-process.
	KindDetector Kind = "DetectorManifest"
	// KindPipelineCleaner identifies content-pipeline transformers executed in-process.
	KindPipelineCleaner Kind = "PipelineCleanerManifest"

	manifestYAML = "plugin.yaml"
	manifestYML  = "plugin.yml"
	manifestJSON = "plugin.json"
)

// Metadata captures common descriptive fields shared across plugin types.
type Metadata struct {
    Name        string `yaml:"name"`
    Slug        string `yaml:"slug"`
    Catalog     string `yaml:"catalog"`
    Description string `yaml:"description"`
    Version     string `yaml:"version"`
}

// DetectorSpec defines runtime settings for detector plugins.
type DetectorSpec struct {
	Main string `yaml:"main"`
}

// PipelineCleanerSpec defines runtime settings for cleaner plugins.
type PipelineCleanerSpec struct {
	Main string `yaml:"main"`
}

// Manifest represents a parsed plugin manifest residing within a directory.
type Manifest struct {
	Dir        string
	File       string
	Raw        string
	APIVersion string
	Kind       Kind
	Metadata   Metadata

	Detector        *DetectorSpec
	PipelineCleaner *PipelineCleanerSpec
}

// MainPath returns the absolute path to the primary script for manifests that define one.
func (m *Manifest) MainPath() (string, error) {
	switch m.Kind {
	case KindDetector:
		if m.Detector == nil {
			return "", fmt.Errorf("detector manifest missing spec")
		}
		return filepath.Join(m.Dir, m.Detector.Main), nil
	case KindPipelineCleaner:
		if m.PipelineCleaner == nil {
			return "", fmt.Errorf("pipeline cleaner manifest missing spec")
		}
		return filepath.Join(m.Dir, m.PipelineCleaner.Main), nil
	default:
		return "", fmt.Errorf("manifest kind %s does not define a main script", m.Kind)
	}
}

// RelativeMainPath returns the relative path of the primary script from the provided root.
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

// Discover scans the provided directory for plugin manifests.
func Discover(root string) ([]*Manifest, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read plugin root: %w", err)
	}

	var manifests []*Manifest
	appendManifest := func(dir string, manifest *Manifest) {
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			rel = filepath.Base(dir)
		}
		parts := strings.Split(rel, string(os.PathSeparator))
		if manifest.Metadata.Slug == "" {
			manifest.Metadata.Slug = filepath.Base(dir)
		}
		if manifest.Metadata.Catalog == "" && len(parts) >= 2 {
			manifest.Metadata.Catalog = parts[0]
		}
		manifests = append(manifests, manifest)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		catalogDir := filepath.Join(root, entry.Name())

		if manifest, err := LoadFromDir(catalogDir); err == nil {
			appendManifest(catalogDir, manifest)
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("load manifest in %s: %w", catalogDir, err)
		}

		subEntries, err := os.ReadDir(catalogDir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read catalog dir %s: %w", catalogDir, err)
		}

		for _, sub := range subEntries {
			if !sub.IsDir() {
				continue
			}
			slugDir := filepath.Join(catalogDir, sub.Name())
			manifest, err := LoadFromDir(slugDir)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("load manifest in %s: %w", slugDir, err)
			}
			appendManifest(slugDir, manifest)
		}
	}

	sort.Slice(manifests, func(i, j int) bool {
		return manifests[i].Dir < manifests[j].Dir
	})
	return manifests, nil
}

// LoadFromDir attempts to load a plugin manifest from the provided directory.
func LoadFromDir(dir string) (*Manifest, error) {
    file, err := locateManifestFile(dir)
    if err != nil {
        return nil, err
	}

	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", file, err)
	}

	var doc struct {
		APIVersion string    `yaml:"apiVersion"`
		Kind       string    `yaml:"kind"`
		Metadata   Metadata  `yaml:"metadata"`
		Spec       yaml.Node `yaml:"spec"`
	}

	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", file, err)
	}

	kind := Kind(strings.TrimSpace(doc.Kind))
	manifest := &Manifest{
		Dir:        dir,
		File:       file,
		Raw:        string(data),
		APIVersion: strings.TrimSpace(doc.APIVersion),
		Kind:       kind,
		Metadata:   doc.Metadata,
	}

    if manifest.Metadata.Slug == "" {
        manifest.Metadata.Slug = filepath.Base(dir)
    }

    switch kind {
    case KindModule:
        // No additional parsing required at this stage.
	case KindDetector:
		if doc.Spec.IsZero() {
			return nil, fmt.Errorf("detector manifest %s missing spec", file)
		}
		var spec DetectorSpec
		if err := doc.Spec.Decode(&spec); err != nil {
			return nil, fmt.Errorf("decode detector spec %s: %w", file, err)
		}
		if strings.TrimSpace(spec.Main) == "" {
			spec.Main = "main.js"
		}
		manifest.Detector = &spec
	case KindPipelineCleaner:
		if doc.Spec.IsZero() {
			return nil, fmt.Errorf("pipeline cleaner manifest %s missing spec", file)
		}
		var spec PipelineCleanerSpec
		if err := doc.Spec.Decode(&spec); err != nil {
			return nil, fmt.Errorf("decode pipeline cleaner spec %s: %w", file, err)
		}
		if strings.TrimSpace(spec.Main) == "" {
			spec.Main = "main.js"
		}
		manifest.PipelineCleaner = &spec
	default:
		return nil, fmt.Errorf("unsupported manifest kind %q in %s", doc.Kind, file)
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
