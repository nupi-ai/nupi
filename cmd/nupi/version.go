package main

import (
	"context"
	"fmt"
	"time"

	"github.com/nupi-ai/nupi/internal/grpcclient"
	nupiversion "github.com/nupi-ai/nupi/internal/version"
	"github.com/spf13/cobra"
)

func newVersionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show client and daemon versions",
		RunE:  runVersion,
	}
	return cmd
}

func runVersion(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)
	clientVersion := nupiversion.String()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var daemonVersion string
	var daemonReachable bool
	var daemonErr error
	gc, err := grpcclient.New()
	if err == nil {
		defer gc.Close()
		gc.DisableVersionCheck() // version command handles mismatch reporting itself
		resp, rpcErr := gc.DaemonStatus(ctx)
		if rpcErr == nil {
			daemonReachable = true
			daemonVersion = resp.GetVersion()
		} else {
			daemonErr = rpcErr
		}
	} else {
		daemonErr = err
	}

	if out.jsonMode {
		data := map[string]any{
			"client": clientVersion,
		}
		if daemonReachable {
			if daemonVersion != "" {
				data["daemon"] = daemonVersion
			} else {
				data["daemon"] = "unknown"
			}
			if w := nupiversion.CheckVersionMismatch(daemonVersion); w != "" {
				data["mismatch"] = true
				data["warning"] = w
			}
		} else {
			data["daemon"] = nil
			if daemonErr != nil {
				data["daemon_error"] = daemonErr.Error()
			}
		}
		return out.Print(data)
	}

	fmt.Printf("Client: %s\n", nupiversion.FormatVersion(clientVersion))
	if daemonReachable {
		if daemonVersion != "" {
			fmt.Printf("Daemon: %s\n", nupiversion.FormatVersion(daemonVersion))
		} else {
			fmt.Println("Daemon: running (version unknown)")
		}
		if w := nupiversion.CheckVersionMismatch(daemonVersion); w != "" {
			fmt.Println(w)
		}
	} else {
		fmt.Printf("Daemon: unavailable (%v)\n", daemonErr)
	}

	return nil
}
