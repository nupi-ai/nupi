package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/grpcclient"
	"github.com/nupi-ai/nupi/internal/marketplace"
	"github.com/nupi-ai/nupi/internal/plugins/installer"
	manifestpkg "github.com/nupi-ai/nupi/internal/plugins/manifest"
	toolhandlers "github.com/nupi-ai/nupi/internal/plugins/tool_handlers"
	"github.com/nupi-ai/nupi/internal/validate"
	"github.com/spf13/cobra"
)

// pluginRow holds data for a single row in the unified plugins table.
type pluginRow struct {
	Type      string
	Name      string
	Slug      string
	Namespace string
	Version   string
	Slot      string
	Status    string
	Health    string
}

// Adapter slot categories where only one plugin can be active at a time.
var adapterSlotCategories = map[string]bool{
	constants.AdapterSlotSTT:    true,
	constants.AdapterSlotTTS:    true,
	constants.AdapterSlotAI:     true,
	constants.AdapterSlotVAD:    true,
	constants.AdapterSlotTunnel: true,
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
		stdout := cmd.OutOrStdout()
		pluginDir := getPluginDir()
		fmt.Fprintf(stdout, "Rebuilding plugin index in: %s\n", pluginDir)

		manifests, err := manifestpkg.Discover(pluginDir)
		if err != nil {
			return fmt.Errorf("discover manifests: %w", err)
		}

		generator := toolhandlers.NewIndexGenerator(pluginDir, manifests)
		if err := generator.Generate(); err != nil {
			return fmt.Errorf("failed to generate index: %w", err)
		}

		fmt.Fprintln(stdout, "Plugin index rebuilt successfully!")
		return nil
	},
}

// pluginsListCmd lists all installed plugins
var pluginsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all installed plugins",
	Long:  `Shows all installed plugins (adapters, tool handlers, pipeline cleaners) with runtime status for adapters.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		stdout := cmd.OutOrStdout()
		pluginDir := getPluginDir()

		rows, err := listAllPlugins(pluginDir)
		if err != nil {
			return err
		}

		if len(rows) == 0 {
			fmt.Fprintln(stdout, "No plugins found.")
			fmt.Fprintf(stdout, "Plugin directory: %s\n", pluginDir)
			return nil
		}

		connected := enrichWithDaemonData(rows)
		enrichWithInstallData(rows)
		printPluginsTable(stdout, rows)
		if !connected {
			fmt.Fprintln(stdout, "\n(daemon not running)")
		}

		return nil
	},
}

// pluginsInfoCmd shows detailed plugin info
var pluginsInfoCmd = &cobra.Command{
	Use:   "info <namespace/slug>",
	Short: "Show plugin information",
	Long:  `Displays manifest metadata and details for the given plugin (namespace/slug or just slug).`,
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
			fullID := manifest.Metadata.Namespace + "/" + slug
			if slug == target || fullID == target {
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

		stdout := cmd.OutOrStdout()
		fmt.Fprintf(stdout, "Name: %s\n", name)
		if selected.Metadata.Slug != "" {
			fmt.Fprintf(stdout, "Slug: %s\n", selected.Metadata.Slug)
		}
		if selected.Metadata.Namespace != "" {
			fmt.Fprintf(stdout, "Namespace: %s\n", selected.Metadata.Namespace)
		}
		fmt.Fprintf(stdout, "Version: %s\n", selected.Metadata.Version)
		fmt.Fprintf(stdout, "Type: %s\n", selected.Type)
		if selected.Metadata.Description != "" {
			fmt.Fprintf(stdout, "Description: %s\n", selected.Metadata.Description)
		}

		switch selected.Type {
		case manifestpkg.PluginTypeAdapter:
			if selected.Adapter != nil {
				fmt.Fprintf(stdout, "Slot: %s\n", selected.Adapter.Slot)
				fmt.Fprintf(stdout, "Transport: %s\n", selected.Adapter.Entrypoint.Transport)
				if selected.Adapter.Entrypoint.Command != "" {
					fmt.Fprintf(stdout, "Command: %s\n", selected.Adapter.Entrypoint.Command)
				}
			}
		case manifestpkg.PluginTypeToolHandler:
			mainPath, err := selected.MainPath()
			if err == nil {
				relMain, relErr := selected.RelativeMainPath(pluginDir)
				if relErr != nil {
					relMain = mainPath
				}
				fmt.Fprintf(stdout, "Entry: %s\n", relMain)

				plugin, loadErr := toolhandlers.LoadPlugin(mainPath)
				if loadErr == nil {
					fmt.Fprintf(stdout, "Commands: %v\n", plugin.Commands)
				}
			}
		case manifestpkg.PluginTypePipelineCleaner:
			mainPath, err := selected.MainPath()
			if err == nil {
				relMain, err := selected.RelativeMainPath(pluginDir)
				if err != nil {
					relMain = mainPath
				}
				fmt.Fprintf(stdout, "Entry: %s\n", relMain)
			}
		}

		return nil
	},
}

// --- Install / Uninstall / Enable / Disable / Search ---

var pluginsInstallCmd = &cobra.Command{
	Use:   "install <namespace/slug | url | path>",
	Short: "Install a plugin",
	Long: `Install a plugin from a marketplace, URL, or local path.

