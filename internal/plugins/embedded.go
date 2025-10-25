package plugins

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

//go:embed all:plugins/**
var embeddedPlugins embed.FS

// ExtractEmbedded copies embedded plugins to the plugin directory.
func ExtractEmbedded(targetDir string) error {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("failed to create plugin directory: %w", err)
	}

	walker := func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel("plugins", path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		targetPath := filepath.Join(targetDir, rel)
		if d.IsDir() {
			return os.MkdirAll(targetPath, 0o755)
		}

		content, err := embeddedPlugins.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read embedded file %s: %w", path, err)
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return fmt.Errorf("failed to create directory for %s: %w", targetPath, err)
		}

		if err := os.WriteFile(targetPath, content, 0o644); err != nil {
			return fmt.Errorf("failed to write file %s: %w", targetPath, err)
		}

		log.Printf("[Plugins] Extracted %s", rel)
		return nil
	}

	if err := fs.WalkDir(embeddedPlugins, "plugins", walker); err != nil {
		return fmt.Errorf("failed to extract plugins: %w", err)
	}

	return nil
}
