package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/server"
	"google.golang.org/grpc"
)

// ---------------------------------------------------------------------------
// Audio mocks
// ---------------------------------------------------------------------------

type mockAudioCaptureStream struct {
	dataCh chan []byte
	closed atomic.Bool
}

func (s *mockAudioCaptureStream) Write(data []byte) error {
	if s.closed.Load() {
		return fmt.Errorf("stream closed")
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case s.dataCh <- cp:
	default:
		return fmt.Errorf("mock capture stream buffer full")
	}
	return nil
}

func (s *mockAudioCaptureStream) Close() error {
	s.closed.Store(true)
	return nil
}

type mockAudioCaptureProvider struct {
	mu      sync.Mutex
	streams []*mockAudioCaptureStream
}

func (m *mockAudioCaptureProvider) OpenStream(sessionID, streamID string, format eventbus.AudioFormat, metadata map[string]string) (server.AudioCaptureStream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stream := &mockAudioCaptureStream{dataCh: make(chan []byte, 64)}
	m.streams = append(m.streams, stream)
	return stream, nil
}

func (m *mockAudioCaptureProvider) latestStream() *mockAudioCaptureStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.streams) == 0 {
		return nil
	}
	return m.streams[len(m.streams)-1]
}

func (m *mockAudioCaptureProvider) streamCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.streams)
}

type mockInterruptCall struct {
	SessionID string
	StreamID  string
	Reason    string
}

type mockAudioPlaybackController struct {
	mu             sync.Mutex
	interruptCalls []mockInterruptCall
	defaultStream  string
	format         eventbus.AudioFormat
}

func (m *mockAudioPlaybackController) DefaultStreamID() string {
	if m.defaultStream != "" {
		return m.defaultStream
	}
	return "tts"
}

func (m *mockAudioPlaybackController) PlaybackFormat() eventbus.AudioFormat {
	return m.format
}

func (m *mockAudioPlaybackController) Interrupt(sessionID, streamID, reason string, metadata map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.interruptCalls = append(m.interruptCalls, mockInterruptCall{sessionID, streamID, reason})
}

func (m *mockAudioPlaybackController) getInterruptCalls() []mockInterruptCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]mockInterruptCall, len(m.interruptCalls))
	copy(cp, m.interruptCalls)
	return cp
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func startWSAudioTestGateway(t *testing.T) (*server.APIServer, *eventbus.Bus, string) {
	t.Helper()

	apiServer, _ := newGatewayTestAPIServer(t)
	bus := eventbus.New()
	t.Cleanup(bus.Shutdown)
	apiServer.SetEventBus(bus)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	gw := New(apiServer, Options{
		ConnectEnabled: true,
		RegisterGRPC:   func(srv *grpc.Server) { registerTestServices(apiServer, srv) },
	})

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	t.Cleanup(func() { gw.Shutdown(context.Background()) })

	baseURL := fmt.Sprintf("ws://%s", info.Connect.Address)
	return apiServer, bus, baseURL
}

