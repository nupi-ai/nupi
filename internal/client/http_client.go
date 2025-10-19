package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultHTTPTimeout = 10 * time.Second
	maxErrorBody       = 8 << 10
)

// ErrShutdownUnavailable indicates the daemon does not expose the shutdown endpoint.
var ErrShutdownUnavailable = errors.New("daemon shutdown endpoint unavailable")

// HTTPClient wraps HTTP interactions with the daemon.
type HTTPClient struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPClient builds an HTTP client with optional custom transport.
func NewHTTPClient(baseURL, token string, transport http.RoundTripper) *HTTPClient {
	trimmed := strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: defaultHTTPTimeout}
	if transport != nil {
		client.Transport = transport
	}

	return &HTTPClient{
		client:  client,
		baseURL: trimmed,
		token:   strings.TrimSpace(token),
	}
}

// BaseURL returns the base HTTP URL.
func (c *HTTPClient) BaseURL() string {
	return c.baseURL
}

// Client exposes the underlying http.Client.
func (c *HTTPClient) Client() *http.Client {
	return c.client
}

// Token returns the configured bearer token.
func (c *HTTPClient) Token() string {
	return c.token
}

// ShutdownDaemon requests a graceful daemon shutdown via the HTTP API.
func (c *HTTPClient) ShutdownDaemon() error {
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/daemon/shutdown", http.NoBody)
	if err != nil {
		return err
	}
	c.attachToken(req)

	resp, err := c.client.Do(req)
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

func (c *HTTPClient) attachToken(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func readAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if len(body) == 0 {
		return errors.New(resp.Status)
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") {
		var payload struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(trimmed), &payload); err == nil {
			if msg := strings.TrimSpace(payload.Error); msg != "" {
				return errors.New(msg)
			}
		}
		// Fall back to returning the raw payload for diagnostics when parsing fails
		// or the server response omits the "error" field.
	}
	return errors.New(trimmed)
}
