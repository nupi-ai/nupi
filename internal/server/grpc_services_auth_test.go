package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/eventbus"
)

type runtimeStubWithPorts struct {
	grpcPort    int
	connectPort int
}

func (r runtimeStubWithPorts) GRPCPort() int        { return r.grpcPort }
func (r runtimeStubWithPorts) ConnectPort() int     { return r.connectPort }
func (r runtimeStubWithPorts) StartTime() time.Time { return time.Unix(0, 0) }

func TestPairingCreatedEventPublished(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)

	sub := eventbus.SubscribeTo(bus, eventbus.Pairing.Created)
	defer sub.Close()

	service := newAuthService(apiServer)
	ctx := context.WithValue(context.Background(), authContextKey{}, storedToken{Role: string(roleAdmin)})

	resp, err := service.CreatePairing(ctx, &apiv1.CreatePairingRequest{
		Name: "iPhone",
		Role: "admin",
	})
	if err != nil {
		t.Fatalf("CreatePairing returned error: %v", err)
	}
	if resp.GetCode() == "" {
		t.Fatal("expected non-empty pairing code")
	}
	if resp.GetConnectUrl() == "" {
		t.Fatal("expected non-empty connect_url in response")
	}

	evt := recvPairingEvent(t, sub)
	if evt.Code != resp.GetCode() {
		t.Fatalf("event code mismatch: got %q, want %q", evt.Code, resp.GetCode())
	}
	if evt.DeviceName != "iPhone" {
		t.Fatalf("event device_name mismatch: got %q, want %q", evt.DeviceName, "iPhone")
	}
	if evt.Role != resp.GetRole() {
		t.Fatalf("event role mismatch: got %q, want %q", evt.Role, resp.GetRole())
	}
	if evt.ConnectURL == "" {
		t.Fatal("expected non-empty connect_url in event")
	}
	if !strings.HasPrefix(evt.ConnectURL, "nupi://pair?") {
		t.Fatalf("connect_url should start with nupi://pair?, got %q", evt.ConnectURL)
	}
	if !strings.Contains(evt.ConnectURL, "code="+resp.GetCode()) {
		t.Fatalf("connect_url should contain code=%s, got %q", resp.GetCode(), evt.ConnectURL)
	}
	if evt.ExpiresAt.IsZero() {
		t.Fatal("expected non-zero expires_at in event")
	}
	if evt.ExpiresAt.Before(time.Now()) {
		t.Fatalf("expected expires_at in the future, got %v", evt.ExpiresAt)
	}
}

func TestPairingClaimedEventPublished(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)

	service := newAuthService(apiServer)
	adminCtx := context.WithValue(context.Background(), authContextKey{}, storedToken{Role: string(roleAdmin)})

	createResp, err := service.CreatePairing(adminCtx, &apiv1.CreatePairingRequest{
		Name: "My Phone",
		Role: "admin",
	})
	if err != nil {
		t.Fatalf("CreatePairing returned error: %v", err)
	}

	sub := eventbus.SubscribeTo(bus, eventbus.Pairing.Claimed)
	defer sub.Close()

	claimResp, err := service.ClaimPairing(context.Background(), &apiv1.ClaimPairingRequest{
		Code:       createResp.GetCode(),
		ClientName: "My Phone App",
	})
	if err != nil {
		t.Fatalf("ClaimPairing returned error: %v", err)
	}
	if claimResp.GetToken() == "" {
		t.Fatal("expected non-empty token from ClaimPairing")
	}

	evt := recvClaimedEvent(t, sub)
	if evt.Code != strings.ToUpper(createResp.GetCode()) {
		t.Fatalf("event code mismatch: got %q, want %q", evt.Code, createResp.GetCode())
	}
	if evt.DeviceName != "My Phone App" {
		t.Fatalf("event device_name mismatch: got %q, want %q", evt.DeviceName, "My Phone App")
	}
	if evt.Role == "" {
		t.Fatal("expected non-empty role in event")
	}
	if evt.ClaimedAt.IsZero() {
		t.Fatal("expected non-zero claimed_at in event")
	}
}

