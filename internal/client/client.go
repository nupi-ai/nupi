package client

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/nupi-ai/nupi/internal/bootstrap"
)

// Client wraps HTTP calls to the daemon.
type Client struct {
	httpClient *http.Client
	baseURL    string
	token      string
}

// NewInitialisedClient constructs a client from explicit parameters.
func NewInitialisedClient(baseURL, token string, tlsConfig *tls.Config) *Client {
	transport := &http.Transport{}
	if tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig
	}

	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   strings.TrimSpace(token),
		httpClient: &http.Client{
			Timeout:   defaultHTTPTimeout,
			Transport: transport,
		},
	}
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

	return NewInitialisedClient(baseURL, token, tlsConfig), nil
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

	boot, err := bootstrap.Load()
	if err != nil {
		return nil, err
	}

	return newFromExplicitWithBootstrap(boot)
}

func newFromExplicitWithBootstrap(boot *bootstrap.Config) (*Client, error) {
	if boot == nil || strings.TrimSpace(boot.BaseURL) == "" {
		return nil, fmt.Errorf("client: explicit base URL required")
	}

	token, opts := ResolveExplicitOptions(boot)

	u, err := url.Parse(boot.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("client: parse base URL: %w", err)
	}

	tlsConfig, err := TLSConfigForExplicit(u, &opts)
	if err != nil {
		return nil, err
	}

	return NewInitialisedClient(u.String(), token, tlsConfig), nil
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