// waitForAudioBridgeReady sends a capabilities request and waits for the
// response, ensuring the handler has entered pumpInput and subscriptions are up.
func waitForAudioBridgeReady(t *testing.T, ctx context.Context, conn *websocket.Conn) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"capabilities"}`)); err != nil {
		t.Fatalf("failed to send capabilities probe: %v", err)
	}
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	for {
		typ, data, err := conn.Read(readCtx)
		if err != nil {
			t.Fatalf("audio bridge did not respond to capabilities within deadline: %v", err)
		}
		if typ == websocket.MessageText {
			var resp wsAudioCapsResponse
			if json.Unmarshal(data, &resp) == nil && resp.Type == "capabilities_response" {
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestWSAudioAuth(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, store := newGatewayTestAPIServer(t)

	certPath, keyPath := generateTestCert(t, t.TempDir())

	if err := store.SaveTransportConfig(context.Background(), configstore.TransportConfig{
		Binding:     "lan",
		TLSCertPath: certPath,
		TLSKeyPath:  keyPath,
	}); err != nil {
		t.Fatalf("failed to set transport config: %v", err)
	}

	bus := eventbus.New()
	t.Cleanup(bus.Shutdown)
	apiServer.SetEventBus(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := New(apiServer, Options{
		ConnectEnabled: true,
		RegisterGRPC:   func(srv *grpc.Server) { registerTestServices(apiServer, srv) },
	})

	info, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Shutdown(context.Background())

	tlsClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test cert
		},
	}
	t.Cleanup(tlsClient.CloseIdleConnections)

	wsURL := fmt.Sprintf("wss://%s/ws/audio/nonexistent", info.Connect.Address)

	// Unauthenticated → 401.
	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	_, _, err = websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{HTTPClient: tlsClient})
	if err == nil {
		t.Fatal("expected dial to fail for unauthenticated request")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401, got: %v", err)
	}

	// Invalid token → 401.
	wsURLBad := fmt.Sprintf("wss://%s/ws/audio/nonexistent?token=invalid-token-value", info.Connect.Address)
	dialCtxBad, dialCancelBad := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancelBad()
	_, _, err = websocket.Dial(dialCtxBad, wsURLBad, &websocket.DialOptions{HTTPClient: tlsClient})
	if err == nil {
		t.Fatal("expected dial to fail for invalid token")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 for invalid token, got: %v", err)
	}

	// Load auto-generated token.
	secVals, loadErr := store.LoadSecuritySettings(context.Background(), "auth.http_tokens")
	if loadErr != nil {
		t.Fatalf("failed to load auth tokens: %v", loadErr)
	}
	raw, ok := secVals["auth.http_tokens"]
	if !ok || raw == "" {
		t.Fatal("no auth tokens found")
	}
	var tokenEntries []struct{ Token string `json:"token"` }
	if err := json.Unmarshal([]byte(raw), &tokenEntries); err != nil {
		t.Fatalf("failed to parse auth tokens: %v", err)
	}
	if len(tokenEntries) == 0 {
		t.Fatal("auth token list is empty")
	}
	token := tokenEntries[0].Token

	// Valid token via query param, nonexistent session → 404.
	wsURLToken := fmt.Sprintf("wss://%s/ws/audio/nonexistent?token=%s", info.Connect.Address, token)
	dialCtx2, dialCancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel2()
	_, _, err = websocket.Dial(dialCtx2, wsURLToken, &websocket.DialOptions{HTTPClient: tlsClient})
	if err == nil {
		t.Fatal("expected dial to fail for nonexistent session")
	}
	if strings.Contains(err.Error(), "401") {
		t.Fatalf("expected session-not-found, got auth error: %v", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404, got: %v", err)
	}

	// Valid token via Bearer header → 404 (session not found).
	wsURLNoToken := fmt.Sprintf("wss://%s/ws/audio/nonexistent", info.Connect.Address)
	dialCtx3, dialCancel3 := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel3()
	_, _, err = websocket.Dial(dialCtx3, wsURLNoToken, &websocket.DialOptions{
		HTTPClient: tlsClient,
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
	if err == nil {
		t.Fatal("expected dial to fail for nonexistent session")
	}
	if strings.Contains(err.Error(), "401") {
		t.Fatalf("expected session-not-found with Bearer header, got auth error: %v", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 with Bearer token, got: %v", err)
	}
}

func TestWSAudioInvalidSession(t *testing.T) {
	skipIfNoNetwork(t)

	_, _, baseURL := startWSAudioTestGateway(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err := websocket.Dial(ctx, baseURL+"/ws/audio/does-not-exist", nil)
	if err == nil {
		t.Fatal("expected dial to fail for nonexistent session")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404, got: %v", err)
	}
}

func TestWSAudioMissingSessionID(t *testing.T) {
	skipIfNoNetwork(t)

	_, _, baseURL := startWSAudioTestGateway(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err := websocket.Dial(ctx, baseURL+"/ws/audio/", nil)
	if err == nil {
		t.Fatal("expected dial to fail for missing session ID")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected 400, got: %v", err)
	}
}

func TestWSAudioIngress(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _, baseURL := startWSAudioTestGateway(t)
	sess := wsCreateTestSession(t, apiServer.SessionMgr())

	mockIngress := &mockAudioCaptureProvider{}
	apiServer.SetAudioIngress(mockIngress)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, baseURL+"/ws/audio/"+sess.ID, nil)
	if err != nil {
		t.Fatalf("failed to dial WS: %v", err)
	}
	defer conn.CloseNow()

	waitForAudioBridgeReady(t, ctx, conn)

	// Send binary audio frames.
	audioData := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	if err := conn.Write(ctx, websocket.MessageBinary, audioData); err != nil {
		t.Fatalf("failed to write audio: %v", err)
	}

	// Wait for the capture stream to receive the data.
	deadline := time.After(3 * time.Second)
	for mockIngress.latestStream() == nil {
		select {
		case <-deadline:
			t.Fatal("capture stream was not opened within deadline")
		case <-time.After(50 * time.Millisecond):
		}
	}

	stream := mockIngress.latestStream()
	select {
	case received := <-stream.dataCh:
		if string(received) != string(audioData) {
			t.Fatalf("expected %v, got %v", audioData, received)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("audio data did not reach capture stream within deadline")
	}
}

func TestWSAudioEgress(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, bus, baseURL := startWSAudioTestGateway(t)
	sess := wsCreateTestSession(t, apiServer.SessionMgr())

	mockEgress := &mockAudioPlaybackController{
		defaultStream: "tts",
		format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
	}
	apiServer.SetAudioEgress(mockEgress)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, baseURL+"/ws/audio/"+sess.ID, nil)
	if err != nil {
		t.Fatalf("failed to dial WS: %v", err)
	}
	defer conn.CloseNow()

	waitForAudioBridgeReady(t, ctx, conn)

	// Publish an egress playback event on the bus.
	ttsData := []byte{0xAA, 0xBB, 0xCC}
	eventbus.Publish(context.Background(), bus, eventbus.Audio.EgressPlayback, eventbus.SourceAudioEgress, eventbus.AudioEgressPlaybackEvent{
		SessionID: sess.ID,
		StreamID:  "tts",
		Sequence:  1,
		Format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
		Data:     ttsData,
		Duration: 20 * time.Millisecond,
		Final:    false,
	})

	// Read WS messages: expect binary frame (audio) then text frame (metadata).
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel()

	var gotBinary []byte
	var gotMeta wsAudioMeta
	gotBinaryFrame := false
	gotMetaFrame := false

	for !gotBinaryFrame || !gotMetaFrame {
		typ, data, rErr := conn.Read(readCtx)
		if rErr != nil {
			t.Fatalf("failed reading egress data: %v", rErr)
		}
		if typ == websocket.MessageBinary && !gotBinaryFrame {
			gotBinary = data
			gotBinaryFrame = true
		}
		if typ == websocket.MessageText && !gotMetaFrame {
			if json.Unmarshal(data, &gotMeta) == nil && gotMeta.Type == "audio_meta" {
				gotMetaFrame = true
			}
		}
	}

	if string(gotBinary) != string(ttsData) {
		t.Fatalf("expected binary %v, got %v", ttsData, gotBinary)
	}
	if gotMeta.Sequence != 1 {
		t.Errorf("expected sequence 1, got %d", gotMeta.Sequence)
	}
	if !gotMeta.First {
		t.Error("expected first=true on first chunk")
	}
	if gotMeta.DurationMs != 20 {
		t.Errorf("expected duration_ms=20, got %d", gotMeta.DurationMs)
	}
	if gotMeta.Format == nil || gotMeta.Format.Encoding != "pcm_s16le" {
		t.Error("expected format with pcm_s16le encoding")
	}

	// --- Final chunk: verify Last=true and firstChunk reset ---
	ttsData2 := []byte{0xDD, 0xEE}
	eventbus.Publish(context.Background(), bus, eventbus.Audio.EgressPlayback, eventbus.SourceAudioEgress, eventbus.AudioEgressPlaybackEvent{
		SessionID: sess.ID,
		StreamID:  "tts",
		Sequence:  2,
		Format:    eventbus.AudioFormat{Encoding: eventbus.AudioEncodingPCM16, SampleRate: 16000, Channels: 1, BitDepth: 16},
		Data:      ttsData2,
		Final:     true,
	})

	readCtx2, readCancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel2()

	gotBinaryFrame = false
	gotMetaFrame = false
	var gotMeta2 wsAudioMeta

	for !gotBinaryFrame || !gotMetaFrame {
		typ, data, rErr := conn.Read(readCtx2)
		if rErr != nil {
			t.Fatalf("failed reading final chunk: %v", rErr)
		}
		if typ == websocket.MessageBinary && !gotBinaryFrame {
			gotBinaryFrame = true
		}
		if typ == websocket.MessageText && !gotMetaFrame {
			if json.Unmarshal(data, &gotMeta2) == nil && gotMeta2.Type == "audio_meta" {
				gotMetaFrame = true
			}
		}
	}

	if gotMeta2.Sequence != 2 {
		t.Errorf("expected sequence 2, got %d", gotMeta2.Sequence)
	}
	if !gotMeta2.Last {
		t.Error("expected last=true on final chunk")
	}
	if gotMeta2.First {
		t.Error("expected first=false on second chunk of same utterance")
	}

	// --- Metadata-only event (empty Data): verify only text frame, no binary ---
	eventbus.Publish(context.Background(), bus, eventbus.Audio.EgressPlayback, eventbus.SourceAudioEgress, eventbus.AudioEgressPlaybackEvent{
		SessionID: sess.ID,
		StreamID:  "tts",
		Sequence:  3,
		Format:    eventbus.AudioFormat{Encoding: eventbus.AudioEncodingPCM16, SampleRate: 16000, Channels: 1, BitDepth: 16},
		Data:      nil,
		Final:     false,
	})

	readCtx3, readCancel3 := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel3()

	var gotMeta3 wsAudioMeta
	for {
		typ, data, rErr := conn.Read(readCtx3)
		if rErr != nil {
			t.Fatalf("failed reading metadata-only event: %v", rErr)
		}
		if typ == websocket.MessageBinary {
			t.Fatal("unexpected binary frame for empty-data event")
		}
		if typ == websocket.MessageText {
			if json.Unmarshal(data, &gotMeta3) == nil && gotMeta3.Type == "audio_meta" {
				break
			}
		}
	}

	if gotMeta3.Sequence != 3 {
		t.Errorf("expected sequence 3, got %d", gotMeta3.Sequence)
	}
	// After Final=true reset, this new utterance's first chunk should have First=true.
	if !gotMeta3.First {
		t.Error("expected first=true after Final reset (new utterance)")
	}
}

func TestWSAudioInterrupt(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, bus, baseURL := startWSAudioTestGateway(t)
	sess := wsCreateTestSession(t, apiServer.SessionMgr())

	mockEgress := &mockAudioPlaybackController{defaultStream: "tts"}
	apiServer.SetAudioEgress(mockEgress)

	// Subscribe to interrupt events on the bus to verify PublishAudioInterrupt.
	interruptSub := eventbus.Subscribe[eventbus.AudioInterruptEvent](bus,
		eventbus.TopicAudioInterrupt,
		eventbus.WithSubscriptionName("test_interrupt_verify"),
		eventbus.WithSubscriptionBuffer(16),
	)
	defer interruptSub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, baseURL+"/ws/audio/"+sess.ID, nil)
	if err != nil {
		t.Fatalf("failed to dial WS: %v", err)
	}
	defer conn.CloseNow()

	waitForAudioBridgeReady(t, ctx, conn)

	// Send interrupt with explicit reason.
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"interrupt","reason":"user_barge_in"}`)); err != nil {
		t.Fatalf("failed to send interrupt: %v", err)
	}

	// Poll until the mock registers the interrupt call.
	deadline := time.After(3 * time.Second)
	for len(mockEgress.getInterruptCalls()) == 0 {
		select {
		case <-deadline:
			t.Fatal("interrupt call not received within deadline")
		case <-time.After(50 * time.Millisecond):
		}
	}

	calls := mockEgress.getInterruptCalls()
	if calls[0].SessionID != sess.ID {
		t.Errorf("expected session %s, got %s", sess.ID, calls[0].SessionID)
	}
	if calls[0].StreamID != "tts" {
		t.Errorf("expected stream tts, got %s", calls[0].StreamID)
	}
	if calls[0].Reason != "user_barge_in" {
		t.Errorf("expected reason user_barge_in, got %s", calls[0].Reason)
	}

	// Verify event bus received AudioInterruptEvent.
	select {
	case env := <-interruptSub.C():
		evt := env.Payload
		if evt.SessionID != sess.ID {
			t.Errorf("bus event: expected session %s, got %s", sess.ID, evt.SessionID)
		}
		if evt.Reason != "user_barge_in" {
			t.Errorf("bus event: expected reason user_barge_in, got %s", evt.Reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("AudioInterruptEvent not published on event bus within deadline")
	}

	// Send interrupt with empty reason — verify default "user_barge_in" fallback.
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"interrupt"}`)); err != nil {
		t.Fatalf("failed to send interrupt with empty reason: %v", err)
	}

	deadline = time.After(3 * time.Second)
	for len(mockEgress.getInterruptCalls()) < 2 {
		select {
		case <-deadline:
			t.Fatal("second interrupt call not received within deadline")
		case <-time.After(50 * time.Millisecond):
		}
	}

	calls = mockEgress.getInterruptCalls()
	if calls[1].Reason != "user_barge_in" {
		t.Errorf("expected default reason user_barge_in, got %s", calls[1].Reason)
	}
}

func TestWSAudioCapabilities(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _, baseURL := startWSAudioTestGateway(t)
	sess := wsCreateTestSession(t, apiServer.SessionMgr())

	mockIngress := &mockAudioCaptureProvider{}
	mockEgress := &mockAudioPlaybackController{
		defaultStream: "tts",
		format: eventbus.AudioFormat{
			Encoding:   eventbus.AudioEncodingPCM16,
			SampleRate: 22050,
			Channels:   1,
			BitDepth:   16,
		},
	}
	apiServer.SetAudioIngress(mockIngress)
	apiServer.SetAudioEgress(mockEgress)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, baseURL+"/ws/audio/"+sess.ID, nil)
	if err != nil {
		t.Fatalf("failed to dial WS: %v", err)
	}
	defer conn.CloseNow()

	// Send capabilities request.
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"capabilities"}`)); err != nil {
		t.Fatalf("failed to send capabilities: %v", err)
	}

	// Read the capabilities response.
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()

	var resp wsAudioCapsResponse
	for {
		typ, data, rErr := conn.Read(readCtx)
		if rErr != nil {
			t.Fatalf("failed reading capabilities response: %v", rErr)
		}
		if typ == websocket.MessageText {
			if json.Unmarshal(data, &resp) == nil && resp.Type == "capabilities_response" {
				break
			}
		}
	}

	if len(resp.Capture) != 1 {
		t.Fatalf("expected 1 capture entry, got %d", len(resp.Capture))
	}
	if resp.Capture[0].StreamID != "mic" {
		t.Errorf("expected capture stream_id=mic, got %s", resp.Capture[0].StreamID)
	}
	if !resp.Capture[0].Ready {
		t.Error("expected capture ready=true")
	}
	if resp.Capture[0].Format == nil || resp.Capture[0].Format.Encoding != "pcm_s16le" {
		t.Error("expected capture format encoding pcm_s16le")
	}

	if len(resp.Playback) != 1 {
		t.Fatalf("expected 1 playback entry, got %d", len(resp.Playback))
	}
	if resp.Playback[0].StreamID != "tts" {
		t.Errorf("expected playback stream_id=tts, got %s", resp.Playback[0].StreamID)
	}
	if !resp.Playback[0].Ready {
		t.Error("expected playback ready=true")
	}
	if resp.Playback[0].Format == nil || resp.Playback[0].Format.SampleRate != 22050 {
		t.Errorf("expected playback sample_rate=22050, got %v", resp.Playback[0].Format)
	}
}

