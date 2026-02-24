package notification

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExpoClient_SendSingle(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var received []ExpoMessage
	var decodeErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// validate method, Content-Type, and Accept headers
		// to catch regressions in request construction.
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
			http.Error(w, "wrong content-type", http.StatusUnsupportedMediaType)
			return
		}
		if accept := r.Header.Get("Accept"); accept != "application/json" {
			t.Errorf("expected Accept application/json, got %q", accept)
		}

		var msgs []ExpoMessage
		if err := json.NewDecoder(r.Body).Decode(&msgs); err != nil {
			mu.Lock()
			decodeErr = err
			mu.Unlock()
			http.Error(w, "decode error", http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = msgs
		mu.Unlock()

		resp := ExpoResponse{
			Data: []ExpoTicket{{Status: "ok", ID: "ticket-1"}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewExpoClient(WithExpoURL(srv.URL))
	result, err := client.Send(context.Background(), []ExpoMessage{
		{
			To:    "ExponentPushToken[abc]",
			Title: "Test",
			Body:  "Hello",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	mu.Lock()
	if decodeErr != nil {
		t.Fatalf("server decode error: %v", decodeErr)
	}
	receivedCount := len(received)
	receivedTo := ""
	if receivedCount > 0 {
		receivedTo = received[0].To
	}
	mu.Unlock()

	if receivedCount != 1 {
		t.Fatalf("expected 1 message received by server, got %d", receivedCount)
	}
	if receivedTo != "ExponentPushToken[abc]" {
		t.Errorf("to = %q, want ExponentPushToken[abc]", receivedTo)
	}

	if len(result.Tickets) != 1 {
		t.Fatalf("expected 1 ticket, got %d", len(result.Tickets))
	}
	if result.Tickets[0].Status != "ok" {
		t.Errorf("ticket status = %q, want ok", result.Tickets[0].Status)
	}
	if len(result.DeviceNotRegistered) != 0 {
		t.Errorf("expected 0 DeviceNotRegistered, got %d", len(result.DeviceNotRegistered))
	}
}

func TestExpoClient_DeviceNotRegistered(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ExpoResponse{
			Data: []ExpoTicket{
				{
					Status:  "error",
					Message: "device not registered",
					Details: json.RawMessage(`{"error":"DeviceNotRegistered"}`),
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewExpoClient(WithExpoURL(srv.URL))
	result, err := client.Send(context.Background(), []ExpoMessage{
		{To: "ExponentPushToken[stale]", Title: "Test", Body: "Hello"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(result.DeviceNotRegistered) != 1 {
		t.Fatalf("expected 1 DeviceNotRegistered, got %d", len(result.DeviceNotRegistered))
	}
	if result.DeviceNotRegistered[0] != "ExponentPushToken[stale]" {
		t.Errorf("stale token = %q, want ExponentPushToken[stale]", result.DeviceNotRegistered[0])
	}
}

func TestExpoClient_EmptyMessages(t *testing.T) {
	t.Parallel()

	client := NewExpoClient()
	result, err := client.Send(context.Background(), nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(result.Tickets) != 0 {
		t.Errorf("expected 0 tickets for empty input, got %d", len(result.Tickets))
	}
}

func TestExpoClient_ServerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	client := NewExpoClient(
		WithExpoURL(srv.URL),
		withRetryBackoffs([]time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}),
	)
	_, err := client.Send(context.Background(), []ExpoMessage{
		{To: "ExponentPushToken[abc]", Title: "Test", Body: "Hello"},
	})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestExpoClient_RetryOn5xx(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requestCount.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("internal error"))
			return
		}
		resp := ExpoResponse{
			Data: []ExpoTicket{{Status: "ok", ID: "ticket-retry"}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewExpoClient(
		WithExpoURL(srv.URL),
		withRetryBackoffs([]time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}),
	)
	result, err := client.Send(context.Background(), []ExpoMessage{
		{To: "ExponentPushToken[abc]", Title: "Test", Body: "Hello"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if rc := requestCount.Load(); rc != 3 {
		t.Errorf("expected 3 requests (2 retries + 1 success), got %d", rc)
	}
	if len(result.Tickets) != 1 {
		t.Fatalf("expected 1 ticket, got %d", len(result.Tickets))
	}
	if result.Tickets[0].Status != "ok" {
		t.Errorf("ticket status = %q, want ok", result.Tickets[0].Status)
	}
}

func TestExpoClient_Batching(t *testing.T) {
	t.Parallel()

	var batchCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batchCount.Add(1)
		var msgs []ExpoMessage
		json.NewDecoder(r.Body).Decode(&msgs)

		tickets := make([]ExpoTicket, len(msgs))
		for i := range msgs {
			tickets[i] = ExpoTicket{Status: "ok", ID: "ticket"}
		}
		resp := ExpoResponse{Data: tickets}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewExpoClient(WithExpoURL(srv.URL))

	// Send 150 messages — should batch into 2 requests (100 + 50).
	messages := make([]ExpoMessage, 150)
	for i := range messages {
		messages[i] = ExpoMessage{To: "ExponentPushToken[x]", Title: "Test"}
	}

	result, err := client.Send(context.Background(), messages)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if bc := batchCount.Load(); bc != 2 {
		t.Errorf("expected 2 batches, got %d", bc)
	}
	if len(result.Tickets) != 150 {
		t.Errorf("expected 150 tickets, got %d", len(result.Tickets))
	}
}

// verify that WithAccessToken sends the Authorization header.
func TestExpoClient_AccessToken(t *testing.T) {
	t.Parallel()

	var gotAuth string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()

		resp := ExpoResponse{Data: []ExpoTicket{{Status: "ok", ID: "t"}}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewExpoClient(WithExpoURL(srv.URL), WithAccessToken("test-secret-token"))
	_, err := client.Send(context.Background(), []ExpoMessage{
		{To: "ExponentPushToken[abc]", Title: "Test"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := "Bearer test-secret-token"; gotAuth != want {
		t.Errorf("Authorization header = %q, want %q", gotAuth, want)
	}
}

// verify that no Authorization header is sent when access token is empty.
func TestExpoClient_NoAccessToken(t *testing.T) {
	t.Parallel()

	var gotAuth string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()

		resp := ExpoResponse{Data: []ExpoTicket{{Status: "ok", ID: "t"}}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewExpoClient(WithExpoURL(srv.URL))
	_, err := client.Send(context.Background(), []ExpoMessage{
		{To: "ExponentPushToken[abc]", Title: "Test"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "" {
		t.Errorf("expected no Authorization header, got %q", gotAuth)
	}
}

// TestExpoClient_MixedBatchResponse verifies correct handling of a batch that
// returns a mix of "ok" tickets, DeviceNotRegistered errors, and other errors.
func TestExpoClient_MixedBatchResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ExpoResponse{
			Data: []ExpoTicket{
				{Status: "ok", ID: "ticket-1"},
				{Status: "error", Message: "device not registered", Details: json.RawMessage(`{"error":"DeviceNotRegistered"}`)},
				{Status: "ok", ID: "ticket-3"},
				{Status: "error", Message: "rate limited", Details: json.RawMessage(`{"error":"MessageRateExceeded"}`)},
				{Status: "ok", ID: "ticket-5"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewExpoClient(WithExpoURL(srv.URL))
	messages := []ExpoMessage{
		{To: "ExponentPushToken[a]", Title: "Test"},
		{To: "ExponentPushToken[stale]", Title: "Test"},
		{To: "ExponentPushToken[b]", Title: "Test"},
		{To: "ExponentPushToken[c]", Title: "Test"},
		{To: "ExponentPushToken[d]", Title: "Test"},
	}

	result, err := client.Send(context.Background(), messages)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(result.Tickets) != 5 {
		t.Fatalf("expected 5 tickets, got %d", len(result.Tickets))
	}

	// Verify ok count.
	var okCount int
	for _, ticket := range result.Tickets {
		if ticket.Status == "ok" {
			okCount++
		}
	}
	if okCount != 3 {
		t.Errorf("expected 3 ok tickets, got %d", okCount)
	}

	// Verify DeviceNotRegistered detection (only the stale token).
	if len(result.DeviceNotRegistered) != 1 {
		t.Fatalf("expected 1 DeviceNotRegistered, got %d", len(result.DeviceNotRegistered))
	}
	if result.DeviceNotRegistered[0] != "ExponentPushToken[stale]" {
		t.Errorf("DeviceNotRegistered[0] = %q, want ExponentPushToken[stale]", result.DeviceNotRegistered[0])
	}
}

// TestExpoClient_MalformedTicketDetails verifies that malformed JSON in ticket
// details doesn't cause panics and that non-DeviceNotRegistered errors are
// counted correctly.
func TestExpoClient_MalformedTicketDetails(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Details is a JSON string instead of a JSON object, so
		// json.Unmarshal into ExpoTicketError (which expects an object) fails.
		resp := ExpoResponse{
			Data: []ExpoTicket{
				{Status: "error", Message: "bad details", Details: json.RawMessage(`"unexpected string"`)},
				{Status: "ok", ID: "ticket-2"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewExpoClient(WithExpoURL(srv.URL))
	result, err := client.Send(context.Background(), []ExpoMessage{
		{To: "ExponentPushToken[a]", Title: "Test"},
		{To: "ExponentPushToken[b]", Title: "Test"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(result.Tickets) != 2 {
		t.Fatalf("expected 2 tickets, got %d", len(result.Tickets))
	}

	// Malformed details should NOT produce DeviceNotRegistered entries.
	if len(result.DeviceNotRegistered) != 0 {
		t.Errorf("expected 0 DeviceNotRegistered, got %d", len(result.DeviceNotRegistered))
	}
}

// TestExpoClient_TimeoutEnforcement verifies that a hanging Expo API server
// does not block the client indefinitely. The HTTP client timeout (or context
// cancellation) should cause Send to return an error within a reasonable time.
func TestExpoClient_TimeoutEnforcement(t *testing.T) {
	t.Parallel()

	// Server that never responds until signalled.
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-done
	}))
	t.Cleanup(func() {
		close(done) // unblock the handler goroutine so srv.Close() can drain
		srv.Close()
	})

	client := NewExpoClient(
		WithExpoURL(srv.URL),
		// Short HTTP timeout to keep test fast.
		WithHTTPClient(&http.Client{Timeout: 200 * time.Millisecond}),
		// No retries — single attempt is sufficient to verify timeout.
		withRetryBackoffs(nil),
	)

	start := time.Now()
	_, err := client.Send(context.Background(), []ExpoMessage{
		{To: "ExponentPushToken[test-timeout-abcdef1234]", Title: "Timeout test"},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from hanging server, got nil")
	}
	// Should complete well within 2s (200ms timeout + margin).
	if elapsed > 2*time.Second {
		t.Errorf("Send took %v, expected it to timeout within 2s", elapsed)
	}
}