Examples:
  nupi plugins install ai.nupi/stt-local-whisper     # from marketplace
  nupi plugins install https://example.com/plugin.zip # from URL
  nupi plugins install ./my-plugin.zip                # from local archive
  nupi plugins install ./my-plugin/                   # from local directory`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := newOutputFormatter(cmd)
		source := args[0]

		store, err := configstore.Open(configstore.Options{})
		if err != nil {
			return out.Error("Failed to open config store", err)
		}
		defer store.Close()

		pluginDir := getPluginDir()
		inst := installer.NewInstaller(store, pluginDir)

		ctx, cancel := context.WithTimeout(context.Background(), constants.Duration5Minutes)
		defer cancel()

		var result *installer.InstallResult

		switch {
		case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://"):
			// URL install
			result, err = inst.InstallFromURL(ctx, source)
		case fileExists(source):
			// Local path install (check file existence first)
			result, err = inst.InstallFromPath(ctx, source)
		case isValidPluginID(source):
			// Marketplace namespace/slug
			parts := strings.SplitN(source, "/", 2)
			mClient := marketplace.NewClient(store)
			result, err = inst.InstallFromMarketplace(ctx, mClient, parts[0], parts[1])
		default:
			// Not a URL, not an existing file, not a valid namespace/slug — give a clear error
			return out.Error("Invalid install source", fmt.Errorf(
				"%q is not a valid URL, existing file path, or plugin ID (namespace/slug)", source))
		}

		if err != nil {
			return out.Error("Failed to install plugin", err)
		}

		return out.Render(CommandResult{
			Data: result,
			HumanReadable: func() error {
				stdout := out.w
				version := result.Version
				if version == "" {
					version = "unknown"
				}
				fmt.Fprintf(stdout, "Installed: %s/%s v%s (disabled)\n", result.Namespace, result.Slug, version)
				fmt.Fprintf(stdout, "\nTo activate run:\n  nupi plugins enable %s/%s\n", result.Namespace, result.Slug)
				triggerPluginReload()
				return nil
			},
		})
	},
}

