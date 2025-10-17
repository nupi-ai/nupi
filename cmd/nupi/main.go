package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	apihttp "github.com/nupi-ai/nupi/internal/api/http"
	"github.com/nupi-ai/nupi/internal/bootstrap"
	"github.com/nupi-ai/nupi/internal/client"
	"github.com/nupi-ai/nupi/internal/config"
	"github.com/nupi-ai/nupi/internal/grpcclient"
	"github.com/nupi-ai/nupi/internal/protocol"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh/terminal"
)

const errorMessageLimit = 2048

// Global variables for use across commands
var (
	rootCmd     *cobra.Command
	instanceDir string
)

// OutputFormatter handles output in JSON or human-readable format
type OutputFormatter struct {
	jsonMode bool
}

// newOutputFormatter creates a new formatter based on the command's --json flag
func newOutputFormatter(cmd *cobra.Command) *OutputFormatter {
	jsonMode, _ := cmd.Flags().GetBool("json")
	return &OutputFormatter{jsonMode: jsonMode}
}

// Print outputs data in the appropriate format
func (f *OutputFormatter) Print(data interface{}) error {
	if f.jsonMode {
		jsonBytes, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Println(string(jsonBytes))
	} else {
		// For non-JSON mode, data should implement a custom string method
		// or we call a custom formatter function
		switch v := data.(type) {
		case string:
			fmt.Println(v)
		default:
			// Fallback to JSON for unknown types
			jsonBytes, _ := json.MarshalIndent(data, "", "  ")
			fmt.Println(string(jsonBytes))
		}
	}
	return nil
}

// Success outputs a success message
func (f *OutputFormatter) Success(message string, data map[string]interface{}) error {
	if f.jsonMode {
		output := map[string]interface{}{
			"success": true,
			"message": message,
		}
		for k, v := range data {
			output[k] = v
		}
		return f.Print(output)
	}
	fmt.Println(message)
	return nil
}

// Error outputs an error message
func (f *OutputFormatter) Error(message string, err error) error {
	if f.jsonMode {
		output := map[string]interface{}{
			"success": false,
			"error":   message,
		}
		if err != nil {
			output["details"] = err.Error()
		}
		jsonBytes, _ := json.MarshalIndent(output, "", "  ")
		fmt.Fprintln(os.Stderr, string(jsonBytes))
	} else {
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", message, err)
		} else {
			fmt.Fprintln(os.Stderr, message)
		}
	}
	return fmt.Errorf("%s: %w", message, err)
}

func init() {
	// Initialize root command
	rootCmd = &cobra.Command{
		Use:   "nupi",
		Short: "Nupi - Asynchronous PTY wrapper for AI CLI tools",
		Long: `Nupi is a PTY wrapper that allows you to run CLI tools asynchronously,
monitor multiple sessions, and attach/detach from running processes.

Designed for AI coding assistants like Claude Code, Aider, and others.`,
	}

	// Add global --json flag
	rootCmd.PersistentFlags().Bool("json", false, "Output in JSON format")

	// Set default instance directory
	homeDir, _ := os.UserHomeDir()
	instanceDir = filepath.Join(homeDir, ".nupi", "instances", "default")
}

