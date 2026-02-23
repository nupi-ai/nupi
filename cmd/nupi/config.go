package main

import (
	"context"
	"fmt"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/constants"
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
	configTransportCmd.Flags().String("tls-cert", "", "Set TLS certificate path")
	configTransportCmd.Flags().String("tls-key", "", "Set TLS key path")
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
	return withClient(cmd, daemonConnectErrorMessage, func(gc *grpcclient.Client, out *OutputFormatter) error {
		flags := cmd.Flags()

		// Check if any update flags were provided.
		hasUpdate := flags.Changed("binding") ||
			flags.Changed("tls-cert") || flags.Changed("tls-key") ||
			flags.Changed("grpc-port") || flags.Changed("grpc-binding")

		if hasUpdate {
			ctx, cancel := context.WithTimeout(context.Background(), constants.Duration5Seconds)
			defer cancel()

			updateCfg := &apiv1.TransportConfig{}
			if flags.Changed("binding") {
				binding, _ := flags.GetString("binding")
				updateCfg.Binding = binding
			}
			if flags.Changed("tls-cert") {
				path, _ := flags.GetString("tls-cert")
				updateCfg.TlsCertPath = path
			}
			if flags.Changed("tls-key") {
				path, _ := flags.GetString("tls-key")
				updateCfg.TlsKeyPath = path
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
		ctx, cancel := context.WithTimeout(context.Background(), constants.Duration5Seconds)
		defer cancel()

		cfg, err := gc.TransportConfig(ctx)
		if err != nil {
			return out.Error("Failed to fetch transport configuration", err)
		}

		payload := map[string]interface{}{
			"config": map[string]interface{}{
				"binding":       cfg.GetBinding(),
				"tls_cert_path": cfg.GetTlsCertPath(),
				"tls_key_path":  cfg.GetTlsKeyPath(),
				"grpc_port":     cfg.GetGrpcPort(),
				"grpc_binding":  cfg.GetGrpcBinding(),
				"auth_required": cfg.GetAuthRequired(),
			},
		}

		return out.Render(CommandResult{
			Data: payload,
			HumanReadable: func() error {
				fmt.Println("Transport configuration:")
				fmt.Printf("  Binding: %s\n", cfg.GetBinding())
				fmt.Printf("  gRPC Binding: %s\n", cfg.GetGrpcBinding())
				fmt.Printf("  gRPC Port: %d\n", cfg.GetGrpcPort())
				if cfg.GetTlsCertPath() != "" {
					fmt.Printf("  TLS Cert: %s\n", cfg.GetTlsCertPath())
				}
				if cfg.GetTlsKeyPath() != "" {
					fmt.Printf("  TLS Key: %s\n", cfg.GetTlsKeyPath())
				}
				fmt.Printf("  Auth Required: %v\n", cfg.GetAuthRequired())
				return nil
			},
		})
	})
}

func configMigrate(cmd *cobra.Command, _ []string) error {
	return withClientTimeout(cmd, constants.Duration5Seconds, func(ctx context.Context, gc *grpcclient.Client) (any, error) {
		resp, err := gc.Migrate(ctx, &apiv1.ConfigMigrateRequest{})
		if err != nil {
			return nil, clientCallFailed("Failed to run configuration migration", err)
		}

		payload := map[string]interface{}{
			"updated_slots":          resp.GetUpdatedSlots(),
			"pending_slots":          resp.GetPendingSlots(),
			"audio_settings_updated": resp.GetAudioSettingsUpdated(),
		}

		return CommandResult{
			Data: payload,
			HumanReadable: func() error {
				fmt.Println("Configuration migration summary:")
				if len(resp.GetUpdatedSlots()) > 0 {
					fmt.Println("  Updated slots:")
					for _, slot := range resp.GetUpdatedSlots() {
						fmt.Printf("    - %s\n", slot)
					}
				} else {
					fmt.Println("  Updated slots: (none)")
				}

				if len(resp.GetPendingSlots()) > 0 {
					fmt.Println("  Pending quickstart slots:")
					for _, slot := range resp.GetPendingSlots() {
						fmt.Printf("    - %s\n", slot)
					}
				} else {
					fmt.Println("  Pending quickstart slots: (none)")
				}

				if resp.GetAudioSettingsUpdated() {
					fmt.Println("  Audio settings: defaults reconciled")
				} else {
					fmt.Println("  Audio settings: already up-to-date")
				}
				return nil
			},
		}, nil
	})
}
