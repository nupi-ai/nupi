package main

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	apihttp "github.com/nupi-ai/nupi/internal/api/http"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/grpcclient"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/wrapperspb"
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

	stdout := out.w
	return withOutputClient(out, daemonConnectErrorMessage, func(gc *grpcclient.Client) error {
		statusCtx, statusCancel := context.WithTimeout(context.Background(), constants.Duration10Seconds)
		defer statusCancel()

		status, err := gc.QuickstartStatus(statusCtx)
		if err != nil {
			return out.Error("Failed to fetch quickstart status", err)
		}

		adaptersFromProto := quickstartAdaptersFromProto(status.GetAdapters())
		if len(adaptersFromProto) > 0 {
			fmt.Fprintln(stdout, "Current adapter status:")
			printAdapterTable(stdout, adaptersFromProto)
			printAdapterRuntimeMessages(stdout, adaptersFromProto)
			fmt.Fprintln(stdout)
		}

		missingRefs := status.GetMissingReferenceAdapters()
		printMissingReferenceAdapters(stdout, missingRefs, true)

		if status.GetCompleted() && len(status.GetPendingSlots()) == 0 {
			fmt.Fprintln(stdout, "Quickstart is already completed. Nothing to do.")
			return nil
		}

		// Fetch available adapters for the wizard.
		listCtx, listCancel := context.WithTimeout(context.Background(), constants.Duration10Seconds)
		defer listCancel()

		listResp, err := gc.ListAdapters(listCtx)
		if err != nil {
			return out.Error("Failed to fetch adapters", err)
		}
		adapters := adaptersFromListResponse(listResp)

		if len(adapters) == 0 {
			fmt.Fprintln(stdout, "No adapters are installed yet. Install adapters before running the wizard.")
			return nil
		}

		reader := bufio.NewReader(cmd.InOrStdin())
		var bindings []*apiv1.QuickstartBinding

		fmt.Fprintln(stdout, "=== Quickstart Wizard ===")
		if status.GetCompleted() {
			fmt.Fprintln(stdout, "Quickstart was marked complete previously, but some slots are pending.")
		}

		for _, slot := range status.GetPendingSlots() {
			fmt.Fprintf(stdout, "\nSlot %s requires an adapter.\n", slot)
			slotAdapters := printAvailableAdaptersForSlot(stdout, slot, adapters)

			for {
				fmt.Fprintf(stdout, "Select adapter for %s (enter number/id, blank to skip): ", slot)
				choice, err := reader.ReadString('\n')
				if err != nil {
					return out.Error("Failed to read input", err)
				}
				choice = strings.TrimSpace(choice)

				if choice == "" {
					fmt.Fprintf(stdout, "Skipping %s. You can assign it later.\n", slot)
					break
				}

				if id, ok := resolveAdapterChoice(choice, slotAdapters, adapters); ok {
					bindings = append(bindings, &apiv1.QuickstartBinding{Slot: slot, AdapterId: id})
					fmt.Fprintf(stdout, "  -> Assigned %s to %s\n", id, slot)
					break
				}

				fmt.Fprintln(stdout, "Invalid selection. Please try again.")
			}
		}

		if len(bindings) == 0 {
			fmt.Fprintln(stdout, "\nNo bindings were selected. Quickstart remains unchanged.")
			return nil
		}

		allowComplete := len(missingRefs) == 0
		complete := false
		if len(bindings) == len(status.GetPendingSlots()) {
			if !allowComplete {
				fmt.Fprintln(stdout, "\nAll pending slots are assigned, but reference adapters are still missing. Quickstart will remain incomplete.")
			} else {
				fmt.Fprint(stdout, "\nAll pending slots have assignments. Mark quickstart as complete? [Y/n]: ")
				answer, _ := reader.ReadString('\n')
				answer = strings.TrimSpace(strings.ToLower(answer))
				complete = answer == "" || answer == "y" || answer == "yes"
			}
		}

		updateCtx, updateCancel := context.WithTimeout(context.Background(), constants.Duration10Seconds)
		defer updateCancel()

		req := &apiv1.UpdateQuickstartRequest{
			Bindings: bindings,
		}
		if complete {
			req.Complete = wrapperspb.Bool(true)
		}

		result, err := gc.UpdateQuickstart(updateCtx, req)
		if err != nil {
			return out.Error("Failed to submit quickstart bindings", err)
		}

		fmt.Fprintln(stdout, "\nQuickstart updated.")
		if complete && result.GetCompleted() {
			fmt.Fprintln(stdout, "Quickstart marked as completed.")
		} else {
			fmt.Fprintf(stdout, "Quickstart completed: %v\n", result.GetCompleted())
		}

		if len(result.GetPendingSlots()) > 0 {
			fmt.Fprintln(stdout, "Pending slots remaining:")
			for _, slot := range result.GetPendingSlots() {
				fmt.Fprintf(stdout, "  - %s\n", slot)
			}
		} else {
			fmt.Fprintln(stdout, "No pending slots remaining.")
		}

		resultAdapters := quickstartAdaptersFromProto(result.GetAdapters())
		if len(resultAdapters) > 0 {
			fmt.Fprintln(stdout, "\nUpdated adapter status:")
			printAdapterTable(stdout, resultAdapters)
			printAdapterRuntimeMessages(stdout, resultAdapters)
		}

		printMissingReferenceAdapters(stdout, result.GetMissingReferenceAdapters(), true)

		return nil
	})
}

