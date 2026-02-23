package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/grpcclient"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

	out := &OutputFormatter{jsonMode: jsonOutput}

	// Connect to daemon via gRPC
	c, err := grpcclient.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	// Get terminal size
	var rows, cols uint32 = 24, 80
	if term.IsTerminal(0) {
		if w, h, err := term.GetSize(0); err == nil {
			cols = uint32(w)
			rows = uint32(h)
		}
	}

	// Create session
	createCtx, createCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer createCancel()

	resp, err := c.CreateSession(createCtx, &apiv1.CreateSessionRequest{
		Command:    command,
		Args:       commandArgs,
		WorkingDir: localWorkDir,
		Env:        os.Environ(),
		Detached:   localDetached,
		Rows:       rows,
		Cols:       cols,
	})
	if err != nil {
		return out.Error("Failed to create session", err)
	}

	session := resp.GetSession()
	if session == nil {
		return out.Error("Failed to create session", fmt.Errorf("empty response"))
	}

	if localDetached {
		return out.Success("Session created and running in background", map[string]interface{}{
			"session_id": session.GetId(),
			"command":    command,
			"args":       commandArgs,
		})
	}

	// Attach to the session
	return attachToExistingSession(c, session.GetId(), true)
}

// listSessions lists all active sessions
func listSessions(cmd *cobra.Command, args []string) error {
	return withClientTimeout(cmd, 5*time.Second, func(ctx context.Context, c *grpcclient.Client, out *OutputFormatter) error {
		resp, err := c.ListSessions(ctx)
		if err != nil {
			return out.Error("Failed to list sessions", err)
		}

		sessions := resp.GetSessions()

		// Build JSON-compatible list.
		list := make([]map[string]interface{}, 0, len(sessions))
		for _, sess := range sessions {
			entry := map[string]interface{}{
				"id":      sess.GetId(),
				"status":  sess.GetStatus(),
				"command": sess.GetCommand(),
				"args":    sess.GetArgs(),
			}
			if sess.GetPid() != 0 {
				entry["pid"] = sess.GetPid()
			}
			list = append(list, entry)
		}

		return out.Render(CommandResult{
			Data: map[string]interface{}{"sessions": list},
			HumanReadable: func() error {
				if len(sessions) == 0 {
					fmt.Println("No active sessions")
					return nil
				}

				fmt.Println("Active sessions:")
				fmt.Println("ID\t\tStatus\t\tCommand")
				fmt.Println("---\t\t---\t\t---")

				for _, sess := range sessions {
					fmt.Printf("%s\t%s\t\t%s %s\n",
						sess.GetId(),
						sess.GetStatus(),
						sess.GetCommand(),
						strings.Join(sess.GetArgs(), " "))
				}

				return nil
			},
		})
	})
}

