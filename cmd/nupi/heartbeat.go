package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/adhocore/gronx"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/spf13/cobra"
)

const heartbeatTimeFormat = "2006-01-02 15:04:05 UTC"

// validateHeartbeatName delegates to the shared constants.ValidateHeartbeatName
// so that CLI and awareness tool handlers use identical validation logic.
func validateHeartbeatName(name string) error {
	return constants.ValidateHeartbeatName(name)
}

func newHeartbeatCommand() *cobra.Command {
	heartbeatCmd := &cobra.Command{
		Use:   "heartbeat",
		Short: "Manage scheduled heartbeat tasks",
		Long: `Manage recurring heartbeat tasks that run on cron schedules.

Heartbeats are periodic tasks that the AI executes automatically.
Use standard 5-field cron syntax: minute hour day month weekday.

Examples:
  nupi heartbeat list
  nupi heartbeat add daily-summary "0 9 * * *" "Write a summary of today's activity"
  nupi heartbeat remove daily-summary`,
	}

	listCmd := &cobra.Command{
		Use:           "list",
		Short:         "List all heartbeat tasks",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          heartbeatList,
	}

	addCmd := &cobra.Command{
		Use:           "add <name> <cron_expr> <prompt>",
		Short:         "Add a heartbeat task",
		Args:          cobra.ExactArgs(3),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          heartbeatAdd,
	}

	removeCmd := &cobra.Command{
		Use:           "remove <name>",
		Short:         "Remove a heartbeat task",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          heartbeatRemove,
	}

	heartbeatCmd.AddCommand(listCmd, addCmd, removeCmd)
	return heartbeatCmd
}

