package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/audioio"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/grpcclient"
	"github.com/nupi-ai/nupi/internal/mapper"
	"github.com/nupi-ai/nupi/internal/voice/slots"
	"github.com/spf13/cobra"
)

const maxAudioFileSize = 500 * 1024 * 1024 // 500 MB guardrail for local files

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

	stderr := out.errW
	go func() {
		select {
		case <-sigs:
			fmt.Fprintln(stderr, "\nInterrupt received, stopping voice stream...")
			cancel()
		case <-ctx.Done():
		}
	}()

	return withOutputClient(out, daemonConnectErrorMessage, func(gc *grpcclient.Client) error {
		caps, err := gc.AudioCapabilities(ctx, &apiv1.GetAudioCapabilitiesRequest{
			SessionId: sessionID,
		})
		if err != nil {
			return out.Error("Failed to fetch audio capabilities", err)
		}
		captureEnabled := hasCaptureEnabled(caps)
		playbackEnabled := hasPlaybackEnabled(caps)
		if !captureEnabled {
			return out.Error("Voice capture unavailable", errors.New("voice capture disabled"))
		}
		if !noPlayback && !playbackEnabled {
			return out.Error("Voice playback unavailable", errors.New("voice playback disabled"))
		}

		var playbackWG sync.WaitGroup
		var playbackErr error
		var playbackReceived bool

		if !noPlayback {
			targetStream := playbackStream
			if targetStream == "" {
				targetStream = slots.TTS
			}
			playbackSrv, err := gc.StreamAudioOut(ctx, &apiv1.StreamAudioOutRequest{
				SessionId: sessionID,
				StreamId:  targetStream,
			})
			if err != nil {
				cancel()
				return out.Error("Failed to subscribe to playback", err)
			}

			playbackWG.Add(1)
			go func() {
				defer playbackWG.Done()

				var wavWriter *audioio.Writer
				var writerErr error
				defer func() {
					if wavWriter != nil {
						_ = wavWriter.Close()
					}
				}()
				for {
					resp, err := playbackSrv.Recv()
					if err != nil {
						if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
							return
						}
						playbackErr = err
						return
					}

					chunk := resp.GetChunk()
					if chunk == nil {
						continue
					}

					if len(chunk.GetData()) > 0 {
						playbackReceived = true
					}

					chunkFormat := mapper.FromProtoAudioFormat(resp.GetFormat())

					if outputPath != "" && len(chunk.GetData()) > 0 {
						if wavWriter == nil {
							file, err := os.Create(outputPath)
							if err != nil {
								playbackErr = fmt.Errorf("open playback file: %w", err)
								return
							}
							format := chunkFormat
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
						if _, err := wavWriter.Write(chunk.GetData()); err != nil {
							playbackErr = fmt.Errorf("write playback chunk: %w", err)
							return
						}
					}

					if chunk.GetLast() {
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

		// Upload audio via gRPC client-streaming.
		counter := &countingReader{reader: input.reader}
		uploadErr := uploadAudioGRPC(ctx, gc, sessionID, streamID, input.format, meta, counter)

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

		payload := map[string]any{
			"session_id":     sessionID,
			"stream_id":      streamID,
			"bytes_uploaded": counter.n,
			"ingress_source": input.description,
		}

		stdout := out.w
		return out.Render(CommandResult{
			Data: payload,
			HumanReadable: func() error {
				fmt.Fprintf(stdout, "Uploaded %d bytes from %s to session %s/%s\n", counter.n, input.description, sessionID, streamID)
				if !noPlayback {
					if playbackReceived {
						fmt.Fprintln(stdout, "Received playback audio")
						if outputPath != "" {
							fmt.Fprintf(stdout, "Saved playback audio to %s\n", outputPath)
						}
					} else {
						fmt.Fprintln(stdout, "Playback stream completed with no audio data")
					}
				}
				return nil
			},
		})
	})
}

func voiceInterrupt(cmd *cobra.Command, defaultReason string) error {
	out := newOutputFormatter(cmd)

	sessionID := strings.TrimSpace(cmd.Flag("session").Value.String())
	if sessionID == "" {
		return out.Error("Session ID is required", nil)
	}
	streamID := strings.TrimSpace(cmd.Flag("stream").Value.String())
	reason := defaultReason
	if f := cmd.Flag("reason"); f != nil {
		if v := strings.TrimSpace(f.Value.String()); v != "" {
			reason = v
		}
	}
	if streamID == "" {
		streamID = slots.TTS
	}

	return withOutputClientTimeout(out, constants.Duration5Seconds, daemonConnectErrorMessage, func(ctx context.Context, gc *grpcclient.Client) (any, error) {
		if err := gc.InterruptTTS(ctx, &apiv1.InterruptTTSRequest{
			SessionId: sessionID,
			StreamId:  streamID,
			Reason:    reason,
		}); err != nil {
			return nil, clientCallFailed("Failed to send interruption", err)
		}

		return CommandResult{
			Data: map[string]any{
				"session_id": sessionID,
				"stream_id":  streamID,
				"reason":     reason,
			},
			HumanReadable: func() error {
				fmt.Fprintf(out.w, "Sent interruption (%s) to session %s\n", reason, sessionID)
				return nil
			},
		}, nil
	})
}

func voiceStatus(cmd *cobra.Command, _ []string) error {
	sessionID := strings.TrimSpace(cmd.Flag("session").Value.String())
	return withClientTimeout(cmd, constants.Duration5Seconds, func(ctx context.Context, gc *grpcclient.Client) (any, error) {
		caps, err := gc.AudioCapabilities(ctx, &apiv1.GetAudioCapabilitiesRequest{
			SessionId: sessionID,
		})
		if err != nil {
			return nil, clientCallFailed("Failed to fetch audio capabilities", err)
		}

		captureEnabled := hasCaptureEnabled(caps)
		playbackEnabled := hasPlaybackEnabled(caps)

		payload := map[string]any{
			"capture":          formatCapabilitiesProtoJSON(caps.GetCapture()),
			"playback":         formatCapabilitiesProtoJSON(caps.GetPlayback()),
			"capture_enabled":  captureEnabled,
			"playback_enabled": playbackEnabled,
		}

		stdout := cmd.OutOrStdout()
		return CommandResult{
			Data: payload,
			HumanReadable: func() error {
				fmt.Fprintf(stdout, "Capture enabled: %v\n", captureEnabled)
				fmt.Fprintln(stdout, "Capture capabilities:")
				if len(caps.GetCapture()) == 0 {
					fmt.Fprintln(stdout, "  (none)")
				} else {
					for _, cap := range caps.GetCapture() {
						fmt.Fprintf(stdout, "  - stream=%s %dHz %dbit %dch\n",
							cap.GetStreamId(),
							cap.GetFormat().GetSampleRate(),
							cap.GetFormat().GetBitDepth(),
							cap.GetFormat().GetChannels(),
						)
					}
				}

				fmt.Fprintf(stdout, "Playback enabled: %v\n", playbackEnabled)
				fmt.Fprintln(stdout, "Playback capabilities:")
				if len(caps.GetPlayback()) == 0 {
					fmt.Fprintln(stdout, "  (none)")
				} else {
					for _, cap := range caps.GetPlayback() {
						fmt.Fprintf(stdout, "  - stream=%s %dHz %dbit %dch\n",
							cap.GetStreamId(),
							cap.GetFormat().GetSampleRate(),
							cap.GetFormat().GetBitDepth(),
							cap.GetFormat().GetChannels(),
						)
					}
				}
				return nil
			},
		}, nil
	})
}

func formatCapabilitiesProtoJSON(caps []*apiv1.AudioCapability) []map[string]any {
	result := make([]map[string]any, 0, len(caps))
	for _, cap := range caps {
		entry := map[string]any{
			"stream_id": cap.GetStreamId(),
			"metadata":  cap.GetMetadata(),
		}
		if f := cap.GetFormat(); f != nil {
			entry["sample_rate"] = f.GetSampleRate()
			entry["channels"] = f.GetChannels()
			entry["bit_depth"] = f.GetBitDepth()
			entry["frame_ms"] = f.GetFrameDurationMs()
		}
		result = append(result, entry)
	}
	return result
}

// hasCaptureEnabled returns true if any capture capability has ready=true in metadata.
func hasCaptureEnabled(caps *apiv1.GetAudioCapabilitiesResponse) bool {
	for _, cap := range caps.GetCapture() {
		if cap.GetMetadata()["ready"] == "true" {
			return true
		}
	}
	return false
}

// hasPlaybackEnabled returns true if any playback capability has ready=true in metadata.
func hasPlaybackEnabled(caps *apiv1.GetAudioCapabilitiesResponse) bool {
	for _, cap := range caps.GetPlayback() {
		if cap.GetMetadata()["ready"] == "true" {
			return true
		}
	}
	return false
}

// uploadAudioGRPC streams PCM data to the daemon via gRPC client-streaming.
func uploadAudioGRPC(ctx context.Context, gc *grpcclient.Client, sessionID, streamID string, format eventbus.AudioFormat, metadata map[string]string, reader io.Reader) error {
	stream, err := gc.StreamAudioIn(ctx)
	if err != nil {
		return fmt.Errorf("open audio stream: %w", err)
	}

	buf := make([]byte, 4096)
	var seq uint64
	first := true

	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			req := &apiv1.StreamAudioInRequest{
				SessionId: sessionID,
				StreamId:  streamID,
				Chunk: &apiv1.AudioChunk{
					Data:     append([]byte(nil), buf[:n]...),
					Sequence: seq,
				},
			}
			if first {
				req.Format = mapper.ToProtoAudioFormat(format)
				req.Chunk.Metadata = metadata
				req.Chunk.First = true
				first = false
			}
			if readErr == io.EOF {
				req.Chunk.Last = true
			}
			if sendErr := stream.Send(req); sendErr != nil {
				return fmt.Errorf("send audio chunk: %w", sendErr)
			}
			seq++
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read audio: %w", readErr)
		}
	}

	if _, err := stream.CloseAndRecv(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("finalise audio stream: %w", err)
	}
	return nil
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
		// Bare os.Stdin is intentional — this reads raw binary audio data
		// from a pipe, not interactive CLI input. cmd.InOrStdin() is not
		// available here (helper function, no *cobra.Command parameter).
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

// capabilityReadyValue extracts the "ready" boolean from capability metadata.
func capabilityReadyValue(cap *apiv1.AudioCapability) bool {
	if cap == nil {
		return false
	}
	val, ok := cap.GetMetadata()["ready"]
	if !ok {
		return false
	}
	b, _ := strconv.ParseBool(val)
	return b
}
