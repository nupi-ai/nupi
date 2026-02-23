package server

import (
	"context"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
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

	a.api.tokenOpsMu.Lock()
	defer a.api.tokenOpsMu.Unlock()

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

	a.api.tokenOpsMu.Lock()
	defer a.api.tokenOpsMu.Unlock()

	id := strings.TrimSpace(req.GetId())
	tokenValue := strings.TrimSpace(req.GetToken())
	if id == "" && tokenValue == "" {
		return nil, status.Error(codes.InvalidArgument, "id or token is required")
	}

	tokens, err := a.api.loadAuthTokens(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load tokens: %v", err)
	}

	// Fallback logic per proto contract: try ID first, then token value.
	matchIdx := -1
	if id != "" {
		for i, tok := range tokens {
			if tok.ID == id {
				matchIdx = i
				break
			}
		}
	}
	if matchIdx == -1 && tokenValue != "" {
		for i, tok := range tokens {
			if tok.Token == tokenValue {
				matchIdx = i
				break
			}
		}
	}

	if matchIdx == -1 {
		return nil, status.Error(codes.NotFound, "token not found")
	}

	removedID := tokens[matchIdx].ID
	newTokens := make([]storedToken, 0, len(tokens)-1)
	for i, tok := range tokens {
		if i != matchIdx {
			newTokens = append(newTokens, tok)
		}
	}

	if err := a.api.storeAuthTokens(ctx, newTokens); err != nil {
		return nil, status.Errorf(codes.Internal, "persist tokens: %v", err)
	}

	a.api.setAuthTokens(newTokens, a.api.AuthRequired())

	// Cascade: remove push tokens registered by the deleted auth token.
	if a.api.configStore != nil {
		if err := a.api.configStore.DeletePushTokensByAuthToken(ctx, removedID); err != nil {
			log.Printf("[Auth] cascade push token cleanup for auth %s: %v", removedID, err)
		}
	}

	return &emptypb.Empty{}, nil
}

func (a *authService) ListPairings(ctx context.Context, _ *apiv1.ListPairingsRequest) (*apiv1.ListPairingsResponse, error) {

	pairings, err := a.api.loadPairings(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load pairings: %v", err)
	}

	// Expired pairings are already filtered by loadPairings. Cleanup is
	// persisted lazily on the next mutating operation (CreatePairing,
	// ClaimPairing) to avoid a TOCTOU race with concurrent writers.

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

	a.api.tokenOpsMu.Lock()
	defer a.api.tokenOpsMu.Unlock()

	pairings, err := a.api.loadPairings(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load pairings: %v", err)
	}

	duration := time.Duration(req.GetExpiresInSeconds()) * time.Second
	if duration <= 0 || duration > constants.Duration30Minutes {
		duration = constants.Duration5Minutes
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

	connectURL := buildPairingConnectURL(ctx, entry.Code, a.api)

	if a.api.eventBus != nil {
		eventbus.Publish(ctx, a.api.eventBus, eventbus.Pairing.Created, eventbus.SourcePairing, eventbus.PairingCreatedEvent{
			Code:       entry.Code,
			DeviceName: entry.Name,
			Role:       entry.Role,
			ConnectURL: connectURL,
			ExpiresAt:  entry.ExpiresAt,
		})
	}

	return &apiv1.CreatePairingResponse{
		Code:       entry.Code,
		Name:       entry.Name,
		Role:       entry.Role,
		ExpiresAt:  timestamppb.New(entry.ExpiresAt),
		ConnectUrl: connectURL,
	}, nil
}

func (a *authService) ClaimPairing(ctx context.Context, req *apiv1.ClaimPairingRequest) (*apiv1.ClaimPairingResponse, error) {
	// ClaimPairing is auth-exempt — no requireRoleGRPC check.

	a.api.tokenOpsMu.Lock()
	defer a.api.tokenOpsMu.Unlock()

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

	if a.api.eventBus != nil {
		eventbus.Publish(ctx, a.api.eventBus, eventbus.Pairing.Claimed, eventbus.SourcePairing, eventbus.PairingClaimedEvent{
			Code:       code,
			DeviceName: name,
			Role:       newEntry.Role,
			ClaimedAt:  time.Now().UTC(),
		})
	}

	// Populate daemon connection info.
	var daemonHost string
	var daemonPort int32
	var tlsEnabled bool
	if a.api.runtime != nil {
		daemonPort = int32(a.api.runtime.GRPCPort())
	}
	if a.api.configStore != nil {
		cfg, err := a.api.configStore.GetTransportConfig(ctx)
		if err == nil {
			cert := strings.TrimSpace(cfg.TLSCertPath)
			key := strings.TrimSpace(cfg.TLSKeyPath)
			if cert != "" && key != "" {
				tlsEnabled = true
			}
			rawGRPC := strings.TrimSpace(cfg.GRPCBinding)
			if rawGRPC == "" {
				rawGRPC = cfg.Binding
			}
			if tlsEnabled {
				daemonHost = "localhost"
			} else if host, err := resolveBindingHost(normalizeBinding(rawGRPC)); err == nil {
				daemonHost = host
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

// buildPairingConnectURL constructs a nupi://pair?... URL containing the
// pairing code and daemon connection details needed by mobile clients.
func buildPairingConnectURL(ctx context.Context, code string, api *APIServer) string {
	host := ""
	port := 0
	tlsEnabled := false

	if api.runtime != nil {
		port = api.runtime.ConnectPort()
	}
	if api.configStore != nil {
		cfg, err := api.configStore.GetTransportConfig(ctx)
		if err == nil {
			cert := strings.TrimSpace(cfg.TLSCertPath)
			key := strings.TrimSpace(cfg.TLSKeyPath)
			if cert != "" && key != "" {
				tlsEnabled = true
			}
			rawBinding := strings.TrimSpace(cfg.Binding)
			if rawBinding == "" {
				rawBinding = cfg.GRPCBinding
			}
			if tlsEnabled {
				host = "localhost"
			} else if h, err := resolveBindingHost(normalizeBinding(rawBinding)); err == nil {
				host = h
			}
		}
	}

	v := url.Values{}
	v.Set("code", code)
	v.Set("host", host)
	v.Set("port", strconv.Itoa(port))
	v.Set("tls", strconv.FormatBool(tlsEnabled))
	return "nupi://pair?" + v.Encode()
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
