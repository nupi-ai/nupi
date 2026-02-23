package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/config"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/grpcclient"
	"github.com/nupi-ai/nupi/internal/procutil"
	nupiversion "github.com/nupi-ai/nupi/internal/version"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

var allowedTokenRoles = constants.StringSet(constants.AllowedTokenRoles)

func newDaemonCommand() *cobra.Command {
	daemonCmd := &cobra.Command{
		Use:           "daemon",
		Short:         "Daemon management commands",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	daemonStatusCmd := &cobra.Command{
		Use:           "status",
		Short:         "Get daemon status",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          daemonStatus,
	}

	daemonStopCmd := &cobra.Command{
		Use:           "stop",
		Short:         "Stop the daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          daemonStop,
	}

	// Auth token management commands
	tokensCmd := &cobra.Command{
		Use:           "tokens",
		Short:         "Manage daemon API tokens",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	tokensListCmd := &cobra.Command{
		Use:           "list",
		Short:         "List API tokens (masked)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          tokensList,
	}

	tokensCreateCmd := &cobra.Command{
		Use:           "create",
		Short:         "Create a new API token",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          tokensCreate,
	}
	tokensCreateCmd.Flags().String("name", "", "Optional display name for the token")
	tokensCreateCmd.Flags().String("role", constants.TokenRoleAdmin, "Token role (admin|read-only)")

	tokensDeleteCmd := &cobra.Command{
		Use:           "delete",
		Short:         "Delete an API token",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          tokensDelete,
	}
	tokensDeleteCmd.Flags().String("id", "", "Token ID to delete")

	tokensCmd.AddCommand(tokensListCmd, tokensCreateCmd, tokensDeleteCmd)

	// Device pairing commands
	pairManageCmd := &cobra.Command{
		Use:           "pair",
		Short:         "Manage device pairing codes",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	pairCreateCmd := &cobra.Command{
		Use:           "create",
		Short:         "Create a new pairing code",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          daemonPairCreate,
	}
	pairCreateCmd.Flags().String("name", "", "Label for the device that will claim the code")
	pairCreateCmd.Flags().String("role", constants.TokenRoleReadOnly, "Role granted to the new token (admin|read-only)")
	pairCreateCmd.Flags().Int("expires-in", 300, "Validity window for the pairing code in seconds")

	pairListCmd := &cobra.Command{
		Use:           "list",
		Short:         "List active pairing codes",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          daemonPairList,
	}

	pairManageCmd.AddCommand(pairCreateCmd, pairListCmd)

	daemonCmd.AddCommand(daemonStatusCmd, daemonStopCmd, tokensCmd, pairManageCmd)
	return daemonCmd
}

// daemonStatus gets the daemon status via gRPC
func daemonStatus(cmd *cobra.Command, args []string) error {
	return withClientTimeout(cmd, 5*time.Second, func(ctx context.Context, gc *grpcclient.Client, out *OutputFormatter) (any, error) {
		gc.DisableVersionCheck() // this command displays version itself; avoid duplicate RPC

		resp, err := gc.DaemonStatus(ctx)
		if err != nil {
			return nil, clientCallFailed("Failed to fetch daemon status", err)
		}

		// Manual mismatch check since auto-check is disabled (AC#2 compliance)
		if w := nupiversion.CheckVersionMismatch(resp.GetVersion()); w != "" {
			fmt.Fprintln(os.Stderr, w)
		}

		status := map[string]interface{}{
			"version":        resp.GetVersion(),
			"sessions_count": resp.GetSessionsCount(),
			"port":           resp.GetPort(),
			"grpc_port":      resp.GetGrpcPort(),
			"binding":        resp.GetBinding(),
			"grpc_binding":   resp.GetGrpcBinding(),
			"auth_required":  resp.GetAuthRequired(),
		}
		if resp.GetUptimeSec() > 0 {
			status["uptime"] = resp.GetUptimeSec()
		}
		if resp.GetTlsEnabled() {
			status["tls_enabled"] = true
		}

		return CommandResult{
			Data: status,
			HumanReadable: func() error {
				fmt.Println("Daemon Status:")
				fmt.Printf("  Version: %s\n", nupiversion.FormatVersion(resp.GetVersion()))
				fmt.Printf("  Sessions: %v\n", status["sessions_count"])
				fmt.Printf("  Port: %v\n", status["port"])
				fmt.Printf("  gRPC Port: %v\n", status["grpc_port"])
				fmt.Printf("  Binding: %v\n", status["binding"])
				fmt.Printf("  gRPC Binding: %v\n", status["grpc_binding"])
				fmt.Printf("  Auth Required: %v\n", status["auth_required"])
				if uptime, ok := status["uptime"]; ok {
					fmt.Printf("  Uptime: %v seconds\n", uptime)
				}
				return nil
			},
		}, nil
	})
}

// daemonStop stops the daemon via gRPC with signal fallback
func daemonStop(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	var (
		apiErr     error
		apiAttempt bool
	)

	if gc, err := grpcclient.New(); err == nil {
		apiAttempt = true
		defer gc.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if _, err := gc.Shutdown(ctx); err == nil {
			return out.Success("Shutdown request sent to daemon", map[string]any{
				"method": "grpc",
			})
		} else {
			apiErr = err
			if st, ok := grpcstatus.FromError(err); ok && st.Code() == codes.Unauthenticated {
				return out.Error("Daemon shutdown requires admin privileges", err)
			}
		}
	} else {
		apiErr = err
	}

	// Fallback to PID-based signal.
	paths := config.GetInstancePaths("")
	data, err := os.ReadFile(paths.Lock)
	if err != nil {
		if apiAttempt {
			return out.Error("Failed to stop daemon via API and local fallback", fmt.Errorf("%v; %w", apiErr, err))
		}
		return out.Error("Failed to read daemon PID", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return out.Error("Invalid daemon PID file", err)
	}

	if err := procutil.TerminateByPID(pid); err != nil {
		return out.Error("Failed to signal daemon", err)
	}

	return out.Success("Sent termination signal to daemon", map[string]any{
		"pid":          pid,
		"method":       "signal",
		"api_fallback": apiErr != nil,
	})
}

func tokensList(cmd *cobra.Command, args []string) error {
	return withClientTimeout(cmd, 5*time.Second, func(ctx context.Context, gc *grpcclient.Client, out *OutputFormatter) (any, error) {
		resp, err := gc.ListTokens(ctx)
		if err != nil {
			return nil, clientCallFailed("Failed to list tokens", err)
		}

		tokens := resp.GetTokens()

		list := make([]map[string]interface{}, 0, len(tokens))
		for _, tok := range tokens {
			entry := map[string]interface{}{
				"id":           tok.GetId(),
				"name":         tok.GetName(),
				"role":         tok.GetRole(),
				"masked_token": tok.GetMaskedValue(),
			}
			if tok.GetCreatedAt() != nil {
				entry["created_at"] = tok.GetCreatedAt().AsTime().UTC().Format(time.RFC3339)
			}
			list = append(list, entry)
		}

		return CommandResult{
			Data: map[string]interface{}{"tokens": list},
			HumanReadable: func() error {
				if len(tokens) == 0 {
					fmt.Println("No API tokens configured")
					return nil
				}

				fmt.Println("API tokens:")
				for _, tok := range tokens {
					name := tok.GetName()
					if strings.TrimSpace(name) == "" {
						name = "-"
					}
					fmt.Printf("  ID: %-12s  Role: %-9s  Name: %-20s  Token: %s\n", tok.GetId(), tok.GetRole(), name, tok.GetMaskedValue())
				}
				return nil
			},
		}, nil
	})
}

func tokensCreate(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	name, _ := cmd.Flags().GetString("name")
	role, _ := cmd.Flags().GetString("role")
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		role = constants.TokenRoleAdmin
	}
	if _, ok := allowedTokenRoles[role]; !ok {
		return out.Error("Role must be 'admin' or 'read-only'", nil)
	}

	return withOutputClientTimeout(out, 5*time.Second, daemonConnectErrorMessage, func(ctx context.Context, gc *grpcclient.Client) (any, error) {
		resp, err := gc.CreateToken(ctx, &apiv1.CreateTokenRequest{
			Name: strings.TrimSpace(name),
			Role: role,
		})
		if err != nil {
			return nil, clientCallFailed("Failed to create token", err)
		}

		payload := map[string]interface{}{
			"token": resp.GetToken(),
		}
		if info := resp.GetInfo(); info != nil {
			payload["id"] = info.GetId()
			payload["name"] = info.GetName()
			payload["role"] = info.GetRole()
		}

		return CommandResult{
			Data: payload,
			HumanReadable: func() error {
				fmt.Println("New API token:")
				fmt.Printf("  Token: %s\n", resp.GetToken())
				if info := resp.GetInfo(); info != nil {
					fmt.Printf("  ID:    %s\n", info.GetId())
					if strings.TrimSpace(info.GetName()) != "" {
						fmt.Printf("  Name:  %s\n", info.GetName())
					}
					fmt.Printf("  Role:  %s\n", info.GetRole())
				}
				fmt.Println("Store this token securely; it will not be shown again.")
				return nil
			},
		}, nil
	})
}

func tokensDelete(cmd *cobra.Command, args []string) error {
	var token string
	if len(args) > 0 {
		token = strings.TrimSpace(args[0])
	}
	idFlag, _ := cmd.Flags().GetString("id")
	idFlag = strings.TrimSpace(idFlag)
	out := newOutputFormatter(cmd)

	if token == "" && idFlag == "" {
		return out.Error("Provide a token or --id", nil)
	}

	return withOutputClientTimeout(out, 5*time.Second, daemonConnectErrorMessage, func(ctx context.Context, gc *grpcclient.Client) (any, error) {
		if err := gc.DeleteToken(ctx, &apiv1.DeleteTokenRequest{
			Id:    idFlag,
			Token: token,
		}); err != nil {
			return nil, clientCallFailed("Failed to delete token", err)
		}

		return CommandResult{
			Data: map[string]any{"deleted": true},
			HumanReadable: func() error {
				fmt.Println("Token deleted")
				return nil
			},
		}, nil
	})
}

func daemonPairCreate(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	name, _ := cmd.Flags().GetString("name")
	role, _ := cmd.Flags().GetString("role")
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		role = constants.TokenRoleReadOnly
	}
	if _, ok := allowedTokenRoles[role]; !ok {
		return out.Error("Role must be 'admin' or 'read-only'", nil)
	}
	expiresIn, _ := cmd.Flags().GetInt("expires-in")
	if expiresIn <= 0 {
		return out.Error("Pairing code validity (--expires-in) must be a positive number of seconds", nil)
	}

	return withOutputClientTimeout(out, 5*time.Second, daemonConnectErrorMessage, func(ctx context.Context, gc *grpcclient.Client) (any, error) {
		resp, err := gc.CreatePairing(ctx, &apiv1.CreatePairingRequest{
			Name:             strings.TrimSpace(name),
			Role:             role,
			ExpiresInSeconds: int32(expiresIn),
		})
		if err != nil {
			return nil, clientCallFailed("Failed to create pairing", err)
		}

		payload := map[string]interface{}{
			"pair_code": resp.GetCode(),
			"name":      resp.GetName(),
			"role":      resp.GetRole(),
		}
		if resp.GetExpiresAt() != nil {
			payload["expires_at"] = resp.GetExpiresAt().AsTime().UTC().Format(time.RFC3339)
		}

		return CommandResult{
			Data: payload,
			HumanReadable: func() error {
				fmt.Println("Pairing code created:")
				fmt.Printf("  Code:   %s\n", resp.GetCode())
				if strings.TrimSpace(resp.GetName()) != "" {
					fmt.Printf("  Name:   %s\n", resp.GetName())
				}
				fmt.Printf("  Role:   %s\n", resp.GetRole())
				if resp.GetExpiresAt() != nil {
					fmt.Printf("  Expires: %s\n", resp.GetExpiresAt().AsTime().UTC().Format(time.RFC3339))
				}
				fmt.Println("Share this code with the device you want to pair.")
				return nil
			},
		}, nil
	})
}

func daemonPairList(cmd *cobra.Command, args []string) error {
	return withClientTimeout(cmd, 5*time.Second, func(ctx context.Context, gc *grpcclient.Client, out *OutputFormatter) (any, error) {
		resp, err := gc.ListPairings(ctx)
		if err != nil {
			return nil, clientCallFailed("Failed to list pairings", err)
		}

		pairings := resp.GetPairings()

		list := make([]map[string]interface{}, 0, len(pairings))
		for _, p := range pairings {
			entry := map[string]interface{}{
				"code": p.GetCode(),
				"name": p.GetName(),
				"role": p.GetRole(),
			}
			if p.GetCreatedAt() != nil {
				entry["created_at"] = p.GetCreatedAt().AsTime().UTC().Format(time.RFC3339)
			}
			if p.GetExpiresAt() != nil {
				entry["expires_at"] = p.GetExpiresAt().AsTime().UTC().Format(time.RFC3339)
			}
			list = append(list, entry)
		}

		return CommandResult{
			Data: map[string]interface{}{"pairings": list},
			HumanReadable: func() error {
				if len(pairings) == 0 {
					fmt.Println("No active pairing codes")
					return nil
				}

				fmt.Println("Active pairing codes:")
				for _, p := range pairings {
					name := p.GetName()
					if strings.TrimSpace(name) == "" {
						name = "-"
					}
					expiresAt := ""
					if p.GetExpiresAt() != nil {
						expiresAt = p.GetExpiresAt().AsTime().UTC().Format(time.RFC3339)
					}
					fmt.Printf("  Code: %s  Role: %-9s  Name: %-20s  Expires: %s\n", p.GetCode(), p.GetRole(), name, expiresAt)
				}
				return nil
			},
		}, nil
	})
}
