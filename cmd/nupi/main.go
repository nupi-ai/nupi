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
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	apihttp "github.com/nupi-ai/nupi/internal/api/http"
	"github.com/nupi-ai/nupi/internal/bootstrap"
	"github.com/nupi-ai/nupi/internal/client"
	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/grpcclient"
	manifestpkg "github.com/nupi-ai/nupi/internal/plugins/manifest"
	"github.com/nupi-ai/nupi/internal/protocol"
	nupiversion "github.com/nupi-ai/nupi/internal/version"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh/terminal"
	"gopkg.in/yaml.v3"
)

const (
	errorMessageLimit            = 2048
	adapterSlugMaxLength         = 64
	adapterLogsScannerInitialBuf = 64 * 1024
	adapterLogsScannerMaxBuf     = 1024 * 1024
)

// Global variables for use across commands
var (
	rootCmd     *cobra.Command
	instanceDir string
)

var (
	allowedAdapterSlots = map[string]struct{}{
		"stt":              {},
		"tts":              {},
		"ai":               {},
		"vad":              {},
		"tunnel":           {},
		"tool-handler":     {},
		"pipeline-cleaner": {},
	}
	allowedEndpointTransports = map[string]struct{}{
		"grpc":    {},
		"http":    {},
		"process": {},
	}
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
	rootCmd.Version = nupiversion.String()
	rootCmd.SetVersionTemplate("{{printf \"%s\\n\" .Version}}")

	// Add global --json flag
	rootCmd.PersistentFlags().Bool("json", false, "Output in JSON format")

	// Set default instance directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to determine home directory: %v\n", err)
		os.Exit(1)
	}
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
		Short:         "Repair configuration defaults (required adapter slots)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          configMigrate,
	}

	configCmd.AddCommand(configTransportCmd, configMigrateCmd)

	adaptersCmd := &cobra.Command{
		Use:           "adapters",
		Short:         "Inspect and control adapter bindings",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	adaptersCmd.PersistentFlags().Bool("grpc", false, "Use gRPC transport for adapter operations")

	adaptersRegisterCmd := &cobra.Command{
		Use:           "register",
		Short:         "Register or update an adapter",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          adaptersRegister,
	}
	adaptersRegisterCmd.Example = `  # Register gRPC-based STT adapter
  nupi adapters register \
    --id nupi-whisper-local-stt \
    --type stt \
    --name "Nupi Whisper Local STT" \
    --version 0.1.0 \
    --endpoint-transport grpc \
    --endpoint-address 127.0.0.1:50051

  # Register process-based AI adapter
  nupi adapters register \
    --id custom-ai \
    --type ai \
    --endpoint-transport process \
    --endpoint-command /path/to/binary \
    --endpoint-arg "--config" \
    --endpoint-arg "/etc/adapter.json"`
	adaptersRegisterCmd.Flags().String("id", "", "Adapter identifier (slug)")
	adaptersRegisterCmd.Flags().String("type", "", "Adapter type (stt/tts/ai/vad/...)")
	adaptersRegisterCmd.Flags().String("name", "", "Human readable adapter name")
	adaptersRegisterCmd.Flags().String("source", "external", "Adapter source/provider")
	adaptersRegisterCmd.Flags().String("version", "", "Adapter version")
	adaptersRegisterCmd.Flags().String("manifest", "", "Optional manifest JSON payload")
	adaptersRegisterCmd.Flags().String("endpoint-transport", "grpc", "Adapter endpoint transport (process|grpc|http)")
	adaptersRegisterCmd.Flags().String("endpoint-address", "", "Adapter endpoint address (for grpc/http transports)")
	adaptersRegisterCmd.Flags().String("endpoint-command", "", "Command to launch adapter (process transport)")
	adaptersRegisterCmd.Flags().StringArray("endpoint-arg", nil, "Argument for adapter command (repeatable)")
	adaptersRegisterCmd.Flags().StringArray("endpoint-env", nil, "Environment variable KEY=VALUE for adapter command (repeatable)")

	adaptersInstallLocalCmd := &cobra.Command{
		Use:           "install-local",
		Short:         "Register an adapter from local manifest and binary",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          adaptersInstallLocal,
	}
	adaptersInstallLocalCmd.Example = `  # Install a local Whisper STT adapter
	nupi adapters install-local \
	  --manifest-file ./adapter-nupi-whisper-local-stt/plugin.yaml \
	  --binary $(pwd)/adapter-nupi-whisper-local-stt/dist/adapter-nupi-whisper-local-stt \
	  --endpoint-address 127.0.0.1:50051 \
	  --slot stt`
	adaptersInstallLocalCmd.Flags().String("manifest-file", "", "Path to adapter manifest (YAML or JSON)")
	adaptersInstallLocalCmd.Flags().String("id", "", "Override adapter identifier (defaults to manifest metadata.slug)")
	adaptersInstallLocalCmd.Flags().String("binary", "", "Path to adapter executable (overrides manifest entrypoint.command)")
	adaptersInstallLocalCmd.Flags().Bool("copy-binary", false, "Copy adapter binary into the instance plugin directory")
	adaptersInstallLocalCmd.Flags().Bool("build", false, "Build the adapter from sources before registration")
	adaptersInstallLocalCmd.Flags().String("adapter-dir", "", "Adapter source directory (required with --build)")
	adaptersInstallLocalCmd.Flags().String("endpoint-address", "", "Address for gRPC/HTTP transport")
	adaptersInstallLocalCmd.Flags().StringArray("endpoint-arg", nil, "Additional command argument (repeatable)")
	adaptersInstallLocalCmd.Flags().StringArray("endpoint-env", nil, "Environment variable KEY=VALUE passed to the adapter (repeatable)")
	adaptersInstallLocalCmd.Flags().String("slot", "", "Optional slot to bind after registration (e.g. stt)")
	adaptersInstallLocalCmd.Flags().String("config", "", "Optional JSON configuration payload for slot binding")

	adaptersLogsCmd := &cobra.Command{
		Use:           "logs",
		Short:         "Stream adapter logs and transcripts",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          adaptersLogs,
	}
	adaptersLogsCmd.Long = `Stream real-time adapter logs and speech transcripts.

Filters:
  --slot=SLOT       Filter logs by slot (e.g. stt)
  --adapter=ID      Filter logs by adapter identifier

Notes:
  - When both --slot and --adapter are provided, transcript entries are omitted
    because transcripts are not yet mapped to specific adapters.
  - Use --json to consume newline-delimited JSON for tooling and pipelines.

Examples:
  nupi adapters logs
  nupi adapters logs --slot=stt
  nupi adapters logs --adapter=adapter.stt.mock
  nupi adapters logs --json | jq .
`
	adaptersLogsCmd.Flags().String("slot", "", "Filter logs by slot (e.g. stt)")
	adaptersLogsCmd.Flags().String("adapter", "", "Filter logs by adapter identifier")

	adaptersListCmd := &cobra.Command{
		Use:           "list",
		Short:         "Show adapter slots, bindings and runtime status (see also: nupi plugins list)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          adaptersList,
	}

	adaptersBindCmd := &cobra.Command{
		Use:           "bind <slot> <adapter>",
		Short:         "Bind an adapter to a slot (optionally with config) and start it",
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          adaptersBind,
	}
	adaptersBindCmd.Flags().String("config", "", "JSON configuration payload sent to the adapter binding")

	adaptersStartCmd := &cobra.Command{
		Use:           "start <slot>",
		Short:         "Start the adapter process for the given slot",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          adaptersStart,
	}

	adaptersStopCmd := &cobra.Command{
		Use:           "stop <slot>",
		Short:         "Stop the adapter process for the given slot (binding is kept)",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          adaptersStop,
	}

	adaptersCmd.AddCommand(adaptersRegisterCmd, adaptersListCmd, adaptersBindCmd, adaptersStartCmd, adaptersStopCmd, adaptersLogsCmd)
	adaptersCmd.AddCommand(adaptersInstallLocalCmd)

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

	voiceCmd := newVoiceCommand()
	promptsCmd := newPromptsCommand()
	rootCmd.AddCommand(runCmd, listCmd, attachCmd, killCmd, loginCmd, inspectCmd, daemonCmd, configCmd, adaptersCmd, quickstartCmd, voiceCmd, pairRootCmd, promptsCmd)

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
	Completed                bool                   `json:"completed"`
	CompletedAt              string                 `json:"completed_at,omitempty"`
	PendingSlots             []string               `json:"pending_slots"`
	Adapters                 []apihttp.AdapterEntry `json:"adapters,omitempty"`
	MissingReferenceAdapters []string               `json:"missing_reference_adapters,omitempty"`
}

