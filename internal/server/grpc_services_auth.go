package server

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type authService struct {
	apiv1.UnimplementedAuthServiceServer
	api *APIServer
}

func newAuthService(api *APIServer) *authService {
	return &authService{api: api}
}

func (a *authService) ListTokens(ctx context.Context, _ *apiv1.ListTokensRequest) (*apiv1.ListTokensResponse, error) {
	if _, err := a.api.requireRoleGRPC(ctx, roleAdmin); err != nil {
		return nil, err
	}

	tokens, err := a.api.loadAuthTokens(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load tokens: %v", err)
	}

	sort.Slice(tokens, func(i, j int) bool {
		return tokens[i].CreatedAt.Before(tokens[j].CreatedAt)
	})

	resp := &apiv1.ListTokensResponse{
		Tokens: make([]*apiv1.TokenInfo, 0, len(tokens)),
	}
	for _, tok := range tokens {
		var createdAt *timestamppb.Timestamp
		if !tok.CreatedAt.IsZero() {
			createdAt = timestamppb.New(tok.CreatedAt.UTC())
		}
		resp.Tokens = append(resp.Tokens, &apiv1.TokenInfo{
			Id:          tok.ID,
			Name:        tok.Name,
			Role:        tok.Role,
			MaskedValue: maskToken(tok.Token),
			CreatedAt:   createdAt,
		})
	}

	return resp, nil
}

func (a *authService) CreateToken(ctx context.Context, req *apiv1.CreateTokenRequest) (*apiv1.CreateTokenResponse, error) {
	if _, err := a.api.requireRoleGRPC(ctx, roleAdmin); err != nil {
		return nil, err
	}

	tokens, err := a.api.loadAuthTokens(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load tokens: %v", err)
	}

	tokenValue, err := generateAPIToken()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate token: %v", err)
	}

	entry := newStoredToken(tokenValue, req.GetName(), req.GetRole())
	if entry.Token == "" {
		return nil, status.Error(codes.Internal, "failed to create token entry")
	}

	tokens = append(tokens, entry)
	if err := a.api.storeAuthTokens(ctx, tokens); err != nil {
		return nil, status.Errorf(codes.Internal, "persist token: %v", err)
	}

	a.api.setAuthTokens(tokens, a.api.AuthRequired())

	var createdAt *timestamppb.Timestamp
	if !entry.CreatedAt.IsZero() {
		createdAt = timestamppb.New(entry.CreatedAt.UTC())
	}

	return &apiv1.CreateTokenResponse{
		Token: tokenValue,
		Info: &apiv1.TokenInfo{
			Id:          entry.ID,
			Name:        entry.Name,
			Role:        entry.Role,
			MaskedValue: maskToken(tokenValue),
			CreatedAt:   createdAt,
		},
	}, nil
}

func (a *authService) DeleteToken(ctx context.Context, req *apiv1.DeleteTokenRequest) (*emptypb.Empty, error) {
	if _, err := a.api.requireRoleGRPC(ctx, roleAdmin); err != nil {
		return nil, err
	}

	id := strings.TrimSpace(req.GetId())
	tokenValue := strings.TrimSpace(req.GetToken())
	if id == "" && tokenValue == "" {
		return nil, status.Error(codes.InvalidArgument, "id or token is required")
	}

	tokens, err := a.api.loadAuthTokens(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load tokens: %v", err)
	}

	newTokens := make([]storedToken, 0, len(tokens))
	removed := false
	for _, tok := range tokens {
		if (id != "" && tok.ID == id) || (tokenValue != "" && tok.Token == tokenValue) {
			removed = true
			continue
		}
		newTokens = append(newTokens, tok)
	}

	if !removed {
		return nil, status.Error(codes.NotFound, "token not found")
	}

	if err := a.api.storeAuthTokens(ctx, newTokens); err != nil {
		return nil, status.Errorf(codes.Internal, "persist tokens: %v", err)
	}

	a.api.setAuthTokens(newTokens, a.api.AuthRequired())
	return &emptypb.Empty{}, nil
}

func (a *authService) ListPairings(ctx context.Context, _ *apiv1.ListPairingsRequest) (*apiv1.ListPairingsResponse, error) {
	if _, err := a.api.requireRoleGRPC(ctx, roleAdmin); err != nil {
		return nil, err
	}

	pairings, err := a.api.loadPairings(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load pairings: %v", err)
	}

	// Persist cleanup of expired pairings.
	if err := a.api.storePairings(ctx, pairings); err != nil {
		log.Printf("[gRPC] failed to persist pairings cleanup: %v", err)
	}

	sort.Slice(pairings, func(i, j int) bool {
		return pairings[i].ExpiresAt.Before(pairings[j].ExpiresAt)
	})

	resp := &apiv1.ListPairingsResponse{
		Pairings: make([]*apiv1.PairingInfo, 0, len(pairings)),
	}
	for _, p := range pairings {
		resp.Pairings = append(resp.Pairings, pairingToProto(p))
	}
	return resp, nil
}