func quickstartStatus(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	return withOutputClientTimeout(out, constants.Duration5Seconds, daemonConnectErrorMessage, func(ctx context.Context, gc *grpcclient.Client) (any, error) {
		payload, err := gc.QuickstartStatus(ctx)
		if err != nil {
			return nil, clientCallFailed("Failed to fetch quickstart status", err)
		}

		status := map[string]interface{}{
			"completed":     payload.GetCompleted(),
			"pending_slots": payload.GetPendingSlots(),
		}
		if payload.GetCompletedAt() != nil {
			status["completed_at"] = payload.GetCompletedAt().AsTime().UTC().Format(time.RFC3339)
		}
		if len(payload.GetMissingReferenceAdapters()) > 0 {
			status["missing_reference_adapters"] = payload.GetMissingReferenceAdapters()
		}
		adapters := quickstartAdaptersFromProto(payload.GetAdapters())
		if len(adapters) > 0 {
			status["adapters"] = adapters
		}

		stdout := out.w
		return CommandResult{
			Data: status,
			HumanReadable: func() error {
				fmt.Fprintf(stdout, "Quickstart completed: %v\n", payload.GetCompleted())
				if payload.GetCompletedAt() != nil {
					fmt.Fprintf(stdout, "Completed at: %s\n", payload.GetCompletedAt().AsTime().UTC().Format(time.RFC3339))
				}
				if len(payload.GetPendingSlots()) == 0 {
					fmt.Fprintln(stdout, "Pending slots: none")
				} else {
					fmt.Fprintln(stdout, "Pending slots:")
					for _, slot := range payload.GetPendingSlots() {
						fmt.Fprintf(stdout, "  - %s\n", slot)
					}
				}

				if len(payload.GetMissingReferenceAdapters()) > 0 {
					printMissingReferenceAdapters(stdout, payload.GetMissingReferenceAdapters(), true)
				}

				if len(adapters) == 0 {
					fmt.Fprintln(stdout, "Adapters: none reported")
				} else {
					fmt.Fprintln(stdout, "\nAdapters:")
					printAdapterTable(stdout, adapters)
					printAdapterRuntimeMessages(stdout, adapters)
				}
				return nil
			},
		}, nil
	})
}

