package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	apihttp "github.com/nupi-ai/nupi/internal/api/http"
	"github.com/nupi-ai/nupi/internal/config"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/grpcclient"
	manifestpkg "github.com/nupi-ai/nupi/internal/plugins/manifest"
	"github.com/nupi-ai/nupi/internal/sanitize"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newAdaptersCommand() *cobra.Command {
	adaptersCmd := &cobra.Command{
		Use:           "adapters",
		Short:         "Inspect and control adapter bindings",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	adaptersRegisterCmd := &cobra.Command{
		Use:           "register",
		Short:         "Register or update an adapter",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          adaptersRegister,
	}
	adaptersRegisterCmd.Example = `  # Register gRPC-based STT adapter
  nupi adapters register \
    --id nupi-whisper-local-stt \
    --type stt \
    --name "Nupi Whisper Local STT" \
    --version 0.1.0 \
    --endpoint-transport grpc \
    --endpoint-address 127.0.0.1:50051

  # Register process-based AI adapter
  nupi adapters register \
    --id custom-ai \
    --type ai \
    --endpoint-transport process \
    --endpoint-command /path/to/binary \
    --endpoint-arg "--config" \
    --endpoint-arg "/etc/adapter.json"`
	adaptersRegisterCmd.Flags().String("id", "", "Adapter identifier (slug)")
	adaptersRegisterCmd.Flags().String("type", "", "Adapter type (stt/tts/ai/vad/...)")
	adaptersRegisterCmd.Flags().String("name", "", "Human readable adapter name")
	adaptersRegisterCmd.Flags().String("source", "external", "Adapter source/provider")
	adaptersRegisterCmd.Flags().String("version", "", "Adapter version")
	adaptersRegisterCmd.Flags().String("manifest", "", "Optional manifest JSON payload")
	adaptersRegisterCmd.Flags().String("endpoint-transport", constants.AdapterTransportGRPC, "Adapter endpoint transport (process|grpc|http)")
	adaptersRegisterCmd.Flags().String("endpoint-address", "", "Adapter endpoint address (for grpc/http transports)")
	adaptersRegisterCmd.Flags().String("endpoint-command", "", "Command to launch adapter (process transport)")
	adaptersRegisterCmd.Flags().StringArray("endpoint-arg", nil, "Argument for adapter command (repeatable)")
	adaptersRegisterCmd.Flags().StringArray("endpoint-env", nil, "Environment variable KEY=VALUE for adapter command (repeatable)")

	adaptersInstallLocalCmd := &cobra.Command{
		Use:           "install-local",
		Short:         "Register an adapter from local manifest and binary",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          adaptersInstallLocal,
	}
	adaptersInstallLocalCmd.Example = `  # Install a local Whisper STT adapter
	nupi adapters install-local \
	  --manifest-file ./adapter-nupi-whisper-local-stt/plugin.yaml \
	  --binary $(pwd)/adapter-nupi-whisper-local-stt/dist/adapter-nupi-whisper-local-stt \
	  --endpoint-address 127.0.0.1:50051 \
	  --slot stt`
	adaptersInstallLocalCmd.Flags().String("manifest-file", "", "Path to adapter manifest (YAML or JSON)")
	adaptersInstallLocalCmd.Flags().String("id", "", "Override adapter identifier (defaults to manifest metadata.slug)")
	adaptersInstallLocalCmd.Flags().String("binary", "", "Path to adapter executable (overrides manifest entrypoint.command)")
	adaptersInstallLocalCmd.Flags().Bool("copy-binary", false, "Copy adapter binary into the instance plugin directory")
	adaptersInstallLocalCmd.Flags().Bool("build", false, "Build the adapter from sources before registration")
	adaptersInstallLocalCmd.Flags().String("adapter-dir", "", "Adapter source directory (required with --build)")
	adaptersInstallLocalCmd.Flags().String("endpoint-address", "", "Address for gRPC/HTTP transport")
	adaptersInstallLocalCmd.Flags().StringArray("endpoint-arg", nil, "Additional command argument (repeatable)")
	adaptersInstallLocalCmd.Flags().StringArray("endpoint-env", nil, "Environment variable KEY=VALUE passed to the adapter (repeatable)")
	adaptersInstallLocalCmd.Flags().String("slot", "", "Optional slot to bind after registration (e.g. stt)")
	adaptersInstallLocalCmd.Flags().String("config", "", "Optional JSON configuration payload for slot binding")
	adaptersInstallLocalCmd.Flags().String("build-timeout", "5m", "Maximum duration for the build step (e.g. 30s, 2m, 5m)")

	adaptersLogsCmd := &cobra.Command{
		Use:           "logs",
		Short:         "Stream adapter logs and transcripts",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          adaptersLogs,
	}
	adaptersLogsCmd.Long = `Stream real-time adapter logs and speech transcripts.

Filters:
  --slot=SLOT       Filter logs by slot (e.g. stt)
  --adapter=ID      Filter logs by adapter identifier

Notes:
  - When both --slot and --adapter are provided, transcript entries are omitted
    because transcripts are not yet mapped to specific adapters.
  - Use --json to consume newline-delimited JSON for tooling and pipelines.

Examples:
  nupi adapters logs
  nupi adapters logs --slot=stt
  nupi adapters logs --adapter=adapter.stt.mock
  nupi adapters logs --json | jq .
`
	adaptersLogsCmd.Flags().String("slot", "", "Filter logs by slot (e.g. stt)")
	adaptersLogsCmd.Flags().String("adapter", "", "Filter logs by adapter identifier")

	adaptersListCmd := &cobra.Command{
		Use:           "list",
		Short:         "Show adapter slots, bindings and runtime status (see also: nupi plugins list)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          adaptersList,
	}

	adaptersBindCmd := &cobra.Command{
		Use:           "bind <slot> <adapter>",
		Short:         "Bind an adapter to a slot (optionally with config) and start it",
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          adaptersBind,
	}
	adaptersBindCmd.Flags().String("config", "", "JSON configuration payload sent to the adapter binding")

	adaptersStartCmd := &cobra.Command{
		Use:           "start <slot>",
		Short:         "Start the adapter process for the given slot",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          adaptersStart,
	}

	adaptersStopCmd := &cobra.Command{
		Use:           "stop <slot>",
		Short:         "Stop the adapter process for the given slot (binding is kept)",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          adaptersStop,
	}

	adaptersCmd.AddCommand(adaptersRegisterCmd, adaptersListCmd, adaptersBindCmd, adaptersStartCmd, adaptersStopCmd, adaptersLogsCmd)
	adaptersCmd.AddCommand(adaptersInstallLocalCmd)
	return adaptersCmd
}

// --- Adapter command handlers ---

func adaptersList(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	out.PrintText(func() {
		fmt.Fprintln(out.ErrWriter(), "Hint: use 'nupi plugins list' for a unified view of all plugins")
	})

	return adaptersListGRPC(out)
}

func adaptersRegister(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	adapterID := getTrimmedFlag(cmd, "id")
	if adapterID == "" {
		return out.Error("--id is required", errors.New("missing adapter id"))
	}

	adapterType := getTrimmedFlag(cmd, "type")
	if adapterType != "" {
		if _, ok := allowedAdapterSlots[adapterType]; !ok {
			return out.Error(fmt.Sprintf("invalid type %q (expected: %s)", adapterType, strings.Join(constants.AllowedAdapterSlots, ", ")), errors.New("invalid adapter type"))
		}
	}

	name := getTrimmedFlag(cmd, "name")
	source := getTrimmedFlag(cmd, "source")
	version := getTrimmedFlag(cmd, "version")
	manifestRaw, _ := cmd.Flags().GetString("manifest")

	manifest, err := parseManifest(manifestRaw)
	if err != nil {
		return out.Error("Manifest must be valid JSON or YAML", err)
	}

	transportOpt := getTrimmedFlag(cmd, "endpoint-transport")
	addressOpt := getTrimmedFlag(cmd, "endpoint-address")
	commandOpt := getTrimmedFlag(cmd, "endpoint-command")
	argsOpt, _ := cmd.Flags().GetStringArray("endpoint-arg")
	envOpt, _ := cmd.Flags().GetStringArray("endpoint-env")

	endpointEnv, err := parseKeyValuePairs(envOpt)
	if err != nil {
		return out.Error("Invalid --endpoint-env value", err)
	}

	var endpoint *apiv1.AdapterEndpointConfig
	if transportOpt != "" || addressOpt != "" || commandOpt != "" || len(argsOpt) > 0 || len(endpointEnv) > 0 {
		if transportOpt == "" {
			return out.Error("--endpoint-transport required when endpoint flags are specified", errors.New("missing transport"))
		}
		if _, ok := allowedEndpointTransports[transportOpt]; !ok {
			return out.Error(fmt.Sprintf("invalid transport: %s (expected: %s)", transportOpt, strings.Join(constants.AllowedAdapterTransports, ", ")), errors.New("invalid transport"))
		}
		switch transportOpt {
		case constants.AdapterTransportGRPC:
			if addressOpt == "" {
				return out.Error("--endpoint-address required for grpc transport", errors.New("missing address"))
			}
		case constants.AdapterTransportHTTP:
			if addressOpt == "" {
				return out.Error("--endpoint-address required for http transport", errors.New("missing address"))
			}
			if commandOpt != "" || len(argsOpt) > 0 {
				return out.Error("--endpoint-command/--endpoint-arg not allowed for http transport", errors.New("conflicting endpoint flags"))
			}
		case constants.AdapterTransportProcess:
			if commandOpt == "" {
				return out.Error("--endpoint-command required for process transport", errors.New("missing command"))
			}
			if addressOpt != "" {
				return out.Error("--endpoint-address not used for process transport", errors.New("unexpected address"))
			}
		}

		endpoint = &apiv1.AdapterEndpointConfig{
			Transport: transportOpt,
			Address:   addressOpt,
			Command:   commandOpt,
			Args:      append([]string(nil), argsOpt...),
			Env:       endpointEnv,
		}
	}

	req := &apiv1.RegisterAdapterRequest{
		AdapterId:    adapterID,
		Source:       source,
		Version:      version,
		Type:         adapterType,
		Name:         name,
		ManifestYaml: string(manifest),
		Endpoint:     endpoint,
	}

	resp, err := adaptersRegisterGRPCFunc(out, req)
	if err != nil {
		return err
	}

	result := adapterRegistrationResultFromProto(resp)
	stdout := cmd.OutOrStdout()
	return out.Render(CommandResult{
		Data: result,
		HumanReadable: func() error {
			fmt.Fprintf(stdout, "Registered adapter %s", resp.GetAdapterId())
			if resp.GetType() != "" {
				fmt.Fprintf(stdout, " (%s)", resp.GetType())
			}
			fmt.Fprintln(stdout)
			return nil
		},
	})
}

func adaptersInstallLocal(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	var cleanupWriter io.Writer = cmd.ErrOrStderr()
	if out.jsonMode {
		cleanupWriter = io.Discard
	}

	manifestPath := getTrimmedFlag(cmd, "manifest-file")
	if manifestPath == "" {
		return out.Error("--manifest-file is required", errors.New("missing manifest file"))
	}
	absManifestPath, err := filepath.Abs(config.ExpandPath(manifestPath))
	if err != nil {
		return out.Error("Failed to resolve manifest path", err)
	}
	manifestDir := filepath.Dir(absManifestPath)

	manifest, manifestRaw, err := loadAdapterManifestFile(manifestPath)
	if err != nil {
		return out.Error("Failed to read manifest", err)
	}

	manifestJSON, err := parseManifest(manifestRaw)
	if err != nil {
		return out.Error("Failed to parse manifest", err)
	}

	adapterID := getTrimmedFlag(cmd, "id")
	if adapterID == "" {
		adapterID = formatAdapterID(manifest.Metadata.Namespace, manifest.Metadata.Slug)
	}
	if adapterID == "" {
		return out.Error("Adapter identifier required", errors.New("manifest metadata.slug missing; use --id"))
	}

	slotType := ""
	if manifest.Adapter != nil {
		slotType = strings.TrimSpace(manifest.Adapter.Slot)
	}
	if slotType == "" {
		return out.Error("Adapter slot missing in manifest", errors.New("manifest spec.slot empty"))
	}
	if _, ok := allowedAdapterSlots[slotType]; !ok {
		return out.Error(fmt.Sprintf("unsupported adapter slot %q", slotType), errors.New("invalid adapter slot"))
	}
	adapterName := strings.TrimSpace(manifest.Metadata.Name)
	copyBinary, _ := cmd.Flags().GetBool("copy-binary")
	buildFlag, _ := cmd.Flags().GetBool("build")
	sourceDir := getTrimmedFlag(cmd, "adapter-dir")
	if sourceDir != "" {
		sourceDir = config.ExpandPath(sourceDir)
		absDir, err := filepath.Abs(sourceDir)
		if err != nil {
			return out.Error("Failed to resolve adapter directory", err)
		}
		sourceDir = absDir
	}

	binaryFlag := getTrimmedFlag(cmd, "binary")
	if binaryFlag != "" {
		binaryFlag = config.ExpandPath(binaryFlag)
		if !filepath.IsAbs(binaryFlag) {
			baseDir := sourceDir
			if baseDir == "" {
				baseDir = manifestDir
			}
			if baseDir != "" {
				binaryFlag = filepath.Join(baseDir, binaryFlag)
			}
		}
		if absBinary, err := filepath.Abs(binaryFlag); err == nil {
			binaryFlag = absBinary
		}
	}

	if buildFlag && binaryFlag != "" {
		return out.Error("Cannot combine --build with --binary", errors.New("conflicting binary options"))
	}
	if buildFlag && sourceDir == "" {
		return out.Error("--adapter-dir is required when --build is set", errors.New("missing adapter directory"))
	}

	// Use manifest slug when available so locally-installed adapters land in the same
	// directory structure as runtime-managed process transports. This keeps assets/config in
	// sync regardless of install path.
	slugSource := strings.TrimSpace(manifest.Metadata.Slug)
	if slugSource == "" {
		slugSource = adapterID
	}
	slug := sanitize.SafeSlug(slugSource)

	var command string
	if buildFlag {
		buildTimeoutStr, _ := cmd.Flags().GetString("build-timeout")
		buildTimeout, err := time.ParseDuration(buildTimeoutStr)
		if err != nil {
			return out.Error("invalid --build-timeout: must be a Go duration like 30s, 2m, 5m", err)
		}
		if buildTimeout <= 0 {
			return out.Error("invalid --build-timeout: must be positive", fmt.Errorf("got %s", buildTimeoutStr))
		}

		outputDir := filepath.Join(sourceDir, "dist")
		distExisted := dirExists(outputDir)
		if err := os.MkdirAll(outputDir, 0o755); err != nil {
			return out.Error("Failed to create dist directory", err)
		}
		outputPath := filepath.Join(outputDir, slug)
		outputFileExisted := fileExists(outputPath)
		buildCtx, cancel := context.WithTimeout(context.Background(), buildTimeout)
		defer cancel()
		buildCmd := exec.CommandContext(buildCtx, "go", "build", "-o", outputPath, "./cmd/adapter")
		buildCmd.Dir = sourceDir
		buildCmd.Env = os.Environ()
		if output, err := buildCmd.CombinedOutput(); err != nil {
			buildErr := err
			if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
				buildErr = fmt.Errorf("%w: %s", err, trimmed)
			}
			cleanupBuildArtifacts(cleanupWriter, outputDir, outputPath, distExisted, outputFileExisted)
			if buildCtx.Err() == context.DeadlineExceeded {
				return out.Error(fmt.Sprintf("build timed out after %s", buildTimeout), buildErr)
			}
			return out.Error("Failed to build adapter", buildErr)
		}
		command = outputPath
	} else if binaryFlag != "" {
		if _, err := os.Stat(binaryFlag); err != nil {
			return out.Error(fmt.Sprintf("adapter binary not found: %s", binaryFlag), err)
		}
		command = binaryFlag
	} else {
		if manifest.Adapter == nil {
			return out.Error("Manifest missing adapter spec", errors.New("adapter spec absent"))
		}
		command = strings.TrimSpace(manifest.Adapter.Entrypoint.Command)
		if command != "" {
			command = config.ExpandPath(command)
			// Bare command names (e.g. "python3") are intentionally NOT joined with
			// baseDir here — they should be resolved via PATH by normalizeCommand
			// below. Only relative paths with a separator (e.g. "./bin/adapter")
			// are anchored to the source/manifest directory. This differs from
			// --binary which always anchors to baseDir because --binary is expected
			// to reference a file path, never a system command.
			if !filepath.IsAbs(command) && containsPathSeparator(command) {
				baseDir := sourceDir
				if baseDir == "" {
					baseDir = manifestDir
				}
				if baseDir != "" {
					command = filepath.Join(baseDir, command)
				}
			}
		}
	}

	if command == "" {
		return out.Error("Manifest missing entrypoint command", errors.New("spec.entrypoint.command empty"))
	}

	args := []string{}
	if manifest.Adapter != nil {
		args = append(args, manifest.Adapter.Entrypoint.Args...)
	}
	extraArgs, _ := cmd.Flags().GetStringArray("endpoint-arg")
	if len(extraArgs) > 0 {
		args = append(args, extraArgs...)
	}

	endpointEnvValues, _ := cmd.Flags().GetStringArray("endpoint-env")
	endpointEnv, err := parseKeyValuePairs(endpointEnvValues)
	if err != nil {
		return out.Error("Invalid --endpoint-env value", err)
	}

	transport := ""
	if manifest.Adapter != nil {
		transport = strings.TrimSpace(manifest.Adapter.Entrypoint.Transport)
	}
	if transport == "" {
		transport = constants.DefaultAdapterTransport
	}
	if _, ok := allowedEndpointTransports[transport]; !ok {
		return out.Error(fmt.Sprintf("manifest transport %q unsupported", transport), errors.New("invalid manifest transport"))
	}

	address := getTrimmedFlag(cmd, "endpoint-address")
	switch transport {
	case constants.AdapterTransportGRPC, constants.AdapterTransportHTTP:
		if address == "" {
			return out.Error(fmt.Sprintf("--endpoint-address required for %s transport", transport), errors.New("missing endpoint address"))
		}
	case constants.AdapterTransportProcess:
		if address != "" {
			return out.Error("--endpoint-address not used for process transport", errors.New("unexpected address"))
		}
	}

	var (
		adapterHome       string
		destPath          string
		adapterDirExisted bool
		destFileExisted   bool
		binaryCopied      bool
	)

	if copyBinary {
		if command == "" {
			return out.Error("--binary or --build required when --copy-binary is set", errors.New("missing adapter binary"))
		}
		srcPath := command
		if !filepath.IsAbs(srcPath) {
			absSrc, err := filepath.Abs(srcPath)
			if err != nil {
				return out.Error("Failed to resolve adapter binary", err)
			}
			srcPath = absSrc
		}
		paths, err := config.EnsureInstanceDirs("")
		if err != nil {
			return out.Error("Failed to prepare instance directories", err)
		}
		adapterHome = filepath.Join(paths.Home, "plugins", slug)
		adapterDirExisted = dirExists(adapterHome)
		binDir := filepath.Join(adapterHome, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			return out.Error("Failed to create adapter bin directory", err)
		}
		destName := filepath.Base(srcPath)
		if destName == "" {
			destName = slug
		}
		destPath = filepath.Join(binDir, destName)
		if rel, err := filepath.Rel(adapterHome, destPath); err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(filepath.ToSlash(rel), "../") {
			return out.Error("Resolved destination escapes adapter directory", errors.New("invalid destination path"))
		}
		destFileExisted = fileExists(destPath)
		if err := copyFile(srcPath, destPath, 0o755); err != nil {
			cleanupAdapterDir(cleanupWriter, adapterHome, destPath, adapterDirExisted, destFileExisted)
			return out.Error("Failed to copy adapter binary", err)
		}
		binaryCopied = true
		command = destPath
	}

	// Normalize command to an absolute path so that the registered endpoint
	// and sanityCheck both operate on the same resolved binary. This avoids
	// mismatches when daemon PATH differs from install-time PATH.
	command = normalizeCommand(command, cleanupWriter)

	regReq := &apiv1.RegisterAdapterRequest{
		AdapterId:    adapterID,
		Source:       "local",
		Type:         slotType,
		Name:         adapterName,
		Version:      strings.TrimSpace(manifest.Metadata.Version),
		ManifestYaml: string(manifestJSON),
		Endpoint: &apiv1.AdapterEndpointConfig{
			Transport: transport,
			Address:   address,
			Command:   command,
			Args:      args,
			Env:       endpointEnv,
		},
	}

	resp, err := adaptersRegisterGRPCFunc(out, regReq)
	if err != nil {
		if binaryCopied {
			cleanupAdapterDir(cleanupWriter, adapterHome, destPath, adapterDirExisted, destFileExisted)
		}
		return err
	}

	// Sanity check: only runs for process transport or when a binary was copied locally.
	// Remote (gRPC/HTTP) adapters without --copy-binary have no local binary to verify,
	// so sc stays nil and JSON output omits the sanity_check key (omitempty).
	var sc *sanityCheckResult
	if transport == constants.AdapterTransportProcess || binaryCopied {
		r := sanityCheck(command)
		sc = &r
	}

	result := installLocalResult{
		Adapter:     adapterRegistrationResultFromProto(resp).Adapter,
		SanityCheck: sc,
	}
	stdout := out.w
	if err := out.Render(CommandResult{
		Data: result,
		HumanReadable: func() error {
			fmt.Fprintf(stdout, "Installed local adapter %s (%s)\n", resp.GetName(), resp.GetAdapterId())
			if sc != nil {
				printSanityCheckResult(stdout, *sc)
			}
			return nil
		},
	}); err != nil {
		return err
	}

	slot := getTrimmedFlag(cmd, "slot")
	if slot == "" {
		return nil
	}

	configRaw := getTrimmedFlag(cmd, "config")
	if configRaw != "" && !json.Valid([]byte(configRaw)) {
		return out.Error("Config must be valid JSON", errors.New("invalid config payload"))
	}

	if err := adaptersBindGRPC(out, slot, adapterID, configRaw); err != nil {
		return err
	}

	out.PrintText(func() {
		fmt.Fprintf(stdout, "Bound adapter %s to %s\n", adapterID, slot)
	})
	return nil
}

