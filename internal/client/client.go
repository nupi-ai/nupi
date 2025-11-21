package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nupi-ai/nupi/internal/bootstrap"
	"github.com/nupi-ai/nupi/internal/protocol"
	"github.com/nupi-ai/nupi/internal/termresize"
)

const (
	frameMagic                byte = 0xBF
	frameTypeOutput           byte = 0x01
	frameHeaderSize                = 4
	websocketHandshakeTimeout      = 10 * time.Second
)

type streamControlKind int

const (
	controlStop streamControlKind = iota
	controlKill
	controlError
)

type streamControl struct {
	kind    streamControlKind
	message string
}

type sessionDTO struct {
	ID        string    `json:"id"`
	Command   string    `json:"command"`
	Args      []string  `json:"args"`
	Status    string    `json:"status"`
	PID       int       `json:"pid"`
	StartTime time.Time `json:"start_time"`
}

type attachResponse struct {
	Session            sessionDTO `json:"session"`
	StreamURL          string     `json:"stream_url"`
	RecordingAvailable bool       `json:"recording_available"`
}

type wsInbound struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Data      json.RawMessage `json:"data"`
}

// Client communicates with the daemon using HTTP and WebSocket transports.
type Client struct {
	baseURL    string
	httpClient *http.Client
	token      string
	dialer     *websocket.Dialer

	mu        sync.Mutex
	wsConn    *websocket.Conn
	sessionID string
	outputCh  chan []byte
	controlCh chan streamControl
	errCh     chan error

	wsWriteMu sync.Mutex

	streamHTTPClient *http.Client
	streamOnce       sync.Once
}

func newClientWithConfig(baseURL string, tlsConfig *tls.Config, token string) *Client {
	trimmedBase := strings.TrimRight(baseURL, "/")

	httpClient := &http.Client{Timeout: defaultHTTPTimeout}
	if tlsConfig != nil {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: tlsConfig,
		}
	}

	dialer := &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  websocketHandshakeTimeout,
		EnableCompression: true,
		TLSClientConfig:   tlsConfig,
	}

	return &Client{
		baseURL:    trimmedBase,
		httpClient: httpClient,
		token:      strings.TrimSpace(token),
		dialer:     dialer,
	}
}

// NewInitialisedClient constructs a client from explicit parameters.
func NewInitialisedClient(baseURL, token string, tlsConfig *tls.Config) *Client {
	return newClientWithConfig(baseURL, tlsConfig, token)
}

// New initialises a client instance bound to the default Nupi instance/profile.
func New() (*Client, error) {
	if base := strings.TrimSpace(os.Getenv("NUPI_BASE_URL")); base != "" {
		return newFromExplicit(base)
	}

	boot, err := bootstrap.Load()
	if err != nil {
		return nil, err
	}
	if boot != nil && strings.TrimSpace(boot.BaseURL) != "" {
		return newFromExplicitWithBootstrap(boot)
	}

	return newFromStore()
}

func newFromStore() (*Client, error) {
	cfg, tokens, err := LoadTransportSettings()
	if err != nil {
		return nil, err
	}

	tlsEnabled := strings.TrimSpace(cfg.TLSCertPath) != "" && strings.TrimSpace(cfg.TLSKeyPath) != ""

	host := DetermineHost(cfg.Binding, tlsEnabled)
	if override := strings.TrimSpace(os.Getenv("NUPI_DAEMON_HOST")); override != "" {
		host = override
	}

	if cfg.Port <= 0 {
		return nil, fmt.Errorf("daemon HTTP port not available; is nupid running?")
	}

	tlsConfig, err := PrepareTLSConfig(cfg, host, tlsEnabled)
	if err != nil {
		return nil, err
	}

	scheme := "http"
	if tlsConfig != nil {
		scheme = "https"
	}

	baseURL := fmt.Sprintf("%s://%s:%d", scheme, host, cfg.Port)

	token := strings.TrimSpace(os.Getenv("NUPI_API_TOKEN"))
	if token == "" && len(tokens) > 0 {
		token = tokens[0]
	}

	return newClientWithConfig(baseURL, tlsConfig, token), nil
}

