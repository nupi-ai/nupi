package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nupi-ai/nupi/internal/bootstrap"
	"github.com/nupi-ai/nupi/internal/client"
	"github.com/spf13/cobra"
)

func newLoginCommand() *cobra.Command {
	loginCmd := &cobra.Command{
		Use:           "login",
		Short:         "Configure remote daemon access for CLI and GUI clients",
		SilenceUsage:  true,
		SilenceErrors: true,
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
		RunE:          pairClaim,
	}
	pairClaimCmd.Flags().String("code", "", "Pairing code provided by the daemon")
	pairClaimCmd.Flags().String("name", "", "Optional name for this device")
	pairClaimCmd.Flags().String("url", "", "Base URL of the daemon (e.g. https://host:port)")
	pairClaimCmd.Flags().Bool("insecure", false, "Skip TLS verification when claiming pairing code")

	pairRootCmd.AddCommand(pairClaimCmd)
	return pairRootCmd
}

func bootstrapLogin(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	show, _ := cmd.Flags().GetBool("show")
	clear, _ := cmd.Flags().GetBool("clear")

	if show {
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

	if clear {
		if err := bootstrap.Remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
			return out.Error("Failed to clear bootstrap configuration", err)
		}
		info := map[string]any{"cleared": true}
		if path, err := bootstrap.Path(); err == nil {
			info["path"] = path
		}
		return out.Success("Bootstrap configuration cleared", info)
	}

	rawURL, _ := cmd.Flags().GetString("url")
	baseURL := strings.TrimSpace(rawURL)
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

	token, _ := cmd.Flags().GetString("token")
	token = strings.TrimSpace(token)

	insecure, _ := cmd.Flags().GetBool("insecure")
	caCert, _ := cmd.Flags().GetString("ca-cert")
	caCert = strings.TrimSpace(caCert)
	serverName, _ := cmd.Flags().GetString("server-name")
	serverName = strings.TrimSpace(serverName)
	name, _ := cmd.Flags().GetString("name")
	name = strings.TrimSpace(name)

	if caCert != "" {
		if _, err := os.Stat(caCert); err != nil {
			return out.Error("CA certificate not accessible", err)
		}
	}

	cfg := &bootstrap.Config{
		BaseURL: baseURL,
	}
	if token != "" {
		cfg.APIToken = token
	}
	if name != "" {
		cfg.Metadata = &bootstrap.MetaSection{Name: name}
	}
	if insecure || caCert != "" || serverName != "" {
		cfg.TLS = &bootstrap.TLSConfig{
			Insecure:   insecure,
			CACertPath: caCert,
			ServerName: serverName,
		}
		if cfg.TLS != nil && !cfg.TLS.Insecure && cfg.TLS.CACertPath == "" && cfg.TLS.ServerName == "" {
			cfg.TLS = nil
		}
	}

	if err := bootstrap.Save(cfg); err != nil {
		return out.Error("Failed to store bootstrap configuration", err)
	}

	info := map[string]any{
		"base_url": baseURL,
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

func pairClaim(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	code, _ := cmd.Flags().GetString("code")
	code = strings.TrimSpace(code)
	if code == "" {
		return out.Error("Pairing code is required", nil)
	}
	name, _ := cmd.Flags().GetString("name")
	baseURL, _ := cmd.Flags().GetString("url")
	baseURL = strings.TrimSpace(baseURL)
	insecure, _ := cmd.Flags().GetBool("insecure")

	if baseURL == "" {
		if c, err := client.New(); err == nil {
			baseURL = c.BaseURL()
			c.Close()
		} else {
			return out.Error("Provide --url when local configuration is unavailable", err)
		}
	}

	pairURL := strings.TrimSuffix(baseURL, "/") + "/auth/pair"
	body, err := json.Marshal(map[string]string{
		"code": strings.ToUpper(code),
		"name": strings.TrimSpace(name),
	})
	if err != nil {
		return out.Error("Failed to encode pairing payload", err)
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	if insecure {
		fmt.Fprintln(os.Stderr, "WARNING: TLS certificate verification is disabled. This is insecure and should only be used for testing.")
		httpClient.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // user explicitly requested --insecure
	}

	req, err := http.NewRequest(http.MethodPost, pairURL, bytes.NewReader(body))
	if err != nil {
		return out.Error("Failed to create request", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return out.Error("Failed to claim pairing code", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return out.Error("Failed to claim pairing code", fmt.Errorf("%s", readErrorMessage(resp)))
	}

	var payload struct {
		Token     string `json:"token"`
		Name      string `json:"name"`
		Role      string `json:"role"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return out.Error("Invalid response from daemon", err)
	}

	if out.jsonMode {
		return out.Print(payload)
	}

	fmt.Println("Pairing successful. Store this token securely:")
	fmt.Printf("  Token: %s\n", payload.Token)
	if strings.TrimSpace(payload.Name) != "" {
		fmt.Printf("  Name:  %s\n", payload.Name)
	}
	fmt.Printf("  Role:  %s\n", payload.Role)
	return nil
}