func adaptersLogs(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	slot := getTrimmedFlag(cmd, "slot")
	adapter := getTrimmedFlag(cmd, "adapter")

	return withOutputClient(out, daemonConnectGRPCErrorMessage, func(gc *grpcclient.Client) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt)
		defer signal.Stop(sigs)
		go func() {
			select {
			case <-sigs:
				cancel()
			case <-ctx.Done():
			}
		}()

		stream, err := gc.StreamAdapterLogs(ctx, &apiv1.StreamAdapterLogsRequest{
			Slot:    slot,
			Adapter: adapter,
		})
		if err != nil {
			return out.Error("Failed to stream adapter logs", err)
		}

		for {
			entry, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return out.Error("Adapter log stream ended with error", err)
			}

			if out.jsonMode {
				printAdapterLogEntryJSON(out.w, entry)
				continue
			}

			printAdapterLogEntry(out.w, entry)
		}
	})
}

func printAdapterLogEntry(w io.Writer, entry *apiv1.AdapterLogStreamEntry) {
	switch payload := entry.GetPayload().(type) {
	case *apiv1.AdapterLogStreamEntry_Log:
		log := payload.Log
		ts := time.Now().UTC()
		if log.GetTimestamp() != nil {
			ts = log.GetTimestamp().AsTime()
		}
		timestamp := ts.Format(time.RFC3339)
		level := strings.ToUpper(strings.TrimSpace(log.GetLevel()))
		if level == "" {
			level = "INFO"
		}
		slot := strings.TrimSpace(log.GetSlot())
		adapterID := strings.TrimSpace(log.GetAdapterId())
		if slot != "" {
			if adapterID != "" {
				fmt.Fprintf(w, "%s [%s] %s %s: %s\n", timestamp, slot, adapterID, level, log.GetMessage())
			} else {
				fmt.Fprintf(w, "%s [%s] %s: %s\n", timestamp, slot, level, log.GetMessage())
			}
		} else {
			if adapterID != "" {
				fmt.Fprintf(w, "%s %s %s: %s\n", timestamp, adapterID, level, log.GetMessage())
			} else {
				fmt.Fprintf(w, "%s %s: %s\n", timestamp, level, log.GetMessage())
			}
		}
	case *apiv1.AdapterLogStreamEntry_Transcript:
		tr := payload.Transcript
		ts := time.Now().UTC()
		if tr.GetTimestamp() != nil {
			ts = tr.GetTimestamp().AsTime()
		}
		timestamp := ts.Format(time.RFC3339)
		stage := "partial"
		if tr.GetIsFinal() {
			stage = "final"
		}
		fmt.Fprintf(w, "%s [transcript %s] session=%s stream=%s conf=%.2f %s\n",
			timestamp, stage, tr.GetSessionId(), tr.GetStreamId(), tr.GetConfidence(), tr.GetText())
	}
}