func (a *authService) CreatePairing(ctx context.Context, req *apiv1.CreatePairingRequest) (*apiv1.CreatePairingResponse, error) {
	if _, err := a.api.requireRoleGRPC(ctx, roleAdmin); err != nil {
		return nil, err
	}

	pairings, err := a.api.loadPairings(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load pairings: %v", err)
	}

	duration := time.Duration(req.GetExpiresInSeconds()) * time.Second
	if duration <= 0 || duration > 30*time.Minute {
		duration = 5 * time.Minute
	}

	code, err := generatePairingCode()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate pairing code: %v", err)
	}

	now := time.Now().UTC()
	entry := pairingEntry{
		Code:      code,
		Name:      strings.TrimSpace(req.GetName()),
		Role:      normalizeRole(req.GetRole()),
		CreatedAt: now,
		ExpiresAt: now.Add(duration),
	}

	pairings = append(pairings, entry)
	pairings = sanitizePairings(pairings, now)
	if err := a.api.storePairings(ctx, pairings); err != nil {
		return nil, status.Errorf(codes.Internal, "persist pairing: %v", err)
	}

	return &apiv1.CreatePairingResponse{
		Code:      entry.Code,
		Name:      entry.Name,
		Role:      entry.Role,
		ExpiresAt: timestamppb.New(entry.ExpiresAt),
	}, nil
}

func (a *authService) ClaimPairing(ctx context.Context, req *apiv1.ClaimPairingRequest) (*apiv1.ClaimPairingResponse, error) {
	// ClaimPairing is auth-exempt — no requireRoleGRPC check.

	code := strings.ToUpper(strings.TrimSpace(req.GetCode()))
	if code == "" {
		return nil, status.Error(codes.InvalidArgument, "code is required")
	}

	pairings, err := a.api.loadPairings(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load pairings: %v", err)
	}

	now := time.Now().UTC()
	index := -1
	var entry pairingEntry
	for i, p := range pairings {
		if strings.EqualFold(p.Code, code) {
			entry = p
			index = i
			break
		}
	}

	if index == -1 {
		return nil, status.Error(codes.NotFound, "pairing code not found")
	}

	// Remove the pairing code (one-time use).
	pairings = append(pairings[:index], pairings[index+1:]...)
	pairings = sanitizePairings(pairings, now)
	if err := a.api.storePairings(ctx, pairings); err != nil {
		return nil, status.Errorf(codes.Internal, "persist pairings: %v", err)
	}

	if now.After(entry.ExpiresAt) {
		return nil, status.Error(codes.FailedPrecondition, "pairing code expired")
	}

	tokens, err := a.api.loadAuthTokens(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load tokens: %v", err)
	}

	tokenValue, err := generateAPIToken()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate token: %v", err)
	}

	name := strings.TrimSpace(req.GetClientName())
	if name == "" {
		name = entry.Name
	}

	newEntry := newStoredToken(tokenValue, name, entry.Role)
	if newEntry.Token == "" {
		return nil, status.Error(codes.Internal, "failed to create token entry")
	}

	tokens = append(tokens, newEntry)
	if err := a.api.storeAuthTokens(ctx, tokens); err != nil {
		return nil, status.Errorf(codes.Internal, "persist tokens: %v", err)
	}

	a.api.setAuthTokens(tokens, a.api.AuthRequired())

	// Populate daemon connection info.
	var daemonHost string
	var daemonPort int32
	var tlsEnabled bool
	if a.api.runtime != nil {
		daemonPort = int32(a.api.runtime.Port())
	}
	if a.api.configStore != nil {
		cfg, err := a.api.configStore.GetTransportConfig(ctx)
		if err == nil {
			daemonHost = cfg.Binding
			cert := strings.TrimSpace(cfg.TLSCertPath)
			key := strings.TrimSpace(cfg.TLSKeyPath)
			if cert != "" && key != "" {
				tlsEnabled = true
			}
		}
	}

	return &apiv1.ClaimPairingResponse{
		Token:      tokenValue,
		DaemonHost: daemonHost,
		DaemonPort: daemonPort,
		Tls:        tlsEnabled,
		Name:       newEntry.Name,
		Role:       newEntry.Role,
		CreatedAt:  timestamppb.New(newEntry.CreatedAt.UTC()),
	}, nil
}

func pairingToProto(p pairingEntry) *apiv1.PairingInfo {
	info := &apiv1.PairingInfo{
		Code: p.Code,
		Name: p.Name,
		Role: p.Role,
	}
	if !p.ExpiresAt.IsZero() {
		info.ExpiresAt = timestamppb.New(p.ExpiresAt.UTC())
	}
	if !p.CreatedAt.IsZero() {
		info.CreatedAt = timestamppb.New(p.CreatedAt.UTC())
	}
	return info
}