func main() {

	// Run command
	runCmd := &cobra.Command{
		Use:                "run [command]",
		Short:              "Run a command in a PTY session (defaults to current shell)",
		Args:               cobra.MinimumNArgs(0),
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE:               runCommand,
	}

	// List command
	listCmd := &cobra.Command{
		Use:           "list",
		Short:         "List all sessions",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          listSessions,
	}

	// Attach command
	attachCmd := &cobra.Command{
		Use:           "attach [session-id]",
		Short:         "Attach to a running session",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          attachToSession,
	}

	// Kill command
	killCmd := &cobra.Command{
		Use:           "kill [session-id]",
		Short:         "Kill a session",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          killSession,
	}

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

	// Inspect command
	inspectCmd := &cobra.Command{
		Use:                "inspect [command]",
		Short:              "Run a command with raw output inspection",
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE:               inspectCommand,
	}

	// Daemon command with subcommands
	daemonCmd := &cobra.Command{
		Use:           "daemon",
		Short:         "Daemon management commands",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Daemon status subcommand
	daemonStatusCmd := &cobra.Command{
		Use:           "status",
		Short:         "Get daemon status",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          daemonStatus,
	}
	daemonStatusCmd.Flags().Bool("grpc", false, "Use gRPC transport for daemon status")

	// Daemon stop subcommand
	daemonStopCmd := &cobra.Command{
		Use:           "stop",
		Short:         "Stop the daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          daemonStop,
	}

	daemonCmd.AddCommand(daemonStatusCmd, daemonStopCmd)

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
		Short:         "Repair configuration defaults (required module slots)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          configMigrate,
	}

	configCmd.AddCommand(configTransportCmd, configMigrateCmd)

	modulesCmd := &cobra.Command{
		Use:           "modules",
		Short:         "Inspect and control module bindings",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	modulesListCmd := &cobra.Command{
		Use:           "list",
		Short:         "Show module slots, bindings and runtime status",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          modulesList,
	}

	modulesBindCmd := &cobra.Command{
		Use:           "bind <slot> <adapter>",
		Short:         "Bind an adapter to a slot (optionally with config) and start it",
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          modulesBind,
	}
	modulesBindCmd.Flags().String("config", "", "JSON configuration payload sent to the adapter binding")

	modulesStartCmd := &cobra.Command{
		Use:           "start <slot>",
		Short:         "Start the module process for the given slot",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          modulesStart,
	}

	modulesStopCmd := &cobra.Command{
		Use:           "stop <slot>",
		Short:         "Stop the module process for the given slot (binding is kept)",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          modulesStop,
	}

	modulesCmd.AddCommand(modulesListCmd, modulesBindCmd, modulesStartCmd, modulesStopCmd)

	quickstartCmd := &cobra.Command{
		Use:           "quickstart",
		Short:         "Quickstart helper commands",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	quickstartInitCmd := &cobra.Command{
		Use:           "init",
		Short:         "Interactive quickstart wizard",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          quickstartInit,
	}

	quickstartStatusCmd := &cobra.Command{
		Use:           "status",
		Short:         "Show quickstart progress",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          quickstartStatus,
	}

	quickstartCompleteCmd := &cobra.Command{
		Use:           "complete",
		Short:         "Mark quickstart as completed and (optionally) bind adapters",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          quickstartComplete,
	}
	quickstartCompleteCmd.Flags().StringSlice("binding", nil, "Assign adapter to slot (slot=adapter) - repeatable")
	quickstartCompleteCmd.Flags().Bool("complete", true, "Mark quickstart as completed after applying bindings")

	quickstartCmd.AddCommand(quickstartInitCmd, quickstartStatusCmd, quickstartCompleteCmd)

	// Add all commands
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

	daemonCmd.AddCommand(tokensCmd, pairManageCmd)

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

	rootCmd.AddCommand(runCmd, listCmd, attachCmd, killCmd, loginCmd, inspectCmd, daemonCmd, configCmd, modulesCmd, quickstartCmd, pairRootCmd)

	// Execute
	if err := rootCmd.Execute(); err != nil {
		// Error is already printed by command handlers
		os.Exit(1)
	}
}

// runCommand runs a command through the daemon
func runCommand(cmd *cobra.Command, args []string) error {
	// Parse flags from args
	var command string
	var commandArgs []string
	localDetached := false
	localWorkDir := ""
	jsonOutput := false

	i := 0
	for i < len(args) {
		arg := args[i]

		if arg == "-d" || arg == "--detach" {
			localDetached = true
			i++
		} else if arg == "--json" {
			jsonOutput = true
			i++
		} else if arg == "-w" || arg == "--workdir" {
			if i+1 < len(args) {
				localWorkDir = args[i+1]
				i += 2
			} else {
				return fmt.Errorf("--workdir requires an argument")
			}
		} else if strings.HasPrefix(arg, "--workdir=") {
			localWorkDir = strings.TrimPrefix(arg, "--workdir=")
			i++
		} else {
			command = arg
			commandArgs = args[i+1:]
			break
		}
	}

	// If no command specified, use current shell
	if command == "" {
		command = os.Getenv("SHELL")
		if command == "" {
			command = "/bin/sh" // Fallback to POSIX shell
		}
		// Use interactive login shell flags for proper environment setup
		commandArgs = []string{"-i", "-l"}
	}

	// If no workdir specified, use current directory
	if localWorkDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			localWorkDir = cwd
		}
	}

	// Connect to daemon
	c, err := client.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return err
	}
	defer c.Close()

	// Create session
	opts := protocol.CreateSessionData{
		Command:    command,
		Args:       commandArgs,
		WorkingDir: localWorkDir,
		Env:        os.Environ(), // Pass all current environment variables
		Detached:   localDetached,
		Rows:       24,
		Cols:       80,
	}

	// Get terminal size if available
	if terminal.IsTerminal(0) {
		if cols, rows, err := terminal.GetSize(0); err == nil {
			opts.Cols = uint16(cols)
			opts.Rows = uint16(rows)
		}
	}

	session, err := c.CreateSession(opts)
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}

	if localDetached {
		out := &OutputFormatter{jsonMode: jsonOutput}
		return out.Success("Session created and running in background", map[string]interface{}{
			"session_id": session.ID,
			"command":    command,
			"args":       commandArgs,
		})
	}

	// Attach to the session
	return attachToExistingSession(c, session.ID, true)
}

