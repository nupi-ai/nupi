package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/nupi-ai/nupi/internal/client"
	"github.com/spf13/cobra"
)

func newQuickstartCommand() *cobra.Command {
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
	return quickstartCmd
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
		status, statusErr := fetchQuickstartStatus(c)
		if statusErr != nil {
			return out.Error("Failed to fetch quickstart status", statusErr)
		}
		if missing := status.MissingReferenceAdapters; len(missing) > 0 {
			return out.Error(
				"Cannot complete quickstart",
				fmt.Errorf("missing reference adapters: %s (install the recommended packages before completing quickstart)", strings.Join(missing, ", ")),
			)
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
			if adapter == "" {
				return out.Error("Binding adapter cannot be empty", nil)
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
