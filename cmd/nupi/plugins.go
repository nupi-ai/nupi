package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/nupi-ai/nupi/internal/detector"
	"github.com/spf13/cobra"
)

// pluginsCmd represents the plugins command group
var pluginsCmd = &cobra.Command{
	Use:   "plugins",
	Short: "Manage tool detection plugins",
	Long:  `Commands for managing JavaScript plugins used for AI tool detection.`,
}

// pluginsRebuildCmd rebuilds the plugin index
var pluginsRebuildCmd = &cobra.Command{
	Use:   "rebuild",
	Short: "Rebuild the plugin index",
	Long: `Scans all JavaScript plugins in the plugin directory and rebuilds the index.json file.
This command should be run after adding or modifying plugins.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		pluginDir := getPluginDir()
		fmt.Printf("Rebuilding plugin index in: %s\n", pluginDir)

		generator := detector.NewIndexGenerator(pluginDir)
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
	Short: "List all plugins",
	Long:  `Shows information about all available tool detection plugins.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		pluginDir := getPluginDir()

		generator := detector.NewIndexGenerator(pluginDir)
		plugins, err := generator.ListPlugins()
		if err != nil {
			return fmt.Errorf("failed to list plugins: %w", err)
		}

		if len(plugins) == 0 {
			fmt.Println("No plugins found.")
			fmt.Printf("Plugin directory: %s\n", pluginDir)
			fmt.Println("\nTo add plugins, place .js files in the plugin directory and run 'nupi plugins rebuild'")
			return nil
		}

		fmt.Printf("Found %d plugin(s):\n", len(plugins))
		fmt.Println("Name              File          Commands")
		fmt.Println("----------------  ------------  --------------------------------")

		for _, plugin := range plugins {
			if errStr, hasError := plugin["error"].(string); hasError {
				// Plugin failed to load
				fmt.Printf("✗ %-15s  %-12s  Error: %s\n",
					plugin["file"],
					"",
					errStr)
			} else {
				// Plugin loaded successfully
				name := fmt.Sprintf("%v", plugin["name"])
				if len(name) > 16 {
					name = name[:15] + "…"
				}

				file := fmt.Sprintf("%v", plugin["file"])
				if len(file) > 12 {
					file = file[:11] + "…"
				}

				commands := ""
				if cmds, ok := plugin["commands"].([]string); ok && len(cmds) > 0 {
					commands = fmt.Sprintf("%v", cmds)
				}

				fmt.Printf("%-17s  %-12s  %s\n", name, file, commands)
			}
		}

		return nil
	},
}

// pluginsInfoCmd shows detailed plugin info
var pluginsInfoCmd = &cobra.Command{
	Use:   "info <plugin-name>",
	Short: "Show plugin information",
	Long:  `Shows detailed information about a specific plugin.`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pluginName := args[0]
		pluginDir := getPluginDir()

		// Try with .js extension if not provided
		if filepath.Ext(pluginName) != ".js" {
			pluginName = pluginName + ".js"
		}

		pluginPath := filepath.Join(pluginDir, pluginName)

		// Load plugin
		plugin, err := detector.LoadPlugin(pluginPath)
		if err != nil {
			return fmt.Errorf("failed to load plugin: %w", err)
		}

		// Display info
		fmt.Println(plugin.GetInfo())

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