// listSessions lists all active sessions
func listSessions(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	sessions, err := c.ListSessions()
	if err != nil {
		return out.Error("Failed to list sessions", err)
	}

	if out.jsonMode {
		return out.Print(map[string]interface{}{"sessions": sessions})
	}

	if len(sessions) == 0 {
		fmt.Println("No active sessions")
		return nil
	}

	fmt.Println("Active sessions:")
	fmt.Println("ID\t\tStatus\t\tCommand")
	fmt.Println("---\t\t---\t\t---")

	for _, sess := range sessions {
		fmt.Printf("%s\t%s\t\t%s %s\n",
			sess.ID,
			sess.Status,
			sess.Command,
			strings.Join(sess.Args, " "))
	}

	return nil
}

// attachToSession attaches to an existing session
func attachToSession(cmd *cobra.Command, args []string) error {
	sessionID := args[0]

	c, err := client.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return err
	}
	defer c.Close()

	fmt.Printf("Attaching to session %s...\n", sessionID)
	fmt.Println("Press Ctrl+C to detach")

	return attachToExistingSession(c, sessionID, true)
}

// attachToExistingSession handles the actual attachment
func attachToExistingSession(c *client.Client, sessionID string, includeHistory bool) error {
	// Attach to session
	if err := c.AttachSession(sessionID, includeHistory); err != nil {
		return fmt.Errorf("failed to attach: %w", err)
	}

	// Set terminal to raw mode if available
	var oldState *terminal.State
	if terminal.IsTerminal(0) {
		var err error
		oldState, err = terminal.MakeRaw(0)
		if err != nil {
			return fmt.Errorf("failed to set raw mode: %w", err)
		}
		defer terminal.Restore(0, oldState)
	}

	// Handle signals
	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGWINCH)
	defer signal.Stop(sigChan)

	sendResize := func() {
		if !terminal.IsTerminal(0) {
			return
		}
		cols, rows, err := terminal.GetSize(0)
		if err != nil {
			return
		}
		if err := c.ResizeSession(sessionID, cols, rows, nil); err != nil && !strings.Contains(err.Error(), "session") {
			fmt.Fprintf(os.Stderr, "Warning: failed to notify resize: %v\n", err)
		}
	}

	// Send initial resize snapshot so modes know local host geometry
	sendResize()

	// Stream output in goroutine
	errChan := make(chan error, 1)
	go func() {
		err := c.StreamOutput(os.Stdout)
		errChan <- err // Send nil or error
	}()

	// Handle input from stdin to session
	go func() {
		buffer := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buffer)
			if err != nil {
				if err != io.EOF {
					errChan <- err
				}
				return
			}
			if n > 0 {
				if err := c.SendInput(sessionID, buffer[:n]); err != nil {
					if !strings.Contains(err.Error(), "broken pipe") {
						errChan <- err
					}
					return
				}
			}
		}
	}()

	for {
		select {
		case sig := <-sigChan:
			switch sig {
			case syscall.SIGWINCH:
				sendResize()
			case syscall.SIGINT, syscall.SIGTERM:
				fmt.Println("\nDetaching from session...")
				c.DetachSession()
				return nil
			}
		case err := <-errChan:
			if err != nil && !strings.Contains(err.Error(), "EOF") {
				return err
			}
			return nil
		}
	}
}

