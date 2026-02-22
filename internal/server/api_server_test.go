package server

import (
	"context"
	"testing"

	"github.com/nupi-ai/nupi/internal/version"
)

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
