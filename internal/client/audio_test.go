package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/voice/slots"
)

func TestUploadAudio(t *testing.T) {
	t.Parallel()

	var (
		receivedBytes []byte
		receivedMeta  string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/audio/ingress" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("session_id"); got != "sess-1" {
			t.Fatalf("unexpected session_id: %s", got)
		}
		if got := r.URL.Query().Get("stream_id"); got != "mic" {
			t.Fatalf("unexpected stream_id: %s", got)
		}
		if got := r.URL.Query().Get("sample_rate"); got != "16000" {
			t.Fatalf("unexpected sample_rate: %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("unexpected auth header: %s", got)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		receivedBytes = body
		receivedMeta = r.URL.Query().Get("metadata")

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := newClientWithConfig(server.URL, nil, "token")
	err := client.UploadAudio(context.Background(), AudioUploadParams{
		SessionID: "sess-1",
		StreamID:  "mic",
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
		Metadata: map[string]string{"foo": "bar"},
		Reader:   strings.NewReader("pcm"),
	})
	if err != nil {
		t.Fatalf("upload audio: %v", err)
	}

	if string(receivedBytes) != "pcm" {
		t.Fatalf("unexpected payload: %q", receivedBytes)
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(receivedMeta), &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta["foo"] != "bar" {
		t.Fatalf("metadata mismatch: %v", meta)
	}
}

func TestOpenAudioPlayback(t *testing.T) {
	t.Parallel()

	payloads := []map[string]any{
		{
			"format": map[string]any{
				"encoding":          "pcm_s16le",
				"sample_rate":       16000,
				"channels":          1,
				"bit_depth":         16,
				"frame_duration_ms": 20,
			},
			"sequence":    1,
			"duration_ms": 20,
			"data":        base64.StdEncoding.EncodeToString([]byte{0x01, 0x02}),
		},
		{
			"sequence":    2,
			"duration_ms": 20,
			"data":        base64.StdEncoding.EncodeToString([]byte{0x03, 0x04}),
			"final":       true,
		},
	}

	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/audio/egress" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("response writer not flushable")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		once.Do(func() {
			for _, payload := range payloads {
				if err := json.NewEncoder(w).Encode(payload); err != nil {
					t.Fatalf("encode payload: %v", err)
				}
				flusher.Flush()
				time.Sleep(10 * time.Millisecond)
			}
		})
	}))
	defer server.Close()

	client := newClientWithConfig(server.URL, nil, "")
	stream, err := client.OpenAudioPlayback(context.Background(), AudioPlaybackParams{
		SessionID: "sess-1",
		StreamID:  slots.TTS,
	})
	if err != nil {
		t.Fatalf("open playback: %v", err)
	}
	defer stream.Close()

	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv first chunk: %v", err)
	}
	if first.Sequence != 1 {
		t.Fatalf("unexpected sequence: %d", first.Sequence)
	}
	if len(first.Data) != 2 || first.Data[0] != 0x01 {
		t.Fatalf("unexpected data: %v", first.Data)
	}
	if first.Format.SampleRate != 16000 || first.Format.BitDepth != 16 {
		t.Fatalf("unexpected format: %+v", first.Format)
	}

	second, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv second chunk: %v", err)
	}
	if !second.Final {
		t.Fatalf("expected final chunk")
	}

	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after final chunk, got %v", err)
	}
}

func TestInterruptAudio(t *testing.T) {
	t.Parallel()

	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/audio/interrupt" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("unexpected auth header: %s", got)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		body = data
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := newClientWithConfig(server.URL, nil, "token")
	err := client.InterruptAudio(context.Background(), AudioInterruptParams{
		SessionID: "sess-1",
		StreamID:  slots.TTS,
		Reason:    "test",
		Metadata:  map[string]string{"foo": "bar"},
	})
	if err != nil {
		t.Fatalf("interrupt audio: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["session_id"] != "sess-1" || payload["stream_id"] != slots.TTS || payload["reason"] != "test" {
		t.Fatalf("unexpected payload: %v", payload)
	}
	meta := payload["metadata"].(map[string]any)
	if meta["foo"] != "bar" {
		t.Fatalf("metadata mismatch: %v", meta)
	}
}