// attachToSession attaches to an existing session
func attachToSession(cmd *cobra.Command, args []string) error {
	sessionID := args[0]

	c, err := grpcclient.New()
	if err != nil {
		return fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer c.Close()

	fmt.Printf("Attaching to session %s...\n", sessionID)
	fmt.Println("Press Ctrl+C to detach")

	return attachToExistingSession(c, sessionID, true)
}

// attachToExistingSession handles the actual attachment via gRPC bidirectional streaming.
func attachToExistingSession(c *grpcclient.Client, sessionID string, includeHistory bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.AttachSession(ctx)
	if err != nil {
		return fmt.Errorf("failed to open attach stream: %w", err)
	}

	// Send init message.
	if err := stream.Send(&apiv1.AttachSessionRequest{
		Payload: &apiv1.AttachSessionRequest_Init{
			Init: &apiv1.AttachInit{
				SessionId:      sessionID,
				IncludeHistory: includeHistory,
			},
		},
	}); err != nil {
		return fmt.Errorf("failed to send init: %w", err)
	}

	// Set terminal to raw mode if available.
	var oldState *term.State
	if term.IsTerminal(0) {
		var err error
		oldState, err = term.MakeRaw(0)
		if err != nil {
			return fmt.Errorf("failed to set raw mode: %w", err)
		}
		defer term.Restore(0, oldState)
	}

	// Serialise all stream.Send() calls — gRPC ClientStream is not safe for
	// concurrent SendMsg from multiple goroutines.
	var sendMu sync.Mutex
	safeSend := func(req *apiv1.AttachSessionRequest) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(req)
	}

	// Handle signals.
	sigChan := make(chan os.Signal, 2)
	notifyAttachSignals(sigChan)
	defer signal.Stop(sigChan)

	sendResize := func() {
		if !term.IsTerminal(0) {
			return
		}
		cols, rows, err := term.GetSize(0)
		if err != nil {
			return
		}
		_ = safeSend(&apiv1.AttachSessionRequest{
			Payload: &apiv1.AttachSessionRequest_Resize{
				Resize: &apiv1.ResizeRequest{
					Cols:   uint32(cols),
					Rows:   uint32(rows),
					Source: "host",
				},
			},
		})
	}

	// Send initial resize snapshot so modes know local host geometry.
	sendResize()

	// Error/completion channel.
	errChan := make(chan error, 2)

	// Output goroutine: receive stream responses and write to stdout.
	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					errChan <- nil
				} else {
					errChan <- err
				}
				return
			}

			switch payload := resp.GetPayload().(type) {
			case *apiv1.AttachSessionResponse_Output:
				if len(payload.Output) > 0 {
					os.Stdout.Write(payload.Output)
				}
			case *apiv1.AttachSessionResponse_Event:
				evt := payload.Event
				if evt.GetType() == apiv1.SessionEventType_SESSION_EVENT_TYPE_KILLED {
					errChan <- nil
					return
				}
			}
		}
	}()

	// Input goroutine: read from stdin and send on stream.
	// Note: os.Stdin.Read is a blocking syscall not interruptible by context
	// cancellation. This goroutine may outlive attachToExistingSession until
	// the next keystroke or process exit. Acceptable for CLI usage.
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
				if sendErr := safeSend(&apiv1.AttachSessionRequest{
					Payload: &apiv1.AttachSessionRequest_Input{
						Input: buffer[:n],
					},
				}); sendErr != nil {
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
			fmt.Print("\r\nDetaching from session...\r\n")
			_ = safeSend(&apiv1.AttachSessionRequest{
				Payload: &apiv1.AttachSessionRequest_Control{
					Control: "detach",
				},
			})
			_ = stream.CloseSend()
			return nil
		case err := <-errChan:
			if err == nil {
				return nil
			}
			// Treat expected gRPC stream termination conditions as clean exit.
			if st, ok := status.FromError(err); ok {
				switch st.Code() {
				case codes.Canceled, codes.Unavailable:
					return nil
				}
			}
			if strings.Contains(err.Error(), "EOF") {
				return nil
			}
			return err
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

	out := &OutputFormatter{jsonMode: jsonOutput}

	// Connect to daemon via gRPC
	c, err := grpcclient.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	// Get terminal size
	var rows, cols uint32 = 24, 80
	if term.IsTerminal(0) {
		if w, h, err := term.GetSize(0); err == nil {
			cols = uint32(w)
			rows = uint32(h)
		}
	}

	// Create session with inspect mode enabled
	createCtx, createCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer createCancel()

	resp, err := c.CreateSession(createCtx, &apiv1.CreateSessionRequest{
		Command:    command,
		Args:       commandArgs,
		WorkingDir: localWorkDir,
		Env:        os.Environ(),
		Detached:   localDetached,
		Rows:       rows,
		Cols:       cols,
		Inspect:    true,
	})
	if err != nil {
		return out.Error("Failed to create session", err)
	}

	session := resp.GetSession()
	if session == nil {
		return out.Error("Failed to create session", fmt.Errorf("empty response"))
	}

	if localDetached {
		return out.Success("Session created in inspection mode", map[string]interface{}{
			"session_id":  session.GetId(),
			"command":     command,
			"args":        commandArgs,
			"inspect":     true,
			"output_file": fmt.Sprintf("~/.nupi/inspect/%s.raw", session.GetId()),
		})
	}

	// Show inspection mode notice in human-readable mode only
	out.PrintText(func() {
		fmt.Printf("\033[33mInspection mode enabled. Raw output will be saved to ~/.nupi/inspect/%s.raw\033[0m\n", session.GetId())
	})

	// Attach to the session
	return attachToExistingSession(c, session.GetId(), true)
}

// killSession kills a running session
func killSession(cmd *cobra.Command, args []string) error {
	sessionID := args[0]
	return withClientTimeout(cmd, 5*time.Second, func(ctx context.Context, c *grpcclient.Client, out *OutputFormatter) error {
		if _, err := c.KillSession(ctx, sessionID); err != nil {
			errMsg := "Failed to kill session"
			if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
				errMsg = fmt.Sprintf("Session %s not found", sessionID)
			}
			return out.Error(errMsg, err)
		}

		return out.Success(fmt.Sprintf("Session %s killed", sessionID), map[string]interface{}{
			"session_id": sessionID,
		})
	})
}