// inspectCommand runs a command with inspection mode enabled
func inspectCommand(cmd *cobra.Command, args []string) error {
	// Parse flags from args (same as runCommand)
	var command string
	var commandArgs []string
	localDetached := false
	localWorkDir := ""
	jsonOutput := false

	i := 0
	for i < len(args) {
		arg := args[i]

		if arg == "-d" || arg == "--detach" {
			localDetached = true
			i++
		} else if arg == "--json" {
			jsonOutput = true
			i++
		} else if arg == "-w" || arg == "--workdir" {
			if i+1 < len(args) {
				localWorkDir = args[i+1]
				i += 2
			} else {
				return fmt.Errorf("--workdir requires an argument")
			}
		} else if strings.HasPrefix(arg, "--workdir=") {
			localWorkDir = strings.TrimPrefix(arg, "--workdir=")
			i++
		} else {
			command = arg
			commandArgs = args[i+1:]
			break
		}
	}

	if command == "" {
		return fmt.Errorf("no command specified")
	}

	// If no workdir specified, use current directory
	if localWorkDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			localWorkDir = cwd
		}
	}

	// Connect to daemon
	c, err := client.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return err
	}
	defer c.Close()

	// Create session with inspect mode enabled
	opts := protocol.CreateSessionData{
		Command:    command,
		Args:       commandArgs,
		WorkingDir: localWorkDir,
		Env:        os.Environ(), // Pass all current environment variables
		Detached:   localDetached,
		Rows:       24,
		Cols:       80,
		Inspect:    true, // Enable inspection mode
	}

	// Get terminal size if available
	if terminal.IsTerminal(0) {
		if cols, rows, err := terminal.GetSize(0); err == nil {
			opts.Cols = uint16(cols)
			opts.Rows = uint16(rows)
		}
	}

	session, err := c.CreateSession(opts)
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}

	out := &OutputFormatter{jsonMode: jsonOutput}

	if localDetached {
		return out.Success("Session created in inspection mode", map[string]interface{}{
			"session_id":  session.ID,
			"command":     command,
			"args":        commandArgs,
			"inspect":     true,
			"output_file": fmt.Sprintf("~/.nupi/inspect/%s.raw", session.ID),
		})
	}

	// Show inspection mode notice in human-readable mode only
	if !out.jsonMode {
		fmt.Printf("\033[33mInspection mode enabled. Raw output will be saved to ~/.nupi/inspect/%s.raw\033[0m\n", session.ID)
	}

	// Attach to the session
	return attachToExistingSession(c, session.ID, true)
}

// killSession kills a running session
func killSession(cmd *cobra.Command, args []string) error {
	sessionID := args[0]
	out := newOutputFormatter(cmd)

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	if err := c.KillSession(sessionID); err != nil {
		errMsg := "Failed to kill session"
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			errMsg = fmt.Sprintf("Session %s not found", sessionID)
		}
		return out.Error(errMsg, err)
	}

	return out.Success(fmt.Sprintf("Session %s killed", sessionID), map[string]interface{}{
		"session_id": sessionID,
	})
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
			"sessions_count": resp.GetSessions(),
			"port":           resp.GetPort(),
			"grpc_port":      resp.GetGrpcPort(),
			"binding":        resp.GetBinding(),
			"grpc_binding":   resp.GetGrpcBinding(),
			"auth_required":  resp.GetAuthRequired(),
		}
		if resp.GetUptimeSec() > 0 {
			status["uptime"] = resp.GetUptimeSec()
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
	resp, err := doRequest(c, req)
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

	return nil
}

