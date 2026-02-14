package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	nupiversion "github.com/nupi-ai/nupi/internal/version"
	"github.com/spf13/cobra"
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
			jsonBytes, err := json.MarshalIndent(data, "", "  ")
			if err != nil {
				return fmt.Errorf("failed to marshal JSON: %w", err)
			}
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
		jsonBytes, marshalErr := json.MarshalIndent(output, "", "  ")
		if marshalErr != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", message, err)
		} else {
			fmt.Fprintln(os.Stderr, string(jsonBytes))
		}
	} else {
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", message, err)
		} else {
			fmt.Fprintln(os.Stderr, message)
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
	)

	// Execute
	if err := rootCmd.Execute(); err != nil {
		// Error is already printed by command handlers
		os.Exit(1)
	}
}