func printAdapterLogEntryJSON(w io.Writer, entry *apiv1.AdapterLogStreamEntry) {
	var m map[string]interface{}
	switch payload := entry.GetPayload().(type) {
	case *apiv1.AdapterLogStreamEntry_Log:
		log := payload.Log
		m = map[string]interface{}{
			"type":    "log",
			"level":   log.GetLevel(),
			"message": log.GetMessage(),
		}
		if log.GetTimestamp() != nil {
			m["timestamp"] = log.GetTimestamp().AsTime().Format(time.RFC3339Nano)
		}
		if s := log.GetSlot(); s != "" {
			m["slot"] = s
		}
		if id := log.GetAdapterId(); id != "" {
			m["adapter_id"] = id
		}
	case *apiv1.AdapterLogStreamEntry_Transcript:
		tr := payload.Transcript
		m = map[string]interface{}{
			"type":       "transcript",
			"text":       tr.GetText(),
			"confidence": tr.GetConfidence(),
			"final":      tr.GetIsFinal(),
		}
		if tr.GetTimestamp() != nil {
			m["timestamp"] = tr.GetTimestamp().AsTime().Format(time.RFC3339Nano)
		}
		if s := tr.GetSessionId(); s != "" {
			m["session_id"] = s
		}
		if s := tr.GetStreamId(); s != "" {
			m["stream_id"] = s
		}
	default:
		return
	}
	data, err := json.Marshal(m)
	if err != nil {
		if errJSON, e := json.Marshal(map[string]string{"error": fmt.Sprintf("failed to marshal log entry: %s", err)}); e == nil {
			fmt.Fprintln(w, string(errJSON))
		}
		return
	}
	fmt.Fprintln(w, string(data))
}