type transportConfigResponse struct {
	Port           int      `json:"port"`
	Binding        string   `json:"binding"`
	TLSCertPath    string   `json:"tls_cert_path,omitempty"`
	TLSKeyPath     string   `json:"tls_key_path,omitempty"`
	AllowedOrigins []string `json:"allowed_origins"`
	GRPCPort       int      `json:"grpc_port"`
	GRPCBinding    string   `json:"grpc_binding"`
	AuthRequired   bool     `json:"auth_required"`
}

type quickstartStatusPayload struct {
	Completed    bool     `json:"completed"`
	CompletedAt  string   `json:"completed_at,omitempty"`
	PendingSlots []string `json:"pending_slots"`
}

type quickstartBindingRequest struct {
	Slot      string `json:"slot"`
	AdapterID string `json:"adapter_id"`
}

type moduleActionRequestPayload struct {
	Slot      string          `json:"slot"`
	AdapterID string          `json:"adapter_id,omitempty"`
	Config    json.RawMessage `json:"config,omitempty"`
}

type adapterInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Source  string `json:"source"`
	Version string `json:"version"`
}

func resolveDaemonBaseURL(c *client.Client) (string, map[string]interface{}, error) {
	status, err := c.GetDaemonStatus()
	if err != nil {
		return "", nil, err
	}

	return c.BaseURL(), status, nil
}

func doRequest(c *client.Client, req *http.Request) (*http.Response, error) {
	if token := strings.TrimSpace(c.Token()); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.HTTPClient().Do(req)
}

func readErrorMessage(resp *http.Response) string {
	limited := io.LimitReader(resp.Body, errorMessageLimit)
	data, err := io.ReadAll(limited)
	if err != nil || len(data) == 0 {
		return strings.TrimSpace(resp.Status)
	}
	return strings.TrimSpace(string(data))
}

func fetchQuickstartStatus(c *client.Client) (quickstartStatusPayload, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL()+"/config/quickstart", nil)
	if err != nil {
		return quickstartStatusPayload{}, err
	}
	resp, err := doRequest(c, req)
	if err != nil {
		return quickstartStatusPayload{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return quickstartStatusPayload{}, errors.New(readErrorMessage(resp))
	}

	var payload quickstartStatusPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return quickstartStatusPayload{}, err
	}

	return payload, nil
}

func runConfigMigration(c *client.Client) (apihttp.ConfigMigrationResult, error) {
	req, err := http.NewRequest(http.MethodPost, c.BaseURL()+"/config/migrate", nil)
	if err != nil {
		return apihttp.ConfigMigrationResult{}, err
	}
	resp, err := doRequest(c, req)
	if err != nil {
		return apihttp.ConfigMigrationResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apihttp.ConfigMigrationResult{}, errors.New(readErrorMessage(resp))
	}

	var payload apihttp.ConfigMigrationResult
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return apihttp.ConfigMigrationResult{}, err
	}

	return payload, nil
}

func fetchModulesOverview(c *client.Client) (apihttp.ModulesOverview, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL()+"/modules", nil)
	if err != nil {
		return apihttp.ModulesOverview{}, err
	}
	resp, err := doRequest(c, req)
	if err != nil {
		return apihttp.ModulesOverview{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apihttp.ModulesOverview{}, errors.New(readErrorMessage(resp))
	}

	var payload apihttp.ModulesOverview
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return apihttp.ModulesOverview{}, err
	}
	return payload, nil
}