func heartbeatList(cmd *cobra.Command, args []string) error {
	formatter := newOutputFormatter(cmd)

	store, err := configstore.Open(configstore.Options{ReadOnly: true})
	if err != nil {
		return formatter.Error("Failed to open config store", err)
	}
	defer store.Close()

	heartbeats, err := store.ListHeartbeats(context.Background())
	if err != nil {
		return formatter.Error("Failed to list heartbeats", err)
	}

	type hbJSON struct {
		ID        int64   `json:"id"`
		Name      string  `json:"name"`
		CronExpr  string  `json:"cron_expr"`
		Prompt    string  `json:"prompt"`
		LastRunAt *string `json:"last_run_at"`
		CreatedAt string  `json:"created_at"`
	}

	result := make([]hbJSON, 0, len(heartbeats))
	for _, h := range heartbeats {
		entry := hbJSON{
			ID:        h.ID,
			Name:      h.Name,
			CronExpr:  h.CronExpr,
			Prompt:    h.Prompt,
			CreatedAt: h.CreatedAt.UTC().Format(time.RFC3339),
		}
		if h.LastRunAt != nil {
			s := h.LastRunAt.UTC().Format(time.RFC3339)
			entry.LastRunAt = &s
		}
		result = append(result, entry)
	}

	stdout := formatter.Writer()
	return formatter.Render(CommandResult{
		Data: map[string]interface{}{
			"heartbeats": result,
			"count":      len(result),
		},
		HumanReadable: func() error {
			if len(heartbeats) == 0 {
				fmt.Fprintln(stdout, "No heartbeats configured.")
				return nil
			}
			tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tCRON\tPROMPT\tLAST RUN")
			for _, h := range heartbeats {
				prompt := h.Prompt
				const maxPromptRunes = 50
				if utf8.RuneCountInString(prompt) > maxPromptRunes {
					runes := []rune(prompt)
					prompt = string(runes[:maxPromptRunes]) + "..."
				}
				prompt = strings.ReplaceAll(prompt, "\n", " ")
				lastRun := "never"
				if h.LastRunAt != nil {
					lastRun = h.LastRunAt.UTC().Format(heartbeatTimeFormat)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", h.Name, h.CronExpr, prompt, lastRun)
			}
			tw.Flush()
			return nil
		},
	})
}

func heartbeatAdd(cmd *cobra.Command, args []string) error {
	formatter := newOutputFormatter(cmd)
	name := strings.TrimSpace(args[0])
	cronExpr := strings.TrimSpace(args[1])
	prompt := strings.TrimSpace(args[2])

	if name == "" {
		return formatter.Error("Invalid arguments", fmt.Errorf("name cannot be empty"))
	}
	if err := validateHeartbeatName(name); err != nil {
		return formatter.Error("Invalid arguments", err)
	}
	if utf8.RuneCountInString(name) > constants.MaxHeartbeatNameLen {
		return formatter.Error("Invalid arguments", fmt.Errorf("name exceeds maximum length of %d characters", constants.MaxHeartbeatNameLen))
	}
	if cronExpr == "" {
		return formatter.Error("Invalid arguments", fmt.Errorf("cron_expr cannot be empty"))
	}
	if utf8.RuneCountInString(cronExpr) > constants.MaxHeartbeatCronExprLen {
		return formatter.Error("Invalid arguments", fmt.Errorf("cron_expr exceeds maximum length of %d characters", constants.MaxHeartbeatCronExprLen))
	}
	if prompt == "" {
		return formatter.Error("Invalid arguments", fmt.Errorf("prompt cannot be empty"))
	}
	if utf8.RuneCountInString(prompt) > constants.MaxHeartbeatPromptLen {
		return formatter.Error("Invalid arguments", fmt.Errorf("prompt exceeds maximum length of %d characters", constants.MaxHeartbeatPromptLen))
	}

	if !gronx.New().IsValid(cronExpr) {
		return formatter.Error("Invalid cron expression", fmt.Errorf("%q is not a valid 5-field cron expression", cronExpr))
	}

	// Warn if next execution is unreasonably far away (e.g., "0 0 31 2 *")
	if nextTime, err := gronx.NextTick(cronExpr, false); err == nil {
		if time.Until(nextTime) > 365*24*time.Hour {
			fmt.Fprintf(formatter.Writer(), "Warning: next execution is more than 1 year away (%s)\n", nextTime.Format(heartbeatTimeFormat))
		}
	}

	store, err := configstore.Open(configstore.Options{})
	if err != nil {
		return formatter.Error("Failed to open config store", err)
	}
	defer store.Close()

	if err := store.InsertHeartbeat(context.Background(), name, cronExpr, prompt, constants.MaxHeartbeatCount); err != nil {
		if configstore.IsDuplicate(err) {
			return formatter.Error("Duplicate heartbeat", fmt.Errorf("heartbeat %q already exists", name))
		}
		return formatter.Error("Failed to add heartbeat", err)
	}

	return formatter.Render(CommandResult{
		Data: map[string]interface{}{
			"name":      name,
			"cron_expr": cronExpr,
			"message":   fmt.Sprintf("Heartbeat %q added", name),
		},
		HumanReadable: func() error {
			fmt.Fprintf(formatter.Writer(), "Heartbeat %q added (cron: %s)\n", name, cronExpr)
			return nil
		},
	})
}

func heartbeatRemove(cmd *cobra.Command, args []string) error {
	formatter := newOutputFormatter(cmd)
	name := strings.TrimSpace(args[0])

	if name == "" {
		return formatter.Error("Invalid arguments", fmt.Errorf("name cannot be empty"))
	}
	if err := validateHeartbeatName(name); err != nil {
		return formatter.Error("Invalid arguments", err)
	}

	store, err := configstore.Open(configstore.Options{})
	if err != nil {
		return formatter.Error("Failed to open config store", err)
	}
	defer store.Close()

	if err := store.DeleteHeartbeat(context.Background(), name); err != nil {
		if configstore.IsNotFound(err) {
			return formatter.Error("Not found", fmt.Errorf("heartbeat %q not found", name))
		}
		return formatter.Error("Failed to remove heartbeat", err)
	}

	return formatter.Render(CommandResult{
		Data: map[string]interface{}{
			"name":    name,
			"message": fmt.Sprintf("Heartbeat %q removed", name),
		},
		HumanReadable: func() error {
			fmt.Fprintf(formatter.Writer(), "Heartbeat %q removed\n", name)
			return nil
		},
	})
}