func quickstartComplete(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	return withOutputClient(out, daemonConnectErrorMessage, func(gc *grpcclient.Client) error {
		flags := cmd.Flags()
		bindingPairs, _ := flags.GetStringSlice("binding")
		completeFlag, _ := flags.GetBool("complete")

		if completeFlag {
			ctx, cancel := context.WithTimeout(context.Background(), constants.Duration5Seconds)
			defer cancel()

			status, err := gc.QuickstartStatus(ctx)
			if err != nil {
				return out.Error("Failed to fetch quickstart status", err)
			}
			if missing := status.GetMissingReferenceAdapters(); len(missing) > 0 {
				return out.Error(
					"Cannot complete quickstart",
					fmt.Errorf("missing reference adapters: %s (install the recommended packages before completing quickstart)", strings.Join(missing, ", ")),
				)
			}
		}

		var bindings []*apiv1.QuickstartBinding
		for _, pair := range bindingPairs {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) != 2 {
				return out.Error("Invalid binding format (expected slot=adapter)", fmt.Errorf("%s", pair))
			}
			slot := strings.TrimSpace(parts[0])
			adapter := strings.TrimSpace(parts[1])
			if slot == "" {
				return out.Error("Binding slot cannot be empty", nil)
			}
			if adapter == "" {
				return out.Error("Binding adapter cannot be empty", nil)
			}
			bindings = append(bindings, &apiv1.QuickstartBinding{Slot: slot, AdapterId: adapter})
		}

		req := &apiv1.UpdateQuickstartRequest{
			Bindings: bindings,
		}
		if completeFlag {
			req.Complete = wrapperspb.Bool(true)
		}

		ctx, cancel := context.WithTimeout(context.Background(), constants.Duration10Seconds)
		defer cancel()

		result, err := gc.UpdateQuickstart(ctx, req)
		if err != nil {
			return out.Error("Failed to update quickstart status", err)
		}

		status := map[string]interface{}{
			"completed":     result.GetCompleted(),
			"pending_slots": result.GetPendingSlots(),
		}
		if result.GetCompletedAt() != nil {
			status["completed_at"] = result.GetCompletedAt().AsTime().UTC().Format(time.RFC3339)
		}
		adapters := quickstartAdaptersFromProto(result.GetAdapters())
		if len(adapters) > 0 {
			status["adapters"] = adapters
		}

		stdout := out.w
		return out.Render(CommandResult{
			Data: status,
			HumanReadable: func() error {
				fmt.Fprintf(stdout, "Quickstart completed: %v\n", result.GetCompleted())
				if result.GetCompletedAt() != nil {
					fmt.Fprintf(stdout, "Completed at: %s\n", result.GetCompletedAt().AsTime().UTC().Format(time.RFC3339))
				}
				if len(result.GetPendingSlots()) == 0 {
					fmt.Fprintln(stdout, "Pending slots: none")
				} else {
					fmt.Fprintln(stdout, "Pending slots:")
					for _, slot := range result.GetPendingSlots() {
						fmt.Fprintf(stdout, "  - %s\n", slot)
					}
				}

				if len(result.GetMissingReferenceAdapters()) > 0 {
					printMissingReferenceAdapters(stdout, result.GetMissingReferenceAdapters(), true)
				}

				if len(adapters) == 0 {
					fmt.Fprintln(stdout, "Adapters: none reported")
				} else {
					fmt.Fprintln(stdout, "\nAdapters:")
					printAdapterTable(stdout, adapters)
					printAdapterRuntimeMessages(stdout, adapters)
				}
				return nil
			},
		})
	})
}

// quickstartAdaptersFromProto converts proto AdapterEntry slice to the display type.
func quickstartAdaptersFromProto(entries []*apiv1.AdapterEntry) []apihttp.AdapterEntry {
	if len(entries) == 0 {
		return nil
	}
	result := make([]apihttp.AdapterEntry, 0, len(entries))
	for _, e := range entries {
		result = append(result, adapterEntryFromProto(e))
	}
	return result
}

// adaptersFromListResponse converts ListAdaptersResponse to the adapterInfo slice used by the wizard.
func adaptersFromListResponse(resp *apiv1.ListAdaptersResponse) []adapterInfo {
	if resp == nil {
		return nil
	}
	result := make([]adapterInfo, 0, len(resp.GetAdapters()))
	for _, a := range resp.GetAdapters() {
		result = append(result, adapterInfo{
			ID:      a.GetId(),
			Name:    a.GetName(),
			Type:    a.GetType(),
			Source:  a.GetSource(),
			Version: a.GetVersion(),
		})
	}
	return result
}
