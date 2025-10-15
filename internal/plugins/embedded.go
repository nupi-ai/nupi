package plugins

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

//go:embed all:plugins/*.js all:plugins/icons/*
var embeddedPlugins embed.FS

// ExtractEmbedded copies embedded plugins to the plugin directory.
func ExtractEmbedded(targetDir string) error {
	// Ensure target directory exists
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("failed to create plugin directory: %w", err)
	}

	// Ensure icons subdirectory exists
	iconsDir := filepath.Join(targetDir, "icons")
	if err := os.MkdirAll(iconsDir, 0o755); err != nil {
		return fmt.Errorf("failed to create icons directory: %w", err)
	}

	// Walk through embedded plugins
	err := fs.WalkDir(embeddedPlugins, "plugins", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip the root directory
		if d.IsDir() {
			return nil
		}

		// Get relative path from "plugins/"
		relPath, _ := filepath.Rel("plugins", path)

		// Determine target path based on file type/location
		var targetPath string
		if filepath.Dir(relPath) == "icons" {
			// Icon files go to icons/ subdirectory
			targetPath = filepath.Join(iconsDir, filepath.Base(path))
		} else if filepath.Ext(path) == ".js" {
			// JS files go to main plugin directory
			targetPath = filepath.Join(targetDir, filepath.Base(path))
		} else {
			// Skip other files
			return nil
		}

		// Read embedded file
		content, err := embeddedPlugins.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read embedded file %s: %w", path, err)
		}

		// Write to target location
		if err := os.WriteFile(targetPath, content, 0o644); err != nil {
			return fmt.Errorf("failed to write file %s: %w", targetPath, err)
		}

		fileType := "plugin"
		if filepath.Dir(relPath) == "icons" {
			fileType = "icon"
		}
		log.Printf("[Plugins] Extracted %s: %s", fileType, filepath.Base(path))
		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to extract plugins: %w", err)
	}

	return nil
}
