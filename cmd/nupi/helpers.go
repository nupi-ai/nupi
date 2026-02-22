package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nupi-ai/nupi/internal/grpcclient"
	"github.com/spf13/cobra"
)

// Shared types used across command files.

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

	daemonConnectErrorMessage     = "Failed to connect to daemon"
	daemonConnectGRPCErrorMessage = "Failed to connect to daemon via gRPC"
)

type ClientHandler func(client *grpcclient.Client, out *OutputFormatter) error

type TimedClientHandler func(ctx context.Context, client *grpcclient.Client, out *OutputFormatter) error

type OutputTimedClientHandler func(ctx context.Context, client *grpcclient.Client) error

func withClient(cmd *cobra.Command, connectErrorMessage string, fn ClientHandler) error {
	out := newOutputFormatter(cmd)
	return withOutputClient(out, connectErrorMessage, func(client *grpcclient.Client) error {
		return fn(client, out)
	})
}

func withClientTimeout(cmd *cobra.Command, timeout time.Duration, fn TimedClientHandler) error {
	return withClientTimeoutMessage(cmd, timeout, daemonConnectErrorMessage, fn)
}

func withClientTimeoutMessage(
	cmd *cobra.Command,
	timeout time.Duration,
	connectErrorMessage string,
	fn TimedClientHandler,
) error {
	return withClient(cmd, connectErrorMessage, func(client *grpcclient.Client, out *OutputFormatter) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return fn(ctx, client, out)
	})
}

func withOutputClient(out *OutputFormatter, connectErrorMessage string, fn func(client *grpcclient.Client) error) error {
	client, err := grpcclient.New()
	if err != nil {
		return out.Error(connectErrorMessage, err)
	}
	defer client.Close()
	return fn(client)
}

func withOutputClientTimeout(
	out *OutputFormatter,
	timeout time.Duration,
	connectErrorMessage string,
	fn OutputTimedClientHandler,
) error {
	return withOutputClient(out, connectErrorMessage, func(client *grpcclient.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return fn(ctx, client)
	})
}

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
