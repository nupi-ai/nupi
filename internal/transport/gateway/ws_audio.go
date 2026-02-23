package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/server"
)

// wsAudioHandler serves WebSocket connections for audio streaming.
// It translates WebSocket frames into audio service calls, enabling mobile
// clients to capture and play back audio where gRPC bidi-streaming is unavailable.
type wsAudioHandler struct {
	apiServer   *server.APIServer
	shutdownCtx context.Context
}

type wsAudioControl struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
}

type wsAudioMeta struct {
	Type       string      `json:"type"`
	Sequence   uint64      `json:"sequence"`
	First      bool        `json:"first,omitempty"`
	Last       bool        `json:"last,omitempty"`
	DurationMs int64       `json:"duration_ms,omitempty"`
	Format     *wsAudioFmt `json:"format,omitempty"`
}

type wsAudioFmt struct {
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	BitDepth   int    `json:"bit_depth"`
}

type wsAudioCapsResponse struct {
	Type     string           `json:"type"`
	Capture  []wsAudioCapInfo `json:"capture"`
	Playback []wsAudioCapInfo `json:"playback"`
}

type wsAudioCapInfo struct {
	StreamID string      `json:"stream_id"`
	Format   *wsAudioFmt `json:"format"`
	Ready    bool        `json:"ready"`
}

func newWSAudioHandler(apiServer *server.APIServer, shutdownCtx context.Context) *wsAudioHandler {
	return &wsAudioHandler{apiServer: apiServer, shutdownCtx: shutdownCtx}
}

func (h *wsAudioHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimPrefix(r.URL.Path, "/ws/audio/")
	sessionID = strings.TrimSuffix(sessionID, "/")
	if sessionID == "" || strings.Contains(sessionID, "/") {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}

	if h.apiServer.AuthRequired() {
		token := r.URL.Query().Get("token")
		if token == "" {
			token = parseBearer(r.Header.Get("Authorization"))
		}
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if _, ok := h.apiServer.AuthenticateToken(token); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	if _, err := h.apiServer.SessionMgr().GetSession(sessionID); err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("[WS Audio] accept error for session %s: %v", sessionID, err)
		return
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(h.shutdownCtx)
	defer cancel()

	conn.SetReadLimit(256 * 1024) // 256 KB max incoming message

	egressStreamID := "tts"
	if egress := h.apiServer.AudioEgress(); egress != nil {
		egressStreamID = egress.DefaultStreamID()
	}

	clientID := fmt.Sprintf("ws_audio_%s", uuid.NewString())
	bus := h.apiServer.EventBus()
	if bus != nil {
		egressSub := eventbus.Subscribe[eventbus.AudioEgressPlaybackEvent](bus,
			eventbus.TopicAudioEgressPlayback,
			eventbus.WithSubscriptionName("ws_audio_egress_"+clientID),
			eventbus.WithSubscriptionBuffer(64),
		)
		defer egressSub.Close()
		go h.pumpEgress(ctx, cancel, conn, egressSub, sessionID, egressStreamID)
	}

	go h.pumpPing(ctx, cancel, conn, sessionID)

	// pumpInput blocks until connection closes; returns capture stream for cleanup.
	if cs := h.pumpInput(ctx, conn, sessionID, egressStreamID); cs != nil {
		cs.Close()
	}
}

// pumpEgress forwards audio playback events as binary frames + metadata text frames.
func (h *wsAudioHandler) pumpEgress(
	ctx context.Context,
	cancel context.CancelFunc,
	conn *websocket.Conn,
	sub *eventbus.TypedSubscription[eventbus.AudioEgressPlaybackEvent],
	sessionID, streamID string,
) {
	firstChunk := true
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			evt := env.Payload
			if evt.SessionID != sessionID || evt.StreamID != streamID {
				continue
			}

			if len(evt.Data) > 0 {
				if wErr := conn.Write(ctx, websocket.MessageBinary, evt.Data); wErr != nil {
					if !isExpectedWSClose(wErr) {
						log.Printf("[WS Audio] egress write error for session %s: %v", sessionID, wErr)
					}
					cancel()
					return
				}
			}

			// NOTE: A text frame from sendCapabilities (called by pumpInput) may
			// be interleaved between the binary audio frame above and the metadata
			// frame below. Clients must match messages by the "type" field rather
			// than assuming audio_meta immediately follows the binary frame.
			meta := wsAudioMeta{
				Type:     "audio_meta",
				Sequence: evt.Sequence,
				Last:     evt.Final,
				Format:   eventFormatToWS(evt.Format),
			}
			if firstChunk {
				meta.First = true
				firstChunk = false
			}
			if evt.Duration > 0 {
				meta.DurationMs = evt.Duration.Milliseconds()
			}
			data, mErr := json.Marshal(meta)
			if mErr != nil {
				log.Printf("[WS Audio] egress meta marshal error for session %s: %v", sessionID, mErr)
				continue
			}
			if wErr := conn.Write(ctx, websocket.MessageText, data); wErr != nil {
				if !isExpectedWSClose(wErr) {
					log.Printf("[WS Audio] egress meta write error for session %s: %v", sessionID, wErr)
				}
				cancel()
				return
			}

			if evt.Final {
				firstChunk = true
			}
		}
	}
}

