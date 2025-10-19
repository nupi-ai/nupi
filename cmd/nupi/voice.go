package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nupi-ai/nupi/internal/audioio"
	"github.com/nupi-ai/nupi/internal/client"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/voice/slots"
	"github.com/spf13/cobra"
)

const maxAudioFileSize = 500 * 1024 * 1024 // 500 MB guardrail for local files

// diagnosticMessages collects user-facing messages for diagnostics, matching slot names
// case-insensitively because CLI requests may use user-supplied casing.
func diagnosticMessages(diags []client.VoiceDiagnostic, slot string) []string {
	if len(diags) == 0 {
		return nil
	}
	var messages []string
	for _, diag := range diags {
		if slot != "" && !strings.EqualFold(diag.Slot, slot) {
			continue
		}
		if msg := strings.TrimSpace(diag.Message); msg != "" {
			messages = append(messages, msg)
		}
	}
	return messages
}

func diagnosticSummary(diags []client.VoiceDiagnostic, slot, fallback string) string {
	msgs := diagnosticMessages(diags, slot)
	if len(msgs) == 0 {
		msgs = diagnosticMessages(diags, "")
	}
	if len(msgs) == 0 {
		return fallback
	}
	return strings.Join(msgs, "; ")
}

func newVoiceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "voice",
		Short:         "Voice streaming helpers",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	start := &cobra.Command{
		Use:           "start",
		Short:         "Stream audio into a session",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          voiceStart,
	}
	start.Flags().String("session", "", "Session identifier (required)")
	start.Flags().String("stream", "mic", "Audio stream ID used for ingress")
	start.Flags().String("input", "", "Audio source (WAV file path or '-' for STDIN PCM)")
	start.Flags().Int("sample-rate", 16000, "Sample rate for raw PCM input (ignored for WAV)")
	start.Flags().Int("channels", 1, "Channel count for raw PCM input (ignored for WAV)")
	start.Flags().Int("bit-depth", 16, "Bit depth for raw PCM input (ignored for WAV)")
	start.Flags().Int("frame-ms", 20, "Optional frame duration hint (milliseconds)")
	start.Flags().StringSlice("metadata", nil, "Additional metadata key=value to attach to ingress")
	start.Flags().String("playback-stream", "", "Override playback stream ID (default uses TTS primary)")
	start.Flags().String("output", "", "Write playback audio to the specified WAV file")
	start.Flags().Bool("no-playback", false, "Skip subscribing to playback audio")

	stop := &cobra.Command{
		Use:           "stop",
		Short:         "Stop active voice playback for a session",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return voiceInterrupt(cmd, "manual_stop")
		},
	}
	stop.Flags().String("session", "", "Session identifier (required)")
	stop.Flags().String("stream", "", "Playback stream ID (defaults to primary TTS)")
	stop.Flags().StringSlice("metadata", nil, "Optional metadata key=value pairs")

	interrupt := &cobra.Command{
		Use:           "interrupt",
		Short:         "Send a manual barge-in request",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			reason, _ := cmd.Flags().GetString("reason")
			return voiceInterrupt(cmd, reason)
		},
	}
	interrupt.Flags().String("session", "", "Session identifier (required)")
	interrupt.Flags().String("stream", "", "Playback stream ID (defaults to primary TTS)")
	interrupt.Flags().String("reason", "client_request", "Reason reported with the interruption")
	interrupt.Flags().StringSlice("metadata", nil, "Optional metadata key=value pairs")

	status := &cobra.Command{
		Use:           "status",
		Short:         "Show audio capture/playback capabilities",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          voiceStatus,
	}
	status.Flags().String("session", "", "Session identifier (optional)")

	cmd.AddCommand(start, stop, interrupt, status)
	return cmd
}