func newFromExplicit(raw string) (*Client, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("client: NUPI_BASE_URL is empty")
	}

	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("client: parse NUPI_BASE_URL: %w", err)
	}
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	if u.Host == "" {
		return nil, fmt.Errorf("client: NUPI_BASE_URL missing host")
	}

	token, tlsOpts := ResolveExplicitOptions(nil)
	tlsConfig, err := TLSConfigForExplicit(u, &tlsOpts)
	if err != nil {
		return nil, err
	}

	return newClientWithConfig(u.String(), tlsConfig, token), nil
}

func newFromExplicitWithBootstrap(boot *bootstrap.Config) (*Client, error) {
	if boot == nil || strings.TrimSpace(boot.BaseURL) == "" {
		return nil, fmt.Errorf("client: explicit base URL required")
	}

	u, err := url.Parse(boot.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("client: parse base URL: %w", err)
	}

	token, opts := ResolveExplicitOptions(boot)
	tlsConfig, err := TLSConfigForExplicit(u, &opts)
	if err != nil {
		return nil, err
	}

	return newClientWithConfig(u.String(), tlsConfig, token), nil
}

// BaseURL returns the base HTTP URL the client is configured to use.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// HTTPClient exposes the configured HTTP client (including TLS settings).
func (c *Client) HTTPClient() *http.Client {
	return c.httpClient
}

// StreamingHTTPClient returns an HTTP client configured for long-lived streams
// (timeouts disabled).
func (c *Client) StreamingHTTPClient() *http.Client {
	return c.streamingHTTPClient()
}

// Token returns the bearer token configured for the client, if any.
func (c *Client) Token() string {
	return c.token
}

// Close terminates any active WebSocket attachment.
func (c *Client) Close() error {
	return c.DetachSession()
}

// DetachSession closes the active WebSocket stream, if any.
func (c *Client) DetachSession() error {
	c.mu.Lock()
	conn := c.wsConn
	sessionID := c.sessionID
	if conn == nil {
		c.mu.Unlock()
		return nil
	}
	c.wsConn = nil
	c.sessionID = ""
	c.outputCh = nil
	c.controlCh = nil
	c.errCh = nil
	c.mu.Unlock()

	detachMsg := map[string]any{
		"type": "detach",
		"data": sessionID,
	}

	c.wsWriteMu.Lock()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = conn.WriteJSON(detachMsg)
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(2*time.Second))
	c.wsWriteMu.Unlock()

	return conn.Close()
}

// CreateSession starts a new PTY session via the REST API.
func (c *Client) CreateSession(opts protocol.CreateSessionData) (*protocol.SessionInfo, error) {
	payload, err := json.Marshal(opts)
	if err != nil {
		return nil, fmt.Errorf("marshal session payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/sessions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.addAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("create session: %w", readAPIError(resp))
	}

	var dto sessionDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		return nil, fmt.Errorf("decode session response: %w", err)
	}

	info := &protocol.SessionInfo{
		ID:        dto.ID,
		Command:   dto.Command,
		Args:      dto.Args,
		Status:    dto.Status,
		PID:       dto.PID,
		CreatedAt: dto.StartTime,
	}
	return info, nil
}

