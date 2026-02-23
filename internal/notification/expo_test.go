package notification

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestExpoClient_SendSingle(t *testing.T) {
	t.Parallel()

	var received []ExpoMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}

		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}

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

	if len(received) != 1 {
		t.Fatalf("expected 1 message received by server, got %d", len(received))
	}
	if received[0].To != "ExponentPushToken[abc]" {
		t.Errorf("to = %q, want ExponentPushToken[abc]", received[0].To)
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
