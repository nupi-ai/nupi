package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nupi-ai/nupi/internal/client"
	"github.com/nupi-ai/nupi/internal/config"
	"github.com/nupi-ai/nupi/internal/grpcclient"
	"github.com/nupi-ai/nupi/internal/procutil"
	"github.com/spf13/cobra"
)

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
	daemonStatusCmd.Flags().Bool("grpc", false, "Use gRPC transport for daemon status")

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
	tokensCreateCmd.Flags().String("role", "admin", "Token role (admin|read-only)")

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
	pairCreateCmd.Flags().String("role", "read-only", "Role granted to the new token (admin|read-only)")
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

// daemonStatus gets the daemon status
func daemonStatus(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)
	useGRPC, _ := cmd.Flags().GetBool("grpc")

	if useGRPC {
		gc, err := grpcclient.New()
		if err != nil {
			return out.Error("Failed to connect to daemon via gRPC", err)
		}
		defer gc.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		resp, err := gc.DaemonStatus(ctx)
		if err != nil {
			return out.Error("Failed to fetch daemon status via gRPC", err)
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

		if out.jsonMode {
			return out.Print(status)
		}

		fmt.Println("Daemon Status (gRPC):")
		fmt.Printf("  Version: %v\n", status["version"])
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
	}

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	_, status, err := resolveDaemonBaseURL(c)
	if err != nil {
		return out.Error("Failed to resolve daemon HTTP endpoint", err)
	}

	if out.jsonMode {
		return out.Print(status)
	}

	fmt.Println("Daemon Status:")
	fmt.Printf("  Version: %v\n", status["version"])
	fmt.Printf("  Sessions: %v\n", status["sessions_count"])
	fmt.Printf("  Port: %v\n", status["port"])
	if grpcPort, ok := status["grpc_port"]; ok {
		fmt.Printf("  gRPC Port: %v\n", grpcPort)
	}
	if binding, ok := status["binding"]; ok {
		fmt.Printf("  Binding: %v\n", binding)
	}
	if grpcBinding, ok := status["grpc_binding"]; ok {
		fmt.Printf("  gRPC Binding: %v\n", grpcBinding)
	}
	if authRequired, ok := status["auth_required"]; ok {
		fmt.Printf("  Auth Required: %v\n", authRequired)
	}
	if uptime, ok := status["uptime"]; ok {
		fmt.Printf("  Uptime: %v seconds\n", uptime)
	}

	return nil
}

// daemonStop stops the daemon
func daemonStop(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	var (
		apiErr      error
		apiAttempt  bool
		apiFallback bool
	)

	if c, err := client.New(); err == nil {
		apiAttempt = true
		defer c.Close()
		if err := c.ShutdownDaemon(); err == nil {
			return out.Success("Shutdown request sent to daemon", map[string]any{
				"method": "api",
			})
		} else {
			apiErr = err
			if strings.Contains(strings.ToLower(err.Error()), "unauthorized") {
				return out.Error("Daemon shutdown requires admin privileges", err)
			}
			if errors.Is(err, client.ErrShutdownUnavailable) {
				apiFallback = true
			}
		}
	} else {
		apiErr = err
	}

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
		"api_fallback": apiFallback || apiErr != nil,
	})
}

