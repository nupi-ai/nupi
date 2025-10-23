package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/daemon"
	nupiversion "github.com/nupi-ai/nupi/internal/version"
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:           "nupid",
		Short:         "Nupi daemon - manages PTY sessions and HTTP API",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runDaemon,
	}
	rootCmd.Version = nupiversion.String()
	rootCmd.SetVersionTemplate("{{printf \"%s\\n\" .Version}}")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runDaemon(cmd *cobra.Command, args []string) error {
	if err := setupLogging(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialise logging: %v\n", err)
	}

	if daemon.IsRunning() {
		return fmt.Errorf("daemon is already running")
	}

	if _, err := config.EnsureInstanceDirs(config.DefaultInstance); err != nil {
		return fmt.Errorf("failed to prepare instance directories: %w", err)
	}
	if _, err := config.EnsureProfileDirs(config.DefaultInstance, config.DefaultProfile); err != nil {
		return fmt.Errorf("failed to prepare profile directories: %w", err)
	}

	store, err := configstore.Open(configstore.Options{
		InstanceName: config.DefaultInstance,
		ProfileName:  config.DefaultProfile,
	})
	if err != nil {
		return fmt.Errorf("failed to open config store: %w", err)
	}

	d, err := daemon.New(daemon.Options{Store: store})
	if err != nil {
		store.Close()
		return fmt.Errorf("failed to create daemon: %w", err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	errChan := make(chan error, 1)
	go func() {
		if err := d.Start(); err != nil {
			errChan <- err
		}
	}()

	paths := config.GetInstancePaths(config.DefaultInstance)
	log.Printf("Nupi daemon started (PID: %d)", os.Getpid())
	log.Printf("Unix socket: %s", paths.Socket)

	select {
	case sig := <-sigChan:
		log.Printf("Received signal %s, shutting down...", sig)
		if err := d.Shutdown(); err != nil {
			log.Printf("Error during shutdown: %v", err)
		}
	case err := <-errChan:
		log.Printf("Daemon error: %v", err)
		return err
	}

	log.Println("Daemon stopped")
	return nil
}

func setupLogging() error {
	paths, err := config.EnsureInstanceDirs(config.DefaultInstance)
	if err != nil {
		return fmt.Errorf("initialise instance directories: %w", err)
	}

	if err := os.MkdirAll(paths.Logs, 0o755); err != nil {
		return fmt.Errorf("create logs directory: %w", err)
	}

	logPath := filepath.Join(paths.Logs, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	multi := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multi)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	log.Printf("=== Nupi Daemon Starting (PID: %d) ===", os.Getpid())
	log.Printf("Log file: %s", logPath)
	return nil
}
