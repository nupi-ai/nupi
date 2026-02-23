package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/spf13/cobra"
)

// Global variables for use across commands
var (
	rootCmd     *cobra.Command
	instanceDir string
)

var (
	allowedAdapterSlots       = constants.StringSet(constants.AllowedAdapterSlots)
	allowedEndpointTransports = constants.StringSet(constants.AllowedAdapterTransports)
)

// OutputFormatter handles output in JSON or human-readable format
type OutputFormatter struct {
	jsonMode bool
	w        io.Writer // standard output destination (from cmd.OutOrStdout)
	errW     io.Writer // error output destination (from cmd.ErrOrStderr)
}

// CommandResult captures command output for both JSON and human-readable modes.
type CommandResult struct {
	Data          any
	HumanReadable func() error
}

// newOutputFormatter creates a new formatter based on the command's --json flag
func newOutputFormatter(cmd *cobra.Command) *OutputFormatter {
	jsonMode, _ := cmd.Flags().GetBool("json")
	return &OutputFormatter{jsonMode: jsonMode, w: cmd.OutOrStdout(), errW: cmd.ErrOrStderr()}
}

// Print outputs data in the appropriate format
func (f *OutputFormatter) Print(data interface{}) error {
	w := f.Writer()
	if f.jsonMode {
		jsonBytes, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Fprintln(w, string(jsonBytes))
	} else {
		// For non-JSON mode, data should implement a custom string method
		// or we call a custom formatter function
		switch v := data.(type) {
		case string:
			fmt.Fprintln(w, v)
		default:
			// Fallback to JSON for unknown types
			jsonBytes, err := json.MarshalIndent(data, "", "  ")
			if err != nil {
				return fmt.Errorf("failed to marshal JSON: %w", err)
			}
			fmt.Fprintln(w, string(jsonBytes))
		}
	}
	return nil
}

// Render prints command output using the active formatter mode.
func (f *OutputFormatter) Render(result CommandResult) error {
	if f.jsonMode {
		return f.Print(result.Data)
	}
	if result.HumanReadable == nil {
		return nil
	}
	return result.HumanReadable()
}

// Writer returns the standard output writer, falling back to os.Stdout if not set.
func (f *OutputFormatter) Writer() io.Writer {
	if f.w != nil {
		return f.w
	}
	return os.Stdout
}

// ErrWriter returns the error output writer, falling back to os.Stderr if not set.
func (f *OutputFormatter) ErrWriter() io.Writer {
	if f.errW != nil {
		return f.errW
	}
	return os.Stderr
}

// PrintText executes the provided function only in human-readable mode.
func (f *OutputFormatter) PrintText(printFn func()) {
	if f.jsonMode || printFn == nil {
		return
	}
	printFn()
}

// Success outputs a success message
func (f *OutputFormatter) Success(message string, data map[string]interface{}) error {
	if f.jsonMode {
		output := map[string]interface{}{
			"message": message,
		}
		for k, v := range data {
			output[k] = v
		}
		return f.Print(output)
	}
	fmt.Fprintln(f.Writer(), message)
	return nil
}

// Error outputs an error message to the error writer (cmd.ErrOrStderr).
func (f *OutputFormatter) Error(message string, err error) error {
	w := f.ErrWriter()
	if f.jsonMode {
		output := map[string]interface{}{
			"error": message,
		}
		if err != nil {
			output["details"] = err.Error()
		}
		jsonBytes, marshalErr := json.MarshalIndent(output, "", "  ")
		if marshalErr != nil {
			fmt.Fprintf(w, "%s: %v\n", message, err)
		} else {
			fmt.Fprintln(w, string(jsonBytes))
		}
	} else {
		if err != nil {
			fmt.Fprintf(w, "%s: %v\n", message, err)
		} else {
			fmt.Fprintln(w, message)
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", message, err)
	}
	return errors.New(message)
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
	// No rootCmd.Version — use `nupi version` subcommand instead (shows client + daemon).

	// Add global --json flag
	rootCmd.PersistentFlags().Bool("json", false, "Output in JSON format")

	// Set default instance directory.
	// Note: bare os.Stderr is intentional here — init() runs before Cobra
	// commands are initialized, so OutputFormatter is not yet available.
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to determine home directory: %v\n", err)
		os.Exit(1)
	}
	instanceDir = filepath.Join(homeDir, ".nupi", "instances", "default")
}

func main() {
	rootCmd.AddCommand(
		newRunCommand(),
		newListCommand(),
		newAttachCommand(),
		newKillCommand(),
		newLoginCommand(),
		newInspectCommand(),
		newDaemonCommand(),
		newConfigCommand(),
		newAdaptersCommand(),
		newQuickstartCommand(),
		newVoiceCommand(),
		newPairRootCommand(),
		newPromptsCommand(),
		newMarketplaceCommand(),
		newVersionCommand(),
	)

	// Execute
	if err := rootCmd.Execute(); err != nil {
		// Error is already printed by command handlers
		os.Exit(1)
	}
}