// pumpInput reads WebSocket messages and dispatches audio input / control.
// Returns the capture stream (if any) for deferred cleanup.
func (h *wsAudioHandler) pumpInput(
	ctx context.Context,
	conn *websocket.Conn,
	sessionID, egressStreamID string,
) server.AudioCaptureStream {
	var captureStream server.AudioCaptureStream

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			if !isExpectedWSClose(err) {
				log.Printf("[WS Audio] read error for session %s: %v", sessionID, err)
			}
			return captureStream
		}

		switch typ {
		case websocket.MessageBinary:
			if len(data) == 0 {
				continue
			}
			if captureStream == nil {
				ingress := h.apiServer.AudioIngress()
				if ingress == nil {
					log.Printf("[WS Audio] audio ingress unavailable for session %s", sessionID)
					continue
				}
				s, oErr := ingress.OpenStream(sessionID, server.DefaultCaptureStreamID, server.DefaultCaptureFormat(), nil)
				if oErr != nil {
					log.Printf("[WS Audio] open capture stream for session %s: %v", sessionID, oErr)
					continue
				}
				captureStream = s
			}
			if wErr := captureStream.Write(data); wErr != nil {
				log.Printf("[WS Audio] capture write error for session %s: %v", sessionID, wErr)
				captureStream.Close()
				captureStream = nil
			}

		case websocket.MessageText:
			var ctrl wsAudioControl
			if jErr := json.Unmarshal(data, &ctrl); jErr != nil {
				log.Printf("[WS Audio] invalid control JSON for session %s: %v", sessionID, jErr)
				continue
			}
			switch ctrl.Type {
			case "interrupt":
				reason := ctrl.Reason
				if reason == "" {
					reason = "user_barge_in"
				}
				h.apiServer.PublishAudioInterrupt(sessionID, egressStreamID, reason, nil)
				if egress := h.apiServer.AudioEgress(); egress != nil {
					egress.Interrupt(sessionID, egressStreamID, reason, nil)
				}
			case "capabilities":
				h.sendCapabilities(ctx, conn, sessionID)
			case "close_stream":
				if captureStream != nil {
					captureStream.Close()
					captureStream = nil
				}
			case "detach":
				conn.Close(websocket.StatusNormalClosure, "detached")
				return captureStream
			default:
				if ctrl.Type != "" {
					log.Printf("[WS Audio] unknown control type %q for session %s", ctrl.Type, sessionID)
				}
			}
		}
	}
}

func (h *wsAudioHandler) sendCapabilities(ctx context.Context, conn *websocket.Conn, sessionID string) {
	resp := wsAudioCapsResponse{
		Type:     "capabilities_response",
		Capture:  []wsAudioCapInfo{},
		Playback: []wsAudioCapInfo{},
	}

	if ingress := h.apiServer.AudioIngress(); ingress != nil {
		resp.Capture = append(resp.Capture, wsAudioCapInfo{
			StreamID: server.DefaultCaptureStreamID,
			Format:   eventFormatToWS(server.DefaultCaptureFormat()),
			Ready:    true,
		})
	}

	if egress := h.apiServer.AudioEgress(); egress != nil {
		resp.Playback = append(resp.Playback, wsAudioCapInfo{
			StreamID: egress.DefaultStreamID(),
			Format:   eventFormatToWS(egress.PlaybackFormat()),
			Ready:    true,
		})
	}

	data, err := json.Marshal(resp)
	if err != nil {
		log.Printf("[WS Audio] capabilities marshal error for session %s: %v", sessionID, err)
		return
	}
	if wErr := conn.Write(ctx, websocket.MessageText, data); wErr != nil {
		if !isExpectedWSClose(wErr) {
			log.Printf("[WS Audio] capabilities write error for session %s: %v", sessionID, wErr)
		}
	}
}

// pumpPing sends periodic WebSocket pings to detect dead connections.
func (h *wsAudioHandler) pumpPing(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, sessionID string) {
	ticker := time.NewTicker(constants.Duration30Seconds)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, constants.Duration5Seconds)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				if !isExpectedWSClose(err) {
					log.Printf("[WS Audio] ping failed for session %s: %v", sessionID, err)
				}
				cancel()
				return
			}
		}
	}
}

func eventFormatToWS(f eventbus.AudioFormat) *wsAudioFmt {
	return &wsAudioFmt{
		Encoding:   string(f.Encoding),
		SampleRate: f.SampleRate,
		Channels:   f.Channels,
		BitDepth:   f.BitDepth,
	}
}
