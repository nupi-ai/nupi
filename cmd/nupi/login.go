package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/bootstrap"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/grpcclient"
	"github.com/spf13/cobra"
)

const (
	bootstrapLoginOptionsKey cmdOptionsContextKey = "bootstrap_login_options"
	pairClaimOptionsKey      cmdOptionsContextKey = "pair_claim_options"
)

type bootstrapLoginMode string

const (
	bootstrapLoginModeShow      bootstrapLoginMode = "show"
	bootstrapLoginModeClear     bootstrapLoginMode = "clear"
	bootstrapLoginModeConfigure bootstrapLoginMode = "configure"
)

type bootstrapLoginOptions struct {
	mode       bootstrapLoginMode
	baseURL    string
	token      string
	insecure   bool
	caCert     string
	serverName string
	name       string
}

type pairClaimOptions struct {
	code string
	name string
}

func newLoginCommand() *cobra.Command {
	loginCmd := &cobra.Command{
		Use:           "login",
		Short:         "Configure remote daemon access for CLI and GUI clients",
		SilenceUsage:  true,
		SilenceErrors: true,
		PreRunE:       bootstrapLoginPreRun,
		RunE:          bootstrapLogin,
	}
	loginCmd.Flags().String("url", "", "Daemon base URL (e.g. https://host:port)")
	loginCmd.Flags().String("token", "", "API token to store for authenticated access")
	loginCmd.Flags().Bool("show", false, "Display stored bootstrap configuration")
	loginCmd.Flags().Bool("insecure", false, "Disable TLS verification (dangerous; testing only)")
	loginCmd.Flags().String("ca-cert", "", "Path to custom CA certificate for TLS verification")
	loginCmd.Flags().String("server-name", "", "Override TLS server name (advanced)")
	loginCmd.Flags().String("name", "", "Optional label for this connection")
	loginCmd.Flags().Bool("clear", false, "Remove stored remote configuration")
	return loginCmd
}

func newPairRootCommand() *cobra.Command {
	pairRootCmd := &cobra.Command{
		Use:           "pair",
		Short:         "Pair this device with a Nupi daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	pairClaimCmd := &cobra.Command{
		Use:           "claim",
		Short:         "Redeem a pairing code for an API token",
		SilenceUsage:  true,
		SilenceErrors: true,
		PreRunE:       pairClaimPreRun,
		RunE:          pairClaim,
	}
	pairClaimCmd.Flags().String("code", "", "Pairing code provided by the daemon")
	pairClaimCmd.Flags().String("name", "", "Optional name for this device")

	pairRootCmd.AddCommand(pairClaimCmd)
	return pairRootCmd
}

func bootstrapLogin(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)
	opts, ok := getCmdOptions[*bootstrapLoginOptions](cmd, bootstrapLoginOptionsKey)
	if !ok || opts == nil {
		return out.Error("Internal CLI error while preparing login options", nil)
	}

	if opts.mode == bootstrapLoginModeShow {
		flags := []string{"clear", "url", "token", "insecure", "ca-cert", "server-name", "name"}
		for _, name := range flags {
			if name == "show" {
				continue
			}
			if cmd.Flags().Changed(name) {
				return out.Error("--show cannot be combined with other login flags", nil)
			}
		}

		cfg, err := bootstrap.Load()
		if err != nil {
			return out.Error("Failed to read bootstrap configuration", err)
		}

		path, pathErr := bootstrap.Path()
		info := map[string]any{
			"path":       path,
			"configured": cfg != nil,
		}
		if pathErr != nil {
			info["path_error"] = pathErr.Error()
		}

		if cfg != nil {
			info["base_url"] = cfg.BaseURL
			info["token_configured"] = strings.TrimSpace(cfg.APIToken) != ""
			if !cfg.UpdatedAt.IsZero() {
				info["updated_at"] = cfg.UpdatedAt.Format(time.RFC3339)
			}
			if cfg.Metadata != nil {
				meta := map[string]any{}
				if cfg.Metadata.Name != "" {
					meta["name"] = cfg.Metadata.Name
				}
				if cfg.Metadata.Description != "" {
					meta["description"] = cfg.Metadata.Description
				}
				if len(meta) > 0 {
					info["meta"] = meta
				}
			}
			if cfg.TLS != nil {
				tlsInfo := map[string]any{
					"insecure": cfg.TLS.Insecure,
				}
				if cfg.TLS.CACertPath != "" {
					tlsInfo["ca_cert_path"] = cfg.TLS.CACertPath
				}
				if cfg.TLS.ServerName != "" {
					tlsInfo["server_name"] = cfg.TLS.ServerName
				}
				info["tls"] = tlsInfo
			}
		}

		return out.Print(info)
	}

	if opts.mode == bootstrapLoginModeClear {
		if err := bootstrap.Remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
			return out.Error("Failed to clear bootstrap configuration", err)
		}
		info := map[string]any{"cleared": true}
		if path, err := bootstrap.Path(); err == nil {
			info["path"] = path
		}
		return out.Success("Bootstrap configuration cleared", info)
	}

	cfg := &bootstrap.Config{
		BaseURL: opts.baseURL,
	}
	if opts.token != "" {
		cfg.APIToken = opts.token
	}
	if opts.name != "" {
		cfg.Metadata = &bootstrap.MetaSection{Name: opts.name}
	}
	if opts.insecure || opts.caCert != "" || opts.serverName != "" {
		cfg.TLS = &bootstrap.TLSConfig{
			Insecure:   opts.insecure,
			CACertPath: opts.caCert,
			ServerName: opts.serverName,
		}
		if cfg.TLS != nil && !cfg.TLS.Insecure && cfg.TLS.CACertPath == "" && cfg.TLS.ServerName == "" {
			cfg.TLS = nil
		}
	}

	if err := bootstrap.Save(cfg); err != nil {
		return out.Error("Failed to store bootstrap configuration", err)
	}

	info := map[string]any{
		"base_url": opts.baseURL,
	}
	if path, err := bootstrap.Path(); err == nil {
		info["path"] = path
	}
	if cfg.Metadata != nil && cfg.Metadata.Name != "" {
		info["name"] = cfg.Metadata.Name
	}
	info["token_configured"] = cfg.APIToken != ""
	if cfg.TLS != nil {
		info["tls_insecure"] = cfg.TLS.Insecure
		if cfg.TLS.CACertPath != "" {
			info["tls_ca_cert"] = cfg.TLS.CACertPath
		}
		if cfg.TLS.ServerName != "" {
			info["tls_server_name"] = cfg.TLS.ServerName
		}
	}

	return out.Success("Bootstrap configuration saved", info)
}

