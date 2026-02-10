package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nupi-ai/nupi/internal/client"
	manifestpkg "github.com/nupi-ai/nupi/internal/plugins/manifest"
	toolhandlers "github.com/nupi-ai/nupi/internal/plugins/tool_handlers"
	"github.com/spf13/cobra"
)

// pluginsCmd represents the plugins command group
var pluginsCmd = &cobra.Command{
	Use:   "plugins",
	Short: "Manage tool handler plugins",
	Long:  `Commands for managing tool handler plugins used for AI tool handling.`,
}

// pluginsRebuildCmd rebuilds the plugin index
var pluginsRebuildCmd = &cobra.Command{
	Use:   "rebuild",
	Short: "Rebuild the handler index",
	Long:  `Scans tool handler plugins in the plugin directory and rebuilds the handlers_index.json file.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		pluginDir := getPluginDir()
		fmt.Printf("Rebuilding plugin index in: %s\n", pluginDir)

		manifests, err := manifestpkg.Discover(pluginDir)
		if err != nil {
			return fmt.Errorf("discover manifests: %w", err)
		}

		generator := toolhandlers.NewIndexGenerator(pluginDir, manifests)
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
	Short: "List tool handler plugins",
	Long:  `Shows information about tool handler plugins available in the current instance.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		pluginDir := getPluginDir()

		manifests, err := manifestpkg.Discover(pluginDir)
		if err != nil {
			return fmt.Errorf("discover manifests: %w", err)
		}

		generator := toolhandlers.NewIndexGenerator(pluginDir, manifests)
		plugins, err := generator.ListPlugins()
		if err != nil {
			return fmt.Errorf("failed to list plugins: %w", err)
		}

		if len(plugins) == 0 {
			fmt.Println("No tool handler plugins found.")
			fmt.Printf("Plugin directory: %s\n", pluginDir)
			fmt.Println("\nAdd tool handler plugins to this directory and run 'nupi plugins rebuild'.")
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

// pluginsWarningsCmd shows plugin discovery warnings
var pluginsWarningsCmd = &cobra.Command{
	Use:   "warnings",
	Short: "Show plugin discovery warnings",
	Long: `Displays warnings for plugins that were skipped during discovery due to manifest errors.

This shows the current state from the most recent plugin discovery scan. Warnings will
disappear when manifests are fixed and the daemon reloads plugins (e.g., after restart
or config change). For historical tracking, check daemon logs.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.New()
		if err != nil {
			return fmt.Errorf("failed to connect to daemon: %w", err)
		}

		count, warnings, err := c.GetPluginWarnings()
		if err != nil {
			return fmt.Errorf("failed to get plugin warnings: %w", err)
		}

		if count == 0 {
			fmt.Println("No plugin discovery warnings.")
			fmt.Println("All plugins loaded successfully.")
			return nil
		}

		fmt.Printf("Found %d plugin(s) with warnings:\n\n", count)
		for i, w := range warnings {
			fmt.Printf("%d. %s\n", i+1, w.Dir)
			fmt.Printf("   Error: %s\n\n", w.Error)
		}

		return nil
	},
}

// pluginsInfoCmd shows detailed plugin info
var pluginsInfoCmd = &cobra.Command{
	Use:   "info <slug>",
	Short: "Show tool handler plugin information",
	Long:  `Displays manifest metadata and exported details for the given tool handler plugin slug.`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		target := args[0]
		pluginDir := getPluginDir()

		manifests, err := manifestpkg.Discover(pluginDir)
		if err != nil {
			return fmt.Errorf("discover manifests: %w", err)
		}

		var selected *manifestpkg.Manifest
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
		if selected.Type != manifestpkg.PluginTypeToolHandler {
			return fmt.Errorf("plugin %q is not a tool-handler", target)
		}

		mainPath, err := selected.MainPath()
		if err != nil {
			return err
		}

		relMain, err := selected.RelativeMainPath(pluginDir)
		if err != nil {
			relMain = mainPath
		}

		plugin, err := toolhandlers.LoadPlugin(mainPath)
		if err != nil {
			return fmt.Errorf("failed to load plugin: %w", err)
		}

		// Fallback to manifest name when jsruntime is unavailable
		if plugin.Name == "" || plugin.Name == filepath.Base(plugin.FilePath) {
			if name := strings.TrimSpace(selected.Metadata.Name); name != "" {
				plugin.Name = name
			}
		}

		fmt.Printf("Name: %s\n", plugin.Name)
		if selected.Metadata.Slug != "" {
			fmt.Printf("Slug: %s\n", selected.Metadata.Slug)
		}
		fmt.Printf("Version: %s\n", selected.Metadata.Version)
		fmt.Printf("Type: %s\n", selected.Type)
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
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to determine home directory: %v\n", err)
		os.Exit(1)
	}
	return filepath.Join(homeDir, ".nupi", "plugins")
}

func init() {
	// Add subcommands
	pluginsCmd.AddCommand(pluginsRebuildCmd)
	pluginsCmd.AddCommand(pluginsListCmd)
	pluginsCmd.AddCommand(pluginsInfoCmd)
	pluginsCmd.AddCommand(pluginsWarningsCmd)

	// Add to root command
	rootCmd.AddCommand(pluginsCmd)
}
