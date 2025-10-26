package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"

	runtimecfg "github.com/nupi-ai/nupi/internal/adapterrunner/runtime"
	nupiversion "github.com/nupi-ai/nupi/internal/version"
)

func main() {
	printVersion := flag.Bool("version", false, "Print adapter-runner version and exit")
	slot := flag.String("slot", "", "Adapter slot (informational)")
	adapter := flag.String("adapter", "", "Adapter identifier (informational)")
	flag.Parse()

	if *printVersion {
		fmt.Println(nupiversion.String())
		return
	}

	cfg, err := runtimecfg.LoadConfigFromEnv()
	if err != nil {
		log.Fatalf("adapter-runner: %v", err)
	}

	if *slot != "" {
		cfg.Slot = *slot
	}
	if *adapter != "" {
		cfg.Adapter = *adapter
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Printf("adapter-runner %s starting (%s/%s)", nupiversion.String(), safe(cfg.Slot), safe(cfg.Adapter))

	proc, err := runtimecfg.Start(ctx, cfg, nil)
	if err != nil {
		log.Fatalf("adapter-runner: failed to start adapter: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- proc.Wait()
	}()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, runtimecfg.DefaultStopSignal())
	defer signal.Stop(sigCh)

	for {
		select {
		case err := <-errCh:
			exitCode := runtimecfg.ExitCode(err)
			if err != nil {
				log.Printf("adapter-runner: adapter exited: %v (code=%d)", err, exitCode)
			} else {
				log.Printf("adapter-runner: adapter exited successfully")
			}
			os.Exit(exitCode)
		case sig := <-sigCh:
			log.Printf("adapter-runner: received signal %s, forwarding to adapter (pid=%d)", sig, proc.Pid())
			if err := proc.Terminate(sig, cfg.GracePeriod); err != nil {
				log.Printf("adapter-runner: graceful termination failed: %v", err)
			}
		}
		// loop exits via os.Exit inside select
	}
}

func safe(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return value
}