type quickstartBindingRequest struct {
	Slot      string `json:"slot"`
	AdapterID string `json:"adapter_id"`
}

type adapterActionRequestPayload struct {
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

const (
	quickstartMissingRefsWarning = "WARN: Missing reference adapters: %s\n"
	quickstartMissingRefsHelp    = "Install the recommended packages before completing quickstart.\n"
)

func printMissingReferenceAdapters(missing []string, showHelp bool) {
	if len(missing) == 0 {
		return
	}
	fmt.Printf(quickstartMissingRefsWarning, strings.Join(missing, ", "))
	if showHelp {
		fmt.Print(quickstartMissingRefsHelp)
	}
}

func adapterTypeForSlot(slot string) string {
	slot = strings.TrimSpace(slot)
	if slot == "" {
		return ""
	}
	if idx := strings.IndexRune(slot, '.'); idx >= 0 {
		if idx == 0 {
			return ""
		}
		slot = slot[:idx]
	}
	return strings.ToLower(slot)
}

func filterAdaptersForSlot(slot string, adapters []adapterInfo) []adapterInfo {
	expectedType := adapterTypeForSlot(slot)
	if expectedType == "" {
		return nil
	}
	filtered := make([]adapterInfo, 0, len(adapters))
	for _, adapter := range adapters {
		if strings.EqualFold(adapter.Type, expectedType) {
			filtered = append(filtered, adapter)
		}
	}
	return filtered
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

func doStreamingRequest(c *client.Client, req *http.Request) (*http.Response, error) {
	if token := strings.TrimSpace(c.Token()); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.StreamingHTTPClient().Do(req)
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

func fetchAdaptersOverview(c *client.Client) (apihttp.AdaptersOverview, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL()+"/adapters", nil)
	if err != nil {
		return apihttp.AdaptersOverview{}, err
	}
	resp, err := doRequest(c, req)
	if err != nil {
		return apihttp.AdaptersOverview{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apihttp.AdaptersOverview{}, errors.New(readErrorMessage(resp))
	}

	var payload apihttp.AdaptersOverview
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return apihttp.AdaptersOverview{}, err
	}
	return payload, nil
}

func adaptersRegisterHTTP(out *OutputFormatter, payload apihttp.AdapterRegistrationRequest) (apihttp.AdapterDescriptor, error) {
	c, err := client.New()
	if err != nil {
		return apihttp.AdapterDescriptor{}, out.Error("Failed to initialise client", err)
	}
	defer c.Close()

	body, err := json.Marshal(payload)
	if err != nil {
		return apihttp.AdapterDescriptor{}, err
	}

	req, err := http.NewRequest(http.MethodPost, c.BaseURL()+"/adapters/register", bytes.NewReader(body))
	if err != nil {
		return apihttp.AdapterDescriptor{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := doRequest(c, req)
	if err != nil {
		return apihttp.AdapterDescriptor{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apihttp.AdapterDescriptor{}, errors.New(readErrorMessage(resp))
	}

	var result apihttp.AdapterRegistrationResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return apihttp.AdapterDescriptor{}, err
	}
	return result.Adapter, nil
}

func loadAdapterManifestFile(path string) (*manifestpkg.Manifest, string, error) {
	content, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, "", err
	}
	manifest, err := manifestpkg.Parse(content)
	if err != nil {
		return nil, "", fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.Type != manifestpkg.PluginTypeAdapter {
		return nil, "", fmt.Errorf("manifest type must be %q", manifestpkg.PluginTypeAdapter)
	}
	if manifest.Adapter == nil {
		return nil, "", fmt.Errorf("adapter manifest missing spec")
	}
	return manifest, string(content), nil
}

func parseManifest(manifestRaw string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(manifestRaw)
	if trimmed == "" {
		return nil, nil
	}

	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed), nil
	}

	var yamlData interface{}
	if err := yaml.Unmarshal([]byte(trimmed), &yamlData); err != nil {
		return nil, fmt.Errorf("invalid YAML manifest: %w", err)
	}

	normalized := normalizeYAML(yamlData)
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

func normalizeYAML(value interface{}) interface{} {
	return normalizeYAMLWithDepth(value, 0, 1024)
}

func normalizeYAMLWithDepth(value interface{}, depth, maxDepth int) interface{} {
	if depth >= maxDepth {
		return fmt.Sprint(value)
	}

	switch v := value.(type) {
	case map[interface{}]interface{}:
		m := make(map[string]interface{}, len(v))
		for key, val := range v {
			keyStr := ""
			if ks, ok := key.(string); ok {
				keyStr = ks
			} else {
				keyStr = fmt.Sprint(key)
			}
			m[keyStr] = normalizeYAMLWithDepth(val, depth+1, maxDepth)
		}
		return m
	case map[string]interface{}:
		m := make(map[string]interface{}, len(v))
		for key, val := range v {
			m[key] = normalizeYAMLWithDepth(val, depth+1, maxDepth)
		}
		return m
	case []interface{}:
		result := make([]interface{}, len(v))
		for i, elem := range v {
			result[i] = normalizeYAMLWithDepth(elem, depth+1, maxDepth)
		}
		return result
	default:
		return v
	}
}

func parseKeyValuePairs(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for _, entry := range values {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid key=value pair: %s", entry)
		}
		key := strings.TrimSpace(parts[0])
		if key == "" {
			return nil, fmt.Errorf("invalid key in %s", entry)
		}
		out[key] = parts[1]
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func postAdapterAction(c *client.Client, endpoint string, payload adapterActionRequestPayload) (apihttp.AdapterEntry, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return apihttp.AdapterEntry{}, err
	}

	req, err := http.NewRequest(http.MethodPost, c.BaseURL()+endpoint, bytes.NewReader(body))
	if err != nil {
		return apihttp.AdapterEntry{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := doRequest(c, req)
	if err != nil {
		return apihttp.AdapterEntry{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apihttp.AdapterEntry{}, errors.New(readErrorMessage(resp))
	}

	var actionResp apihttp.AdapterActionResult
	if err := json.NewDecoder(resp.Body).Decode(&actionResp); err != nil {
		return apihttp.AdapterEntry{}, err
	}
	return actionResp.Adapter, nil
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

func printAvailableAdaptersForSlot(slot string, adapters []adapterInfo) []adapterInfo {
	if len(adapters) == 0 {
		fmt.Println("Available adapters:")
		fmt.Println("  (no adapters installed)")
		return nil
	}

	expectedType := adapterTypeForSlot(slot)
	filtered := filterAdaptersForSlot(slot, adapters)

	if len(filtered) > 0 {
		if expectedType != "" {
			fmt.Printf("Available adapters for %s (type: %s):\n", slot, expectedType)
		} else {
			fmt.Printf("Available adapters for %s:\n", slot)
		}
	} else {
		if expectedType != "" {
			fmt.Printf("No adapters of type %s installed. Showing all adapters:\n", expectedType)
		} else {
			fmt.Println("Available adapters:")
		}
		filtered = adapters
	}

	for idx, adapter := range filtered {
		label := adapter.ID
		if adapter.Name != "" {
			label = fmt.Sprintf("%s (%s)", adapter.Name, adapter.ID)
		}
		fmt.Printf("  %d) %s [%s]\n", idx+1, label, adapter.Type)
	}
	return filtered
}

func resolveAdapterChoice(input string, ordered []adapterInfo, all []adapterInfo) (string, bool) {
	if idx, err := strconv.Atoi(input); err == nil {
		if idx >= 1 && idx <= len(ordered) {
			return ordered[idx-1].ID, true
		}
		return "", false
	}

	for _, adapter := range all {
		if strings.EqualFold(adapter.ID, input) || strings.EqualFold(adapter.Name, input) {
			return adapter.ID, true
		}
	}

	return "", false
}

func adaptersList(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	if !out.jsonMode {
		fmt.Fprintln(os.Stderr, "Hint: use 'nupi plugins list' for a unified view of all plugins")
	}

	useGRPC, _ := cmd.Flags().GetBool("grpc")
	if useGRPC {
		return adaptersListGRPC(out)
	}
	return adaptersListHTTP(out)
}

func adaptersRegister(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	adapterID, _ := cmd.Flags().GetString("id")
	adapterID = strings.TrimSpace(adapterID)
	if adapterID == "" {
		return out.Error("--id is required", errors.New("missing adapter id"))
	}

	adapterType, _ := cmd.Flags().GetString("type")
	adapterType = strings.TrimSpace(adapterType)
	if adapterType != "" {
		if _, ok := allowedAdapterSlots[adapterType]; !ok {
			return out.Error(fmt.Sprintf("invalid type %q (expected: stt, tts, ai, vad, tunnel, tool-handler, pipeline-cleaner)", adapterType), errors.New("invalid adapter type"))
		}
	}

	name, _ := cmd.Flags().GetString("name")
	name = strings.TrimSpace(name)
	source, _ := cmd.Flags().GetString("source")
	source = strings.TrimSpace(source)
	version, _ := cmd.Flags().GetString("version")
	version = strings.TrimSpace(version)
	manifestRaw, _ := cmd.Flags().GetString("manifest")

	manifest, err := parseManifest(manifestRaw)
	if err != nil {
		return out.Error("Manifest must be valid JSON or YAML", err)
	}

	transportOpt, _ := cmd.Flags().GetString("endpoint-transport")
	transportOpt = strings.TrimSpace(transportOpt)
	addressOpt, _ := cmd.Flags().GetString("endpoint-address")
	addressOpt = strings.TrimSpace(addressOpt)
	commandOpt, _ := cmd.Flags().GetString("endpoint-command")
	commandOpt = strings.TrimSpace(commandOpt)
	argsOpt, _ := cmd.Flags().GetStringArray("endpoint-arg")
	envOpt, _ := cmd.Flags().GetStringArray("endpoint-env")

	endpointEnv, err := parseKeyValuePairs(envOpt)
	if err != nil {
		return out.Error("Invalid --endpoint-env value", err)
	}

	var endpoint *apihttp.AdapterEndpointConfig
	if transportOpt != "" || addressOpt != "" || commandOpt != "" || len(argsOpt) > 0 || len(endpointEnv) > 0 {
		if transportOpt == "" {
			return out.Error("--endpoint-transport required when endpoint flags are specified", errors.New("missing transport"))
		}
		if _, ok := allowedEndpointTransports[transportOpt]; !ok {
			return out.Error(fmt.Sprintf("invalid transport: %s (expected: grpc, http, process)", transportOpt), errors.New("invalid transport"))
		}
		switch transportOpt {
		case "grpc":
			if addressOpt == "" {
				return out.Error("--endpoint-address required for grpc transport", errors.New("missing address"))
			}
		case "http":
			if addressOpt == "" {
				return out.Error("--endpoint-address required for http transport", errors.New("missing address"))
			}
			if commandOpt != "" || len(argsOpt) > 0 {
				return out.Error("--endpoint-command/--endpoint-arg not allowed for http transport", errors.New("conflicting endpoint flags"))
			}
		case "process":
			if commandOpt == "" {
				return out.Error("--endpoint-command required for process transport", errors.New("missing command"))
			}
			if addressOpt != "" {
				return out.Error("--endpoint-address not used for process transport", errors.New("unexpected address"))
			}
		}

		endpoint = &apihttp.AdapterEndpointConfig{
			Transport: transportOpt,
			Address:   addressOpt,
			Command:   commandOpt,
			Args:      append([]string(nil), argsOpt...),
			Env:       endpointEnv,
		}
	}

	useGRPC, _ := cmd.Flags().GetBool("grpc")
	if useGRPC {
		return out.Error("Adapter registration is only available over HTTP. Use without --grpc flag", errors.New("register RPC not in proto"))
	}

	payload := apihttp.AdapterRegistrationRequest{
		AdapterID: adapterID,
		Source:    source,
		Version:   version,
		Type:      adapterType,
		Name:      name,
		Manifest:  manifest,
		Endpoint:  endpoint,
	}

	adapter, err := adaptersRegisterHTTP(out, payload)
	if err != nil {
		return err
	}

	if out.jsonMode {
		return out.Print(apihttp.AdapterRegistrationResult{Adapter: adapter})
	}

	fmt.Printf("Registered adapter %s", adapter.ID)
	if adapter.Type != "" {
		fmt.Printf(" (%s)", adapter.Type)
	}
	fmt.Println()
	return nil
}

func adaptersInstallLocal(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	manifestPath, _ := cmd.Flags().GetString("manifest-file")
	manifestPath = strings.TrimSpace(manifestPath)
	if manifestPath == "" {
		return out.Error("--manifest-file is required", errors.New("missing manifest file"))
	}
	absManifestPath, err := filepath.Abs(config.ExpandPath(manifestPath))
	if err != nil {
		return out.Error("Failed to resolve manifest path", err)
	}
	manifestDir := filepath.Dir(absManifestPath)

	manifest, manifestRaw, err := loadAdapterManifestFile(manifestPath)
	if err != nil {
		return out.Error("Failed to read manifest", err)
	}

	manifestJSON, err := parseManifest(manifestRaw)
	if err != nil {
		return out.Error("Failed to parse manifest", err)
	}

	adapterIDFlag, _ := cmd.Flags().GetString("id")
	adapterID := strings.TrimSpace(adapterIDFlag)
	if adapterID == "" {
		adapterID = formatAdapterID(manifest.Metadata.Catalog, manifest.Metadata.Slug)
	}
	if adapterID == "" {
		return out.Error("Adapter identifier required", errors.New("manifest metadata.slug missing; use --id"))
	}

	slotType := ""
	if manifest.Adapter != nil {
		slotType = strings.TrimSpace(manifest.Adapter.Slot)
	}
	if slotType == "" {
		return out.Error("Adapter slot missing in manifest", errors.New("manifest spec.slot empty"))
	}
	if _, ok := allowedAdapterSlots[slotType]; !ok {
		return out.Error(fmt.Sprintf("unsupported adapter slot %q", slotType), errors.New("invalid adapter slot"))
	}
	adapterName := strings.TrimSpace(manifest.Metadata.Name)
	copyBinary, _ := cmd.Flags().GetBool("copy-binary")
	buildFlag, _ := cmd.Flags().GetBool("build")
	sourceDirFlag, _ := cmd.Flags().GetString("adapter-dir")
	sourceDir := strings.TrimSpace(sourceDirFlag)
	if sourceDir != "" {
		sourceDir = config.ExpandPath(sourceDir)
		absDir, err := filepath.Abs(sourceDir)
		if err != nil {
			return out.Error("Failed to resolve adapter directory", err)
		}
		sourceDir = absDir
	}

	binaryFlag, _ := cmd.Flags().GetString("binary")
	binaryFlag = strings.TrimSpace(binaryFlag)
	if binaryFlag != "" {
		binaryFlag = config.ExpandPath(binaryFlag)
		if !filepath.IsAbs(binaryFlag) {
			baseDir := sourceDir
			if baseDir == "" {
				baseDir = manifestDir
			}
			if baseDir != "" {
				binaryFlag = filepath.Join(baseDir, binaryFlag)
			}
		}
		if absBinary, err := filepath.Abs(binaryFlag); err == nil {
			binaryFlag = absBinary
		}
	}

	if buildFlag && binaryFlag != "" {
		return out.Error("Cannot combine --build with --binary", errors.New("conflicting binary options"))
	}
	if buildFlag && sourceDir == "" {
		return out.Error("--adapter-dir is required when --build is set", errors.New("missing adapter directory"))
	}

	// Use manifest slug when available so locally-installed adapters land in the same
	// directory structure as runtime-managed process transports. This keeps assets/config in
	// sync regardless of install path.
	slugSource := strings.TrimSpace(manifest.Metadata.Slug)
	if slugSource == "" {
		slugSource = adapterID
	}
	slug := sanitizeAdapterSlug(slugSource)

	var command string
	if buildFlag {
		outputDir := filepath.Join(sourceDir, "dist")
		if err := os.MkdirAll(outputDir, 0o755); err != nil {
			return out.Error("Failed to create dist directory", err)
		}
		outputPath := filepath.Join(outputDir, slug)
		buildCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		buildCmd := exec.CommandContext(buildCtx, "go", "build", "-o", outputPath, "./cmd/adapter")
		buildCmd.Dir = sourceDir
		buildCmd.Env = os.Environ()
		if output, err := buildCmd.CombinedOutput(); err != nil {
			buildErr := err
			if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
				buildErr = fmt.Errorf("%w: %s", err, trimmed)
			}
			return out.Error("Failed to build adapter", buildErr)
		}
		command = outputPath
	} else if binaryFlag != "" {
		if _, err := os.Stat(binaryFlag); err != nil {
			return out.Error(fmt.Sprintf("adapter binary not found: %s", binaryFlag), err)
		}
		command = binaryFlag
	} else {
		if manifest.Adapter == nil {
			return out.Error("Manifest missing adapter spec", errors.New("adapter spec absent"))
		}
		command = strings.TrimSpace(manifest.Adapter.Entrypoint.Command)
		if command != "" {
			command = config.ExpandPath(command)
			if !filepath.IsAbs(command) && strings.Contains(command, string(os.PathSeparator)) {
				baseDir := sourceDir
				if baseDir == "" {
					baseDir = manifestDir
				}
				if baseDir != "" {
					command = filepath.Join(baseDir, command)
				}
			}
		}
	}

	if command == "" {
		return out.Error("Manifest missing entrypoint command", errors.New("spec.entrypoint.command empty"))
	}

	args := []string{}
	if manifest.Adapter != nil {
		args = append(args, manifest.Adapter.Entrypoint.Args...)
	}
	extraArgs, _ := cmd.Flags().GetStringArray("endpoint-arg")
	if len(extraArgs) > 0 {
		args = append(args, extraArgs...)
	}

	endpointEnvValues, _ := cmd.Flags().GetStringArray("endpoint-env")
	endpointEnv, err := parseKeyValuePairs(endpointEnvValues)
	if err != nil {
		return out.Error("Invalid --endpoint-env value", err)
	}

	transport := ""
	if manifest.Adapter != nil {
		transport = strings.TrimSpace(manifest.Adapter.Entrypoint.Transport)
	}
	if transport == "" {
		transport = "process"
	}
	if _, ok := allowedEndpointTransports[transport]; !ok {
		return out.Error(fmt.Sprintf("manifest transport %q unsupported", transport), errors.New("invalid manifest transport"))
	}

	addressFlag, _ := cmd.Flags().GetString("endpoint-address")
	address := strings.TrimSpace(addressFlag)
	switch transport {
	case "grpc", "http":
		if address == "" {
			return out.Error(fmt.Sprintf("--endpoint-address required for %s transport", transport), errors.New("missing endpoint address"))
		}
	case "process":
		if command == "" {
			return out.Error("--binary or manifest entrypoint.command required for process transport", errors.New("missing command"))
		}
		if address != "" {
			return out.Error("--endpoint-address not used for process transport", errors.New("unexpected address"))
		}
	}

	if copyBinary {
		if command == "" {
			return out.Error("--binary or --build required when --copy-binary is set", errors.New("missing adapter binary"))
		}
		srcPath := command
		if !filepath.IsAbs(srcPath) {
			absSrc, err := filepath.Abs(srcPath)
			if err != nil {
				return out.Error("Failed to resolve adapter binary", err)
			}
			srcPath = absSrc
		}
		paths, err := config.EnsureInstanceDirs("")
		if err != nil {
			return out.Error("Failed to prepare instance directories", err)
		}
		adapterHome := filepath.Join(paths.Home, "plugins", slug)
		binDir := filepath.Join(adapterHome, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			return out.Error("Failed to create adapter bin directory", err)
		}
		destName := filepath.Base(srcPath)
		if destName == "" {
			destName = slug
		}
		destPath := filepath.Join(binDir, destName)
		if rel, err := filepath.Rel(adapterHome, destPath); err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(filepath.ToSlash(rel), "../") {
			return out.Error("Resolved destination escapes adapter directory", errors.New("invalid destination path"))
		}
		if err := copyFile(srcPath, destPath, 0o755); err != nil {
			return out.Error("Failed to copy adapter binary", err)
		}
		command = destPath
	}

	payload := apihttp.AdapterRegistrationRequest{
		AdapterID: adapterID,
		Source:    "local",
		Type:      slotType,
		Name:      adapterName,
		Manifest:  manifestJSON,
	}

	payload.Endpoint = &apihttp.AdapterEndpointConfig{
		Transport: transport,
		Address:   address,
		Command:   command,
		Args:      args,
		Env:       endpointEnv,
	}

	adapter, err := adaptersRegisterHTTP(out, payload)
	if err != nil {
		return err
	}

	if out.jsonMode {
		if err := out.Print(apihttp.AdapterRegistrationResult{Adapter: adapter}); err != nil {
			return err
		}
	} else {
		fmt.Printf("Installed local adapter %s (%s)\n", adapter.Name, adapter.ID)
	}

	slot, _ := cmd.Flags().GetString("slot")
	slot = strings.TrimSpace(slot)
	if slot == "" {
		return nil
	}

	configRaw, _ := cmd.Flags().GetString("config")
	configRaw = strings.TrimSpace(configRaw)
	var bindConfig json.RawMessage
	if configRaw != "" {
		if !json.Valid([]byte(configRaw)) {
			return out.Error("Config must be valid JSON", errors.New("invalid config payload"))
		}
		bindConfig = json.RawMessage(configRaw)
	}

	if err := adaptersBindHTTP(out, slot, adapterID, bindConfig); err != nil {
		return err
	}

	if !out.jsonMode {
		fmt.Printf("Bound adapter %s to %s\n", adapterID, slot)
	}
	return nil
}

func adaptersLogs(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	useGRPC, _ := cmd.Flags().GetBool("grpc")
	if useGRPC {
		return out.Error("Adapter logs are only available over HTTP. Use without --grpc flag", errors.New("logs RPC not in proto"))
	}

	slot, _ := cmd.Flags().GetString("slot")
	adapter, _ := cmd.Flags().GetString("adapter")
	slot = strings.TrimSpace(slot)
	adapter = strings.TrimSpace(adapter)

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	params := url.Values{}
	if slot != "" {
		params.Set("slot", slot)
	}
	if adapter != "" {
		params.Set("adapter", adapter)
	}

	endpoint := c.BaseURL() + "/adapters/logs"
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return out.Error("Failed to create request", err)
	}

	resp, err := doStreamingRequest(c, req)
	if err != nil {
		return out.Error("Failed to fetch adapter logs", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return out.Error("Failed to fetch adapter logs", fmt.Errorf("%s", readErrorMessage(resp)))
	}

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, adapterLogsScannerInitialBuf)
	scanner.Buffer(buf, adapterLogsScannerMaxBuf)

	for scanner.Scan() {
		line := scanner.Text()
		if out.jsonMode {
			fmt.Println(line)
			continue
		}

		var entry apihttp.AdapterLogStreamEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			fmt.Fprintf(os.Stderr, "decode log entry failed: %v\n", err)
			fmt.Println(line)
			continue
		}
		printAdapterLogEntry(entry)
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		if errors.Is(err, bufio.ErrTooLong) {
			return out.Error("Adapter log entry exceeded 1MB limit", err)
		}
		return out.Error("Adapter log stream ended with error", err)
	}

	return nil
}

func printAdapterLogEntry(entry apihttp.AdapterLogStreamEntry) {
	ts := entry.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	timestamp := ts.Format(time.RFC3339)

	switch entry.Type {
	case "log":
		level := strings.ToUpper(strings.TrimSpace(entry.Level))
		if level == "" {
			level = "INFO"
		}
		slot := strings.TrimSpace(entry.Slot)
		entryAdapterID := strings.TrimSpace(entry.AdapterID)
		if slot != "" {
			if entryAdapterID != "" {
				fmt.Printf("%s [%s] %s %s: %s\n", timestamp, slot, entryAdapterID, level, entry.Message)
			} else {
				fmt.Printf("%s [%s] %s: %s\n", timestamp, slot, level, entry.Message)
			}
		} else {
			if entryAdapterID != "" {
				fmt.Printf("%s %s %s: %s\n", timestamp, entryAdapterID, level, entry.Message)
			} else {
				fmt.Printf("%s %s: %s\n", timestamp, level, entry.Message)
			}
		}
	case "transcript":
		stage := "partial"
		if entry.Final {
			stage = "final"
		}
		fmt.Printf("%s [transcript %s] session=%s stream=%s conf=%.2f %s\n",
			timestamp, stage, entry.SessionID, entry.StreamID, entry.Confidence, entry.Text)
	default:
		fmt.Printf("%s [unknown type=%s] %s\n", timestamp, entry.Type, entry.Message)
	}
}

func adaptersBind(cmd *cobra.Command, args []string) error {
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

	useGRPC, _ := cmd.Flags().GetBool("grpc")
	if useGRPC {
		return adaptersBindGRPC(out, slot, adapter, string(raw))
	}
	return adaptersBindHTTP(out, slot, adapter, raw)
}

func adaptersStart(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	slot := strings.TrimSpace(args[0])
	if slot == "" {
		return out.Error("Slot must be provided", errors.New("invalid arguments"))
	}

	useGRPC, _ := cmd.Flags().GetBool("grpc")
	if useGRPC {
		return adaptersStartGRPC(out, slot)
	}
	return adaptersStartHTTP(out, slot)
}

func adaptersStop(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	slot := strings.TrimSpace(args[0])
	if slot == "" {
		return out.Error("Slot must be provided", errors.New("invalid arguments"))
	}

	useGRPC, _ := cmd.Flags().GetBool("grpc")
	if useGRPC {
		return adaptersStopGRPC(out, slot)
	}
	return adaptersStopHTTP(out, slot)
}

func adaptersListHTTP(out *OutputFormatter) error {
	c, err := client.New()
	if err != nil {
		return out.Error("Failed to initialise client", err)
	}
	defer c.Close()

	overview, err := fetchAdaptersOverview(c)
	if err != nil {
		return out.Error("Failed to fetch adapters overview", err)
	}

	if out.jsonMode {
		return out.Print(overview)
	}

	if len(overview.Adapters) == 0 {
		fmt.Println("No adapter slots found.")
		return nil
	}

	printAdapterTable(overview.Adapters)
	printAdapterRuntimeMessages(overview.Adapters)
	return nil
}

func adaptersListGRPC(out *OutputFormatter) error {
	gc, err := grpcclient.New()
	if err != nil {
		return out.Error("Failed to connect to daemon via gRPC", err)
	}
	defer gc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := gc.AdaptersOverview(ctx)
	if err != nil {
		return out.Error("Failed to fetch adapters overview via gRPC", err)
	}

	overview := adaptersOverviewFromProto(resp)
	if out.jsonMode {
		return out.Print(overview)
	}

	if len(overview.Adapters) == 0 {
		fmt.Println("No adapter slots found.")
		return nil
	}

	printAdapterTable(overview.Adapters)
	printAdapterRuntimeMessages(overview.Adapters)
	return nil
}

func adaptersBindHTTP(out *OutputFormatter, slot, adapter string, raw json.RawMessage) error {
	c, err := client.New()
	if err != nil {
		return out.Error("Failed to initialise client", err)
	}
	defer c.Close()

	entry, err := postAdapterAction(c, "/adapters/bind", adapterActionRequestPayload{
		Slot:      slot,
		AdapterID: adapter,
		Config:    raw,
	})
	if err != nil {
		return out.Error("Failed to bind adapter", err)
	}

	if out.jsonMode {
		return out.Print(apihttp.AdapterActionResult{Adapter: entry})
	}

	printAdapterSummary("Bound", entry)
	return nil
}

func adaptersBindGRPC(out *OutputFormatter, slot, adapter, cfg string) error {
	gc, err := grpcclient.New()
	if err != nil {
		return out.Error("Failed to connect to daemon via gRPC", err)
	}
	defer gc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := &apiv1.BindAdapterRequest{
		Slot:       slot,
		AdapterId:  adapter,
		ConfigJson: strings.TrimSpace(cfg),
	}

	resp, err := gc.BindAdapter(ctx, req)
	if err != nil {
		return out.Error("Failed to bind adapter via gRPC", err)
	}

	entry := adapterEntryFromProto(resp.GetAdapter())
	if out.jsonMode {
		return out.Print(apihttp.AdapterActionResult{Adapter: entry})
	}

	printAdapterSummary("Bound", entry)
	return nil
}

func adaptersStartHTTP(out *OutputFormatter, slot string) error {
	c, err := client.New()
	if err != nil {
		return out.Error("Failed to initialise client", err)
	}
	defer c.Close()

	entry, err := postAdapterAction(c, "/adapters/start", adapterActionRequestPayload{Slot: slot})
	if err != nil {
		return out.Error("Failed to start adapter", err)
	}

	if out.jsonMode {
		return out.Print(apihttp.AdapterActionResult{Adapter: entry})
	}

	printAdapterSummary("Started", entry)
	return nil
}

func adaptersStartGRPC(out *OutputFormatter, slot string) error {
	gc, err := grpcclient.New()
	if err != nil {
		return out.Error("Failed to connect to daemon via gRPC", err)
	}
	defer gc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := gc.StartAdapter(ctx, slot)
	if err != nil {
		return out.Error("Failed to start adapter via gRPC", err)
	}

	entry := adapterEntryFromProto(resp.GetAdapter())
	if out.jsonMode {
		return out.Print(apihttp.AdapterActionResult{Adapter: entry})
	}

	printAdapterSummary("Started", entry)
	return nil
}

func adaptersStopHTTP(out *OutputFormatter, slot string) error {
	c, err := client.New()
	if err != nil {
		return out.Error("Failed to initialise client", err)
	}
	defer c.Close()

	entry, err := postAdapterAction(c, "/adapters/stop", adapterActionRequestPayload{Slot: slot})
	if err != nil {
		return out.Error("Failed to stop adapter", err)
	}

	if out.jsonMode {
		return out.Print(apihttp.AdapterActionResult{Adapter: entry})
	}

	printAdapterSummary("Stopped", entry)
	return nil
}

func adaptersStopGRPC(out *OutputFormatter, slot string) error {
	gc, err := grpcclient.New()
	if err != nil {
		return out.Error("Failed to connect to daemon via gRPC", err)
	}
	defer gc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := gc.StopAdapter(ctx, slot)
	if err != nil {
		return out.Error("Failed to stop adapter via gRPC", err)
	}

	entry := adapterEntryFromProto(resp.GetAdapter())
	if out.jsonMode {
		return out.Print(apihttp.AdapterActionResult{Adapter: entry})
	}

	printAdapterSummary("Stopped", entry)
	return nil
}

func adaptersOverviewFromProto(resp *apiv1.AdaptersOverviewResponse) apihttp.AdaptersOverview {
	if resp == nil {
		return apihttp.AdaptersOverview{}
	}
	out := apihttp.AdaptersOverview{
		Adapters: make([]apihttp.AdapterEntry, 0, len(resp.GetAdapters())),
	}
	for _, entry := range resp.GetAdapters() {
		out.Adapters = append(out.Adapters, adapterEntryFromProto(entry))
	}
	return out
}

func adapterEntryFromProto(entry *apiv1.AdapterEntry) apihttp.AdapterEntry {
	if entry == nil {
		return apihttp.AdapterEntry{}
	}
	out := apihttp.AdapterEntry{
		Slot:      entry.GetSlot(),
		Status:    entry.GetStatus(),
		Config:    entry.GetConfigJson(),
		UpdatedAt: entry.GetUpdatedAt(),
	}
	if entry.AdapterId != nil {
		id := strings.TrimSpace(entry.GetAdapterId())
		if id != "" {
			out.AdapterID = &id
		}
	}
	if rt := entry.GetRuntime(); rt != nil {
		runtime := apihttp.AdapterRuntime{
			AdapterID: rt.GetAdapterId(),
			Health:    rt.GetHealth(),
			Message:   rt.GetMessage(),
			Extra:     rt.GetExtra(),
		}
		if updated := rt.GetUpdatedAt(); updated != nil {
			runtime.UpdatedAt = updated.AsTime().UTC().Format(time.RFC3339)
		}
		if ts := rt.GetStartedAt(); ts != nil {
			started := ts.AsTime().UTC().Format(time.RFC3339)
			runtime.StartedAt = &started
		}
		out.Runtime = &runtime
	}
	return out
}

func formatAdapterID(catalog, slug string) string {
	catalog = strings.TrimSpace(catalog)
	slug = strings.TrimSpace(slug)
	if catalog == "" {
		catalog = "others"
	}
	return catalog + "/" + slug
}

func adapterLabel(entry apihttp.AdapterEntry) string {
	if entry.AdapterID == nil || strings.TrimSpace(*entry.AdapterID) == "" {
		return "-"
	}
	return *entry.AdapterID
}

func adapterHealthLabel(entry apihttp.AdapterEntry) string {
	if entry.Runtime == nil || strings.TrimSpace(entry.Runtime.Health) == "" {
		return "-"
	}
	return entry.Runtime.Health
}

func sortedAdapters(entries []apihttp.AdapterEntry) []apihttp.AdapterEntry {
	if len(entries) == 0 {
		return nil
	}
	sorted := append([]apihttp.AdapterEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		return strings.Compare(sorted[i].Slot, sorted[j].Slot) < 0
	})
	return sorted
}

func printAdapterTable(entries []apihttp.AdapterEntry) {
	sorted := sortedAdapters(entries)
	if len(sorted) == 0 {
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SLOT\tADAPTER\tSTATUS\tHEALTH\tUPDATED")
	for _, entry := range sorted {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			entry.Slot,
			adapterLabel(entry),
			entry.Status,
			adapterHealthLabel(entry),
			entry.UpdatedAt,
		)
	}
	w.Flush()
}

func printAdapterRuntimeMessages(entries []apihttp.AdapterEntry) {
	for _, entry := range sortedAdapters(entries) {
		if entry.Runtime != nil && strings.TrimSpace(entry.Runtime.Message) != "" {
			fmt.Printf("%s: %s\n", entry.Slot, entry.Runtime.Message)
		}
	}
}

func printAdapterSummary(action string, entry apihttp.AdapterEntry) {
	fmt.Printf("%s slot %s -> %s (status: %s)\n", action, entry.Slot, adapterLabel(entry), entry.Status)
	if entry.Runtime != nil {
		fmt.Printf("  Health: %s\n", adapterHealthLabel(entry))
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

	if len(status.Adapters) > 0 {
		fmt.Println("Current adapter status:")
		printAdapterTable(status.Adapters)
		printAdapterRuntimeMessages(status.Adapters)
		fmt.Println()
	}

	missingRefs := status.MissingReferenceAdapters
	printMissingReferenceAdapters(missingRefs, true)

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
		slotAdapters := printAvailableAdaptersForSlot(slot, adapters)

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

			if id, ok := resolveAdapterChoice(choice, slotAdapters, adapters); ok {
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

	allowComplete := len(missingRefs) == 0
	complete := false
	if len(bindings) == len(status.PendingSlots) {
		if !allowComplete {
			fmt.Println("\nAll pending slots are assigned, but reference adapters are still missing. Quickstart will remain incomplete.")
		} else {
			fmt.Print("\nAll pending slots have assignments. Mark quickstart as complete? [Y/n]: ")
			answer, _ := reader.ReadString('\n')
			answer = strings.TrimSpace(strings.ToLower(answer))
			complete = answer == "" || answer == "y" || answer == "yes"
		}
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

	if len(result.Adapters) > 0 {
		fmt.Println("\nUpdated adapter status:")
		printAdapterTable(result.Adapters)
		printAdapterRuntimeMessages(result.Adapters)
	}

	printMissingReferenceAdapters(result.MissingReferenceAdapters, true)

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

	if len(payload.MissingReferenceAdapters) > 0 {
		printMissingReferenceAdapters(payload.MissingReferenceAdapters, true)
	}

	if len(payload.Adapters) == 0 {
		fmt.Println("Adapters: none reported")
	} else {
		fmt.Println("\nAdapters:")
		printAdapterTable(payload.Adapters)
		printAdapterRuntimeMessages(payload.Adapters)
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

	if completeFlag {
		if status, statusErr := fetchQuickstartStatus(c); statusErr == nil {
			if missing := status.MissingReferenceAdapters; len(missing) > 0 {
				return out.Error(
					"Cannot complete quickstart",
					fmt.Errorf("missing reference adapters: %s (install the recommended packages before completing quickstart)", strings.Join(missing, ", ")),
				)
			}
		}
	}

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

	if len(status.Adapters) == 0 {
		fmt.Println("Adapters: none reported")
	} else {
		fmt.Println("\nAdapters:")
		printAdapterTable(status.Adapters)
		printAdapterRuntimeMessages(status.Adapters)
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

func sanitizeAdapterSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.' || r == '/':
			b.WriteRune('-')
		}
	}
	res := strings.Trim(b.String(), "-_")
	if res == "" {
		return "adapter"
	}
	if len(res) > adapterSlugMaxLength {
		return res[:adapterSlugMaxLength]
	}
	return res
}

func copyFile(src, dst string, perm fs.FileMode) error {
	cleanSrc := filepath.Clean(src)
	cleanDst := filepath.Clean(dst)

	absSrc, err := filepath.Abs(cleanSrc)
	if err != nil {
		return err
	}
	absDst, err := filepath.Abs(cleanDst)
	if err != nil {
		return err
	}

	if absSrc == absDst {
		return fmt.Errorf("source and destination are the same: %s", absSrc)
	}

	srcInfo, err := os.Lstat(absSrc)
	if err != nil {
		return err
	}
	if srcInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source is a symlink: %s", absSrc)
	}
	if !srcInfo.Mode().IsRegular() {
		return fmt.Errorf("source must be a regular file: %s", absSrc)
	}

	if _, err := os.Stat(absDst); err == nil {
		return fmt.Errorf("destination already exists: %s", absDst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(absDst), 0o755); err != nil {
		return err
	}

	srcFile, err := os.Open(absSrc)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(absDst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}

	if err := dstFile.Chmod(perm); err != nil {
		return err
	}

	return nil
}

// newPromptsCommand creates the prompts management command
func newPromptsCommand() *cobra.Command {
	promptsCmd := &cobra.Command{
		Use:   "prompts",
		Short: "Manage AI prompt templates",
		Long: `Manage AI prompt templates used by Nupi.

Prompt templates are stored in the SQLite configuration database and can be
edited to customize how Nupi interacts with AI adapters.

Available templates:
  - user_intent: Interprets user voice/text commands
  - session_output: Analyzes terminal output for notifications
  - history_summary: Summarizes conversation history
  - clarification: Handles follow-up responses

Templates use Go text/template syntax. The marker "---USER---" separates the
system prompt from the user prompt.`,
	}

	// nupi prompts list
	listCmd := &cobra.Command{
		Use:           "list",
		Short:         "List available prompt templates",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          promptsList,
	}

	// nupi prompts show <template>
	showCmd := &cobra.Command{
		Use:           "show <template>",
		Short:         "Show contents of a prompt template",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          promptsShow,
	}

	// nupi prompts edit <template>
	editCmd := &cobra.Command{
		Use:           "edit <template>",
		Short:         "Edit a prompt template in your default editor",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          promptsEdit,
	}

	// nupi prompts reset [template]
	resetCmd := &cobra.Command{
		Use:           "reset [template]",
		Short:         "Reset prompt template(s) to defaults",
		Long:          "Reset one or all prompt templates to their default values. Without arguments, resets all templates.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          promptsReset,
	}
	resetCmd.Flags().Bool("all", false, "Reset all templates")

	promptsCmd.AddCommand(listCmd, showCmd, editCmd, resetCmd)
	return promptsCmd
}

func openPromptsStore() (*configstore.Store, error) {
	return configstore.Open(configstore.Options{})
}

// validPromptEventTypes returns the list of valid event types for prompt templates.
// Uses shared definitions from config/store package (single source of truth).
func validPromptEventTypes() []string {
	descriptions := configstore.PromptEventDescriptions()
	types := make([]string, 0, len(descriptions))
	for t := range descriptions {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// validatePromptEventType checks if the given event type is valid.
// Uses shared definitions from config/store package (single source of truth).
func validatePromptEventType(eventType string) error {
	descriptions := configstore.PromptEventDescriptions()
	if _, ok := descriptions[eventType]; !ok {
		return fmt.Errorf("unknown template type: %q (valid types: %s)",
			eventType, strings.Join(validPromptEventTypes(), ", "))
	}
	return nil
}

func promptsList(cmd *cobra.Command, args []string) error {
	formatter := newOutputFormatter(cmd)

	store, err := openPromptsStore()
	if err != nil {
		return formatter.Error("Failed to open config store", err)
	}
	defer store.Close()

	templates, err := store.ListPromptTemplates(context.Background())
	if err != nil {
		return formatter.Error("Failed to list templates", err)
	}

	descriptions := configstore.PromptEventDescriptions()

	if formatter.jsonMode {
		result := make([]map[string]interface{}, 0, len(templates))
		for _, t := range templates {
			result = append(result, map[string]interface{}{
				"event_type":  t.EventType,
				"is_custom":   t.IsCustom,
				"updated_at":  t.UpdatedAt,
				"description": descriptions[t.EventType],
			})
		}
		return formatter.Print(map[string]interface{}{
			"templates": result,
		})
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TEMPLATE\tSTATUS\tDESCRIPTION")
	for _, t := range templates {
		status := "default"
		if t.IsCustom {
			status = "custom"
		}
		desc := descriptions[t.EventType]
		fmt.Fprintf(tw, "%s\t%s\t%s\n", t.EventType, status, desc)
	}
	tw.Flush()
	return nil
}

func promptsShow(cmd *cobra.Command, args []string) error {
	formatter := newOutputFormatter(cmd)
	templateName := strings.TrimSuffix(args[0], ".tmpl") // Support both "user_intent" and "user_intent.tmpl"

	// Validate event type before querying store
	if err := validatePromptEventType(templateName); err != nil {
		return formatter.Error(err.Error(), nil)
	}

	store, err := openPromptsStore()
	if err != nil {
		return formatter.Error("Failed to open config store", err)
	}
	defer store.Close()

	pt, err := store.GetPromptTemplate(context.Background(), templateName)
	if err != nil {
		if configstore.IsNotFound(err) {
			return formatter.Error(fmt.Sprintf("Template not found: %s", templateName), nil)
		}
		return formatter.Error("Failed to get template", err)
	}

	if formatter.jsonMode {
		return formatter.Print(map[string]interface{}{
			"template":  pt.EventType,
			"is_custom": pt.IsCustom,
			"content":   pt.Content,
		})
	}

	status := "default"
	if pt.IsCustom {
		status = "custom"
	}
	fmt.Printf("# Template: %s (%s)\n\n", pt.EventType, status)
	fmt.Print(pt.Content)
	if !strings.HasSuffix(pt.Content, "\n") {
		fmt.Println()
	}
	return nil
}

func promptsEdit(cmd *cobra.Command, args []string) error {
	templateName := strings.TrimSuffix(args[0], ".tmpl")

	// Validate event type
	if err := validatePromptEventType(templateName); err != nil {
		return err
	}

	store, err := openPromptsStore()
	if err != nil {
		return fmt.Errorf("failed to open config store: %w", err)
	}
	defer store.Close()

	// Get current content
	pt, err := store.GetPromptTemplate(context.Background(), templateName)
	if err != nil {
		if configstore.IsNotFound(err) {
			return fmt.Errorf("template not found: %s", templateName)
		}
		return fmt.Errorf("failed to get template: %w", err)
	}

	// Write to temp file for editing
	tmpFile, err := os.CreateTemp("", fmt.Sprintf("nupi-prompt-%s-*.tmpl", templateName))
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.WriteString(pt.Content); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	tmpFile.Close()

	// Get editor from environment
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi" // fallback
	}

	// Open editor
	editorCmd := exec.Command(editor, tmpPath)
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr

	if err := editorCmd.Run(); err != nil {
		return fmt.Errorf("failed to open editor: %w", err)
	}

	// Read edited content
	newContent, err := os.ReadFile(tmpPath)
	if err != nil {
		return fmt.Errorf("failed to read edited file: %w", err)
	}

	// Save to store
	if err := store.SetPromptTemplate(context.Background(), templateName, string(newContent)); err != nil {
		return fmt.Errorf("failed to save template: %w", err)
	}

	fmt.Printf("Template saved: %s\n", templateName)
	fmt.Println("Note: Restart the daemon for changes to take effect.")
	return nil
}

func promptsReset(cmd *cobra.Command, args []string) error {
	formatter := newOutputFormatter(cmd)
	resetAll, _ := cmd.Flags().GetBool("all")

	store, err := openPromptsStore()
	if err != nil {
		return formatter.Error("Failed to open config store", err)
	}
	defer store.Close()

	defaults := configstore.DefaultPromptTemplates()

	var toReset []string
	if resetAll || len(args) == 0 {
		for name := range defaults {
			toReset = append(toReset, name)
		}
		sort.Strings(toReset)
	} else {
		templateName := strings.TrimSuffix(args[0], ".tmpl")
		if err := validatePromptEventType(templateName); err != nil {
			return formatter.Error(err.Error(), nil)
		}
		toReset = append(toReset, templateName)
	}

	var reset []string
	for _, name := range toReset {
		if err := store.ResetPromptTemplate(context.Background(), name, defaults[name]); err != nil {
			return formatter.Error(fmt.Sprintf("Failed to reset %s", name), err)
		}
		reset = append(reset, name)
	}

	if formatter.jsonMode {
		return formatter.Print(map[string]interface{}{
			"reset":   reset,
			"message": "Templates reset to defaults",
		})
	}

	if len(reset) > 0 {
		fmt.Printf("Reset templates: %s\n", strings.Join(reset, ", "))
		fmt.Println("Note: Restart the daemon for changes to take effect.")
	}
	return nil
}