var pluginsUninstallCmd = &cobra.Command{
	Use:           "uninstall <namespace/slug>",
	Short:         "Uninstall a plugin",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := newOutputFormatter(cmd)

		namespace, slug, err := parsePluginID(args[0])
		if err != nil {
			return out.Error("Invalid plugin identifier", err)
		}

		store, err := configstore.Open(configstore.Options{})
		if err != nil {
			return out.Error("Failed to open config store", err)
		}
		defer store.Close()

		pluginDir := getPluginDir()
		inst := installer.NewInstaller(store, pluginDir)

		ctx, cancel := context.WithTimeout(context.Background(), constants.Duration30Seconds)
		defer cancel()

		if err := inst.Uninstall(ctx, namespace, slug); err != nil {
			return out.Error("Failed to uninstall plugin", err)
		}

		// Trigger daemon reload if running
		triggerPluginReload()

		return out.Success(fmt.Sprintf("Uninstalled: %s/%s", namespace, slug), map[string]interface{}{
			"namespace": namespace,
			"slug":      slug,
		})
	},
}

var pluginsEnableCmd = &cobra.Command{
	Use:           "enable <namespace/slug>",
	Short:         "Enable (activate) a plugin",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := newOutputFormatter(cmd)

		namespace, slug, err := parsePluginID(args[0])
		if err != nil {
			return out.Error("Invalid plugin identifier", err)
		}

		store, err := configstore.Open(configstore.Options{})
		if err != nil {
			return out.Error("Failed to open config store", err)
		}
		defer store.Close()

		ctx, cancel := context.WithTimeout(context.Background(), constants.Duration10Seconds)
		defer cancel()

		// Check plugin exists
		p, err := store.GetInstalledPlugin(ctx, namespace, slug)
		if err != nil {
			return out.Error(fmt.Sprintf("Plugin %s/%s not found", namespace, slug), err)
		}

		if p.Enabled {
			return out.Success(fmt.Sprintf("Plugin %s/%s is already enabled.", namespace, slug), map[string]interface{}{
				"namespace": namespace,
				"slug":      slug,
				"status":    "already_enabled",
			})
		}

		// Check slot conflict for adapter plugins
		pluginDir := getPluginDir()
		mf := loadManifestForPlugin(pluginDir, namespace, slug)
		if mf != nil && mf.Type == manifestpkg.PluginTypeAdapter && mf.Adapter != nil {
			slotCategory := mf.Adapter.Slot
			if adapterSlotCategories[slotCategory] {
				// Check if another plugin occupies this slot
				installed, err := store.ListInstalledPlugins(ctx)
				if err != nil {
					return out.Error("Failed to check slot conflicts", err)
				}

				for _, other := range installed {
					if !other.Enabled || (other.Namespace == namespace && other.Slug == slug) {
						continue
					}
					otherMF := loadManifestForPlugin(pluginDir, other.Namespace, other.Slug)
					if otherMF != nil && otherMF.Type == manifestpkg.PluginTypeAdapter && otherMF.Adapter != nil {
						if otherMF.Adapter.Slot == slotCategory {
							return out.Error(
								fmt.Sprintf("%s slot is already bound to '%s/%s'", strings.ToUpper(slotCategory), other.Namespace, other.Slug),
								fmt.Errorf("disable it first: nupi plugins disable %s/%s", other.Namespace, other.Slug),
							)
						}
					}
				}
			}
		}

		if err := store.SetPluginEnabled(ctx, namespace, slug, true); err != nil {
			return out.Error("Failed to enable plugin", err)
		}

		// Trigger daemon reload
		triggerPluginReload()

		return out.Success(fmt.Sprintf("Enabled: %s/%s", namespace, slug), map[string]interface{}{
			"namespace": namespace,
			"slug":      slug,
		})
	},
}

var pluginsDisableCmd = &cobra.Command{
	Use:           "disable <namespace/slug>",
	Short:         "Disable (deactivate) a plugin",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := newOutputFormatter(cmd)

		namespace, slug, err := parsePluginID(args[0])
		if err != nil {
			return out.Error("Invalid plugin identifier", err)
		}

		store, err := configstore.Open(configstore.Options{})
		if err != nil {
			return out.Error("Failed to open config store", err)
		}
		defer store.Close()

		ctx, cancel := context.WithTimeout(context.Background(), constants.Duration10Seconds)
		defer cancel()

		if err := store.SetPluginEnabled(ctx, namespace, slug, false); err != nil {
			return out.Error("Failed to disable plugin", err)
		}

		// Trigger daemon reload
		triggerPluginReload()

		return out.Success(fmt.Sprintf("Disabled: %s/%s", namespace, slug), map[string]interface{}{
			"namespace": namespace,
			"slug":      slug,
		})
	},
}

var pluginsSearchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search for plugins in registered marketplaces",
	Long: `Search for plugins across all marketplace sources.

Examples:
  nupi plugins search                         # list all available
  nupi plugins search whisper                  # search by name/description
  nupi plugins search --category stt           # filter by category
  nupi plugins search --namespace ai.nupi      # search in specific marketplace`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := newOutputFormatter(cmd)

		query := ""
		if len(args) > 0 {
			query = args[0]
		}

		category, _ := cmd.Flags().GetString("category")
		namespace, _ := cmd.Flags().GetString("namespace")

		// Open read-write: Search may need to refresh and cache marketplace indices
		store, err := configstore.Open(configstore.Options{})
		if err != nil {
			return out.Error("Failed to open config store", err)
		}
		defer store.Close()

		mClient := marketplace.NewClient(store)

		ctx, cancel := context.WithTimeout(context.Background(), constants.Duration30Seconds)
		defer cancel()

		results, err := mClient.Search(ctx, query, category, namespace)
		if err != nil {
			return out.Error("Failed to search plugins", err)
		}

		if len(results) == 0 {
			return out.Render(CommandResult{
				Data: []interface{}{},
				HumanReadable: func() error {
					fmt.Fprintln(out.w, "No plugins found.")
					return nil
				},
			})
		}

		return out.Render(CommandResult{
			Data: results,
			HumanReadable: func() error {
				w := tabwriter.NewWriter(out.w, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "NAMESPACE/SLUG\tNAME\tCATEGORY\tVERSION\tDESCRIPTION")
				for _, r := range results {
					desc := r.Plugin.Description
					if len(desc) > 60 {
						desc = desc[:57] + "..."
					}
					version := r.Plugin.Latest.Version
					if version == "" {
						version = "-"
					}
					fmt.Fprintf(w, "%s/%s\t%s\t%s\t%s\t%s\n",
						r.Namespace, r.Plugin.Slug,
						r.Plugin.DisplayName(),
						r.Plugin.Category,
						version,
						desc,
					)
				}
				w.Flush()
				return nil
			},
		})
	},
}

// --- Helper functions ---

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

	// Default to home directory.
	// Bare os.Stderr is justified: this is a pre-Cobra bootstrap helper with
	// no access to a cobra.Command or OutputFormatter.
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to determine home directory: %v\n", err)
		os.Exit(1)
	}
	return filepath.Join(homeDir, ".nupi", "plugins")
}

// fileExists checks if a file or directory exists at the given path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// isValidPluginID checks if s is a valid namespace/slug plugin identifier.
func isValidPluginID(s string) bool {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return false
	}
	return validate.Ident(parts[0]) && validate.Ident(parts[1])
}

// parsePluginID splits "namespace/slug" into parts.
func parsePluginID(id string) (namespace, slug string, err error) {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid plugin ID %q: expected namespace/slug", id)
	}
	if !validate.Ident(parts[0]) || !validate.Ident(parts[1]) {
		return "", "", fmt.Errorf("invalid plugin ID %q: namespace and slug must match [a-zA-Z0-9][a-zA-Z0-9._-]*", id)
	}
	return parts[0], parts[1], nil
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
			Type:      string(mf.Type),
			Name:      name,
			Slug:      mf.Metadata.Slug,
			Namespace: mf.Metadata.Namespace,
			Version:   mf.Metadata.Version,
			Slot:      slot,
			Status:    "-",
			Health:    "-",
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
	if err := withPlainClientTimeout(constants.Duration5Seconds, "failed to connect to daemon", func(ctx context.Context, gc *grpcclient.Client) error {
		resp, err := gc.AdaptersOverview(ctx)
		if err != nil {
			return err
		}

		overview := adaptersOverviewFromProto(resp)

		// Build lookup map: namespace/slug -> AdapterEntry
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
			// Try matching by namespace/slug format used in adapter registration
			key := formatAdapterID(rows[i].Namespace, rows[i].Slug)
			if idx, ok := adapterMap[key]; ok {
				entry := overview.Adapters[idx]
				rows[i].Status = entry.Status
				if entry.Runtime != nil && strings.TrimSpace(entry.Runtime.Health) != "" {
					rows[i].Health = entry.Runtime.Health
				}
			}
		}
		return nil
	}); err != nil {
		return false
	}

	return true
}

