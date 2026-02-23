package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nupi-ai/nupi/internal/constants"
)

const (
	defaultExpoURL    = "https://exp.host/--/api/v2/push/send"
	maxBatchSize      = 100
	defaultHTTPTimeout = constants.Duration10Seconds
)

// ExpoMessage represents a single push notification to send via the Expo Push API.
type ExpoMessage struct {
	To        string            `json:"to"`
	Title     string            `json:"title,omitempty"`
	Body      string            `json:"body,omitempty"`
	Data      map[string]string `json:"data,omitempty"`
	Sound     string            `json:"sound,omitempty"`
	Priority  string            `json:"priority,omitempty"`
	ChannelID string            `json:"channelId,omitempty"`
}

// ExpoTicket is returned by the Expo Push API for each sent message.
type ExpoTicket struct {
	Status  string          `json:"status"` // "ok" or "error"
	ID      string          `json:"id,omitempty"`
	Message string          `json:"message,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

// ExpoTicketError represents the error detail from Expo.
type ExpoTicketError struct {
	Error string `json:"error"` // e.g. "DeviceNotRegistered"
}

// ExpoResponse is the top-level response from the Expo Push API.
type ExpoResponse struct {
	Data   []ExpoTicket `json:"data"`
	Errors []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"errors,omitempty"`
}

// defaultRetryBackoffs defines the delays between retry attempts for transient errors.
var defaultRetryBackoffs = []time.Duration{constants.Duration500Milliseconds, constants.Duration1Second, constants.Duration2Seconds}

// ExpoClient sends push notifications via the Expo Push API.
type ExpoClient struct {
	url            string
	client         *http.Client
	accessToken    string
	retryBackoffs  []time.Duration
}

// ExpoClientOption configures an ExpoClient.
type ExpoClientOption func(*ExpoClient)

// WithExpoURL overrides the Expo Push API endpoint (useful for testing).
func WithExpoURL(url string) ExpoClientOption {
	return func(c *ExpoClient) { c.url = url }
}

// WithHTTPClient overrides the HTTP client used for requests.
func WithHTTPClient(client *http.Client) ExpoClientOption {
	return func(c *ExpoClient) { c.client = client }
}

// WithAccessToken sets the Expo Push API access token for authenticated requests.
func WithAccessToken(token string) ExpoClientOption {
	return func(c *ExpoClient) { c.accessToken = token }
}

// withRetryBackoffs overrides the retry backoff durations (for testing).
func withRetryBackoffs(backoffs []time.Duration) ExpoClientOption {
	return func(c *ExpoClient) { c.retryBackoffs = backoffs }
}

// NewExpoClient creates an Expo Push API client.
func NewExpoClient(opts ...ExpoClientOption) *ExpoClient {
	c := &ExpoClient{
		url: defaultExpoURL,
		client: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
		retryBackoffs: defaultRetryBackoffs,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// SendResult contains the outcome of sending a batch of notifications.
type SendResult struct {
	Tickets             []ExpoTicket
	DeviceNotRegistered []string // tokens that should be cleaned up
}

// Send sends push notifications in batches of up to 100. Returns tickets
// for each message and a list of tokens that returned DeviceNotRegistered.
func (c *ExpoClient) Send(ctx context.Context, messages []ExpoMessage) (*SendResult, error) {
	if len(messages) == 0 {
		return &SendResult{}, nil
	}

	result := &SendResult{}

	for i := 0; i < len(messages); i += maxBatchSize {
		end := i + maxBatchSize
		if end > len(messages) {
			end = len(messages)
		}
		batch := messages[i:end]

		tickets, batchErr := c.sendBatch(ctx, batch)
		if batchErr != nil && len(tickets) == 0 {
			return result, fmt.Errorf("notification: send batch %d-%d: %w", i, end, batchErr)
		}

		for j, ticket := range tickets {
			if ticket.Status == "error" {
				var detail ExpoTicketError
				if err := json.Unmarshal(ticket.Details, &detail); err == nil {
					if detail.Error == "DeviceNotRegistered" && i+j < len(messages) {
						result.DeviceNotRegistered = append(result.DeviceNotRegistered, messages[i+j].To)
					}
				}
			}
		}

		result.Tickets = append(result.Tickets, tickets...)
	}

	return result, nil
}

func (c *ExpoClient) sendBatch(ctx context.Context, messages []ExpoMessage) ([]ExpoTicket, error) {
	body, err := json.Marshal(messages)
	if err != nil {
		return nil, fmt.Errorf("marshal messages: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= len(c.retryBackoffs); attempt++ {
		// Wait before retrying (skip on first attempt).
		if attempt > 0 {
			delay := c.retryBackoffs[attempt-1]
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		tickets, err, shouldRetry := c.doSendBatch(ctx, body)
		if err == nil {
			return tickets, nil
		}
		lastErr = err
		if !shouldRetry {
			return nil, err
		}
	}
	return nil, lastErr
}

// doSendBatch performs a single HTTP request to the Expo Push API.
// Returns (tickets, error, shouldRetry). shouldRetry is true only for
// network errors and HTTP 5xx status codes.
func (c *ExpoClient) doSendBatch(ctx context.Context, body []byte) ([]ExpoTicket, error, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err), false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		// Network error — retryable.
		return nil, fmt.Errorf("send request: %w", err), true
	}
	defer resp.Body.Close()

	const maxResponseSize = 1 << 20 // 1MB
	limited := io.LimitReader(resp.Body, maxResponseSize+1)
	respBody, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err), false
	}
	if len(respBody) > maxResponseSize {
		return nil, fmt.Errorf("expo API response exceeds %d bytes", maxResponseSize), false
	}

	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode >= 500
		return nil, fmt.Errorf("expo API returned status %d: %s", resp.StatusCode, string(respBody)), retryable
	}

	var expoResp ExpoResponse
	if err := json.Unmarshal(respBody, &expoResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err), false
	}

	if len(expoResp.Errors) > 0 {
		msgs := make([]string, len(expoResp.Errors))
		for i, e := range expoResp.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("expo API errors: %s", strings.Join(msgs, "; ")), false
	}

	return expoResp.Data, nil, false
}
