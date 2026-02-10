// Package napdial provides shared gRPC dialing utilities for NAP adapter clients.
//
// All NAP adapter clients (AI, STT, TTS, VAD) use this package to establish
// gRPC connections. This centralizes transport credential configuration,
// making it the single point for future TLS support.
//
// Currently all local adapters use insecure credentials (process transport
// communicates over localhost). When remote/tunnel deployment is supported,
// TLS will be configured here based on the adapter endpoint settings.
package napdial

import (
	"context"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type dialerContextKey struct{}

// ContextWithDialer attaches a custom dialer to the context.
// This is primarily for tests using bufconn without real network sockets.
func ContextWithDialer(ctx context.Context, dialer func(context.Context, string) (net.Conn, error)) context.Context {
	if ctx == nil || dialer == nil {
		return ctx
	}
	return context.WithValue(ctx, dialerContextKey{}, dialer)
}

// DialerFromContext extracts a custom dialer from the context, if present.
func DialerFromContext(ctx context.Context) func(context.Context, string) (net.Conn, error) {
	if ctx == nil {
		return nil
	}
	dialer, _ := ctx.Value(dialerContextKey{}).(func(context.Context, string) (net.Conn, error))
	return dialer
}

// DialOptions returns gRPC dial options for connecting to a NAP adapter.
// Includes transport credentials and optional custom dialer from context.
//
// TODO(#NAP-TLS): Add TLS support based on adapter endpoint configuration.
// When implemented, this function should accept TLS parameters from the
// AdapterEndpoint and return credentials.NewTLS() for remote connections.
func DialOptions(ctx context.Context) []grpc.DialOption {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	if dialer := DialerFromContext(ctx); dialer != nil {
		opts = append(opts, grpc.WithContextDialer(dialer))
	}
	return opts
}
