package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/nupi-ai/nupi/internal/detector"
	"github.com/nupi-ai/nupi/internal/pluginmanifest"
	"github.com/spf13/cobra"
)

// pluginsCmd represents the plugins command group
var pluginsCmd = &cobra.Command{
	Use:   "plugins",
	Short: "Manage tool detection plugins",
	Long:  `Commands for managing detector plugins used for AI tool detection.`,
}

// pluginsRebuildCmd rebuilds the plugin index
var pluginsRebuildCmd = &cobra.Command{
	Use:   "rebuild",
	Short: "Rebuild the detector index",
	Long:  `Scans detector plugin packages in the plugin directory and rebuilds the index.json file.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		pluginDir := getPluginDir()
		fmt.Printf("Rebuilding plugin index in: %s\n", pluginDir)

		manifests, err := pluginmanifest.Discover(pluginDir)
		if err != nil {
			return fmt.Errorf("discover manifests: %w", err)
		}

		generator := detector.NewIndexGenerator(pluginDir, manifests)
		if err := generator.Generate(); err != nil {
			return fmt.Errorf("failed to generate index: %w", err)
		}

		fmt.Println("Plugin index rebuilt successfully!")
		return nil
	},
}

// pluginsListCmd lists all plugins
var pluginsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List detector plugins",
	Long:  `Shows information about detector plugins available in the current instance.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		pluginDir := getPluginDir()

		manifests, err := pluginmanifest.Discover(pluginDir)
		if err != nil {
			return fmt.Errorf("discover manifests: %w", err)
		}

		generator := detector.NewIndexGenerator(pluginDir, manifests)
		plugins, err := generator.ListPlugins()
		if err != nil {
			return fmt.Errorf("failed to list plugins: %w", err)
		}

		if len(plugins) == 0 {
			fmt.Println("No detector plugins found.")
			fmt.Printf("Plugin directory: %s\n", pluginDir)
			fmt.Println("\nAdd detector packages to this directory and run 'nupi plugins rebuild'.")
			return nil
		}

		fmt.Printf("Found %d plugin(s):\n", len(plugins))
		fmt.Println("Name              File                      Commands")
		fmt.Println("----------------  ------------------------  --------------------------------")

		for _, plugin := range plugins {
			if errStr, hasError := plugin["error"].(string); hasError {
				fmt.Printf("✗ %-15s  %-24s  Error: %s\n",
					plugin["file"],
					"",
					errStr)
			} else {
				name := fmt.Sprintf("%v", plugin["name"])
				if len(name) > 16 {
					name = name[:15] + "…"
				}

				file := fmt.Sprintf("%v", plugin["file"])
				if len(file) > 24 {
					file = file[:23] + "…"
				}

				commands := ""
				if cmds, ok := plugin["commands"].([]string); ok && len(cmds) > 0 {
					commands = fmt.Sprintf("%v", cmds)
				}

				fmt.Printf("%-17s  %-24s  %s\n", name, file, commands)
			}
		}

		return nil
	},
}

// pluginsInfoCmd shows detailed plugin info
var pluginsInfoCmd = &cobra.Command{
	Use:   "info <slug>",
	Short: "Show detector plugin information",
	Long:  `Displays manifest metadata and exported details for the given detector plugin slug.`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		target := args[0]
		pluginDir := getPluginDir()

		manifests, err := pluginmanifest.Discover(pluginDir)
		if err != nil {
			return fmt.Errorf("discover manifests: %w", err)
		}

		var selected *pluginmanifest.Manifest
		for _, manifest := range manifests {
			slug := manifest.Metadata.Slug
			if slug == "" {
				slug = filepath.Base(manifest.Dir)
			}
			if slug == target {
				selected = manifest
				break
			}
		}

		if selected == nil {
			return fmt.Errorf("plugin %q not found", target)
		}
		if selected.Kind != pluginmanifest.KindDetector {
			return fmt.Errorf("plugin %q is not a detector", target)
		}

		mainPath, err := selected.MainPath()
		if err != nil {
			return err
		}

		relMain, err := selected.RelativeMainPath(pluginDir)
		if err != nil {
			relMain = mainPath
		}

		plugin, err := detector.LoadPlugin(mainPath)
		if err != nil {
			return fmt.Errorf("failed to load plugin: %w", err)
		}

		fmt.Printf("Name: %s\n", plugin.Name)
		if selected.Metadata.Slug != "" {
			fmt.Printf("Slug: %s\n", selected.Metadata.Slug)
		}
		fmt.Printf("Version: %s\n", selected.Metadata.Version)
		fmt.Printf("Kind: %s\n", selected.Kind)
		fmt.Printf("Entry: %s\n", relMain)
		if selected.Metadata.Description != "" {
			fmt.Printf("Description: %s\n", selected.Metadata.Description)
		}
		fmt.Printf("Commands: %v\n", plugin.Commands)

		return nil
	},
}

// getPluginDir returns the plugin directory path
func getPluginDir() string {
	// Check environment variable first
	if dir := os.Getenv("NUPI_PLUGIN_DIR"); dir != "" {
		return dir
	}

	// Check config
	if instanceDir != "" {
		return filepath.Join(instanceDir, "plugins")
	}

	// Default to home directory
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".nupi", "plugins")
}

func init() {
	// Add subcommands
	pluginsCmd.AddCommand(pluginsRebuildCmd)
	pluginsCmd.AddCommand(pluginsListCmd)
	pluginsCmd.AddCommand(pluginsInfoCmd)

	// Add to root command
	rootCmd.AddCommand(pluginsCmd)
}