func postModuleAction(c *client.Client, endpoint string, payload moduleActionRequestPayload) (apihttp.ModuleEntry, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return apihttp.ModuleEntry{}, err
	}

	req, err := http.NewRequest(http.MethodPost, c.BaseURL()+endpoint, bytes.NewReader(body))
	if err != nil {
		return apihttp.ModuleEntry{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := doRequest(c, req)
	if err != nil {
		return apihttp.ModuleEntry{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apihttp.ModuleEntry{}, errors.New(readErrorMessage(resp))
	}

	var actionResp apihttp.ModuleActionResult
	if err := json.NewDecoder(resp.Body).Decode(&actionResp); err != nil {
		return apihttp.ModuleEntry{}, err
	}
	return actionResp.Module, nil
}

func fetchAdapters(c *client.Client) ([]adapterInfo, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL()+"/config/adapters", nil)
	if err != nil {
		return nil, err
	}
	resp, err := doRequest(c, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(readErrorMessage(resp))
	}

	var payload struct {
		Adapters []adapterInfo `json:"adapters"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

	return payload.Adapters, nil
}

func printAvailableAdapters(adapters []adapterInfo) {
	if len(adapters) == 0 {
		fmt.Println("  (no adapters installed)")
		return
	}

	fmt.Println("Available adapters:")
	for idx, adapter := range adapters {
		label := adapter.ID
		if adapter.Name != "" {
			label = fmt.Sprintf("%s (%s)", adapter.Name, adapter.ID)
		}
		fmt.Printf("  %d) %s [%s]\n", idx+1, label, adapter.Type)
	}
}

func resolveAdapterChoice(input string, adapters []adapterInfo) (string, bool) {
	if idx, err := strconv.Atoi(input); err == nil {
		if idx >= 1 && idx <= len(adapters) {
			return adapters[idx-1].ID, true
		}
	}

	for _, adapter := range adapters {
		if strings.EqualFold(adapter.ID, input) || strings.EqualFold(adapter.Name, input) {
			return adapter.ID, true
		}
	}

	return "", false
}

func modulesList(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to initialise client", err)
	}
	defer c.Close()

	overview, err := fetchModulesOverview(c)
	if err != nil {
		return out.Error("Failed to fetch modules overview", err)
	}

	if out.jsonMode {
		return out.Print(overview)
	}

	if len(overview.Modules) == 0 {
		fmt.Println("No module slots found.")
		return nil
	}

	sort.Slice(overview.Modules, func(i, j int) bool {
		return strings.Compare(overview.Modules[i].Slot, overview.Modules[j].Slot) < 0
	})

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SLOT\tADAPTER\tSTATUS\tHEALTH\tUPDATED")
	for _, entry := range overview.Modules {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			entry.Slot,
			moduleAdapterLabel(entry),
			entry.Status,
			moduleHealthLabel(entry),
			entry.UpdatedAt,
		)
	}
	w.Flush()

	for _, entry := range overview.Modules {
		if entry.Runtime != nil && strings.TrimSpace(entry.Runtime.Message) != "" {
			fmt.Printf("%s: %s\n", entry.Slot, entry.Runtime.Message)
		}
	}
	return nil
}

func modulesBind(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	slot := strings.TrimSpace(args[0])
	adapter := strings.TrimSpace(args[1])
	if slot == "" || adapter == "" {
		return out.Error("Slot and adapter must be provided", errors.New("invalid arguments"))
	}

	cfg, err := cmd.Flags().GetString("config")
	if err != nil {
		return out.Error("Failed to read --config flag", err)
	}

	var raw json.RawMessage
	if trimmed := strings.TrimSpace(cfg); trimmed != "" {
		if !json.Valid([]byte(trimmed)) {
			return out.Error("Config must be valid JSON", fmt.Errorf("invalid config payload"))
		}
		raw = json.RawMessage(trimmed)
	}

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to initialise client", err)
	}
	defer c.Close()

	entry, err := postModuleAction(c, "/modules/bind", moduleActionRequestPayload{
		Slot:      slot,
		AdapterID: adapter,
		Config:    raw,
	})
	if err != nil {
		return out.Error("Failed to bind module", err)
	}

	if out.jsonMode {
		return out.Print(apihttp.ModuleActionResult{Module: entry})
	}

	printModuleSummary("Bound", entry)
	return nil
}

func modulesStart(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	slot := strings.TrimSpace(args[0])
	if slot == "" {
		return out.Error("Slot must be provided", errors.New("invalid arguments"))
	}

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to initialise client", err)
	}
	defer c.Close()

	entry, err := postModuleAction(c, "/modules/start", moduleActionRequestPayload{Slot: slot})
	if err != nil {
		return out.Error("Failed to start module", err)
	}

	if out.jsonMode {
		return out.Print(apihttp.ModuleActionResult{Module: entry})
	}

	printModuleSummary("Started", entry)
	return nil
}

func modulesStop(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	slot := strings.TrimSpace(args[0])
	if slot == "" {
		return out.Error("Slot must be provided", errors.New("invalid arguments"))
	}

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to initialise client", err)
	}
	defer c.Close()

	entry, err := postModuleAction(c, "/modules/stop", moduleActionRequestPayload{Slot: slot})
	if err != nil {
		return out.Error("Failed to stop module", err)
	}

	if out.jsonMode {
		return out.Print(apihttp.ModuleActionResult{Module: entry})
	}

	printModuleSummary("Stopped", entry)
	return nil
}

func moduleAdapterLabel(entry apihttp.ModuleEntry) string {
	if entry.AdapterID == nil || strings.TrimSpace(*entry.AdapterID) == "" {
		return "-"
	}
	return *entry.AdapterID
}

func moduleHealthLabel(entry apihttp.ModuleEntry) string {
	if entry.Runtime == nil || strings.TrimSpace(entry.Runtime.Health) == "" {
		return "-"
	}
	health := entry.Runtime.Health
	if strings.TrimSpace(entry.Runtime.ModuleID) != "" {
		health = fmt.Sprintf("%s (%s)", health, entry.Runtime.ModuleID)
	}
	return health
}

func printModuleSummary(action string, entry apihttp.ModuleEntry) {
	fmt.Printf("%s slot %s -> %s (status: %s)\n", action, entry.Slot, moduleAdapterLabel(entry), entry.Status)
	if entry.Runtime != nil {
		fmt.Printf("  Health: %s\n", moduleHealthLabel(entry))
		if entry.Runtime.Message != "" {
			fmt.Printf("  Message: %s\n", entry.Runtime.Message)
		}
		if entry.Runtime.StartedAt != nil && *entry.Runtime.StartedAt != "" {
			fmt.Printf("  Started: %s\n", *entry.Runtime.StartedAt)
		}
		fmt.Printf("  Updated: %s\n", entry.Runtime.UpdatedAt)
	}
}

func quickstartInit(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)
	if out.jsonMode {
		return out.Error("Quickstart wizard is interactive and does not support --json", nil)
	}

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	status, err := fetchQuickstartStatus(c)
	if err != nil {
		return out.Error("Failed to fetch quickstart status", err)
	}

	if status.Completed && len(status.PendingSlots) == 0 {
		fmt.Println("Quickstart is already completed. Nothing to do.")
		return nil
	}

	adapters, err := fetchAdapters(c)
	if err != nil {
		return out.Error("Failed to fetch adapters", err)
	}

	if len(adapters) == 0 {
		fmt.Println("No adapters are installed yet. Install adapters before running the wizard.")
		return nil
	}

	reader := bufio.NewReader(os.Stdin)
	var bindings []quickstartBindingRequest

	fmt.Println("=== Quickstart Wizard ===")
	if status.Completed {
		fmt.Println("Quickstart was marked complete previously, but some slots are pending.")
	}

	for _, slot := range status.PendingSlots {
		fmt.Printf("\nSlot %s requires an adapter.\n", slot)
		printAvailableAdapters(adapters)

		for {
			fmt.Printf("Select adapter for %s (enter number/id, blank to skip): ", slot)
			choice, err := reader.ReadString('\n')
			if err != nil {
				return out.Error("Failed to read input", err)
			}
			choice = strings.TrimSpace(choice)

			if choice == "" {
				fmt.Printf("Skipping %s. You can assign it later.\n", slot)
				break
			}

			if id, ok := resolveAdapterChoice(choice, adapters); ok {
				bindings = append(bindings, quickstartBindingRequest{Slot: slot, AdapterID: id})
				fmt.Printf("  -> Assigned %s to %s\n", id, slot)
				break
			}

			fmt.Println("Invalid selection. Please try again.")
		}
	}

	if len(bindings) == 0 {
		fmt.Println("\nNo bindings were selected. Quickstart remains unchanged.")
		return nil
	}

	complete := false
	if len(bindings) == len(status.PendingSlots) {
		fmt.Print("\nAll pending slots have assignments. Mark quickstart as complete? [Y/n]: ")
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		complete = answer == "" || answer == "y" || answer == "yes"
	}

	reqPayload := map[string]interface{}{
		"bindings": bindings,
	}
	if complete {
		reqPayload["complete"] = true
	}

	body, err := json.Marshal(reqPayload)
	if err != nil {
		return out.Error("Failed to encode quickstart request", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.BaseURL()+"/config/quickstart", bytes.NewReader(body))
	if err != nil {
		return out.Error("Failed to construct quickstart request", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := doRequest(c, req)
	if err != nil {
		return out.Error("Failed to submit quickstart bindings", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return out.Error("Quickstart update failed", errors.New(readErrorMessage(resp)))
	}

	var result quickstartStatusPayload
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return out.Error("Invalid response from daemon", err)
	}

	fmt.Println("\nQuickstart updated.")
	if complete && result.Completed {
		fmt.Println("Quickstart marked as completed.")
	} else {
		fmt.Printf("Quickstart completed: %v\n", result.Completed)
	}

	if len(result.PendingSlots) > 0 {
		fmt.Println("Pending slots remaining:")
		for _, slot := range result.PendingSlots {
			fmt.Printf("  - %s\n", slot)
		}
	} else {
		fmt.Println("No pending slots remaining.")
	}

	return nil
}

func quickstartStatus(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	payload, err := fetchQuickstartStatus(c)
	if err != nil {
		return out.Error("Failed to fetch quickstart status", err)
	}

	if out.jsonMode {
		return out.Print(payload)
	}

	fmt.Printf("Quickstart completed: %v\n", payload.Completed)
	if payload.CompletedAt != "" {
		fmt.Printf("Completed at: %s\n", payload.CompletedAt)
	}
	if len(payload.PendingSlots) == 0 {
		fmt.Println("Pending slots: none")
	} else {
		fmt.Println("Pending slots:")
		for _, slot := range payload.PendingSlots {
			fmt.Printf("  - %s\n", slot)
		}
	}

	return nil
}

func quickstartComplete(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	flags := cmd.Flags()
	bindingPairs, _ := flags.GetStringSlice("binding")
	completeFlag, _ := flags.GetBool("complete")

	payload := make(map[string]interface{})
	if len(bindingPairs) > 0 {
		bindings := make([]map[string]string, 0, len(bindingPairs))
		for _, pair := range bindingPairs {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) != 2 {
				return out.Error("Invalid binding format (expected slot=adapter)", errors.New(pair))
			}
			slot := strings.TrimSpace(parts[0])
			adapter := strings.TrimSpace(parts[1])
			if slot == "" {
				return out.Error("Binding slot cannot be empty", nil)
			}
			bindings = append(bindings, map[string]string{
				"slot":       slot,
				"adapter_id": adapter,
			})
		}
		payload["bindings"] = bindings
	}
	payload["complete"] = completeFlag

	body, err := json.Marshal(payload)
	if err != nil {
		return out.Error("Failed to encode request payload", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.BaseURL()+"/config/quickstart", bytes.NewReader(body))
	if err != nil {
		return out.Error("Failed to create request", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := doRequest(c, req)
	if err != nil {
		return out.Error("Failed to update quickstart status", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return out.Error("Quickstart update failed", errors.New(readErrorMessage(resp)))
	}

	var status quickstartStatusPayload
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return out.Error("Invalid response from daemon", err)
	}

	if out.jsonMode {
		return out.Print(status)
	}

	fmt.Printf("Quickstart completed: %v\n", status.Completed)
	if status.CompletedAt != "" {
		fmt.Printf("Completed at: %s\n", status.CompletedAt)
	}
	if len(status.PendingSlots) == 0 {
		fmt.Println("Pending slots: none")
	} else {
		fmt.Println("Pending slots:")
		for _, slot := range status.PendingSlots {
			fmt.Printf("  - %s\n", slot)
		}
	}

	return nil
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

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return out.Error("Failed to signal daemon", err)
	}

	return out.Success("Sent SIGTERM to daemon", map[string]any{
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
	body, _ := json.Marshal(payload)
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
		httpClient.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
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