// AttachSession establishes a WebSocket stream for the given session.
func (c *Client) AttachSession(sessionID string, includeHistory bool) error {
	payload := map[string]any{"include_history": includeHistory}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal attach payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/sessions/%s/attach", c.baseURL, sessionID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.addAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("attach session: %w", readAPIError(resp))
	}

	var attach attachResponse
	if err := json.NewDecoder(resp.Body).Decode(&attach); err != nil {
		return fmt.Errorf("decode attach response: %w", err)
	}

	streamURL := attach.StreamURL
	if strings.TrimSpace(streamURL) == "" {
		streamURL = makeWebsocketURL(c.baseURL, sessionID)
	}

	if err := c.openWebSocket(streamURL, sessionID); err != nil {
		return err
	}

	attachMsg := map[string]any{
		"type": "attach",
		"data": sessionID,
	}
	if err := c.writeJSON(attachMsg); err != nil {
		return fmt.Errorf("attach session: %w", err)
	}
	return nil
}

// SendInput forwards user input to the active session.
func (c *Client) SendInput(sessionID string, input []byte) error {
	if len(input) == 0 {
		return nil
	}
	if err := c.ensureAttached(sessionID); err != nil {
		return err
	}

	payload := map[string]any{
		"type": "input",
		"data": map[string]any{
			"sessionId": sessionID,
			"input":     string(input),
		},
	}
	if err := c.writeJSON(payload); err != nil {
		return fmt.Errorf("send input: %w", err)
	}
	return nil
}

// ResizeSession notifies the daemon about host-side terminal size changes.
func (c *Client) ResizeSession(sessionID string, cols, rows int, metadata map[string]any) error {
	if err := c.ensureAttached(sessionID); err != nil {
		return err
	}

	data := map[string]any{
		"sessionId": sessionID,
		"cols":      cols,
		"rows":      rows,
		"source":    string(termresize.SourceHost),
	}
	if len(metadata) > 0 {
		data["meta"] = metadata
	}

	payload := map[string]any{
		"type": "resize",
		"data": data,
	}
	if err := c.writeJSON(payload); err != nil {
		return fmt.Errorf("resize session: %w", err)
	}
	return nil
}

// StreamOutput streams PTY output until completion or error.
func (c *Client) StreamOutput(dst io.Writer) error {
	return c.StreamOutputContext(context.Background(), dst)
}

