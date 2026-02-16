package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nupi-ai/nupi/internal/grpcclient"
	manifestpkg "github.com/nupi-ai/nupi/internal/plugins/manifest"
	toolhandlers "github.com/nupi-ai/nupi/internal/plugins/tool_handlers"
	"github.com/spf13/cobra"
)

// pluginRow holds data for a single row in the unified plugins table.
type pluginRow struct {
	Type    string
	Name    string
	Slug    string
	Catalog string
	Version string
	Slot    string
	Status  string
	Health  string
}

// pluginsCmd represents the plugins command group
var pluginsCmd = &cobra.Command{
	Use:   "plugins",
	Short: "Manage plugins",
	Long:  `Commands for managing plugins (adapters, tool handlers, pipeline cleaners).`,
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

// pluginsListCmd lists all installed plugins
var pluginsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all installed plugins",
	Long:  `Shows all installed plugins (adapters, tool handlers, pipeline cleaners) with runtime status for adapters.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		pluginDir := getPluginDir()

		rows, err := listAllPlugins(pluginDir)
		if err != nil {
			return err
		}

		if len(rows) == 0 {
			fmt.Println("No plugins found.")
			fmt.Printf("Plugin directory: %s\n", pluginDir)
			return nil
		}

		connected := enrichWithDaemonData(rows)
		printPluginsTable(rows)
		if !connected {
			fmt.Println("\n(daemon not running)")
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
		gc, err := grpcclient.New()
		if err != nil {
			return fmt.Errorf("failed to connect to daemon: %w", err)
		}
		defer gc.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		resp, err := gc.GetPluginWarnings(ctx)
		if err != nil {
			return fmt.Errorf("failed to get plugin warnings: %w", err)
		}

		warnings := resp.GetWarnings()
		if len(warnings) == 0 {
			fmt.Println("No plugin discovery warnings.")
			fmt.Println("All plugins loaded successfully.")
			return nil
		}

		fmt.Printf("Found %d plugin(s) with warnings:\n\n", len(warnings))
		for i, w := range warnings {
			fmt.Printf("%d. %s\n", i+1, w.GetDir())
			fmt.Printf("   Error: %s\n\n", w.GetError())
		}

		return nil
	},
}

// pluginsInfoCmd shows detailed plugin info
var pluginsInfoCmd = &cobra.Command{
	Use:   "info <slug>",
	Short: "Show plugin information",
	Long:  `Displays manifest metadata and details for the given plugin slug.`,
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

		name := strings.TrimSpace(selected.Metadata.Name)
		if name == "" {
			name = selected.Metadata.Slug
		}

		fmt.Printf("Name: %s\n", name)
		if selected.Metadata.Slug != "" {
			fmt.Printf("Slug: %s\n", selected.Metadata.Slug)
		}
		fmt.Printf("Version: %s\n", selected.Metadata.Version)
		fmt.Printf("Type: %s\n", selected.Type)
		if selected.Metadata.Description != "" {
			fmt.Printf("Description: %s\n", selected.Metadata.Description)
		}

		switch selected.Type {
		case manifestpkg.PluginTypeAdapter:
			if selected.Adapter != nil {
				fmt.Printf("Slot: %s\n", selected.Adapter.Slot)
				fmt.Printf("Transport: %s\n", selected.Adapter.Entrypoint.Transport)
				if selected.Adapter.Entrypoint.Command != "" {
					fmt.Printf("Command: %s\n", selected.Adapter.Entrypoint.Command)
				}
			}
		case manifestpkg.PluginTypeToolHandler:
			mainPath, err := selected.MainPath()
			if err == nil {
				relMain, relErr := selected.RelativeMainPath(pluginDir)
				if relErr != nil {
					relMain = mainPath
				}
				fmt.Printf("Entry: %s\n", relMain)

				plugin, loadErr := toolhandlers.LoadPlugin(mainPath)
				if loadErr == nil {
					fmt.Printf("Commands: %v\n", plugin.Commands)
				}
			}
		case manifestpkg.PluginTypePipelineCleaner:
			mainPath, err := selected.MainPath()
			if err == nil {
				relMain, err := selected.RelativeMainPath(pluginDir)
				if err != nil {
					relMain = mainPath
				}
				fmt.Printf("Entry: %s\n", relMain)
			}
		}

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

// listAllPlugins discovers all manifests and builds pluginRow entries.
func listAllPlugins(pluginDir string) ([]pluginRow, error) {
	manifests, err := manifestpkg.Discover(pluginDir)
	if err != nil {
		return nil, fmt.Errorf("discover manifests: %w", err)
	}

	rows := make([]pluginRow, 0, len(manifests))
	for _, mf := range manifests {
		name := strings.TrimSpace(mf.Metadata.Name)
		if name == "" {
			name = mf.Metadata.Slug
		}

		slot := "-"
		if mf.Type == manifestpkg.PluginTypeAdapter && mf.Adapter != nil {
			slot = mf.Adapter.Slot
		}

		rows = append(rows, pluginRow{
			Type:    string(mf.Type),
			Name:    name,
			Slug:    mf.Metadata.Slug,
			Catalog: mf.Metadata.Catalog,
			Version: mf.Metadata.Version,
			Slot:    slot,
			Status:  "-",
			Health:  "-",
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Type != rows[j].Type {
			return rows[i].Type < rows[j].Type
		}
		return rows[i].Name < rows[j].Name
	})

	return rows, nil
}

// enrichWithDaemonData tries to fetch adapter runtime data from the daemon.
// Returns true if the daemon connection succeeded.
func enrichWithDaemonData(rows []pluginRow) bool {
	gc, err := grpcclient.New()
	if err != nil {
		return false
	}
	defer gc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := gc.AdaptersOverview(ctx)
	if err != nil {
		return false
	}

	overview := adaptersOverviewFromProto(resp)

	// Build lookup map: catalog/slug -> AdapterEntry
	adapterMap := make(map[string]int, len(overview.Adapters))
	for i, entry := range overview.Adapters {
		if entry.AdapterID != nil {
			adapterMap[*entry.AdapterID] = i
		}
	}

	for i := range rows {
		if rows[i].Type != string(manifestpkg.PluginTypeAdapter) {
			continue
		}
		// Try matching by catalog/slug format used in adapter registration
		key := formatAdapterID(rows[i].Catalog, rows[i].Slug)
		if idx, ok := adapterMap[key]; ok {
			entry := overview.Adapters[idx]
			rows[i].Status = entry.Status
			if entry.Runtime != nil && strings.TrimSpace(entry.Runtime.Health) != "" {
				rows[i].Health = entry.Runtime.Health
			}
		}
	}

	return true
}

// printPluginsTable renders the unified plugins table.
func printPluginsTable(rows []pluginRow) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TYPE\tNAME\tSLUG\tSLOT\tSTATUS\tHEALTH")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Type, r.Name, r.Slug, r.Slot, r.Status, r.Health)
	}
	w.Flush()
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
