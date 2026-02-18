package marketplace

import (
	"fmt"
	"strings"

	"github.com/nupi-ai/nupi/internal/validate"
	"gopkg.in/yaml.v3"
)

// Index represents a parsed marketplace index.yaml.
type Index struct {
	APIVersion  string        `yaml:"apiVersion"`
	Namespace   string        `yaml:"namespace"`
	Name        string        `yaml:"name"`
	Description string        `yaml:"description"`
	Homepage    string        `yaml:"homepage"`
	Updated     string        `yaml:"updated"`
	Plugins     []IndexPlugin `yaml:"plugins"`
}

// IndexPlugin describes a single plugin entry in the marketplace index.
type IndexPlugin struct {
	Slug        string       `yaml:"slug"`
	Name        string       `yaml:"name"`
	Description string       `yaml:"description"`
	Category    string       `yaml:"category"`
	Author      string       `yaml:"author"`
	License     string       `yaml:"license"`
	Icon        string       `yaml:"icon"`
	Readme      string       `yaml:"readme"`
	Homepage    string       `yaml:"homepage"`
	Latest      LatestEntry  `yaml:"latest"`
}

// LatestEntry holds the latest release info for a plugin.
type LatestEntry struct {
	Version       string              `yaml:"version"`
	Released      string              `yaml:"released"`
	Compatibility string              `yaml:"compatibility"`
	Archives      map[string]*Archive `yaml:"archives"`
}

// Archive describes a downloadable plugin archive for a specific platform.
type Archive struct {
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
	Size   int64  `yaml:"size"`
}

// DisplayName returns the human-readable name, falling back to slug.
func (p *IndexPlugin) DisplayName() string {
	if p.Name != "" {
		return p.Name
	}
	return p.Slug
}

// ParseIndex parses a marketplace index from raw YAML bytes.
func ParseIndex(data []byte) (*Index, error) {
	var idx Index
	if err := yaml.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parse marketplace index: %w", err)
	}

	if err := validateIndex(&idx); err != nil {
		return nil, err
	}

	return &idx, nil
}

func validateIndex(idx *Index) error {
	if idx.APIVersion == "" {
		return fmt.Errorf("marketplace index missing required field: apiVersion")
	}
	if !strings.HasPrefix(idx.APIVersion, "marketplace.nupi.ai/") {
		return fmt.Errorf("marketplace index has unsupported apiVersion: %s", idx.APIVersion)
	}
	if strings.TrimSpace(idx.Namespace) == "" {
		return fmt.Errorf("marketplace index missing required field: namespace")
	}
	if !validate.IdentRe.MatchString(idx.Namespace) || len(idx.Namespace) > 128 {
		return fmt.Errorf("marketplace index has invalid namespace %q (must match [a-zA-Z0-9][a-zA-Z0-9._-]*, max 128 chars)", idx.Namespace)
	}

	for i, p := range idx.Plugins {
		if strings.TrimSpace(p.Slug) == "" {
			return fmt.Errorf("marketplace index plugin #%d missing required field: slug", i)
		}
		if !validate.IdentRe.MatchString(p.Slug) || len(p.Slug) > 128 {
			return fmt.Errorf("marketplace index plugin #%d has invalid slug %q (must match [a-zA-Z0-9][a-zA-Z0-9._-]*, max 128 chars)", i, p.Slug)
		}
		if strings.TrimSpace(p.Category) == "" {
			return fmt.Errorf("marketplace index plugin %q missing required field: category", p.Slug)
		}
		if len(p.Latest.Archives) == 0 {
			return fmt.Errorf("marketplace index plugin %q missing required field: latest.archives", p.Slug)
		}
		for platform, archive := range p.Latest.Archives {
			if archive == nil {
				return fmt.Errorf("marketplace index plugin %q archive %q is nil", p.Slug, platform)
			}
			archiveURL := strings.TrimSpace(archive.URL)
			if archiveURL == "" {
				return fmt.Errorf("marketplace index plugin %q archive %q missing url", p.Slug, platform)
			}
			if err := validate.HTTPURL(archiveURL); err != nil {
				return fmt.Errorf("marketplace index plugin %q archive %q has invalid url: %w", p.Slug, platform, err)
			}
			if strings.TrimSpace(archive.SHA256) == "" {
				return fmt.Errorf("marketplace index plugin %q archive %q missing sha256", p.Slug, platform)
			}
		}
	}

	return nil
}

// ArchiveForPlatform selects the appropriate archive for the given OS/arch.
// Falls back to "any" if the specific platform is not available.
func (p *IndexPlugin) ArchiveForPlatform(goos, goarch string) (*Archive, string, error) {
	key := goos + "-" + goarch
	if a, ok := p.Latest.Archives[key]; ok {
		return a, key, nil
	}
	if a, ok := p.Latest.Archives["any"]; ok {
		return a, "any", nil
	}
	return nil, "", fmt.Errorf("plugin %q not available for %s", p.Slug, key)
}