func TestWSAudioDisconnect(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _, baseURL := startWSAudioTestGateway(t)
	sess := wsCreateTestSession(t, apiServer.SessionMgr())

	mockIngress := &mockAudioCaptureProvider{}
	apiServer.SetAudioIngress(mockIngress)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, baseURL+"/ws/audio/"+sess.ID, nil)
	if err != nil {
		t.Fatalf("failed to dial WS: %v", err)
	}

	waitForAudioBridgeReady(t, ctx, conn)

	// Send binary frame to open a capture stream.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{0x01, 0x02}); err != nil {
		t.Fatalf("failed to write audio: %v", err)
	}

	// Wait for stream to be opened.
	deadline := time.After(3 * time.Second)
	for mockIngress.latestStream() == nil {
		select {
		case <-deadline:
			t.Fatal("capture stream was not opened within deadline")
		case <-time.After(50 * time.Millisecond):
		}
	}

	stream := mockIngress.latestStream()

	// Abruptly close the WebSocket.
	conn.CloseNow()

	// Verify the capture stream was closed.
	deadline = time.After(3 * time.Second)
	for !stream.closed.Load() {
		select {
		case <-deadline:
			t.Fatal("capture stream was not closed after disconnect")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Session should still be running.
	if !sess.PTY.IsRunning() {
		t.Fatal("expected session to still be running after disconnect")
	}
}

func TestWSAudioDetach(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _, baseURL := startWSAudioTestGateway(t)
	sess := wsCreateTestSession(t, apiServer.SessionMgr())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, baseURL+"/ws/audio/"+sess.ID, nil)
	if err != nil {
		t.Fatalf("failed to dial WS: %v", err)
	}
	defer conn.CloseNow()

	waitForAudioBridgeReady(t, ctx, conn)

	// Send detach.
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"detach"}`)); err != nil {
		t.Fatalf("failed to send detach: %v", err)
	}

	// Read until close frame.
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel()
	var readErr error
	for {
		_, _, readErr = conn.Read(readCtx)
		if readErr != nil {
			break
		}
	}
	if cs := websocket.CloseStatus(readErr); cs != websocket.StatusNormalClosure {
		t.Fatalf("expected StatusNormalClosure (1000), got %d, err: %v", cs, readErr)
	}
}

func TestWSAudioCloseStream(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _, baseURL := startWSAudioTestGateway(t)
	sess := wsCreateTestSession(t, apiServer.SessionMgr())

	mockIngress := &mockAudioCaptureProvider{}
	apiServer.SetAudioIngress(mockIngress)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, baseURL+"/ws/audio/"+sess.ID, nil)
	if err != nil {
		t.Fatalf("failed to dial WS: %v", err)
	}
	defer conn.CloseNow()

	waitForAudioBridgeReady(t, ctx, conn)

	// Send binary frame to open a capture stream.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{0x01}); err != nil {
		t.Fatalf("failed to write audio: %v", err)
	}

	// Wait for stream to open.
	deadline := time.After(3 * time.Second)
	for mockIngress.streamCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("capture stream was not opened within deadline")
		case <-time.After(50 * time.Millisecond):
		}
	}

	firstStream := mockIngress.latestStream()

	// Send close_stream.
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"close_stream"}`)); err != nil {
		t.Fatalf("failed to send close_stream: %v", err)
	}

	// Wait for the first stream to be closed.
	deadline = time.After(3 * time.Second)
	for !firstStream.closed.Load() {
		select {
		case <-deadline:
			t.Fatal("first stream was not closed after close_stream")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Send another binary frame — should open a new stream.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{0x02}); err != nil {
		t.Fatalf("failed to write second audio frame: %v", err)
	}

	deadline = time.After(3 * time.Second)
	for mockIngress.streamCount() < 2 {
		select {
		case <-deadline:
			t.Fatal("second capture stream was not opened after close_stream")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Connection should still be open — verify by sending a capabilities request.
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"capabilities"}`)); err != nil {
		t.Fatalf("connection unexpectedly closed after close_stream: %v", err)
	}
}
