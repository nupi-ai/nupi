package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/nupi-ai/nupi/internal/client"
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

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	baseURL, _, err := resolveDaemonBaseURL(c)
	if err != nil {
		return out.Error("Failed to resolve daemon HTTP endpoint", err)
	}
	flags := cmd.Flags()

	payload := make(map[string]interface{})
	if flags.Changed("binding") {
		binding, _ := flags.GetString("binding")
		payload["binding"] = binding
	}
	if flags.Changed("port") {
		p, _ := flags.GetInt("port")
		payload["port"] = p
	}
	if flags.Changed("tls-cert") {
		path, _ := flags.GetString("tls-cert")
		payload["tls_cert_path"] = path
	}
	if flags.Changed("tls-key") {
		path, _ := flags.GetString("tls-key")
		payload["tls_key_path"] = path
	}
	if flags.Changed("allowed-origin") {
		origins, _ := flags.GetStringSlice("allowed-origin")
		payload["allowed_origins"] = origins
	}
	if flags.Changed("grpc-port") {
		gp, _ := flags.GetInt("grpc-port")
		payload["grpc_port"] = gp
	}
	if flags.Changed("grpc-binding") {
		gb, _ := flags.GetString("grpc-binding")
		payload["grpc_binding"] = gb
	}

	var newToken string

	if len(payload) > 0 {
		body, err := json.Marshal(payload)
		if err != nil {
			return out.Error("Failed to encode update payload", err)
		}

		req, err := http.NewRequest(http.MethodPut, baseURL+"/config/transport", bytes.NewReader(body))
		if err != nil {
			return out.Error("Failed to construct update request", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := doRequest(c, req)
		if err != nil {
			return out.Error("Failed to update transport configuration", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 300 {
			msg := readErrorMessage(resp)
			return out.Error("Transport configuration update failed", fmt.Errorf("%s", msg))
		}

		if resp.StatusCode == http.StatusOK {
			var updateResp struct {
				Status    string `json:"status"`
				Binding   string `json:"binding"`
				AuthToken string `json:"auth_token"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&updateResp); err != nil {
				return out.Error("Invalid response from daemon", err)
			}
			newToken = strings.TrimSpace(updateResp.AuthToken)
		} else if resp.StatusCode != http.StatusNoContent {
			msg := readErrorMessage(resp)
			return out.Error("Unexpected response while updating transport configuration", fmt.Errorf("%s", msg))
		}
	}

	req, err := http.NewRequest(http.MethodGet, baseURL+"/config/transport", nil)
	if err != nil {
		return out.Error("Failed to create request", err)
	}
	resp, err := doStreamingRequest(c, req)
	if err != nil {
		return out.Error("Failed to fetch transport configuration", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg := readErrorMessage(resp)
		return out.Error("Failed to fetch transport configuration", fmt.Errorf("%s", msg))
	}

	var cfg transportConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return out.Error("Invalid response from daemon", err)
	}

	if out.jsonMode {
		payload := map[string]interface{}{
			"config": cfg,
		}
		if newToken != "" {
			payload["new_api_token"] = newToken
		}
		return out.Print(payload)
	}

	fmt.Println("Transport configuration:")
	fmt.Printf("  Binding: %s\n", cfg.Binding)
	fmt.Printf("  HTTP Port: %d\n", cfg.Port)
	fmt.Printf("  gRPC Binding: %s\n", cfg.GRPCBinding)
	fmt.Printf("  gRPC Port: %d\n", cfg.GRPCPort)
	if cfg.TLSCertPath != "" {
		fmt.Printf("  TLS Cert: %s\n", cfg.TLSCertPath)
	}
	if cfg.TLSKeyPath != "" {
		fmt.Printf("  TLS Key: %s\n", cfg.TLSKeyPath)
	}
	if len(cfg.AllowedOrigins) > 0 {
		fmt.Printf("  Allowed Origins: %s\n", strings.Join(cfg.AllowedOrigins, ", "))
	} else {
		fmt.Println("  Allowed Origins: (none)")
	}
	fmt.Printf("  Auth Required: %v\n", cfg.AuthRequired)

	if newToken != "" {
		fmt.Println()
		fmt.Println("A new API token was generated:")
		fmt.Printf("  %s\n", newToken)
		fmt.Println("Store it securely; it will be required for LAN/public connections.")
	}

	return nil
}

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