func adaptersBind(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	slot := strings.TrimSpace(args[0])
	adapter := strings.TrimSpace(args[1])
	if slot == "" || adapter == "" {
		return out.Error("Slot and adapter must be provided", errors.New("invalid arguments"))
	}

	cfg, err := cmd.Flags().GetString("config")
	if err != nil {
		return out.Error("Failed to read --config flag", err)
	}

	cfgTrimmed := strings.TrimSpace(cfg)
	if cfgTrimmed != "" && !json.Valid([]byte(cfgTrimmed)) {
		return out.Error("Config must be valid JSON", fmt.Errorf("invalid config payload"))
	}

	return adaptersBindGRPC(out, slot, adapter, cfgTrimmed)
}

func adaptersStart(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	slot := strings.TrimSpace(args[0])
	if slot == "" {
		return out.Error("Slot must be provided", errors.New("invalid arguments"))
	}

	return adaptersStartGRPC(out, slot)
}

func adaptersStop(cmd *cobra.Command, args []string) error {
	out := newOutputFormatter(cmd)

	slot := strings.TrimSpace(args[0])
	if slot == "" {
		return out.Error("Slot must be provided", errors.New("invalid arguments"))
	}

	return adaptersStopGRPC(out, slot)
}

// --- gRPC transport handlers ---