func voiceStart(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)

	sessionID := strings.TrimSpace(cmd.Flag("session").Value.String())
	if sessionID == "" {
		return out.Error("Session ID is required", nil)
	}
	streamID := strings.TrimSpace(cmd.Flag("stream").Value.String())
	inputPath := cmd.Flag("input").Value.String()
	sampleRate, _ := cmd.Flags().GetInt("sample-rate")
	channels, _ := cmd.Flags().GetInt("channels")
	bitDepth, _ := cmd.Flags().GetInt("bit-depth")
	frameMS, _ := cmd.Flags().GetInt("frame-ms")
	metadataEntries, _ := cmd.Flags().GetStringSlice("metadata")
	playbackStream := strings.TrimSpace(cmd.Flag("playback-stream").Value.String())
	outputPath := strings.TrimSpace(cmd.Flag("output").Value.String())
	noPlayback, _ := cmd.Flags().GetBool("no-playback")

	meta, err := parseMetadata(metadataEntries)
	if err != nil {
		return out.Error("Invalid metadata", err)
	}
	if meta == nil {
		meta = make(map[string]string)
	}
	if _, ok := meta["client"]; !ok {
		meta["client"] = "cli"
	}

	input, err := prepareAudioInput(inputPath, sampleRate, channels, bitDepth, frameMS)
	if err != nil {
		return out.Error("Failed to prepare audio input", err)
	}
	if input.close != nil {
		defer input.close()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	go func() {
		select {
		case <-sigs:
			fmt.Fprintln(os.Stderr, "\nInterrupt received, stopping voice stream...")
			cancel()
		case <-ctx.Done():
		}
	}()

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	caps, err := c.AudioCapabilities(ctx, sessionID)
	if err != nil {
		return out.Error("Failed to fetch audio capabilities", err)
	}
	if !caps.CaptureEnabled {
		message := diagnosticSummary(caps.Diagnostics, slots.STTPrimary, "voice capture disabled")
		return out.Error("Voice capture unavailable", errors.New(message))
	}
	if !noPlayback && !caps.PlaybackEnabled {
		message := diagnosticSummary(caps.Diagnostics, slots.TTSPrimary, "voice playback disabled")
		return out.Error("Voice playback unavailable", errors.New(message))
	}

	var playbackWG sync.WaitGroup
	var playbackErr error
	var playbackBytes int64
	var playbackChunks int

	if !noPlayback {
		targetStream := playbackStream
		if targetStream == "" {
			targetStream = slots.TTSPrimary
		}
		playback, err := c.OpenAudioPlayback(ctx, client.AudioPlaybackParams{
			SessionID: sessionID,
			StreamID:  targetStream,
		})
		if err != nil {
			cancel()
			return out.Error("Failed to subscribe to playback", err)
		}

		playbackWG.Add(1)
		go func() {
			defer playbackWG.Done()
			defer playback.Close()

			var wavWriter *audioio.Writer
			var writerErr error
			defer func() {
				if wavWriter != nil {
					_ = wavWriter.Close()
				}
			}()
			for {
				chunk, err := playback.Recv()
				if err != nil {
					if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
						return
					}
					playbackErr = err
					return
				}

				playbackChunks++
				playbackBytes += int64(len(chunk.Data))

				if outputPath != "" && len(chunk.Data) > 0 {
					if wavWriter == nil {
						file, err := os.Create(outputPath)
						if err != nil {
							playbackErr = fmt.Errorf("open playback file: %w", err)
							return
						}
						format := chunk.Format
						if format.SampleRate == 0 {
							format.SampleRate = input.format.SampleRate
						}
						if format.Channels == 0 {
							format.Channels = input.format.Channels
						}
						if format.BitDepth == 0 {
							format.BitDepth = input.format.BitDepth
						}
						wavWriter, err = audioio.NewWriter(file, format)
						if err != nil {
							_ = file.Close()
							playbackErr = fmt.Errorf("initialise playback WAV writer: %w", err)
							return
						}
					}
					if _, err := wavWriter.Write(chunk.Data); err != nil {
						playbackErr = fmt.Errorf("write playback chunk: %w", err)
						return
					}
				}

				if chunk.Final {
					if wavWriter != nil {
						if err := wavWriter.Close(); err != nil {
							writerErr = fmt.Errorf("finalise playback WAV: %w", err)
						}
					}
					if writerErr != nil && playbackErr == nil {
						playbackErr = writerErr
					}
					return
				}
			}
		}()
	}

	counter := &countingReader{reader: input.reader}
	uploadErr := c.UploadAudio(ctx, client.AudioUploadParams{
		SessionID: sessionID,
		StreamID:  streamID,
		Format:    input.format,
		Metadata:  meta,
		Reader:    counter,
	})

	if !noPlayback {
		playbackWG.Wait()
	}

	var errs []error
	if uploadErr != nil {
		label := "upload failed"
		if errors.Is(uploadErr, context.Canceled) {
			label = "upload cancelled"
		}
		errs = append(errs, fmt.Errorf("%s: %w", label, uploadErr))
	}
	if playbackErr != nil {
		errs = append(errs, fmt.Errorf("playback failed: %w", playbackErr))
	}
	if len(errs) > 0 {
		return out.Error("Voice streaming failed", errors.Join(errs...))
	}

	if out.jsonMode {
		payload := map[string]any{
			"session_id":      sessionID,
			"stream_id":       streamID,
			"bytes_uploaded":  counter.n,
			"ingress_source":  input.description,
			"playback_chunks": playbackChunks,
			"playback_bytes":  playbackBytes,
		}
		return out.Print(payload)
	}

	fmt.Fprintf(os.Stdout, "Uploaded %d bytes from %s to session %s/%s\n", counter.n, input.description, sessionID, streamID)
	if !noPlayback {
		if playbackBytes > 0 {
			fmt.Fprintf(os.Stdout, "Received %d playback chunks (%d bytes)\n", playbackChunks, playbackBytes)
			if outputPath != "" {
				fmt.Fprintf(os.Stdout, "Saved playback audio to %s\n", outputPath)
			}
		} else {
			fmt.Fprintln(os.Stdout, "Playback stream completed with no audio data")
		}
	}
	return nil
}

