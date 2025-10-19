package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// AudioUploadParams encapsulates arguments required to push audio to the daemon.
type AudioUploadParams struct {
	SessionID string
	StreamID  string
	Format    eventbus.AudioFormat
	Metadata  map[string]string
	Reader    io.Reader
}

// UploadAudio streams PCM data to the daemon's audio ingress endpoint.
func (c *Client) UploadAudio(ctx context.Context, params AudioUploadParams) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if params.Reader == nil {
		return errors.New("audio upload: reader nil")
	}
	sessionID := strings.TrimSpace(params.SessionID)
	if sessionID == "" {
		return errors.New("audio upload: session_id required")
	}
	streamID := strings.TrimSpace(params.StreamID)
	if streamID == "" {
		streamID = "mic"
	}

	values := url.Values{}
	values.Set("session_id", sessionID)
	values.Set("stream_id", streamID)

	format := params.Format
	if format.SampleRate > 0 {
		values.Set("sample_rate", strconv.Itoa(format.SampleRate))
	}
	if format.Channels > 0 {
		values.Set("channels", strconv.Itoa(format.Channels))
	}
	if format.BitDepth > 0 {
		values.Set("bit_depth", strconv.Itoa(format.BitDepth))
	}
	if format.FrameDuration > 0 {
		frameMS := int((format.FrameDuration + time.Millisecond/2) / time.Millisecond)
		if frameMS > 0 {
			values.Set("frame_ms", strconv.Itoa(frameMS))
		}
	}
	if enc := strings.TrimSpace(string(format.Encoding)); enc != "" {
		values.Set("encoding", enc)
	}
	if len(params.Metadata) > 0 {
		payload, err := json.Marshal(params.Metadata)
		if err != nil {
			return fmt.Errorf("audio upload: encode metadata: %w", err)
		}
		values.Set("metadata", string(payload))
	}

	endpoint := fmt.Sprintf("%s/audio/ingress?%s", c.baseURL, values.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, params.Reader)
	if err != nil {
		return fmt.Errorf("audio upload: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	c.addAuth(req)

	resp, err := c.streamingHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("audio upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("audio upload: %w", readAPIError(resp))
	}
	return nil
}

// AudioPlaybackParams defines filters for playback subscription.
type AudioPlaybackParams struct {
	SessionID string
	StreamID  string
}

type audioPlaybackPayload struct {
	Format     *audioFormatPayload `json:"format,omitempty"`
	Sequence   uint64              `json:"sequence,omitempty"`
	DurationMs uint32              `json:"duration_ms,omitempty"`
	Final      bool                `json:"final,omitempty"`
	Error      string              `json:"error,omitempty"`
	Metadata   map[string]string   `json:"metadata,omitempty"`
	Data       string              `json:"data,omitempty"`
}

type audioFormatPayload struct {
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	BitDepth   int    `json:"bit_depth"`
	FrameMs    uint32 `json:"frame_duration_ms"`
}

// AudioPlaybackChunk represents a single playback frame from the daemon.
type AudioPlaybackChunk struct {
	Sequence uint64
	Format   eventbus.AudioFormat
	Data     []byte
	Duration time.Duration
	Final    bool
	Metadata map[string]string
}

// AudioPlaybackStream decodes progressive JSON payloads from the daemon.
type AudioPlaybackStream struct {
	resp      *http.Response
	decoder   *json.Decoder
	ctx       context.Context
	lastFmt   eventbus.AudioFormat
	formatSet bool
}

// OpenAudioPlayback subscribes to outgoing audio chunks over HTTP streaming.
func (c *Client) OpenAudioPlayback(ctx context.Context, params AudioPlaybackParams) (*AudioPlaybackStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sessionID := strings.TrimSpace(params.SessionID)
	if sessionID == "" {
		return nil, errors.New("audio playback: session_id required")
	}
	streamID := strings.TrimSpace(params.StreamID)
	if streamID == "" {
		streamID = "tts.primary"
	}

	values := url.Values{}
	values.Set("session_id", sessionID)
	values.Set("stream_id", streamID)

	endpoint := fmt.Sprintf("%s/audio/egress?%s", c.baseURL, values.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("audio playback: %w", err)
	}
	c.addAuth(req)

	resp, err := c.streamingHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("audio playback: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, fmt.Errorf("audio playback: %w", readAPIError(resp))
	}

	return &AudioPlaybackStream{
		resp:    resp,
		decoder: json.NewDecoder(resp.Body),
		ctx:     ctx,
	}, nil
}

// Recv retrieves the next playback chunk, returning io.EOF when the stream ends.
func (s *AudioPlaybackStream) Recv() (*AudioPlaybackChunk, error) {
	if s == nil || s.decoder == nil {
		return nil, io.EOF
	}
	if s.ctx != nil {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
	}

	var payload audioPlaybackPayload
	if err := s.decoder.Decode(&payload); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("audio playback decode: %w", err)
	}
	if strings.TrimSpace(payload.Error) != "" {
		return nil, errors.New(payload.Error)
	}
	if payload.Format != nil {
		s.lastFmt = eventbus.AudioFormat{
			Encoding:      eventbus.AudioEncoding(payload.Format.Encoding),
			SampleRate:    payload.Format.SampleRate,
			Channels:      payload.Format.Channels,
			BitDepth:      payload.Format.BitDepth,
			FrameDuration: time.Duration(payload.Format.FrameMs) * time.Millisecond,
		}
		s.formatSet = true
	}

	data, err := base64.StdEncoding.DecodeString(payload.Data)
	if err != nil {
		return nil, fmt.Errorf("audio playback: decode data: %w", err)
	}

	format := s.lastFmt
	if !s.formatSet {
		format = eventbus.AudioFormat{}
	}
	chunk := &AudioPlaybackChunk{
		Sequence: payload.Sequence,
		Format:   format,
		Data:     data,
		Duration: time.Duration(payload.DurationMs) * time.Millisecond,
		Final:    payload.Final,
		Metadata: payload.Metadata,
	}
	return chunk, nil
}

// Close releases the underlying HTTP response body.
func (s *AudioPlaybackStream) Close() error {
	if s == nil || s.resp == nil {
		return nil
	}
	err := s.resp.Body.Close()
	s.resp = nil
	s.decoder = nil
	return err
}

// AudioInterruptParams configures a manual TTS interruption request.
type AudioInterruptParams struct {
	SessionID string
	StreamID  string
	Reason    string
	Metadata  map[string]string
}

type audioInterruptRequest struct {
	SessionID string            `json:"session_id"`
	StreamID  string            `json:"stream_id,omitempty"`
	Reason    string            `json:"reason,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// InterruptAudio issues a manual interruption against the voice pipeline.
func (c *Client) InterruptAudio(ctx context.Context, params AudioInterruptParams) error {
	if ctx == nil {
		ctx = context.Background()
	}
	sessionID := strings.TrimSpace(params.SessionID)
	if sessionID == "" {
		return errors.New("audio interrupt: session_id required")
	}
	streamID := strings.TrimSpace(params.StreamID)

	payload := audioInterruptRequest{
		SessionID: sessionID,
		StreamID:  streamID,
		Reason:    strings.TrimSpace(params.Reason),
	}
	if len(params.Metadata) > 0 {
		payload.Metadata = params.Metadata
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("audio interrupt: encode payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/audio/interrupt", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("audio interrupt: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.addAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("audio interrupt: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("audio interrupt: %w", readAPIError(resp))
	}
	return nil
}
