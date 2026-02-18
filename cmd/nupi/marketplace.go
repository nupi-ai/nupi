package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/marketplace"
	"github.com/nupi-ai/nupi/internal/validate"
	"github.com/spf13/cobra"
)

func newMarketplaceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "marketplace",
		Short:         "Manage plugin marketplace sources",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	listCmd := &cobra.Command{
		Use:           "list",
		Short:         "List registered marketplace sources",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          marketplaceList,
	}

	addCmd := &cobra.Command{
		Use:           "add <url-to-index.yaml>",
		Short:         "Add a marketplace source",
		Long:          "Fetches index.yaml from the given URL, extracts namespace, validates format, and registers the marketplace.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          marketplaceAdd,
	}

	removeCmd := &cobra.Command{
		Use:           "remove <namespace>",
		Short:         "Remove a marketplace source",
		Long:          "Removes a marketplace. All plugins from this marketplace must be uninstalled first.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          marketplaceRemove,
	}

	refreshCmd := &cobra.Command{
		Use:           "refresh",
		Short:         "Refresh all marketplace indices",
		Long:          "Force-fetches the latest index.yaml from all registered marketplace sources, ignoring cache.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          marketplaceRefresh,
	}

	cmd.AddCommand(listCmd, addCmd, removeCmd, refreshCmd)
	return cmd
}

func openMarketplaceClient() (*marketplace.Client, *configstore.Store, error) {
	store, err := configstore.Open(configstore.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("open config store: %w", err)
	}
	return marketplace.NewClient(store), store, nil
}

func marketplaceList(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	store, err := configstore.Open(configstore.Options{ReadOnly: true})
	if err != nil {
		return out.Error("Failed to open config store", err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	marketplaces, err := store.ListMarketplaces(ctx)
	if err != nil {
		return out.Error("Failed to list marketplaces", err)
	}

	if len(marketplaces) == 0 {
		if out.jsonMode {
			return out.Print([]interface{}{})
		}
		fmt.Println("No marketplaces registered.")
		return nil
	}

	if out.jsonMode {
		return out.Print(marketplaces)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tURL\tBUILTIN\tLAST REFRESHED")
	for _, m := range marketplaces {
		builtin := ""
		if m.IsBuiltin {
			builtin = "yes"
		}
		url := m.URL
		if url == "" {
			url = "(local)"
		}
		lastRefreshed := m.LastRefreshed
		if lastRefreshed == "" {
			lastRefreshed = "never"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.Namespace, url, builtin, lastRefreshed)
	}
	w.Flush()
	return nil
}

func marketplaceAdd(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	client, store, err := openMarketplaceClient()
	if err != nil {
		return out.Error("Failed to open config store", err)
	}
	defer store.Close()

	// Block marketplace URLs pointing to private/internal IPs (SSRF prevention).
	if err := validate.RejectPrivateURL(args[0]); err != nil {
		return out.Error("Invalid marketplace URL", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	m, err := client.Add(ctx, args[0])
	if err != nil {
		return out.Error("Failed to add marketplace", err)
	}

	if out.jsonMode {
		return out.Print(m)
	}

	fmt.Printf("Added marketplace %q from %s\n", m.Namespace, m.URL)
	return nil
}

func marketplaceRemove(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	client, store, err := openMarketplaceClient()
	if err != nil {
		return out.Error("Failed to open config store", err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Remove(ctx, args[0]); err != nil {
		return out.Error("Failed to remove marketplace", err)
	}

	return out.Success(fmt.Sprintf("Removed marketplace %q", args[0]), map[string]interface{}{
		"namespace": args[0],
	})
}

func marketplaceRefresh(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	client, store, err := openMarketplaceClient()
	if err != nil {
		return out.Error("Failed to open config store", err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Refresh(ctx); err != nil {
		return out.Error("Failed to refresh marketplaces", err)
	}

	return out.Success("All marketplace indices refreshed.", nil)
}