func pairClaim(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	opts, ok := getCmdOptions[*pairClaimOptions](cmd, pairClaimOptionsKey)
	if !ok || opts == nil {
		return out.Error("Internal CLI error while preparing pairing claim", nil)
	}

	return withOutputClientTimeout(out, constants.Duration10Seconds, daemonConnectGRPCErrorMessage, func(ctx context.Context, gc *grpcclient.Client) (any, error) {
		resp, err := gc.ClaimPairing(ctx, &apiv1.ClaimPairingRequest{
			Code:       strings.ToUpper(opts.code),
			ClientName: opts.name,
		})
		if err != nil {
			return nil, clientCallFailed("Failed to claim pairing code", err)
		}

		result := map[string]interface{}{
			"token": resp.GetToken(),
			"name":  resp.GetName(),
			"role":  resp.GetRole(),
		}
		if resp.GetCreatedAt() != nil {
			result["created_at"] = resp.GetCreatedAt().AsTime().Format(time.RFC3339)
		}

		return CommandResult{
			Data: result,
			HumanReadable: func() error {
				fmt.Println("Pairing successful. Store this token securely:")
				fmt.Printf("  Token: %s\n", resp.GetToken())
				if strings.TrimSpace(resp.GetName()) != "" {
					fmt.Printf("  Name:  %s\n", resp.GetName())
				}
				fmt.Printf("  Role:  %s\n", resp.GetRole())
				return nil
			},
		}, nil
	})
}

func bootstrapLoginPreRun(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)
	show, _ := cmd.Flags().GetBool("show")
	clear, _ := cmd.Flags().GetBool("clear")

	if show {
		setCmdOptions(cmd, bootstrapLoginOptionsKey, &bootstrapLoginOptions{mode: bootstrapLoginModeShow})
		return nil
	}
	if clear {
		setCmdOptions(cmd, bootstrapLoginOptionsKey, &bootstrapLoginOptions{mode: bootstrapLoginModeClear})
		return nil
	}

	baseURL := getTrimmedFlag(cmd, "url")
	if baseURL == "" {
		return out.Error("Base URL (--url) is required unless --clear is used", nil)
	}
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + baseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return out.Error("Invalid daemon URL", err)
	}
	baseURL = strings.TrimRight(parsed.String(), "/")

	token := getTrimmedFlag(cmd, "token")
	insecure, _ := cmd.Flags().GetBool("insecure")
	caCert := getTrimmedFlag(cmd, "ca-cert")
	serverName := getTrimmedFlag(cmd, "server-name")
	name := getTrimmedFlag(cmd, "name")
	if caCert != "" {
		if _, err := os.Stat(caCert); err != nil {
			return out.Error("CA certificate not accessible", err)
		}
	}

	setCmdOptions(cmd, bootstrapLoginOptionsKey, &bootstrapLoginOptions{
		mode:       bootstrapLoginModeConfigure,
		baseURL:    baseURL,
		token:      token,
		insecure:   insecure,
		caCert:     caCert,
		serverName: serverName,
		name:       name,
	})
	return nil
}

func pairClaimPreRun(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)
	code := getTrimmedFlag(cmd, "code")
	if code == "" {
		return out.Error("Pairing code is required", nil)
	}
	setCmdOptions(cmd, pairClaimOptionsKey, &pairClaimOptions{
		code: code,
		name: getTrimmedFlag(cmd, "name"),
	})
	return nil
}
