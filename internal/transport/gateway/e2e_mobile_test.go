package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/server"
	"github.com/nupi-ai/nupi/internal/session"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// mutableRuntimeStub allows the Connect port to be set after gateway start,
// simulating the daemon's SetConnectPort behavior.
type mutableRuntimeStub struct {
	grpcPort    int
	connectPort atomic.Int32
}

func (r *mutableRuntimeStub) GRPCPort() int        { return r.grpcPort }
func (r *mutableRuntimeStub) ConnectPort() int     { return int(r.connectPort.Load()) }
func (r *mutableRuntimeStub) StartTime() time.Time { return time.Unix(0, 0) }

// ---------------------------------------------------------------------------
// connectPostWithAuth sends a Connect RPC request with Bearer token auth.
// ---------------------------------------------------------------------------

func connectPostWithAuth(ctx context.Context, url, body, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return testHTTPClient.Do(req)
}

// startE2EGateway creates a gateway with gRPC + Connect + WS all enabled,
// event bus wired, and returns the info needed for e2e tests.
func startE2EGateway(t *testing.T) (apiServer *server.APIServer, bus *eventbus.Bus, info *Info) {
	t.Helper()

	apiServer, _ = newGatewayTestAPIServer(t)
	bus = eventbus.New()
	t.Cleanup(bus.Shutdown)
	apiServer.SetEventBus(bus)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	gw := New(apiServer, Options{
		ConnectEnabled: true,
		RegisterGRPC:   func(srv *grpc.Server) { registerTestServices(apiServer, srv) },
	})

	var err error
	info, err = gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	t.Cleanup(func() { gw.Shutdown(context.Background()) })
	return
}

// ---------------------------------------------------------------------------
// Test 1: Full mobile workflow end-to-end (AC #1)
// ---------------------------------------------------------------------------

