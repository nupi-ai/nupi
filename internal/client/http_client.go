package client

import (
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

// Client wraps HTTP calls to the daemon.
type Client struct {
	httpClient *http.Client
	baseURL    string
	token      string
}

// NewInitialisedClient constructs a client from explicit parameters.
func NewInitialisedClient(baseURL, token string, transport http.RoundTripper) *Client {
	trimmed := strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: defaultHTTPTimeout}
	if transport != nil {
		client.Transport = transport
	}

	return &Client{
		baseURL:    trimmed,
		token:      strings.TrimSpace(token),
		httpClient: client,
	}
}

// BaseURL returns the base HTTP URL the client is configured to use.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// HTTPClient exposes the configured HTTP client (including TLS settings).
func (c *Client) HTTPClient() *http.Client {
	return c.httpClient
}

// Token returns the bearer token configured for the client, if any.
func (c *Client) Token() string {
	return c.token
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

func (c *Client) addAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func readAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if len(body) == 0 {
		return errors.New(resp.Status)
	}
	return errors.New(strings.TrimSpace(string(body)))
}
