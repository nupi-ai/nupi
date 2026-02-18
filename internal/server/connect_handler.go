package server

import (
	"sync"

	"connectrpc.com/vanguard"
	"connectrpc.com/vanguard/vanguardgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
	"google.golang.org/protobuf/encoding/protojson"
)

var registerJSONCodecOnce sync.Once

// NewConnectTranscoder wraps the given gRPC server with a Vanguard transcoder
// that exposes all registered services via Connect RPC (HTTP/JSON) protocol.
// This enables mobile and web clients to call unary RPCs using standard Fetch API.
//
// The transcoder reuses the same service implementations and interceptors
// (auth, language) already registered on the gRPC server — zero business logic
// duplication.
func NewConnectTranscoder(grpcServer *grpc.Server) (*vanguard.Transcoder, error) {
	registerJSONCodecOnce.Do(func() {
		encoding.RegisterCodec(vanguardgrpc.NewCodec(&vanguard.JSONCodec{
			MarshalOptions: protojson.MarshalOptions{
				EmitUnpopulated: true,
			},
			UnmarshalOptions: protojson.UnmarshalOptions{
				DiscardUnknown: true,
			},
		}))
	})

	return vanguardgrpc.NewTranscoder(grpcServer)
}
