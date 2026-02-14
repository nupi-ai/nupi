package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/nupi-ai/nupi/internal/client"
	"github.com/nupi-ai/nupi/internal/protocol"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh/terminal"
)

func newRunCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "run [command]",
		Short:              "Run a command in a PTY session (defaults to current shell)",
		Args:               cobra.MinimumNArgs(0),
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE:               runCommand,
	}
}

func newListCommand() *cobra.Command {
	return &cobra.Command{
		Use:           "list",
		Short:         "List all sessions",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          listSessions,
	}
}

func newAttachCommand() *cobra.Command {
	return &cobra.Command{
		Use:           "attach [session-id]",
		Short:         "Attach to a running session",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          attachToSession,
	}
}

func newInspectCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "inspect [command]",
		Short:              "Run a command with raw output inspection",
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE:               inspectCommand,
	}
}

func newKillCommand() *cobra.Command {
	return &cobra.Command{
		Use:           "kill [session-id]",
		Short:         "Kill a session",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          killSession,
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
	notifyAttachSignals(sigChan)
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
			if isResizeSignal(sig) {
				sendResize()
				continue
			}
			// Non-resize signal (SIGINT or SIGTERM) — detach.
			fmt.Println("\nDetaching from session...")
			c.DetachSession()
			return nil
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