// StreamOutputContext streams PTY output honouring the provided context.
func (c *Client) StreamOutputContext(ctx context.Context, dst io.Writer) error {
	c.mu.Lock()
	outputCh := c.outputCh
	controlCh := c.controlCh
	errCh := c.errCh
	c.mu.Unlock()

	if outputCh == nil || controlCh == nil || errCh == nil {
		return errors.New("client: no active session stream")
	}

	for {
		select {
		case chunk, ok := <-outputCh:
			if !ok {
				select {
				case err, ok := <-errCh:
					if ok && err != nil {
						return err
					}
				default:
				}
				return nil
			}
			if len(chunk) > 0 {
				if _, err := dst.Write(chunk); err != nil {
					return err
				}
			}
		case ctrl, ok := <-controlCh:
			if !ok {
				return nil
			}
			switch ctrl.kind {
			case controlStop, controlKill:
				return nil
			case controlError:
				if ctrl.message == "" {
					ctrl.message = "session error"
				}
				return errors.New(ctrl.message)
			}
		case err, ok := <-errCh:
			if ok && err != nil {
				return err
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// GetDaemonStatus fetches daemon metadata via REST.
func (c *Client) GetDaemonStatus() (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/daemon/status", nil)
	if err != nil {
		return nil, err
	}
	c.addAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon status: %w", readAPIError(resp))
	}

	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decode status: %w", err)
	}
	return status, nil
}

// PluginWarning represents a plugin that was skipped during discovery.
type PluginWarning struct {
	Dir   string `json:"dir"`
	Error string `json:"error"`
}

// GetPluginWarnings returns plugin discovery warnings from the daemon.
func (c *Client) GetPluginWarnings() (int, []PluginWarning, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/plugins/warnings", nil)
	if err != nil {
		return 0, nil, err
	}
	c.addAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, nil, fmt.Errorf("get plugin warnings: %w", readAPIError(resp))
	}

	var result struct {
		Count    int             `json:"count"`
		Warnings []PluginWarning `json:"warnings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, nil, fmt.Errorf("decode warnings: %w", err)
	}
	return result.Count, result.Warnings, nil
}

// ListSessions returns all sessions via REST.
func (c *Client) ListSessions() ([]protocol.SessionInfo, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/sessions", nil)
	if err != nil {
		return nil, err
	}
	c.addAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list sessions: %w", readAPIError(resp))
	}

	var dtos []sessionDTO
	if err := json.NewDecoder(resp.Body).Decode(&dtos); err != nil {
		return nil, fmt.Errorf("decode sessions: %w", err)
	}

	out := make([]protocol.SessionInfo, 0, len(dtos))
	for _, dto := range dtos {
		out = append(out, protocol.SessionInfo{
			ID:        dto.ID,
			Command:   dto.Command,
			Args:      append([]string(nil), dto.Args...),
			Status:    dto.Status,
			PID:       dto.PID,
			CreatedAt: dto.StartTime,
		})
	}
	return out, nil
}

// KillSession requests termination of a session via REST.
func (c *Client) KillSession(sessionID string) error {
	req, err := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/sessions/%s", c.baseURL, sessionID), nil)
	if err != nil {
		return err
	}
	c.addAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("kill session: %w", readAPIError(resp))
	}
	return nil
}

// ShutdownDaemon requests a graceful daemon shutdown via the HTTP API.
func (c *Client) ShutdownDaemon() error {
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/daemon/shutdown", http.NoBody)
	if err != nil {
		return err
	}
	c.addAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted {
		return nil
	}

	errResp := readAPIError(resp)
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
		return fmt.Errorf("shutdown daemon: %w: %w", ErrShutdownUnavailable, errResp)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("shutdown daemon unauthorized: %w", errResp)
	}
	return fmt.Errorf("shutdown daemon: %w", errResp)
}

func (c *Client) openWebSocket(streamURL, sessionID string) error {
	u, err := url.Parse(streamURL)
	if err != nil {
		return fmt.Errorf("parse stream url: %w", err)
	}
	if !u.IsAbs() {
		base, err := url.Parse(c.baseURL)
		if err != nil {
			return fmt.Errorf("parse base url: %w", err)
		}
		u = base.ResolveReference(u)
	}

	header := http.Header{}
	if c.token != "" {
		header.Set("Authorization", "Bearer "+c.token)
	}

	conn, _, err := c.dialer.Dial(u.String(), header)
	if err != nil {
		return fmt.Errorf("websocket dial: %w", err)
	}

	outputCh := make(chan []byte, 128)
	controlCh := make(chan streamControl, 8)
	errCh := make(chan error, 1)

	c.mu.Lock()
	if c.wsConn != nil {
		_ = c.wsConn.Close()
	}
	c.wsConn = conn
	c.sessionID = sessionID
	c.outputCh = outputCh
	c.controlCh = controlCh
	c.errCh = errCh
	c.mu.Unlock()

	go c.readLoop(conn, sessionID, outputCh, controlCh, errCh)
	return nil
}

func (c *Client) writeJSON(payload interface{}) error {
	c.mu.Lock()
	conn := c.wsConn
	c.mu.Unlock()

	if conn == nil {
		return errors.New("client: websocket connection not established")
	}

	c.wsWriteMu.Lock()
	defer c.wsWriteMu.Unlock()

	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteJSON(payload)
}

func (c *Client) readLoop(conn *websocket.Conn, sessionID string, outputCh chan<- []byte, controlCh chan<- streamControl, errCh chan<- error) {
	defer close(outputCh)
	defer close(controlCh)
	defer close(errCh)

	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			if !isNormalClose(err) {
				errCh <- err
			}
			return
		}

		switch messageType {
		case websocket.BinaryMessage:
			if err := c.handleBinaryFrame(payload, sessionID, outputCh); err != nil {
				errCh <- err
				return
			}
		case websocket.TextMessage:
			if err := c.handleJSONMessage(payload, sessionID, controlCh, errCh); err != nil {
				errCh <- err
				return
			}
		}
	}
}

func (c *Client) handleBinaryFrame(frame []byte, sessionID string, outputCh chan<- []byte) error {
	id, payload, err := decodeBinaryFrame(frame)
	if err != nil {
		return err
	}
	if id != "" && id != sessionID {
		return nil
	}
	if len(payload) == 0 {
		return nil
	}
	select {
	case outputCh <- payload:
	default:
	}
	return nil
}

func (c *Client) handleJSONMessage(frame []byte, sessionID string, controlCh chan<- streamControl, errCh chan<- error) error {
	var msg wsInbound
	if err := json.Unmarshal(frame, &msg); err != nil {
		return fmt.Errorf("decode websocket message: %w", err)
	}

	switch strings.ToLower(msg.Type) {
	case "output":
		var payload map[string]string
		if err := json.Unmarshal(msg.Data, &payload); err != nil {
			return nil
		}
		if text := payload["data"]; text != "" {
			select {
			case errCh <- errors.New(text):
			default:
			}
		}
	case "status":
		var status string
		if err := json.Unmarshal(msg.Data, &status); err != nil {
			return nil
		}
		if strings.EqualFold(status, "stopped") {
			c.pushControl(controlCh, streamControl{kind: controlStop})
		}
	case "session_killed":
		if msg.SessionID == "" || msg.SessionID == sessionID {
			c.pushControl(controlCh, streamControl{kind: controlKill})
		}
	case "error":
		var details string
		if len(msg.Data) > 0 {
			if err := json.Unmarshal(msg.Data, &details); err != nil {
				details = strings.TrimSpace(string(msg.Data))
			}
		}
		if details == "" {
			details = "session error"
		}
		c.pushControl(controlCh, streamControl{kind: controlError, message: details})
	}

	return nil
}

func (c *Client) pushControl(ch chan<- streamControl, ctrl streamControl) {
	select {
	case ch <- ctrl:
	default:
	}
}

func (c *Client) ensureAttached(sessionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wsConn == nil {
		return errors.New("client: no active session attached")
	}
	if sessionID != "" && c.sessionID != sessionID {
		return fmt.Errorf("client: session %s not attached (current %s)", sessionID, c.sessionID)
	}
	return nil
}

func decodeBinaryFrame(frame []byte) (string, []byte, error) {
	if len(frame) < frameHeaderSize {
		return "", nil, fmt.Errorf("frame too short")
	}
	if frame[0] != frameMagic || frame[1] != frameTypeOutput {
		return "", nil, fmt.Errorf("unexpected frame header")
	}
	idLen := int(binary.LittleEndian.Uint16(frame[2:4]))
	if frameHeaderSize+idLen > len(frame) {
		return "", nil, fmt.Errorf("invalid frame length")
	}
	sessionID := string(frame[frameHeaderSize : frameHeaderSize+idLen])
	payload := make([]byte, len(frame)-frameHeaderSize-idLen)
	copy(payload, frame[frameHeaderSize+idLen:])
	return sessionID, payload, nil
}

func isNormalClose(err error) bool {
	if err == nil {
		return true
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	return false
}

func makeWebsocketURL(base, sessionID string) string {
	if base == "" || sessionID == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/ws"
	query := u.Query()
	query.Set("s", sessionID)
	u.RawQuery = query.Encode()
	return u.String()
}

func (c *Client) addAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func (c *Client) streamingHTTPClient() *http.Client {
	if c.httpClient == nil {
		return &http.Client{Timeout: 0}
	}
	c.streamOnce.Do(func() {
		clone := *c.httpClient
		clone.Timeout = 0
		c.streamHTTPClient = &clone
	})
	return c.streamHTTPClient
}