// adaptersRegisterGRPCFunc is the package-level function variable used for adapter registration.
// Tests can replace it to avoid requiring a running daemon.
var adaptersRegisterGRPCFunc = adaptersRegisterGRPC

func adaptersRegisterGRPC(out *OutputFormatter, req *apiv1.RegisterAdapterRequest) (*apiv1.RegisterAdapterResponse, error) {
	var resp *apiv1.RegisterAdapterResponse
	if err := withOutputClientTimeout(out, constants.Duration5Seconds, daemonConnectGRPCErrorMessage, func(ctx context.Context, gc *grpcclient.Client) (any, error) {
		var err error
		resp, err = gc.RegisterAdapter(ctx, req)
		if err != nil {
			return nil, clientCallFailed("Failed to register adapter via gRPC", err)
		}
		return nil, nil
	}); err != nil {
		return nil, err
	}
	return resp, nil
}

func adapterRegistrationResultFromProto(resp *apiv1.RegisterAdapterResponse) apihttp.AdapterRegistrationResult {
	return apihttp.AdapterRegistrationResult{
		Adapter: apihttp.AdapterDescriptor{
			ID:      resp.GetAdapterId(),
			Source:  resp.GetSource(),
			Version: resp.GetVersion(),
			Type:    resp.GetType(),
			Name:    resp.GetName(),
		},
	}
}

func adaptersListGRPC(out *OutputFormatter) error {
	return withOutputClientTimeout(out, constants.Duration5Seconds, daemonConnectGRPCErrorMessage, func(ctx context.Context, gc *grpcclient.Client) (any, error) {
		resp, err := gc.AdaptersOverview(ctx)
		if err != nil {
			return nil, clientCallFailed("Failed to fetch adapters overview via gRPC", err)
		}

		overview := adaptersOverviewFromProto(resp)
		return CommandResult{
			Data: overview,
			HumanReadable: func() error {
				if len(overview.Adapters) == 0 {
					fmt.Fprintln(out.w, "No adapter slots found.")
					return nil
				}

				printAdapterTable(out.w, overview.Adapters)
				printAdapterRuntimeMessages(out.w, overview.Adapters)
				return nil
			},
		}, nil
	})
}

func adaptersBindGRPC(out *OutputFormatter, slot, adapter, cfg string) error {
	return withOutputClientTimeout(out, constants.Duration5Seconds, daemonConnectGRPCErrorMessage, func(ctx context.Context, gc *grpcclient.Client) (any, error) {
		req := &apiv1.BindAdapterRequest{
			Slot:       slot,
			AdapterId:  adapter,
			ConfigJson: strings.TrimSpace(cfg),
		}

		resp, err := gc.BindAdapter(ctx, req)
		if err != nil {
			return nil, clientCallFailed("Failed to bind adapter via gRPC", err)
		}

		entry := adapterEntryFromProto(resp.GetAdapter())
		return CommandResult{
			Data: apihttp.AdapterActionResult{Adapter: entry},
			HumanReadable: func() error {
				printAdapterSummary(out.w, "Bound", entry)
				return nil
			},
		}, nil
	})
}

func adaptersStartGRPC(out *OutputFormatter, slot string) error {
	return withOutputClientTimeout(out, constants.Duration5Seconds, daemonConnectGRPCErrorMessage, func(ctx context.Context, gc *grpcclient.Client) (any, error) {
		resp, err := gc.StartAdapter(ctx, slot)
		if err != nil {
			return nil, clientCallFailed("Failed to start adapter via gRPC", err)
		}

		entry := adapterEntryFromProto(resp.GetAdapter())
		return CommandResult{
			Data: apihttp.AdapterActionResult{Adapter: entry},
			HumanReadable: func() error {
				printAdapterSummary(out.w, "Started", entry)
				return nil
			},
		}, nil
	})
}

func adaptersStopGRPC(out *OutputFormatter, slot string) error {
	return withOutputClientTimeout(out, constants.Duration5Seconds, daemonConnectGRPCErrorMessage, func(ctx context.Context, gc *grpcclient.Client) (any, error) {
		resp, err := gc.StopAdapter(ctx, slot)
		if err != nil {
			return nil, clientCallFailed("Failed to stop adapter via gRPC", err)
		}

		entry := adapterEntryFromProto(resp.GetAdapter())
		return CommandResult{
			Data: apihttp.AdapterActionResult{Adapter: entry},
			HumanReadable: func() error {
				printAdapterSummary(out.w, "Stopped", entry)
				return nil
			},
		}, nil
	})
}

// --- Proto conversions ---

func adaptersOverviewFromProto(resp *apiv1.AdaptersOverviewResponse) apihttp.AdaptersOverview {
	if resp == nil {
		return apihttp.AdaptersOverview{}
	}
	out := apihttp.AdaptersOverview{
		Adapters: make([]apihttp.AdapterEntry, 0, len(resp.GetAdapters())),
	}
	for _, entry := range resp.GetAdapters() {
		out.Adapters = append(out.Adapters, adapterEntryFromProto(entry))
	}
	return out
}

