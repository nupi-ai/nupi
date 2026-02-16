package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/client"
	"github.com/nupi-ai/nupi/internal/grpcclient"
	"github.com/spf13/cobra"
)

func newConfigCommand() *cobra.Command {
	configCmd := &cobra.Command{
		Use:           "config",
		Short:         "Configuration management commands",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	configTransportCmd := &cobra.Command{
		Use:           "transport",
		Short:         "Show or update transport configuration",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          configTransport,
	}
	configTransportCmd.Flags().String("binding", "", "Set binding mode (loopback|lan|public)")
	configTransportCmd.Flags().Int("port", 0, "Set HTTP port (0 for auto-select)")
	configTransportCmd.Flags().String("tls-cert", "", "Set TLS certificate path")
	configTransportCmd.Flags().String("tls-key", "", "Set TLS key path")
	configTransportCmd.Flags().StringSlice("allowed-origin", nil, "Allowed origins for HTTP API (repeatable)")
	configTransportCmd.Flags().Int("grpc-port", 0, "Set gRPC port (0 for auto-select)")
	configTransportCmd.Flags().String("grpc-binding", "", "Set gRPC binding (loopback|lan|public)")

	configMigrateCmd := &cobra.Command{
		Use:           "migrate",
		Short:         "Repair configuration defaults (required adapter slots)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          configMigrate,
	}

	configCmd.AddCommand(configTransportCmd, configMigrateCmd)
	return configCmd
}

func configTransport(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	gc, err := grpcclient.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer gc.Close()

	flags := cmd.Flags()

	// Check if any update flags were provided.
	hasUpdate := flags.Changed("binding") || flags.Changed("port") ||
		flags.Changed("tls-cert") || flags.Changed("tls-key") ||
		flags.Changed("allowed-origin") || flags.Changed("grpc-port") ||
		flags.Changed("grpc-binding")

	if hasUpdate {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		updateCfg := &apiv1.TransportConfig{}
		if flags.Changed("binding") {
			binding, _ := flags.GetString("binding")
			updateCfg.Binding = binding
		}
		if flags.Changed("port") {
			p, _ := flags.GetInt("port")
			updateCfg.Port = int32(p)
		}
		if flags.Changed("tls-cert") {
			path, _ := flags.GetString("tls-cert")
			updateCfg.TlsCertPath = path
		}
		if flags.Changed("tls-key") {
			path, _ := flags.GetString("tls-key")
			updateCfg.TlsKeyPath = path
		}
		if flags.Changed("allowed-origin") {
			origins, _ := flags.GetStringSlice("allowed-origin")
			updateCfg.AllowedOrigins = origins
		}
		if flags.Changed("grpc-port") {
			gp, _ := flags.GetInt("grpc-port")
			updateCfg.GrpcPort = int32(gp)
		}
		if flags.Changed("grpc-binding") {
			gb, _ := flags.GetString("grpc-binding")
			updateCfg.GrpcBinding = gb
		}

		if _, err := gc.UpdateTransportConfig(ctx, updateCfg); err != nil {
			return out.Error("Failed to update transport configuration", err)
		}
	}

	// Fetch current config.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg, err := gc.TransportConfig(ctx)
	if err != nil {
		return out.Error("Failed to fetch transport configuration", err)
	}

	if out.jsonMode {
		return out.Print(map[string]interface{}{
			"config": map[string]interface{}{
				"binding":         cfg.GetBinding(),
				"port":            cfg.GetPort(),
				"tls_cert_path":   cfg.GetTlsCertPath(),
				"tls_key_path":    cfg.GetTlsKeyPath(),
				"allowed_origins": cfg.GetAllowedOrigins(),
				"grpc_port":       cfg.GetGrpcPort(),
				"grpc_binding":    cfg.GetGrpcBinding(),
				"auth_required":   cfg.GetAuthRequired(),
			},
		})
	}

	fmt.Println("Transport configuration:")
	fmt.Printf("  Binding: %s\n", cfg.GetBinding())
	fmt.Printf("  HTTP Port: %d\n", cfg.GetPort())
	fmt.Printf("  gRPC Binding: %s\n", cfg.GetGrpcBinding())
	fmt.Printf("  gRPC Port: %d\n", cfg.GetGrpcPort())
	if cfg.GetTlsCertPath() != "" {
		fmt.Printf("  TLS Cert: %s\n", cfg.GetTlsCertPath())
	}
	if cfg.GetTlsKeyPath() != "" {
		fmt.Printf("  TLS Key: %s\n", cfg.GetTlsKeyPath())
	}
	if len(cfg.GetAllowedOrigins()) > 0 {
		fmt.Printf("  Allowed Origins: %s\n", strings.Join(cfg.GetAllowedOrigins(), ", "))
	} else {
		fmt.Println("  Allowed Origins: (none)")
	}
	fmt.Printf("  Auth Required: %v\n", cfg.GetAuthRequired())

	return nil
}

// configMigrate remains HTTP-based as there is no gRPC endpoint for config migration.
func configMigrate(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	summary, err := runConfigMigration(c)
	if err != nil {
		return out.Error("Failed to run configuration migration", err)
	}

	if out.jsonMode {
		return out.Print(summary)
	}

	fmt.Println("Configuration migration summary:")
	if len(summary.UpdatedSlots) > 0 {
		fmt.Println("  Updated slots:")
		for _, slot := range summary.UpdatedSlots {
			fmt.Printf("    - %s\n", slot)
		}
	} else {
		fmt.Println("  Updated slots: (none)")
	}

	if len(summary.PendingSlots) > 0 {
		fmt.Println("  Pending quickstart slots:")
		for _, slot := range summary.PendingSlots {
			fmt.Printf("    - %s\n", slot)
		}
	} else {
		fmt.Println("  Pending quickstart slots: (none)")
	}

	if summary.AudioSettingsUpdated {
		fmt.Println("  Audio settings: defaults reconciled")
	} else {
		fmt.Println("  Audio settings: already up-to-date")
	}

	return nil
}
