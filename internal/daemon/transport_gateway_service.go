package daemon

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/nupi-ai/nupi/internal/server"
	transportgateway "github.com/nupi-ai/nupi/internal/transport/gateway"
	"google.golang.org/grpc"
)

type gatewayService struct {
	gateway *transportgateway.Gateway
	info    *RuntimeInfo
}

func newGatewayService(api *server.APIServer, info *RuntimeInfo) *gatewayService {
	return &gatewayService{
		gateway: transportgateway.New(api, transportgateway.Options{
			RegisterGRPC: func(srv *grpc.Server) {
				server.RegisterGRPCServices(api, srv)
			},
		}),
		info: info,
	}
}

func (s *gatewayService) Start(ctx context.Context) error {
	info, err := s.gateway.Start(ctx)
	if err != nil {
		return err
	}

	if s.info != nil {
		if info.HTTP.Port > 0 {
			s.info.SetPort(info.HTTP.Port)
			log.Printf("Transport gateway HTTP listening on %s://%s", info.HTTP.Scheme, info.HTTP.Address)
		}
		if info.GRPC.Port > 0 {
			s.info.SetGRPCPort(info.GRPC.Port)
			log.Printf("Transport gateway gRPC listening on %s://%s", info.GRPC.Scheme, info.GRPC.Address)
		}
	}

	return nil
}

func (s *gatewayService) Shutdown(ctx context.Context) error {
	if err := s.gateway.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *gatewayService) Errors() <-chan error {
	return s.gateway.Errors()
}
