package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"

	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/spf13/cobra"
)

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

	result := make([]map[string]interface{}, 0, len(templates))
	for _, t := range templates {
		result = append(result, map[string]interface{}{
			"event_type":  t.EventType,
			"is_custom":   t.IsCustom,
			"updated_at":  t.UpdatedAt,
			"description": descriptions[t.EventType],
		})
	}

	return formatter.Render(CommandResult{
		Data: map[string]interface{}{
			"templates": result,
		},
		HumanReadable: func() error {
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
		},
	})
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

	return formatter.Render(CommandResult{
		Data: map[string]interface{}{
			"template":  pt.EventType,
			"is_custom": pt.IsCustom,
			"content":   pt.Content,
		},
		HumanReadable: func() error {
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
		},
	})
}

func promptsEdit(cmd *cobra.Command, args []string) error {
	formatter := newOutputFormatter(cmd)
	templateName := strings.TrimSuffix(args[0], ".tmpl")

	// Validate event type
	if err := validatePromptEventType(templateName); err != nil {
		return formatter.Error(err.Error(), nil)
	}

	store, err := openPromptsStore()
	if err != nil {
		return formatter.Error("Failed to open config store", err)
	}
	defer store.Close()

	// Get current content
	pt, err := store.GetPromptTemplate(context.Background(), templateName)
	if err != nil {
		if configstore.IsNotFound(err) {
			return formatter.Error(fmt.Sprintf("Template not found: %s", templateName), nil)
		}
		return formatter.Error("Failed to get template", err)
	}

	// Write to temp file for editing
	tmpFile, err := os.CreateTemp("", fmt.Sprintf("nupi-prompt-%s-*.tmpl", templateName))
	if err != nil {
		return formatter.Error("Failed to create temp file", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.WriteString(pt.Content); err != nil {
		tmpFile.Close()
		return formatter.Error("Failed to write temp file", err)
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
		return formatter.Error("Failed to open editor", err)
	}

	// Read edited content
	newContent, err := os.ReadFile(tmpPath)
	if err != nil {
		return formatter.Error("Failed to read edited file", err)
	}

	// Save to store
	if err := store.SetPromptTemplate(context.Background(), templateName, string(newContent)); err != nil {
		return formatter.Error("Failed to save template", err)
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

	return formatter.Render(CommandResult{
		Data: map[string]interface{}{
			"reset":   reset,
			"message": "Templates reset to defaults",
		},
		HumanReadable: func() error {
			if len(reset) > 0 {
				fmt.Printf("Reset templates: %s\n", strings.Join(reset, ", "))
				fmt.Println("Note: Restart the daemon for changes to take effect.")
			}
			return nil
		},
	})
}