func tokensList(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	req, err := http.NewRequest(http.MethodGet, c.BaseURL()+"/auth/tokens", nil)
	if err != nil {
		return out.Error("Failed to create request", err)
	}
	resp, err := doRequest(c, req)
	if err != nil {
		return out.Error("Failed to list tokens", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return out.Error("Failed to list tokens", fmt.Errorf("%s", readErrorMessage(resp)))
	}

	var payload struct {
		Tokens []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Role        string `json:"role"`
			MaskedToken string `json:"masked_token"`
			CreatedAt   string `json:"created_at"`
		} `json:"tokens"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return out.Error("Invalid response from daemon", err)
	}

	if out.jsonMode {
		return out.Print(payload)
	}

	if len(payload.Tokens) == 0 {
		fmt.Println("No API tokens configured")
		return nil
	}

	fmt.Println("API tokens:")
	for _, tok := range payload.Tokens {
		name := tok.Name
		if strings.TrimSpace(name) == "" {
			name = "-"
		}
		fmt.Printf("  ID: %-12s  Role: %-9s  Name: %-20s  Token: %s\n", tok.ID, tok.Role, name, tok.MaskedToken)
	}

	return nil
}

func tokensCreate(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	name, _ := cmd.Flags().GetString("name")
	role, _ := cmd.Flags().GetString("role")
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		role = "admin"
	}
	if role != "admin" && role != "read-only" {
		return out.Error("Role must be 'admin' or 'read-only'", nil)
	}

	body, err := json.Marshal(map[string]string{
		"name": strings.TrimSpace(name),
		"role": role,
	})
	if err != nil {
		return out.Error("Failed to encode request", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.BaseURL()+"/auth/tokens", bytes.NewReader(body))
	if err != nil {
		return out.Error("Failed to create request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := doRequest(c, req)
	if err != nil {
		return out.Error("Failed to create token", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return out.Error("Failed to create token", fmt.Errorf("%s", readErrorMessage(resp)))
	}

	var payload struct {
		Token string `json:"token"`
		ID    string `json:"id"`
		Name  string `json:"name"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return out.Error("Invalid response from daemon", err)
	}

	if out.jsonMode {
		return out.Print(payload)
	}

	fmt.Println("New API token:")
	fmt.Printf("  Token: %s\n", payload.Token)
	fmt.Printf("  ID:    %s\n", payload.ID)
	if strings.TrimSpace(payload.Name) != "" {
		fmt.Printf("  Name:  %s\n", payload.Name)
	}
	fmt.Printf("  Role:  %s\n", payload.Role)
	fmt.Println("Store this token securely; it will not be shown again.")
	return nil
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

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	payload := map[string]string{}
	if token != "" {
		payload["token"] = token
	}
	if idFlag != "" {
		payload["id"] = idFlag
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return out.Error("Failed to encode request payload", err)
	}
	req, err := http.NewRequest(http.MethodDelete, c.BaseURL()+"/auth/tokens", bytes.NewReader(body))
	if err != nil {
		return out.Error("Failed to construct delete request", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := doRequest(c, req)
	if err != nil {
		return out.Error("Failed to delete token", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return out.Error("Failed to delete token", fmt.Errorf("%s", readErrorMessage(resp)))
	}

	if out.jsonMode {
		return out.Print(map[string]any{"deleted": true})
	}

	fmt.Println("Token deleted")
	return nil
}

func daemonPairCreate(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	name, _ := cmd.Flags().GetString("name")
	role, _ := cmd.Flags().GetString("role")
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		role = "read-only"
	}
	if role != "admin" && role != "read-only" {
		return out.Error("Role must be 'admin' or 'read-only'", nil)
	}
	expiresIn, _ := cmd.Flags().GetInt("expires-in")
	if expiresIn <= 0 {
		return out.Error("Pairing code validity (--expires-in) must be a positive number of seconds", nil)
	}

	body, err := json.Marshal(map[string]any{
		"name":               strings.TrimSpace(name),
		"role":               role,
		"expires_in_seconds": expiresIn,
	})
	if err != nil {
		return out.Error("Failed to encode pairing request", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.BaseURL()+"/auth/pairings", bytes.NewReader(body))
	if err != nil {
		return out.Error("Failed to create request", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := doRequest(c, req)
	if err != nil {
		return out.Error("Failed to create pairing", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return out.Error("Failed to create pairing", fmt.Errorf("%s", readErrorMessage(resp)))
	}

	var payload struct {
		Code      string `json:"pair_code"`
		Name      string `json:"name"`
		Role      string `json:"role"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return out.Error("Invalid response from daemon", err)
	}

	if out.jsonMode {
		return out.Print(payload)
	}

	fmt.Println("Pairing code created:")
	fmt.Printf("  Code:   %s\n", payload.Code)
	if strings.TrimSpace(payload.Name) != "" {
		fmt.Printf("  Name:   %s\n", payload.Name)
	}
	fmt.Printf("  Role:   %s\n", payload.Role)
	if payload.ExpiresAt != "" {
		fmt.Printf("  Expires: %s\n", payload.ExpiresAt)
	}
	fmt.Println("Share this code with the device you want to pair.")
	return nil
}

func daemonPairList(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	req, err := http.NewRequest(http.MethodGet, c.BaseURL()+"/auth/pairings", nil)
	if err != nil {
		return out.Error("Failed to create request", err)
	}

	resp, err := doRequest(c, req)
	if err != nil {
		return out.Error("Failed to list pairings", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return out.Error("Failed to list pairings", fmt.Errorf("%s", readErrorMessage(resp)))
	}

	var payload struct {
		Pairings []struct {
			Code      string `json:"code"`
			Name      string `json:"name"`
			Role      string `json:"role"`
			CreatedAt string `json:"created_at"`
			ExpiresAt string `json:"expires_at"`
		} `json:"pairings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return out.Error("Invalid response from daemon", err)
	}

	if out.jsonMode {
		return out.Print(payload)
	}

	if len(payload.Pairings) == 0 {
		fmt.Println("No active pairing codes")
		return nil
	}

	fmt.Println("Active pairing codes:")
	for _, pairing := range payload.Pairings {
		name := pairing.Name
		if strings.TrimSpace(name) == "" {
			name = "-"
		}
		fmt.Printf("  Code: %s  Role: %-9s  Name: %-20s  Expires: %s\n", pairing.Code, pairing.Role, name, pairing.ExpiresAt)
	}
	return nil
}