func adapterEntryFromProto(entry *apiv1.AdapterEntry) apihttp.AdapterEntry {
	if entry == nil {
		return apihttp.AdapterEntry{}
	}
	out := apihttp.AdapterEntry{
		Slot:   entry.GetSlot(),
		Status: entry.GetStatus(),
		Config: entry.GetConfigJson(),
		UpdatedAt: func() string {
			if ts := entry.GetUpdatedAt(); ts != nil {
				return ts.AsTime().UTC().Format(time.RFC3339)
			}
			return ""
		}(),
	}
	if entry.AdapterId != nil {
		id := strings.TrimSpace(entry.GetAdapterId())
		if id != "" {
			out.AdapterID = &id
		}
	}
	if rt := entry.GetRuntime(); rt != nil {
		runtime := apihttp.AdapterRuntime{
			AdapterID: rt.GetAdapterId(),
			Health:    rt.GetHealth(),
			Message:   rt.GetMessage(),
			Extra:     rt.GetExtra(),
		}
		if updated := rt.GetUpdatedAt(); updated != nil {
			runtime.UpdatedAt = updated.AsTime().UTC().Format(time.RFC3339)
		}
		if ts := rt.GetStartedAt(); ts != nil {
			started := ts.AsTime().UTC().Format(time.RFC3339)
			runtime.StartedAt = &started
		}
		out.Runtime = &runtime
	}
	return out
}

// --- Display helpers ---

func formatAdapterID(namespace, slug string) string {
	namespace = strings.TrimSpace(namespace)
	slug = strings.TrimSpace(slug)
	if namespace == "" {
		namespace = "others"
	}
	if slug == "" {
		return ""
	}
	return namespace + "/" + slug
}

func adapterLabel(entry apihttp.AdapterEntry) string {
	if entry.AdapterID == nil || strings.TrimSpace(*entry.AdapterID) == "" {
		return "-"
	}
	return *entry.AdapterID
}

func adapterHealthLabel(entry apihttp.AdapterEntry) string {
	if entry.Runtime == nil || strings.TrimSpace(entry.Runtime.Health) == "" {
		return "-"
	}
	return entry.Runtime.Health
}

func sortedAdapters(entries []apihttp.AdapterEntry) []apihttp.AdapterEntry {
	if len(entries) == 0 {
		return nil
	}
	sorted := append([]apihttp.AdapterEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		return strings.Compare(sorted[i].Slot, sorted[j].Slot) < 0
	})
	return sorted
}

func printAdapterTable(out io.Writer, entries []apihttp.AdapterEntry) {
	sorted := sortedAdapters(entries)
	if len(sorted) == 0 {
		return
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SLOT\tADAPTER\tSTATUS\tHEALTH\tUPDATED")
	for _, entry := range sorted {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			entry.Slot,
			adapterLabel(entry),
			entry.Status,
			adapterHealthLabel(entry),
			entry.UpdatedAt,
		)
	}
	w.Flush()
}

func printAdapterRuntimeMessages(w io.Writer, entries []apihttp.AdapterEntry) {
	for _, entry := range sortedAdapters(entries) {
		if entry.Runtime != nil && strings.TrimSpace(entry.Runtime.Message) != "" {
			fmt.Fprintf(w, "%s: %s\n", entry.Slot, entry.Runtime.Message)
		}
	}
}

func printAdapterSummary(w io.Writer, action string, entry apihttp.AdapterEntry) {
	fmt.Fprintf(w, "%s slot %s -> %s (status: %s)\n", action, entry.Slot, adapterLabel(entry), entry.Status)
	if entry.Runtime != nil {
		fmt.Fprintf(w, "  Health: %s\n", adapterHealthLabel(entry))
		if entry.Runtime.Message != "" {
			fmt.Fprintf(w, "  Message: %s\n", entry.Runtime.Message)
		}
		if entry.Runtime.StartedAt != nil && *entry.Runtime.StartedAt != "" {
			fmt.Fprintf(w, "  Started: %s\n", *entry.Runtime.StartedAt)
		}
		fmt.Fprintf(w, "  Updated: %s\n", entry.Runtime.UpdatedAt)
	}
}

func printAvailableAdaptersForSlot(w io.Writer, slot string, adapters []adapterInfo) []adapterInfo {
	if len(adapters) == 0 {
		fmt.Fprintln(w, "Available adapters:")
		fmt.Fprintln(w, "  (no adapters installed)")
		return nil
	}

	expectedType := adapterTypeForSlot(slot)
	filtered := filterAdaptersForSlot(slot, adapters)

	if len(filtered) > 0 {
		if expectedType != "" {
			fmt.Fprintf(w, "Available adapters for %s (type: %s):\n", slot, expectedType)
		} else {
			fmt.Fprintf(w, "Available adapters for %s:\n", slot)
		}
	} else {
		if expectedType != "" {
			fmt.Fprintf(w, "No adapters of type %s installed. Showing all adapters:\n", expectedType)
		} else {
			fmt.Fprintln(w, "Available adapters:")
		}
		filtered = adapters
	}

	for idx, adapter := range filtered {
		label := adapter.ID
		if adapter.Name != "" {
			label = fmt.Sprintf("%s (%s)", adapter.Name, adapter.ID)
		}
		fmt.Fprintf(w, "  %d) %s [%s]\n", idx+1, label, adapter.Type)
	}
	return filtered
}

func resolveAdapterChoice(input string, ordered []adapterInfo, all []adapterInfo) (string, bool) {
	if idx, err := strconv.Atoi(input); err == nil {
		if idx >= 1 && idx <= len(ordered) {
			return ordered[idx-1].ID, true
		}
		return "", false
	}

	for _, adapter := range all {
		if strings.EqualFold(adapter.ID, input) || strings.EqualFold(adapter.Name, input) {
			return adapter.ID, true
		}
	}

	return "", false
}

// --- Manifest parsing ---

func loadAdapterManifestFile(path string) (*manifestpkg.Manifest, string, error) {
	content, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, "", err
	}
	manifest, err := manifestpkg.Parse(content)
	if err != nil {
		return nil, "", fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.Type != manifestpkg.PluginTypeAdapter {
		return nil, "", fmt.Errorf("manifest type must be %q", manifestpkg.PluginTypeAdapter)
	}
	if manifest.Adapter == nil {
		return nil, "", fmt.Errorf("adapter manifest missing spec")
	}
	return manifest, string(content), nil
}

func parseManifest(manifestRaw string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(manifestRaw)
	if trimmed == "" {
		return nil, nil
	}

	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed), nil
	}

	var yamlData interface{}
	if err := yaml.Unmarshal([]byte(trimmed), &yamlData); err != nil {
		return nil, fmt.Errorf("invalid YAML manifest: %w", err)
	}

	normalized := normalizeYAML(yamlData)
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

func normalizeYAML(value interface{}) interface{} {
	return normalizeYAMLWithDepth(value, 0, 1024)
}

