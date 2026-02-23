package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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

func getTrimmedFlag(cmd *cobra.Command, name string) string {
	val, _ := cmd.Flags().GetString(name)
	return strings.TrimSpace(val)
}

func getRequiredFlag(cmd *cobra.Command, name string, out *OutputFormatter) (string, error) {
	val := getTrimmedFlag(cmd, name)
	if val == "" {
		return "", out.Error(fmt.Sprintf("--%s is required", name), nil)
	}
	return val, nil
}

type ClientHandler func(client *grpcclient.Client, out *OutputFormatter) error

type TimedClientHandler func(ctx context.Context, client *grpcclient.Client) (any, error)

type ClientSuccessResult struct {
	Message string
	Data    map[string]interface{}
}

type ClientCallError struct {
	Message string
	Err     error
}

func (e *ClientCallError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *ClientCallError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func clientSuccess(message string, data map[string]interface{}) ClientSuccessResult {
	return ClientSuccessResult{
		Message: message,
		Data:    data,
	}
}

func clientCallFailed(message string, err error) error {
	return &ClientCallError{
		Message: message,
		Err:     err,
	}
}

func finalizeClientResult(out *OutputFormatter, result any, err error) error {
	if err != nil {
		var callErr *ClientCallError
		if errors.As(err, &callErr) {
			return out.Error(callErr.Message, callErr.Err)
		}
		return err
	}

	if result == nil {
		return nil
	}

	switch v := result.(type) {
	case CommandResult:
		return out.Render(v)
	case *CommandResult:
		if v == nil {
			return nil
		}
		return out.Render(*v)
	case ClientSuccessResult:
		return out.Success(v.Message, v.Data)
	case *ClientSuccessResult:
		if v == nil {
			return nil
		}
		return out.Success(v.Message, v.Data)
	default:
		return out.Print(v)
	}
}

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
	out := newOutputFormatter(cmd)
	return withOutputClientTimeout(out, timeout, connectErrorMessage, fn)
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
	fn TimedClientHandler,
) error {
	return withOutputClient(out, connectErrorMessage, func(client *grpcclient.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		result, err := fn(ctx, client)
		return finalizeClientResult(out, result, err)
	})
}

func withPlainClient(connectErrorMessage string, fn func(client *grpcclient.Client) error) error {
	client, err := grpcclient.New()
	if err != nil {
		return fmt.Errorf("%s: %w", connectErrorMessage, err)
	}
	defer client.Close()
	return fn(client)
}

func withPlainClientTimeout(
	timeout time.Duration,
	connectErrorMessage string,
	fn func(ctx context.Context, client *grpcclient.Client) error,
) error {
	return withPlainClient(connectErrorMessage, func(client *grpcclient.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return fn(ctx, client)
	})
}

func printMissingReferenceAdapters(w io.Writer, missing []string, showHelp bool) {
	if len(missing) == 0 {
		return
	}
	fmt.Fprintf(w, quickstartMissingRefsWarning, strings.Join(missing, ", "))
	if showHelp {
		fmt.Fprint(w, quickstartMissingRefsHelp)
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
