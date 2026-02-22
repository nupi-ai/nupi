package server

import (
	"context"
	"strings"
	"testing"

	"github.com/nupi-ai/nupi/internal/version"
)

func TestNewAPIServerRequiresSessionManager(t *testing.T) {
	store := openTestStore(t)

	server, err := NewAPIServer(nil, store, runtimeStub{})
	if err == nil {
		t.Fatal("expected error when session manager is nil")
	}
	if server != nil {
		t.Fatal("expected nil server on constructor error")
	}
	if !strings.Contains(err.Error(), "session manager is required") {
		t.Fatalf("expected session manager validation error, got %v", err)
	}
}

func TestDaemonStatusReturnsRealVersion(t *testing.T) {
	srv, _ := newTestAPIServer(t)

	snap, err := srv.daemonStatus(context.Background())
	if err != nil {
		t.Fatalf("daemonStatus() error: %v", err)
	}

	if snap.Version != version.String() {
		t.Errorf("daemonStatus().Version = %q, want %q (from version.String())", snap.Version, version.String())
	}
}

func TestDaemonStatusFields(t *testing.T) {
	srv, _ := newTestAPIServer(t)

	snap, err := srv.daemonStatus(context.Background())
	if err != nil {
		t.Fatalf("daemonStatus() error: %v", err)
	}

	// SessionsCount should be 0 when no sessions are active.
	if snap.SessionsCount != 0 {
		t.Errorf("SessionsCount = %d, want 0 (no active sessions)", snap.SessionsCount)
	}

	// AuthRequired is determined by the config store's security settings.
	// With a fresh test store, auth should not be required.
	if snap.AuthRequired {
		t.Error("AuthRequired = true, want false for fresh test server")
	}

	// UptimeSeconds should be non-negative.
	if snap.UptimeSeconds < 0 {
		t.Errorf("UptimeSeconds = %f, want >= 0", snap.UptimeSeconds)
	}
}