func normalizeYAMLWithDepth(value interface{}, depth, maxDepth int) interface{} {
	if depth >= maxDepth {
		return fmt.Sprint(value)
	}

	switch v := value.(type) {
	case map[interface{}]interface{}:
		m := make(map[string]interface{}, len(v))
		for key, val := range v {
			keyStr := ""
			if ks, ok := key.(string); ok {
				keyStr = ks
			} else {
				keyStr = fmt.Sprint(key)
			}
			m[keyStr] = normalizeYAMLWithDepth(val, depth+1, maxDepth)
		}
		return m
	case map[string]interface{}:
		m := make(map[string]interface{}, len(v))
		for key, val := range v {
			m[key] = normalizeYAMLWithDepth(val, depth+1, maxDepth)
		}
		return m
	case []interface{}:
		result := make([]interface{}, len(v))
		for i, elem := range v {
			result[i] = normalizeYAMLWithDepth(elem, depth+1, maxDepth)
		}
		return result
	default:
		return v
	}
}

func parseKeyValuePairs(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for _, entry := range values {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid key=value pair: %s", entry)
		}
		key := strings.TrimSpace(parts[0])
		if key == "" {
			return nil, fmt.Errorf("invalid key in %s", entry)
		}
		out[key] = parts[1]
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// --- File utilities ---

func copyFile(src, dst string, perm fs.FileMode) error {
	cleanSrc := filepath.Clean(src)
	cleanDst := filepath.Clean(dst)

	absSrc, err := filepath.Abs(cleanSrc)
	if err != nil {
		return err
	}
	absDst, err := filepath.Abs(cleanDst)
	if err != nil {
		return err
	}

	if absSrc == absDst {
		return fmt.Errorf("source and destination are the same: %s", absSrc)
	}

	srcInfo, err := os.Lstat(absSrc)
	if err != nil {
		return err
	}
	if srcInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source is a symlink: %s", absSrc)
	}
	if !srcInfo.Mode().IsRegular() {
		return fmt.Errorf("source must be a regular file: %s", absSrc)
	}

	if _, err := os.Stat(absDst); err == nil {
		return fmt.Errorf("destination already exists: %s", absDst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(absDst), 0o755); err != nil {
		return err
	}

	srcFile, err := os.Open(absSrc)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(absDst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			dstFile.Close()
		}
	}()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}

	if err := dstFile.Chmod(perm); err != nil {
		return err
	}

	closed = true
	return dstFile.Close()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func cleanupBuildArtifacts(w io.Writer, distDir, outputPath string, distExisted, outputFileExisted bool) {
	if distExisted {
		if outputFileExisted {
			// Output file pre-existed — do not delete it (preserve pre-install state).
			return
		}
		if err := os.Remove(outputPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(w, "warning: failed to clean up build artifact %s: %v\n", outputPath, err)
		} else if err == nil {
			fmt.Fprintf(w, "cleaned up build artifacts: %s\n", outputPath)
		}
	} else {
		if err := os.RemoveAll(distDir); err != nil {
			fmt.Fprintf(w, "warning: failed to clean up build directory %s: %v\n", distDir, err)
		} else {
			fmt.Fprintf(w, "cleaned up build artifacts: %s\n", distDir)
		}
	}
}

func cleanupAdapterDir(w io.Writer, adapterHome, destPath string, adapterDirExisted, destFileExisted bool) {
	if adapterDirExisted {
		if destFileExisted {
			// Destination binary pre-existed — do not delete it (preserve pre-install state).
			return
		}
		if err := os.Remove(destPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(w, "warning: failed to clean up adapter binary %s: %v\n", destPath, err)
		} else if err == nil {
			fmt.Fprintf(w, "cleaned up adapter binary: %s\n", destPath)
		}
		// Remove bin/ directory if now empty (may have been created by this install attempt).
		_ = os.Remove(filepath.Dir(destPath))
	} else {
		if err := os.RemoveAll(adapterHome); err != nil {
			fmt.Fprintf(w, "warning: failed to clean up adapter directory %s: %v\n", adapterHome, err)
		} else {
			fmt.Fprintf(w, "cleaned up adapter directory: %s\n", adapterHome)
		}
	}
}

// --- Sanity check ---

type sanityCheckEntry struct {
	Name        string `json:"name"`
	Passed      bool   `json:"passed"`
	Message     string `json:"message,omitempty"`
	Remediation string `json:"remediation,omitempty"`
}