func voiceInterrupt(cmd *cobra.Command, defaultReason string) error {
	out := newOutputFormatter(cmd)

	sessionID := strings.TrimSpace(cmd.Flag("session").Value.String())
	if sessionID == "" {
		return out.Error("Session ID is required", nil)
	}
	streamID := strings.TrimSpace(cmd.Flag("stream").Value.String())
	reason := strings.TrimSpace(cmd.Flag("reason").Value.String())
	if reason == "" {
		reason = defaultReason
	}
	if streamID == "" {
		streamID = slots.TTSPrimary
	}
	metadataEntries, _ := cmd.Flags().GetStringSlice("metadata")
	meta, err := parseMetadata(metadataEntries)
	if err != nil {
		return out.Error("Invalid metadata", err)
	}

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	caps, err := c.AudioCapabilities(context.Background(), sessionID)
	if err != nil {
		return out.Error("Failed to fetch audio capabilities", err)
	}
	if !caps.PlaybackEnabled {
		message := diagnosticSummary(caps.Diagnostics, slots.TTSPrimary, "voice playback disabled")
		return out.Error("Voice playback unavailable", errors.New(message))
	}

	err = c.InterruptAudio(context.Background(), client.AudioInterruptParams{
		SessionID: sessionID,
		StreamID:  streamID,
		Reason:    reason,
		Metadata:  meta,
	})
	if err != nil {
		return out.Error("Failed to send interruption", err)
	}

	if out.jsonMode {
		return out.Print(map[string]any{
			"session_id": sessionID,
			"stream_id":  streamID,
			"reason":     reason,
		})
	}

	fmt.Fprintf(os.Stdout, "Sent interruption (%s) to session %s\n", reason, sessionID)
	return nil
}

func voiceStatus(cmd *cobra.Command, _ []string) error {
	out := newOutputFormatter(cmd)
	sessionID := strings.TrimSpace(cmd.Flag("session").Value.String())

	c, err := client.New()
	if err != nil {
		return out.Error("Failed to connect to daemon", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	caps, err := c.AudioCapabilities(ctx, sessionID)
	if err != nil {
		return out.Error("Failed to fetch audio capabilities", err)
	}

	if out.jsonMode {
		payload := map[string]any{
			"capture":          formatCapabilitiesJSON(caps.Capture),
			"playback":         formatCapabilitiesJSON(caps.Playback),
			"capture_enabled":  caps.CaptureEnabled,
			"playback_enabled": caps.PlaybackEnabled,
		}
		if len(caps.Diagnostics) > 0 {
			payload["diagnostics"] = caps.Diagnostics
		}
		return out.Print(payload)
	}

	fmt.Printf("Capture enabled: %v\n", caps.CaptureEnabled)
	fmt.Println("Capture capabilities:")
	if len(caps.Capture) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, cap := range caps.Capture {
			fmt.Printf("  - stream=%s %dHz %dbit %dch\n",
				cap.StreamID,
				cap.Format.SampleRate,
				cap.Format.BitDepth,
				cap.Format.Channels,
			)
		}
	}

	fmt.Printf("Playback enabled: %v\n", caps.PlaybackEnabled)
	fmt.Println("Playback capabilities:")
	if len(caps.Playback) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, cap := range caps.Playback {
			fmt.Printf("  - stream=%s %dHz %dbit %dch\n",
				cap.StreamID,
				cap.Format.SampleRate,
				cap.Format.BitDepth,
				cap.Format.Channels,
			)
		}
	}

	if len(caps.Diagnostics) > 0 {
		fmt.Println("Diagnostics:")
		for _, diag := range caps.Diagnostics {
			fmt.Printf("  - [%s] %s\n", diag.Slot, diag.Message)
		}
	}
	return nil
}