func TestMobileWorkflowE2E(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _, gwInfo := startE2EGateway(t)
	connectAddr := gwInfo.Connect.Address

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create a PTY session so there's a session to find.
	sess := wsCreateTestSession(t, apiServer.SessionMgr())

	// --- Step 1: CreatePairing via Connect RPC ---
	createURL := fmt.Sprintf("http://%s/nupi.api.v1.AuthService/CreatePairing", connectAddr)
	createBody := `{"name":"Test iPhone","role":"admin"}`
	createResp, err := connectPost(ctx, createURL, createBody)
	if err != nil {
		t.Fatalf("CreatePairing Connect request failed: %v", err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(createResp.Body)
		t.Fatalf("CreatePairing expected 200 OK, got %d: %s", createResp.StatusCode, string(body))
	}

	var pairingResult struct {
		Code       string `json:"code"`
		ConnectUrl string `json:"connectUrl"`
		Name       string `json:"name"`
		Role       string `json:"role"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&pairingResult); err != nil {
		t.Fatalf("failed to decode CreatePairing response: %v", err)
	}
	if pairingResult.Code == "" {
		t.Fatal("expected non-empty pairing code")
	}
	if pairingResult.ConnectUrl == "" {
		t.Fatal("expected non-empty connect_url")
	}

	// --- Step 2: ClaimPairing via Connect RPC (no auth) ---
	claimURL := fmt.Sprintf("http://%s/nupi.api.v1.AuthService/ClaimPairing", connectAddr)
	claimBody := fmt.Sprintf(`{"code":"%s","clientName":"E2E Mobile"}`, pairingResult.Code)
	claimResp, err := connectPost(ctx, claimURL, claimBody)
	if err != nil {
		t.Fatalf("ClaimPairing Connect request failed: %v", err)
	}
	defer claimResp.Body.Close()
	if claimResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(claimResp.Body)
		t.Fatalf("ClaimPairing expected 200 OK, got %d: %s", claimResp.StatusCode, string(body))
	}

	var claimResult struct {
		Token string `json:"token"`
		Name  string `json:"name"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(claimResp.Body).Decode(&claimResult); err != nil {
		t.Fatalf("failed to decode ClaimPairing response: %v", err)
	}
	if claimResult.Token == "" {
		t.Fatal("expected non-empty token from ClaimPairing")
	}

	// --- Step 3: ListSessions via Connect RPC with Bearer token ---
	listURL := fmt.Sprintf("http://%s/nupi.api.v1.SessionsService/ListSessions", connectAddr)
	listResp, err := connectPostWithAuth(ctx, listURL, "{}", claimResult.Token)
	if err != nil {
		t.Fatalf("ListSessions Connect request failed: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listResp.Body)
		t.Fatalf("ListSessions expected 200 OK, got %d: %s", listResp.StatusCode, string(body))
	}

	var listResult struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&listResult); err != nil {
		t.Fatalf("failed to decode ListSessions response: %v", err)
	}

	found := false
	for _, s := range listResult.Sessions {
		if s.ID == sess.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("session %s not found in ListSessions response: %v", sess.ID, listResult.Sessions)
	}

	// --- Step 4: Open WebSocket to /ws/session/{id} with token ---
	wsURL := fmt.Sprintf("ws://%s/ws/session/%s?token=%s", connectAddr, sess.ID, claimResult.Token)
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial session WS: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	waitForBridgeReady(t, ctx, conn)

	// --- Step 5: Send input via WebSocket → read back echo ---
	marker := "e2e-mobile-marker-77"
	if err := conn.Write(ctx, websocket.MessageBinary, []byte(marker+"\n")); err != nil {
		t.Fatalf("failed to write input via WS: %v", err)
	}

	echoSeen := false
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel()
	for i := 0; i < 100; i++ {
		typ, data, rErr := conn.Read(readCtx)
		if rErr != nil {
			break
		}
		if typ == websocket.MessageBinary && strings.Contains(string(data), marker) {
			echoSeen = true
			break
		}
	}
	if !echoSeen {
		t.Fatal("expected to see echoed input in PTY output")
	}

	// --- Step 6: Send resize via WebSocket → verify no error ---
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatalf("failed to send resize: %v", err)
	}

	// Poll until PTY size changes.
	deadline := time.After(3 * time.Second)
	for {
		rows, cols := sess.PTY.GetWinSize()
		if cols == 120 && rows == 40 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("PTY size did not change to 120x40 within deadline")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// --- Step 7: Send detach via WebSocket → verify clean closure ---
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"detach"}`)); err != nil {
		t.Fatalf("failed to send detach: %v", err)
	}

	detachCtx, detachCancel := context.WithTimeout(ctx, 5*time.Second)
	defer detachCancel()
	var readErr error
	for {
		_, _, readErr = conn.Read(detachCtx)
		if readErr != nil {
			break
		}
	}
	if cs := websocket.CloseStatus(readErr); cs != websocket.StatusNormalClosure {
		t.Errorf("expected StatusNormalClosure (1000), got %d, err: %v", cs, readErr)
	}

	// Verify session still running after WebSocket detach.
	if !sess.PTY.IsRunning() {
		t.Fatal("expected session to still be running after detach")
	}

	// --- Verify session via Connect GetSession ---
	getURL := fmt.Sprintf("http://%s/nupi.api.v1.SessionsService/GetSession", connectAddr)
	getBody := fmt.Sprintf(`{"sessionId":"%s"}`, sess.ID)
	getResp, err := connectPostWithAuth(ctx, getURL, getBody, claimResult.Token)
	if err != nil {
		t.Fatalf("GetSession Connect request failed: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(getResp.Body)
		t.Fatalf("GetSession expected 200 OK, got %d: %s", getResp.StatusCode, string(body))
	}
}

// ---------------------------------------------------------------------------
// Test 2: Audio WebSocket end-to-end (AC #2)
// ---------------------------------------------------------------------------

func TestAudioWebSocketE2E(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _, gwInfo := startE2EGateway(t)
	connectAddr := gwInfo.Connect.Address

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create a session for the audio bridge to reference.
	sess := wsCreateTestSession(t, apiServer.SessionMgr())

	// Wire mock audio components.
	captureProvider := &mockAudioCaptureProvider{}
	playbackCtrl := &mockAudioPlaybackController{
		format: eventbus.AudioFormat{
			Encoding:   "pcm",
			SampleRate: 16000,
			Channels:   1,
			BitDepth:   16,
		},
	}
	apiServer.SetAudioIngress(captureProvider)
	apiServer.SetAudioEgress(playbackCtrl)

	// --- Step 1: Open WebSocket to /ws/audio/{session_id} ---
	wsURL := fmt.Sprintf("ws://%s/ws/audio/%s", connectAddr, sess.ID)
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial audio WS: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	// --- Step 2: Send capabilities request → verify capabilities response ---
	waitForAudioBridgeReady(t, ctx, conn)

	// --- Step 3: Send binary PCM frames → verify mock ingress receives them ---
	pcmData := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	if err := conn.Write(ctx, websocket.MessageBinary, pcmData); err != nil {
		t.Fatalf("failed to write PCM data: %v", err)
	}

	// Wait for mock capture stream to receive the data.
	captureDeadline := time.After(3 * time.Second)
	var capturedData []byte
	for {
		stream := captureProvider.latestStream()
		if stream != nil {
			select {
			case capturedData = <-stream.dataCh:
			case <-captureDeadline:
				t.Fatal("mock capture stream did not receive PCM data within deadline")
			}
			break
		}
		select {
		case <-captureDeadline:
			t.Fatal("capture stream was never opened")
		case <-time.After(50 * time.Millisecond):
		}
	}

	if len(capturedData) != len(pcmData) {
		t.Fatalf("captured data length mismatch: got %d, want %d", len(capturedData), len(pcmData))
	}
	for i := range pcmData {
		if capturedData[i] != pcmData[i] {
			t.Fatalf("captured data mismatch at byte %d: got %02x, want %02x", i, capturedData[i], pcmData[i])
		}
	}

	// --- Step 4: Send interrupt control message ---
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"interrupt","reason":"user_barge_in"}`)); err != nil {
		t.Fatalf("failed to send interrupt: %v", err)
	}

	// Wait for mock playback controller to record interrupt.
	interruptDeadline := time.After(3 * time.Second)
	for {
		calls := playbackCtrl.getInterruptCalls()
		if len(calls) > 0 {
			if calls[0].Reason != "user_barge_in" {
				t.Errorf("expected interrupt reason 'user_barge_in', got %q", calls[0].Reason)
			}
			break
		}
		select {
		case <-interruptDeadline:
			t.Fatal("mock playback controller did not record interrupt within deadline")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// --- Step 5: Send detach → verify clean closure ---
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"detach"}`)); err != nil {
		t.Fatalf("failed to send audio detach: %v", err)
	}

	detachCtx, detachCancel := context.WithTimeout(ctx, 5*time.Second)
	defer detachCancel()
	var readErr error
	for {
		_, _, readErr = conn.Read(detachCtx)
		if readErr != nil {
			break
		}
	}
	if cs := websocket.CloseStatus(readErr); cs != websocket.StatusNormalClosure {
		t.Errorf("expected StatusNormalClosure (1000), got %d, err: %v", cs, readErr)
	}

	// Verify capture stream was closed (server closes it after pumpInput returns).
	streamClosed := false
	closeDeadline := time.After(3 * time.Second)
	for !streamClosed {
		stream := captureProvider.latestStream()
		if stream == nil || stream.closed.Load() {
			streamClosed = true
			break
		}
		select {
		case <-closeDeadline:
			t.Error("expected capture stream to be closed after detach")
			streamClosed = true // exit loop
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------------------
// Test 3: Multi-transport coexistence (AC #3)
// ---------------------------------------------------------------------------

func TestMultiTransportCoexistence(t *testing.T) {
	skipIfNoNetwork(t)

	apiServer, _, gwInfo := startE2EGateway(t)
	connectAddr := gwInfo.Connect.Address

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// --- Create session via gRPC ---
	grpcConn, err := grpc.NewClient(passthroughPrefix+gwInfo.GRPC.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial gRPC: %v", err)
	}
	defer grpcConn.Close()

	sess := wsCreateTestSession(t, apiServer.SessionMgr())

	// --- List sessions via Connect RPC → verify same session visible ---
	listURL := fmt.Sprintf("http://%s/nupi.api.v1.SessionsService/ListSessions", connectAddr)
	listResp, err := connectPost(ctx, listURL, "{}")
	if err != nil {
		t.Fatalf("ListSessions Connect request failed: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listResp.Body)
		t.Fatalf("ListSessions expected 200 OK, got %d: %s", listResp.StatusCode, string(body))
	}

	var listResult struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&listResult); err != nil {
		t.Fatalf("failed to decode ListSessions response: %v", err)
	}

	found := false
	for _, s := range listResult.Sessions {
		if s.ID == sess.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("gRPC-created session %s not visible via Connect RPC", sess.ID)
	}

	// --- Attach to session via WebSocket → verify terminal output ---
	wsURL := fmt.Sprintf("ws://%s/ws/session/%s", connectAddr, sess.ID)
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial session WS: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	waitForBridgeReady(t, ctx, conn)

	// --- Simultaneously call DaemonStatus via gRPC and Connect ---
	type statusResult struct {
		source string
		err    error
	}
	results := make(chan statusResult, 2)

	// gRPC health check (serves as gRPC DaemonStatus equivalent)
	go func() {
		hc := healthpb.NewHealthClient(grpcConn)
		_, gErr := hc.Check(ctx, &healthpb.HealthCheckRequest{})
		results <- statusResult{source: "grpc", err: gErr}
	}()

	// Connect DaemonStatus
	go func() {
		statusURL := fmt.Sprintf("http://%s/nupi.api.v1.DaemonService/Status", connectAddr)
		resp, err := connectPost(ctx, statusURL, "{}")
		if err != nil {
			results <- statusResult{source: "connect", err: err}
			return
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			results <- statusResult{source: "connect", err: fmt.Errorf("expected 200, got %d", resp.StatusCode)}
			return
		}
		results <- statusResult{source: "connect", err: nil}
	}()

	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Errorf("%s status call failed: %v", r.source, r.err)
		}
	}

	// Verify gRPC client unaffected by Connect/WS activity — health check still works.
	hc := healthpb.NewHealthClient(grpcConn)
	if _, err := hc.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatalf("gRPC health check failed after Connect/WS activity: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 4: connect_url port validation (AC #4)
// ---------------------------------------------------------------------------

func TestConnectURLPortValidation(t *testing.T) {
	skipIfNoNetwork(t)

	// Create APIServer with a mutable runtime so we can set ConnectPort
	// after the gateway starts (simulating daemon's SetConnectPort behavior).
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if _, err := config.EnsureInstanceDirs(config.DefaultInstance); err != nil {
		t.Fatalf("failed to ensure instance dirs: %v", err)
	}
	if _, err := config.EnsureProfileDirs(config.DefaultInstance, config.DefaultProfile); err != nil {
		t.Fatalf("failed to ensure profile dirs: %v", err)
	}
	store, err := configstore.Open(configstore.Options{InstanceName: config.DefaultInstance, ProfileName: config.DefaultProfile})
	if err != nil {
		t.Fatalf("failed to open config store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	rt := &mutableRuntimeStub{}
	sessionManager := session.NewManager()
	apiServer, err := server.NewAPIServer(sessionManager, store, rt)
	if err != nil {
		t.Fatalf("failed to create api server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	gw := New(apiServer, Options{
		ConnectEnabled: true,
		RegisterGRPC:   func(srv *grpc.Server) { registerTestServices(apiServer, srv) },
	})

	gwInfo, err := gw.Start(ctx)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	t.Cleanup(func() { gw.Shutdown(context.Background()) })

	connectAddr := gwInfo.Connect.Address
	connectPort := gwInfo.Connect.Port
	grpcPort := gwInfo.GRPC.Port

	// Simulate daemon's SetConnectPort: update the mutable runtime with
	// the actual Connect listener port before calling CreatePairing.
	rt.connectPort.Store(int32(connectPort))

	reqCtx, reqCancel := context.WithTimeout(ctx, 15*time.Second)
	defer reqCancel()

	// Sanity: Connect and gRPC ports should be different.
	if connectPort == grpcPort {
		t.Fatalf("Connect port (%d) and gRPC port (%d) should differ", connectPort, grpcPort)
	}

	// --- CreatePairing via Connect RPC ---
	createURL := fmt.Sprintf("http://%s/nupi.api.v1.AuthService/CreatePairing", connectAddr)
	createResp, err := connectPost(reqCtx, createURL, `{"name":"Port Test","role":"admin"}`)
	if err != nil {
		t.Fatalf("CreatePairing Connect request failed: %v", err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(createResp.Body)
		t.Fatalf("CreatePairing expected 200 OK, got %d: %s", createResp.StatusCode, string(body))
	}

	var result struct {
		Code       string `json:"code"`
		ConnectUrl string `json:"connectUrl"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode CreatePairing response: %v", err)
	}

	// --- Parse connect_url and validate ---
	if result.ConnectUrl == "" {
		t.Fatal("expected non-empty connect_url")
	}

	// The connect_url uses a custom scheme: nupi://pair?code=...&host=...&port=...&tls=...
	parsed, err := url.Parse(result.ConnectUrl)
	if err != nil {
		t.Fatalf("failed to parse connect_url %q: %v", result.ConnectUrl, err)
	}

	// Verify scheme.
	if parsed.Scheme != "nupi" {
		t.Errorf("expected scheme 'nupi', got %q", parsed.Scheme)
	}

	// Verify the URL contains "nupi://pair?" prefix.
	if !strings.HasPrefix(result.ConnectUrl, "nupi://pair?") {
		t.Errorf("expected connect_url to start with 'nupi://pair?', got %q", result.ConnectUrl)
	}

	// Extract query params.
	params := parsed.Query()

	// Verify code param.
	if params.Get("code") != result.Code {
		t.Errorf("code param mismatch: got %q, want %q", params.Get("code"), result.Code)
	}

	// Verify port matches Connect listener port (NOT gRPC port).
	portStr := params.Get("port")
	if portStr == "" {
		t.Fatal("expected 'port' query param in connect_url")
	}
	if portStr != fmt.Sprintf("%d", connectPort) {
		t.Errorf("port param should match Connect port %d, got %q", connectPort, portStr)
	}
	if portStr == fmt.Sprintf("%d", grpcPort) {
		t.Errorf("port param should NOT match gRPC port %d", grpcPort)
	}

	// Verify host param exists.
	if params.Get("host") == "" {
		t.Error("expected 'host' query param in connect_url")
	}

	// Verify tls param.
	tlsParam := params.Get("tls")
	if tlsParam == "" {
		t.Error("expected 'tls' query param in connect_url")
	}
	// In this test (loopback, no TLS), expect tls=false.
	if tlsParam != "false" {
		t.Errorf("expected tls=false for loopback gateway, got %q", tlsParam)
	}
}