// containsPathSeparator is a cross-platform heuristic that reports whether s
// looks like a path (as opposed to a bare command name). It checks for both '/'
// and '\' unconditionally so that relative paths like "./bin/adapter" are
// correctly recognised on all platforms. On Unix '\' is technically a valid
// filename character, but in practice adapter paths never contain literal
// backslashes, so treating it as a path indicator is safe here.
func containsPathSeparator(s string) bool {
	return strings.ContainsAny(s, `/\`)
}

// normalizeCommand resolves a command string to an absolute path. Bare command
// names (no path separator) are resolved via exec.LookPath; relative paths with
// a separator are resolved via filepath.Abs. Already-absolute or empty commands
// are returned unchanged. If resolution fails, the original value is returned
// and a warning is logged to w (if non-nil) so the user knows the daemon will
// receive an unresolved command.
func normalizeCommand(command string, w io.Writer) string {
	if command == "" || filepath.IsAbs(command) {
		return command
	}
	if !containsPathSeparator(command) {
		// Bare command name — resolve via PATH.
		if p, err := exec.LookPath(command); err == nil {
			return p
		}
		if w != nil {
			fmt.Fprintf(w, "⚠ Could not resolve %q in PATH — registering as-is (daemon may have a different PATH)\n", command)
		}
	} else {
		// Relative path with separator — resolve to absolute.
		if p, err := filepath.Abs(command); err == nil {
			return p
		}
		if w != nil {
			fmt.Fprintf(w, "⚠ Could not resolve relative path %q to absolute — registering as-is\n", command)
		}
	}
	return command
}

// sanityCheckResult holds the outcome of a post-install binary sanity check.
// BinaryPath is always populated (even on failure) for diagnostic purposes.
type sanityCheckResult struct {
	Passed     bool               `json:"passed"`
	BinaryPath string             `json:"binary_path"`
	Checks     []sanityCheckEntry `json:"checks"`
}

func sanityCheck(binaryPath string) sanityCheckResult {
	if binaryPath == "" {
		return sanityCheckResult{
			Passed: false,
			Checks: []sanityCheckEntry{{
				Name:        "binary_exists",
				Passed:      false,
				Message:     "binary path is empty",
				Remediation: "no binary path provided — check --binary flag, manifest entrypoint.command, or build output",
			}},
		}
	}

	// binaryPath is expected to be already normalized to an absolute path by
	// normalizeCommand (called before registration). No additional LookPath
	// resolution is needed here.
	resolved := binaryPath

	result := sanityCheckResult{
		Passed:     true,
		BinaryPath: resolved,
	}

	// Check 1: Binary exists at expected path and is a regular file.
	// os.Stat intentionally follows symlinks — a symlink to a valid executable is fine
	// for process transport (exec will resolve it). copyFile uses Lstat to reject symlinks
	// only when physically copying the binary into the instance directory.
	info, err := os.Stat(resolved)
	if err != nil && runtime.GOOS == "windows" {
		// On Windows, exec resolves PATHEXT automatically, so a path like
		// "C:\...\adapter" works at runtime even if only "adapter.exe" exists.
		// Try appending known executable extensions before reporting failure.
		if alt, altInfo, ok := resolveWindowsPATHEXT(resolved); ok {
			resolved = alt
			result.BinaryPath = alt
			info = altInfo
			err = nil
		}
	}
	if err != nil {
		result.Passed = false
		entry := sanityCheckEntry{
			Name:   "binary_exists",
			Passed: false,
		}
		if errors.Is(err, os.ErrNotExist) {
			entry.Message = fmt.Sprintf("binary not found at %s", resolved)
			entry.Remediation = fmt.Sprintf("binary not found at %s — check --binary flag or build output", resolved)
		} else {
			entry.Message = fmt.Sprintf("cannot access binary at %s: %v", resolved, err)
			entry.Remediation = fmt.Sprintf("cannot access binary at %s: %v — check file permissions", resolved, err)
		}
		result.Checks = append(result.Checks, entry)
		return result // Cannot check further if binary is inaccessible.
	}
	if !info.Mode().IsRegular() {
		result.Passed = false
		result.Checks = append(result.Checks, sanityCheckEntry{
			Name:        "binary_exists",
			Passed:      false,
			Message:     fmt.Sprintf("path %s is not a regular file", resolved),
			Remediation: fmt.Sprintf("path %s is not a regular file — check --binary flag or build output", resolved),
		})
		return result // Cannot check executable if not a regular file.
	}
	result.Checks = append(result.Checks, sanityCheckEntry{
		Name:    "binary_exists",
		Passed:  true,
		Message: fmt.Sprintf("binary found at %s", resolved),
	})

	// Check 2: Binary has executable permission.
	if runtime.GOOS == "windows" {
		// On Windows, POSIX execute bits are meaningless — the OS determines
		// executability from file extension.
		entry := checkWindowsExecutableExtension(resolved)
		if !entry.Passed {
			result.Passed = false
		}
		result.Checks = append(result.Checks, entry)
	} else {
		if info.Mode().Perm()&0111 == 0 {
			result.Passed = false
			result.Checks = append(result.Checks, sanityCheckEntry{
				Name:        "binary_executable",
				Passed:      false,
				Message:     fmt.Sprintf("binary at %s is not executable", resolved),
				Remediation: fmt.Sprintf("binary at %s is not executable — run: chmod +x '%s'", resolved, strings.ReplaceAll(resolved, "'", `'\''`)),
			})
		} else {
			result.Checks = append(result.Checks, sanityCheckEntry{
				Name:    "binary_executable",
				Passed:  true,
				Message: "binary is executable",
			})
		}
	}

	return result
}

// windowsExecutableExtensions lists file extensions that Windows treats as
// directly executable. Shared by checkWindowsExecutableExtension and
// resolveWindowsPATHEXT to keep the recognised set in one place.
var windowsExecutableExtensions = []string{".exe", ".bat", ".cmd", ".com"}

// checkWindowsExecutableExtension checks whether a file has a known Windows
// executable extension. It returns a passing sanityCheckEntry when the extension
// is recognised or a failed entry when it is not. Extracted as a standalone
// helper so it can be unit-tested cross-platform.
func checkWindowsExecutableExtension(path string) sanityCheckEntry {
	ext := strings.ToLower(filepath.Ext(path))
	for _, winExt := range windowsExecutableExtensions {
		if ext == winExt {
			return sanityCheckEntry{
				Name:    "binary_executable",
				Passed:  true,
				Message: fmt.Sprintf("binary has executable extension %s", ext),
			}
		}
	}
	return sanityCheckEntry{
		Name:        "binary_executable",
		Passed:      false,
		Message:     fmt.Sprintf("binary at %s does not have a Windows executable extension", path),
		Remediation: fmt.Sprintf("binary at %s does not have a recognised Windows executable extension (%s)", path, strings.Join(windowsExecutableExtensions, ", ")),
	}
}

// resolveWindowsPATHEXT tries appending executable extensions to path and returns
// the first match that os.Stat succeeds on. It consults the PATHEXT environment
// variable first (standard Windows behaviour) and falls back to the hardcoded
// windowsExecutableExtensions list when PATHEXT is unset. This mirrors the
// resolution that os/exec performs at runtime, preventing false-negative sanity
// check failures for paths like "C:\...\adapter" when "adapter.exe" exists.
func resolveWindowsPATHEXT(path string) (string, os.FileInfo, bool) {
	// Only attempt resolution when the path has no extension — if it already
	// has one (e.g. ".sh"), the user explicitly chose it.
	if filepath.Ext(path) != "" {
		return "", nil, false
	}
	exts := windowsExecutableExtensions
	if pathext := os.Getenv("PATHEXT"); pathext != "" {
		parts := strings.Split(strings.ToLower(pathext), ";")
		exts = make([]string, 0, len(parts))
		for _, p := range parts {
			if s := strings.TrimSpace(p); s != "" {
				exts = append(exts, s)
			}
		}
	}
	for _, ext := range exts {
		if ext == "" {
			continue
		}
		candidate := path + ext
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, info, true
		}
	}
	return "", nil, false
}

// installLocalResult wraps registration output with optional sanity check for JSON mode.
type installLocalResult struct {
	Adapter     apihttp.AdapterDescriptor `json:"adapter"`
	SanityCheck *sanityCheckResult        `json:"sanity_check,omitempty"`
}

func printSanityCheckResult(w io.Writer, result sanityCheckResult) {
	if result.Passed {
		// Build human-readable summary from check names (underscore → space).
		// binary_exists is always true when the summary runs (prerequisite for
		// other checks), so it's excluded to avoid redundancy.
		var parts []string
		for _, c := range result.Checks {
			if c.Passed && c.Name != "binary_exists" {
				parts = append(parts, strings.ReplaceAll(c.Name, "_", " "))
			}
		}
		if len(parts) == 0 {
			fmt.Fprintln(w, "✓ Sanity check passed")
		} else {
			fmt.Fprintf(w, "✓ Sanity check passed: %s\n", strings.Join(parts, ", "))
		}
		return
	}
	fmt.Fprintln(w, "⚠ Sanity check failed:")
	for _, c := range result.Checks {
		if !c.Passed {
			fmt.Fprintf(w, "  %s\n", c.Remediation)
		}
	}
}
