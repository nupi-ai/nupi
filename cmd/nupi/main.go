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
	"github.com/nupi-ai/nupi/internal/grpcclient"
	"github.com/nupi-ai/nupi/internal/protocol"
	nupiversion "github.com/nupi-ai/nupi/internal/version"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh/terminal"
	"gopkg.in/yaml.v3"
)

const (
	errorMessageLimit           = 2048
	moduleSlugMaxLength         = 64
	moduleLogsScannerInitialBuf = 64 * 1024
	moduleLogsScannerMaxBuf     = 1024 * 1024
)

// Global variables for use across commands
var (
	rootCmd     *cobra.Command
	instanceDir string
)

var (
	allowedModuleTypes = map[string]struct{}{
		"stt":          {},
		"tts":          {},
		"ai":           {},
		"vad":          {},
		"tunnel":       {},
		"detector":     {},
		"tool-cleaner": {},
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
	modulesCmd.PersistentFlags().Bool("grpc", false, "Use gRPC transport for module operations")

	modulesRegisterCmd := &cobra.Command{
		Use:           "register",
		Short:         "Register or update a module adapter",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          modulesRegister,
	}
	modulesRegisterCmd.Example = `  # Register gRPC-based STT module
  nupi modules register \
    --id nupi-whisper-local-stt \
    --type stt \
    --name "Nupi Whisper Local STT" \
    --version 0.1.0 \
    --endpoint-transport grpc \
    --endpoint-address 127.0.0.1:50051

  # Register process-based AI module
  nupi modules register \
    --id custom-ai \
    --type ai \
    --endpoint-transport process \
    --endpoint-command /path/to/binary \
    --endpoint-arg "--config" \
    --endpoint-arg "/etc/module.json"`
	modulesRegisterCmd.Flags().String("id", "", "Adapter identifier (slug)")
	modulesRegisterCmd.Flags().String("type", "", "Adapter type (stt/tts/ai/vad/...)")
	modulesRegisterCmd.Flags().String("name", "", "Human readable adapter name")
	modulesRegisterCmd.Flags().String("source", "external", "Adapter source/provider")
	modulesRegisterCmd.Flags().String("version", "", "Adapter version")
	modulesRegisterCmd.Flags().String("manifest", "", "Optional manifest JSON payload")
	modulesRegisterCmd.Flags().String("endpoint-transport", "grpc", "Module endpoint transport (process|grpc|http)")
	modulesRegisterCmd.Flags().String("endpoint-address", "", "Module endpoint address (for grpc/http transports)")
	modulesRegisterCmd.Flags().String("endpoint-command", "", "Command to launch module (process transport)")
	modulesRegisterCmd.Flags().StringArray("endpoint-arg", nil, "Argument for module command (repeatable)")
	modulesRegisterCmd.Flags().StringArray("endpoint-env", nil, "Environment variable KEY=VALUE for module command (repeatable)")

	modulesInstallLocalCmd := &cobra.Command{
		Use:           "install-local",
		Short:         "Register a module from local manifest and binary",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          modulesInstallLocal,
	}
	modulesInstallLocalCmd.Example = `  # Install a local Whisper STT module
	nupi modules install-local \
	  --manifest-file ./module-nupi-whisper-local-stt/module.yaml \
	  --binary $(pwd)/module-nupi-whisper-local-stt/dist/module-nupi-whisper-local-stt \
	  --endpoint-address 127.0.0.1:50051 \
	  --slot stt`
	modulesInstallLocalCmd.Flags().String("manifest-file", "", "Path to module manifest (YAML or JSON)")
	modulesInstallLocalCmd.Flags().String("id", "", "Override adapter identifier (defaults to manifest metadata.slug)")
	modulesInstallLocalCmd.Flags().String("binary", "", "Path to module executable (overrides manifest entrypoint.command)")
	modulesInstallLocalCmd.Flags().Bool("copy-binary", false, "Copy module binary into the instance plugin directory")
	modulesInstallLocalCmd.Flags().Bool("build", false, "Build the module from sources before registration")
	modulesInstallLocalCmd.Flags().String("module-dir", "", "Module source directory (required with --build)")
	modulesInstallLocalCmd.Flags().String("endpoint-address", "", "Address for gRPC/HTTP transport")
	modulesInstallLocalCmd.Flags().StringArray("endpoint-arg", nil, "Additional command argument (repeatable)")
	modulesInstallLocalCmd.Flags().StringArray("endpoint-env", nil, "Environment variable KEY=VALUE passed to the module (repeatable)")
	modulesInstallLocalCmd.Flags().String("slot", "", "Optional slot to bind after registration (e.g. stt)")
	modulesInstallLocalCmd.Flags().String("config", "", "Optional JSON configuration payload for slot binding")

	modulesLogsCmd := &cobra.Command{
		Use:           "logs",
		Short:         "Stream module logs and transcripts",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          modulesLogs,
	}
	modulesLogsCmd.Long = `Stream real-time module logs and speech transcripts.

Filters:
  --slot=SLOT       Filter logs by slot (e.g. stt)
  --adapter=ID      Filter logs by adapter identifier

Notes:
  - When both --slot and --adapter are provided, transcript entries are omitted
    because transcripts are not yet mapped to specific modules.
  - Use --json to consume newline-delimited JSON for tooling and pipelines.

Examples:
  nupi modules logs
  nupi modules logs --slot=stt
  nupi modules logs --adapter=adapter.stt.mock
  nupi modules logs --json | jq .
`
	modulesLogsCmd.Flags().String("slot", "", "Filter logs by slot (e.g. stt)")
	modulesLogsCmd.Flags().String("adapter", "", "Filter logs by adapter identifier")

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

	modulesCmd.AddCommand(modulesRegisterCmd, modulesListCmd, modulesBindCmd, modulesStartCmd, modulesStopCmd, modulesLogsCmd)
	modulesCmd.AddCommand(modulesInstallLocalCmd)

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
	rootCmd.AddCommand(runCmd, listCmd, attachCmd, killCmd, loginCmd, inspectCmd, daemonCmd, configCmd, modulesCmd, quickstartCmd, voiceCmd, pairRootCmd)

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
	Completed                bool                  `json:"completed"`
	CompletedAt              string                `json:"completed_at,omitempty"`
	PendingSlots             []string              `json:"pending_slots"`
	Modules                  []apihttp.ModuleEntry `json:"modules,omitempty"`
	MissingReferenceAdapters []string              `json:"missing_reference_adapters,omitempty"`
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

func modulesRegisterHTTP(out *OutputFormatter, payload apihttp.ModuleRegistrationRequest) (apihttp.ModuleAdapter, error) {
	c, err := client.New()
	if err != nil {
		return apihttp.ModuleAdapter{}, out.Error("Failed to initialise client", err)
	}
	defer c.Close()

	body, err := json.Marshal(payload)
	if err != nil {
		return apihttp.ModuleAdapter{}, err
	}

	req, err := http.NewRequest(http.MethodPost, c.BaseURL()+"/modules/register", bytes.NewReader(body))
	if err != nil {
		return apihttp.ModuleAdapter{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := doRequest(c, req)
	if err != nil {
		return apihttp.ModuleAdapter{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apihttp.ModuleAdapter{}, errors.New(readErrorMessage(resp))
	}

	var result apihttp.ModuleRegistrationResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return apihttp.ModuleAdapter{}, err
	}
	return result.Adapter, nil
}

type moduleManifestSpec struct {
	Metadata struct {
		Name string `yaml:"name"`
		Slug string `yaml:"slug"`
	} `yaml:"metadata"`
	Spec struct {
		ModuleType string `yaml:"moduleType"`
		Entrypoint struct {
			Command   string   `yaml:"command"`
			Args      []string `yaml:"args"`
			Transport string   `yaml:"transport"`
			ListenEnv string   `yaml:"listenEnv"`
		} `yaml:"entrypoint"`
	} `yaml:"spec"`
}

func loadModuleManifestFile(path string) (moduleManifestSpec, string, error) {
	var spec moduleManifestSpec
	content, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return spec, "", err
	}
	if err := yaml.Unmarshal(content, &spec); err != nil {
		return spec, "", fmt.Errorf("parse manifest: %w", err)
	}
	return spec, string(content), nil
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

func modulesList(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	useGRPC, _ := cmd.Flags().GetBool("grpc")
	if useGRPC {
		return modulesListGRPC(out)
	}
	return modulesListHTTP(out)
}

func modulesRegister(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	adapterID, _ := cmd.Flags().GetString("id")
	adapterID = strings.TrimSpace(adapterID)
	if adapterID == "" {
		return out.Error("--id is required", errors.New("missing adapter id"))
	}

	adapterType, _ := cmd.Flags().GetString("type")
	adapterType = strings.TrimSpace(adapterType)
	if adapterType != "" {
		if _, ok := allowedModuleTypes[adapterType]; !ok {
			return out.Error(fmt.Sprintf("invalid type %q (expected: stt, tts, ai, vad, tunnel, detector, tool-cleaner)", adapterType), errors.New("invalid adapter type"))
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

	var endpoint *apihttp.ModuleEndpointConfig
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

		endpoint = &apihttp.ModuleEndpointConfig{
			Transport: transportOpt,
			Address:   addressOpt,
			Command:   commandOpt,
			Args:      append([]string(nil), argsOpt...),
			Env:       endpointEnv,
		}
	}

	useGRPC, _ := cmd.Flags().GetBool("grpc")
	if useGRPC {
		return out.Error("Modules register over gRPC is not supported yet", errors.New("grpc not implemented"))
	}

	payload := apihttp.ModuleRegistrationRequest{
		AdapterID: adapterID,
		Source:    source,
		Version:   version,
		Type:      adapterType,
		Name:      name,
		Manifest:  manifest,
		Endpoint:  endpoint,
	}

	adapter, err := modulesRegisterHTTP(out, payload)
	if err != nil {
		return err
	}

	if out.jsonMode {
		return out.Print(apihttp.ModuleRegistrationResult{Adapter: adapter})
	}

	fmt.Printf("Registered adapter %s", adapter.ID)
	if adapter.Type != "" {
		fmt.Printf(" (%s)", adapter.Type)
	}
	fmt.Println()
	return nil
}

func modulesInstallLocal(cmd *cobra.Command, _ []string) error {
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

	spec, manifestRaw, err := loadModuleManifestFile(manifestPath)
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
		adapterID = strings.TrimSpace(spec.Metadata.Slug)
	}
	if adapterID == "" {
		return out.Error("Adapter identifier required", errors.New("manifest metadata.slug missing; use --id"))
	}

	moduleType := strings.TrimSpace(spec.Spec.ModuleType)
	if moduleType == "" {
		return out.Error("Module type missing in manifest", errors.New("manifest spec.moduleType empty"))
	}
	if _, ok := allowedModuleTypes[moduleType]; !ok {
		return out.Error(fmt.Sprintf("unsupported module type %q", moduleType), errors.New("invalid module type"))
	}
	copyBinary, _ := cmd.Flags().GetBool("copy-binary")
	buildFlag, _ := cmd.Flags().GetBool("build")
	moduleDirFlag, _ := cmd.Flags().GetString("module-dir")
	moduleDir := strings.TrimSpace(moduleDirFlag)
	if moduleDir != "" {
		moduleDir = config.ExpandPath(moduleDir)
		absDir, err := filepath.Abs(moduleDir)
		if err != nil {
			return out.Error("Failed to resolve module directory", err)
		}
		moduleDir = absDir
	}

	binaryFlag, _ := cmd.Flags().GetString("binary")
	binaryFlag = strings.TrimSpace(binaryFlag)
	if binaryFlag != "" {
		binaryFlag = config.ExpandPath(binaryFlag)
		if !filepath.IsAbs(binaryFlag) {
			baseDir := moduleDir
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
	if buildFlag && moduleDir == "" {
		return out.Error("--module-dir is required when --build is set", errors.New("missing module directory"))
	}

	slug := sanitizeModuleSlug(adapterID)

	var command string
	if buildFlag {
		outputDir := filepath.Join(moduleDir, "dist")
		if err := os.MkdirAll(outputDir, 0o755); err != nil {
			return out.Error("Failed to create dist directory", err)
		}
		outputPath := filepath.Join(outputDir, slug)
		buildCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		buildCmd := exec.CommandContext(buildCtx, "go", "build", "-o", outputPath, "./cmd/adapter")
		buildCmd.Dir = moduleDir
		buildCmd.Env = os.Environ()
		if output, err := buildCmd.CombinedOutput(); err != nil {
			buildErr := err
			if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
				buildErr = fmt.Errorf("%w: %s", err, trimmed)
			}
			return out.Error("Failed to build module", buildErr)
		}
		command = outputPath
	} else if binaryFlag != "" {
		if _, err := os.Stat(binaryFlag); err != nil {
			return out.Error(fmt.Sprintf("module binary not found: %s", binaryFlag), err)
		}
		command = binaryFlag
	} else {
		command = strings.TrimSpace(spec.Spec.Entrypoint.Command)
		if command != "" {
			command = config.ExpandPath(command)
			if !filepath.IsAbs(command) && strings.Contains(command, string(os.PathSeparator)) {
				baseDir := moduleDir
				if baseDir == "" {
					baseDir = manifestDir
				}
				if baseDir != "" {
					command = filepath.Join(baseDir, command)
				}
			}
		}
	}

	args := append([]string(nil), spec.Spec.Entrypoint.Args...)
	extraArgs, _ := cmd.Flags().GetStringArray("endpoint-arg")
	if len(extraArgs) > 0 {
		args = append(args, extraArgs...)
	}

	endpointEnvValues, _ := cmd.Flags().GetStringArray("endpoint-env")
	endpointEnv, err := parseKeyValuePairs(endpointEnvValues)
	if err != nil {
		return out.Error("Invalid --endpoint-env value", err)
	}

	transport := strings.TrimSpace(spec.Spec.Entrypoint.Transport)
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
			return out.Error("--binary or --build required when --copy-binary is set", errors.New("missing module binary"))
		}
		srcPath := command
		if !filepath.IsAbs(srcPath) {
			absSrc, err := filepath.Abs(srcPath)
			if err != nil {
				return out.Error("Failed to resolve module binary", err)
			}
			srcPath = absSrc
		}
		paths, err := config.EnsureInstanceDirs("")
		if err != nil {
			return out.Error("Failed to prepare instance directories", err)
		}
		moduleHome := filepath.Join(paths.Home, "plugins", slug)
		binDir := filepath.Join(moduleHome, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			return out.Error("Failed to create module bin directory", err)
		}
		destName := filepath.Base(srcPath)
		if destName == "" {
			destName = slug
		}
		destPath := filepath.Join(binDir, destName)
		if rel, err := filepath.Rel(moduleHome, destPath); err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(filepath.ToSlash(rel), "../") {
			return out.Error("Resolved destination escapes module directory", errors.New("invalid destination path"))
		}
		if err := copyFile(srcPath, destPath, 0o755); err != nil {
			return out.Error("Failed to copy module binary", err)
		}
		command = destPath
	}

	adapterName := strings.TrimSpace(spec.Metadata.Name)

	payload := apihttp.ModuleRegistrationRequest{
		AdapterID: adapterID,
		Source:    "local",
		Type:      moduleType,
		Name:      adapterName,
		Manifest:  manifestJSON,
	}

	payload.Endpoint = &apihttp.ModuleEndpointConfig{
		Transport: transport,
		Address:   address,
		Command:   command,
		Args:      args,
		Env:       endpointEnv,
	}

	adapter, err := modulesRegisterHTTP(out, payload)
	if err != nil {
		return err
	}

	if out.jsonMode {
		if err := out.Print(apihttp.ModuleRegistrationResult{Adapter: adapter}); err != nil {
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

	if err := modulesBindHTTP(out, slot, adapterID, bindConfig); err != nil {
		return err
	}

	if !out.jsonMode {
		fmt.Printf("Bound adapter %s to %s\n", adapterID, slot)
	}
	return nil
}

func modulesLogs(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	useGRPC, _ := cmd.Flags().GetBool("grpc")
	if useGRPC {
		return out.Error("Modules logs over gRPC is not supported yet", errors.New("grpc not implemented"))
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

	endpoint := c.BaseURL() + "/modules/logs"
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return out.Error("Failed to create request", err)
	}

	resp, err := doRequest(c, req)
	if err != nil {
		return out.Error("Failed to fetch module logs", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return out.Error("Failed to fetch module logs", fmt.Errorf("%s", readErrorMessage(resp)))
	}

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, moduleLogsScannerInitialBuf)
	scanner.Buffer(buf, moduleLogsScannerMaxBuf)

	for scanner.Scan() {
		line := scanner.Text()
		if out.jsonMode {
			fmt.Println(line)
			continue
		}

		var entry apihttp.ModuleLogStreamEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			fmt.Fprintf(os.Stderr, "decode log entry failed: %v\n", err)
			fmt.Println(line)
			continue
		}
		printModuleLogEntry(entry)
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		if errors.Is(err, bufio.ErrTooLong) {
			return out.Error("Module log entry exceeded 1MB limit", err)
		}
		return out.Error("Module log stream ended with error", err)
	}

	return nil
}

func printModuleLogEntry(entry apihttp.ModuleLogStreamEntry) {
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
		moduleID := strings.TrimSpace(entry.ModuleID)
		if slot != "" {
			if moduleID != "" {
				fmt.Printf("%s [%s] %s %s: %s\n", timestamp, slot, moduleID, level, entry.Message)
			} else {
				fmt.Printf("%s [%s] %s: %s\n", timestamp, slot, level, entry.Message)
			}
		} else {
			if moduleID != "" {
				fmt.Printf("%s %s %s: %s\n", timestamp, moduleID, level, entry.Message)
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

	useGRPC, _ := cmd.Flags().GetBool("grpc")
	if useGRPC {
		return modulesBindGRPC(out, slot, adapter, string(raw))
	}
	return modulesBindHTTP(out, slot, adapter, raw)
}

func modulesStart(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	slot := strings.TrimSpace(args[0])
	if slot == "" {
		return out.Error("Slot must be provided", errors.New("invalid arguments"))
	}

	useGRPC, _ := cmd.Flags().GetBool("grpc")
	if useGRPC {
		return modulesStartGRPC(out, slot)
	}
	return modulesStartHTTP(out, slot)
}

func modulesStop(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	slot := strings.TrimSpace(args[0])
	if slot == "" {
		return out.Error("Slot must be provided", errors.New("invalid arguments"))
	}

	useGRPC, _ := cmd.Flags().GetBool("grpc")
	if useGRPC {
		return modulesStopGRPC(out, slot)
	}
	return modulesStopHTTP(out, slot)
}

func modulesListHTTP(out *OutputFormatter) error {
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

	printModuleTable(overview.Modules)
	printModuleRuntimeMessages(overview.Modules)
	return nil
}

func modulesListGRPC(out *OutputFormatter) error {
	gc, err := grpcclient.New()
	if err != nil {
		return out.Error("Failed to connect to daemon via gRPC", err)
	}
	defer gc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := gc.ModulesOverview(ctx)
	if err != nil {
		return out.Error("Failed to fetch modules overview via gRPC", err)
	}

	overview := modulesOverviewFromProto(resp)
	if out.jsonMode {
		return out.Print(overview)
	}

	if len(overview.Modules) == 0 {
		fmt.Println("No module slots found.")
		return nil
	}

	printModuleTable(overview.Modules)
	printModuleRuntimeMessages(overview.Modules)
	return nil
}

func modulesBindHTTP(out *OutputFormatter, slot, adapter string, raw json.RawMessage) error {
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

func modulesBindGRPC(out *OutputFormatter, slot, adapter, cfg string) error {
	gc, err := grpcclient.New()
	if err != nil {
		return out.Error("Failed to connect to daemon via gRPC", err)
	}
	defer gc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := &apiv1.BindModuleRequest{
		Slot:       slot,
		AdapterId:  adapter,
		ConfigJson: strings.TrimSpace(cfg),
	}

	resp, err := gc.BindModule(ctx, req)
	if err != nil {
		return out.Error("Failed to bind module via gRPC", err)
	}

	entry := moduleEntryFromProto(resp.GetModule())
	if out.jsonMode {
		return out.Print(apihttp.ModuleActionResult{Module: entry})
	}

	printModuleSummary("Bound", entry)
	return nil
}

func modulesStartHTTP(out *OutputFormatter, slot string) error {
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

func modulesStartGRPC(out *OutputFormatter, slot string) error {
	gc, err := grpcclient.New()
	if err != nil {
		return out.Error("Failed to connect to daemon via gRPC", err)
	}
	defer gc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := gc.StartModule(ctx, slot)
	if err != nil {
		return out.Error("Failed to start module via gRPC", err)
	}

	entry := moduleEntryFromProto(resp.GetModule())
	if out.jsonMode {
		return out.Print(apihttp.ModuleActionResult{Module: entry})
	}

	printModuleSummary("Started", entry)
	return nil
}

func modulesStopHTTP(out *OutputFormatter, slot string) error {
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

func modulesStopGRPC(out *OutputFormatter, slot string) error {
	gc, err := grpcclient.New()
	if err != nil {
		return out.Error("Failed to connect to daemon via gRPC", err)
	}
	defer gc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := gc.StopModule(ctx, slot)
	if err != nil {
		return out.Error("Failed to stop module via gRPC", err)
	}

	entry := moduleEntryFromProto(resp.GetModule())
	if out.jsonMode {
		return out.Print(apihttp.ModuleActionResult{Module: entry})
	}

	printModuleSummary("Stopped", entry)
	return nil
}

func modulesOverviewFromProto(resp *apiv1.ModulesOverviewResponse) apihttp.ModulesOverview {
	if resp == nil {
		return apihttp.ModulesOverview{}
	}
	out := apihttp.ModulesOverview{
		Modules: make([]apihttp.ModuleEntry, 0, len(resp.GetModules())),
	}
	for _, entry := range resp.GetModules() {
		out.Modules = append(out.Modules, moduleEntryFromProto(entry))
	}
	return out
}

func moduleEntryFromProto(entry *apiv1.ModuleEntry) apihttp.ModuleEntry {
	if entry == nil {
		return apihttp.ModuleEntry{}
	}
	out := apihttp.ModuleEntry{
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
		runtime := apihttp.ModuleRuntime{
			ModuleID: rt.GetModuleId(),
			Health:   rt.GetHealth(),
			Message:  rt.GetMessage(),
			Extra:    rt.GetExtra(),
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

func sortedModules(entries []apihttp.ModuleEntry) []apihttp.ModuleEntry {
	if len(entries) == 0 {
		return nil
	}
	sorted := append([]apihttp.ModuleEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		return strings.Compare(sorted[i].Slot, sorted[j].Slot) < 0
	})
	return sorted
}

func printModuleTable(entries []apihttp.ModuleEntry) {
	sorted := sortedModules(entries)
	if len(sorted) == 0 {
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SLOT\tADAPTER\tSTATUS\tHEALTH\tUPDATED")
	for _, entry := range sorted {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			entry.Slot,
			moduleAdapterLabel(entry),
			entry.Status,
			moduleHealthLabel(entry),
			entry.UpdatedAt,
		)
	}
	w.Flush()
}

func printModuleRuntimeMessages(entries []apihttp.ModuleEntry) {
	for _, entry := range sortedModules(entries) {
		if entry.Runtime != nil && strings.TrimSpace(entry.Runtime.Message) != "" {
			fmt.Printf("%s: %s\n", entry.Slot, entry.Runtime.Message)
		}
	}
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

	if len(status.Modules) > 0 {
		fmt.Println("Current module status:")
		printModuleTable(status.Modules)
		printModuleRuntimeMessages(status.Modules)
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

	if len(result.Modules) > 0 {
		fmt.Println("\nUpdated module status:")
		printModuleTable(result.Modules)
		printModuleRuntimeMessages(result.Modules)
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

	if len(payload.Modules) == 0 {
		fmt.Println("Modules: none reported")
	} else {
		fmt.Println("\nModules:")
		printModuleTable(payload.Modules)
		printModuleRuntimeMessages(payload.Modules)
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

	if len(status.Modules) == 0 {
		fmt.Println("Modules: none reported")
	} else {
		fmt.Println("\nModules:")
		printModuleTable(status.Modules)
		printModuleRuntimeMessages(status.Modules)
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

func sanitizeModuleSlug(value string) string {
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
		return "module"
	}
	if len(res) > moduleSlugMaxLength {
		return res[:moduleSlugMaxLength]
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