// enrichWithInstallData merges enabled/disabled status from the installed_plugins table.
func enrichWithInstallData(rows []pluginRow) {
	store, err := configstore.Open(configstore.Options{ReadOnly: true})
	if err != nil {
		log.Printf("[Plugins] WARNING: could not open config store for install data: %v", err)
		return
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), constants.Duration2Seconds)
	defer cancel()

	installed, err := store.ListInstalledPlugins(ctx)
	if err != nil {
		log.Printf("[Plugins] WARNING: could not list installed plugins: %v", err)
		return
	}

	lookup := make(map[string]configstore.InstalledPluginWithNamespace, len(installed))
	for _, p := range installed {
		lookup[p.Namespace+"/"+p.Slug] = p
	}

	for i := range rows {
		key := rows[i].Namespace + "/" + rows[i].Slug
		if p, ok := lookup[key]; ok {
			if rows[i].Status == "-" {
				if p.Enabled {
					rows[i].Status = "enabled"
				} else {
					rows[i].Status = "disabled"
				}
			}
		}
	}
}

// printPluginsTable renders the unified plugins table to the given writer.
func printPluginsTable(dst io.Writer, rows []pluginRow) {
	w := tabwriter.NewWriter(dst, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TYPE\tNAMESPACE/SLUG\tNAME\tSLOT\tSTATUS\tHEALTH")
	for _, r := range rows {
		nsSlug := r.Namespace + "/" + r.Slug
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Type, nsSlug, r.Name, r.Slot, r.Status, r.Health)
	}
	w.Flush()
}

// loadManifestForPlugin loads the manifest for a specific plugin by namespace/slug.
func loadManifestForPlugin(pluginDir, namespace, slug string) *manifestpkg.Manifest {
	dir := filepath.Join(pluginDir, namespace, slug)
	mf, err := manifestpkg.LoadFromDir(dir)
	if err != nil {
		return nil
	}
	return mf
}

// triggerPluginReload attempts to tell the daemon to reload plugins.
// Silently ignores errors (daemon may not be running).
func triggerPluginReload() {
	_ = withPlainClientTimeout(constants.Duration5Seconds, "failed to connect to daemon", func(ctx context.Context, gc *grpcclient.Client) error {
		_, err := gc.ReloadPlugins(ctx)
		return err
	})
}

func init() {
	// Search flags
	pluginsSearchCmd.Flags().String("category", "", "Filter by category (stt, tts, ai, vad, tool-handler, cleaner)")
	pluginsSearchCmd.Flags().String("namespace", "", "Search in specific marketplace namespace")

	// Add subcommands
	pluginsCmd.AddCommand(pluginsRebuildCmd)
	pluginsCmd.AddCommand(pluginsListCmd)
	pluginsCmd.AddCommand(pluginsInfoCmd)
	pluginsCmd.AddCommand(pluginsInstallCmd)
	pluginsCmd.AddCommand(pluginsUninstallCmd)
	pluginsCmd.AddCommand(pluginsEnableCmd)
	pluginsCmd.AddCommand(pluginsDisableCmd)
	pluginsCmd.AddCommand(pluginsSearchCmd)

	// Add to root command
	rootCmd.AddCommand(pluginsCmd)
}