func TestPairingConnectURLFormat(t *testing.T) {
	t.Run("with runtime", func(t *testing.T) {
		apiServer, _ := newTestAPIServer(t)
		// Override runtime with specific ports
		apiServer.runtime = runtimeStubWithPorts{grpcPort: 9090, connectPort: 8080}

		url := buildPairingConnectURL(context.Background(), "ABCDE12345", apiServer)

		if !strings.HasPrefix(url, "nupi://pair?") {
			t.Fatalf("URL should start with nupi://pair?, got %q", url)
		}
		if !strings.Contains(url, "code=ABCDE12345") {
			t.Fatalf("URL should contain code=ABCDE12345, got %q", url)
		}
		if !strings.Contains(url, "port=8080") {
			t.Fatalf("URL should contain port=8080 (ConnectPort, not GRPCPort), got %q", url)
		}
		if !strings.Contains(url, "tls=false") {
			t.Fatalf("URL should contain tls=false, got %q", url)
		}
	})

	t.Run("without runtime", func(t *testing.T) {
		apiServer, _ := newTestAPIServer(t)
		apiServer.runtime = nil

		url := buildPairingConnectURL(context.Background(), "TESTCODE99", apiServer)

		if !strings.HasPrefix(url, "nupi://pair?") {
			t.Fatalf("URL should start with nupi://pair?, got %q", url)
		}
		if !strings.Contains(url, "code=TESTCODE99") {
			t.Fatalf("URL should contain code=TESTCODE99, got %q", url)
		}
		if !strings.Contains(url, "port=0") {
			t.Fatalf("URL should contain port=0 when no runtime, got %q", url)
		}
	})
}

func TestPairingExpiredCodeClaimFails(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	service := newAuthService(apiServer)
	adminCtx := context.WithValue(context.Background(), authContextKey{}, storedToken{Role: string(roleAdmin)})

	// Create pairing with very short TTL (1 second)
	createResp, err := service.CreatePairing(adminCtx, &apiv1.CreatePairingRequest{
		Name:             "Expiring Device",
		Role:             "admin",
		ExpiresInSeconds: 1,
	})
	if err != nil {
		t.Fatalf("CreatePairing returned error: %v", err)
	}

	// Wait for expiry
	time.Sleep(1100 * time.Millisecond)

	_, err = service.ClaimPairing(context.Background(), &apiv1.ClaimPairingRequest{
		Code: createResp.GetCode(),
	})
	if err == nil {
		t.Fatal("expected error when claiming expired pairing code")
	}
	// loadPairings filters expired entries, so the error may be "not found" or "expired"
	errMsg := err.Error()
	if !strings.Contains(errMsg, "expired") && !strings.Contains(errMsg, "not found") {
		t.Fatalf("expected 'expired' or 'not found' in error message, got %v", err)
	}
}

func TestPairingNoEventWithoutBus(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	// eventBus is nil by default in newTestAPIServer

	service := newAuthService(apiServer)
	adminCtx := context.WithValue(context.Background(), authContextKey{}, storedToken{Role: string(roleAdmin)})

	// Should not panic even without event bus
	resp, err := service.CreatePairing(adminCtx, &apiv1.CreatePairingRequest{
		Name: "No Bus Device",
		Role: "admin",
	})
	if err != nil {
		t.Fatalf("CreatePairing without bus returned error: %v", err)
	}
	if resp.GetCode() == "" {
		t.Fatal("expected non-empty code")
	}

	// ClaimPairing without bus should also work
	_, err = service.ClaimPairing(context.Background(), &apiv1.ClaimPairingRequest{
		Code: resp.GetCode(),
	})
	if err != nil {
		t.Fatalf("ClaimPairing without bus returned error: %v", err)
	}
}

func TestPairingConnectURLInResponse(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	apiServer.runtime = runtimeStubWithPorts{grpcPort: 9090, connectPort: 8080}

	service := newAuthService(apiServer)
	adminCtx := context.WithValue(context.Background(), authContextKey{}, storedToken{Role: string(roleAdmin)})

	resp, err := service.CreatePairing(adminCtx, &apiv1.CreatePairingRequest{
		Name: "URL Test",
		Role: "admin",
	})
	if err != nil {
		t.Fatalf("CreatePairing returned error: %v", err)
	}

	url := resp.GetConnectUrl()
	if !strings.HasPrefix(url, "nupi://pair?") {
		t.Fatalf("connect_url should start with nupi://pair?, got %q", url)
	}
	if !strings.Contains(url, fmt.Sprintf("code=%s", resp.GetCode())) {
		t.Fatalf("connect_url should contain code, got %q", url)
	}
	if !strings.Contains(url, "port=8080") {
		t.Fatalf("connect_url should contain port=8080, got %q", url)
	}
}

func recvPairingEvent(t *testing.T, sub *eventbus.TypedSubscription[eventbus.PairingCreatedEvent]) eventbus.PairingCreatedEvent {
	t.Helper()
	select {
	case env := <-sub.C():
		return env.Payload
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for pairing.created event")
	}
	return eventbus.PairingCreatedEvent{}
}

func recvClaimedEvent(t *testing.T, sub *eventbus.TypedSubscription[eventbus.PairingClaimedEvent]) eventbus.PairingClaimedEvent {
	t.Helper()
	select {
	case env := <-sub.C():
		return env.Payload
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for pairing.claimed event")
	}
	return eventbus.PairingClaimedEvent{}
}