func formatCapabilitiesJSON(caps []client.AudioCapabilityInfo) []map[string]any {
	result := make([]map[string]any, 0, len(caps))
	for _, cap := range caps {
		result = append(result, map[string]any{
			"stream_id":   cap.StreamID,
			"sample_rate": cap.Format.SampleRate,
			"channels":    cap.Format.Channels,
			"bit_depth":   cap.Format.BitDepth,
			"frame_ms":    cap.Format.FrameDurationMs,
			"metadata":    cap.Metadata,
		})
	}
	return result
}

type audioInput struct {
	reader      io.Reader
	close       func() error
	format      eventbus.AudioFormat
	description string
}

func prepareAudioInput(path string, sampleRate, channels, bitDepth, frameMS int) (audioInput, error) {
	format := eventbus.AudioFormat{
		Encoding:   eventbus.AudioEncodingPCM16,
		SampleRate: sampleRate,
		Channels:   channels,
		BitDepth:   bitDepth,
	}
	if frameMS > 0 {
		format.FrameDuration = time.Duration(frameMS) * time.Millisecond
	}

	if path == "" || path == "-" {
		if sampleRate <= 0 || channels <= 0 || bitDepth <= 0 {
			return audioInput{}, errors.New("raw input requires --sample-rate, --channels and --bit-depth")
		}
		return audioInput{
			reader:      os.Stdin,
			format:      format,
			description: "STDIN",
		}, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return audioInput{}, err
	}
	stat, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return audioInput{}, err
	}
	if stat.Size() > maxAudioFileSize {
		_ = file.Close()
		return audioInput{}, fmt.Errorf("audio file too large: %d bytes (max %d)", stat.Size(), maxAudioFileSize)
	}

	if strings.EqualFold(filepath.Ext(path), ".wav") {
		wavReader, err := audioio.NewReader(file)
		if err != nil {
			_ = file.Close()
			return audioInput{}, err
		}
		format = wavReader.Format()
		if frameMS > 0 {
			format.FrameDuration = time.Duration(frameMS) * time.Millisecond
		}
		return audioInput{
			reader:      wavReader,
			close:       wavReader.Close,
			format:      format,
			description: path,
		}, nil
	}

	if sampleRate <= 0 || channels <= 0 || bitDepth <= 0 {
		_ = file.Close()
		return audioInput{}, errors.New("raw input requires --sample-rate, --channels and --bit-depth")
	}

	return audioInput{
		reader: file,
		close:  file.Close,
		format: format,
		description: func() string {
			if abs, err := filepath.Abs(path); err == nil {
				return abs
			}
			return path
		}(),
	}, nil
}

func parseMetadata(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	meta := make(map[string]string, len(entries))
	for _, entry := range entries {
		pair := strings.SplitN(entry, "=", 2)
		if len(pair) != 2 {
			return nil, fmt.Errorf("invalid metadata entry: %s", entry)
		}
		key := strings.TrimSpace(pair[0])
		value := strings.TrimSpace(pair[1])
		if key == "" {
			return nil, fmt.Errorf("metadata key empty: %s", entry)
		}
		meta[key] = value
	}
	return meta, nil
}

type countingReader struct {
	reader io.Reader
	n      int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.reader.Read(p)
	cr.n += int64(n)
	return n, err
}
