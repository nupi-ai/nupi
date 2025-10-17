package server

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/nupi-ai/nupi/internal/api"
	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/protocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type daemonService struct {
	apiv1.UnimplementedDaemonServiceServer
	api *APIServer
}

func newDaemonService(api *APIServer) *daemonService {
	return &daemonService{api: api}
}

func (d *daemonService) Status(ctx context.Context, _ *apiv1.DaemonStatusRequest) (*apiv1.DaemonStatusResponse, error) {
	snapshot, err := d.api.daemonStatusSnapshot(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to compute daemon status: %v", err)
	}
	return &apiv1.DaemonStatusResponse{
		Version:      snapshot.Version,
		Sessions:     int32(snapshot.SessionsCount),
		Port:         int32(snapshot.Port),
		GrpcPort:     int32(snapshot.GRPCPort),
		Binding:      snapshot.Binding,
		GrpcBinding:  snapshot.GRPCBinding,
		AuthRequired: snapshot.AuthRequired,
		UptimeSec:    snapshot.UptimeSeconds,
	}, nil
}

type sessionsService struct {
	apiv1.UnimplementedSessionsServiceServer
	api *APIServer
}

func newSessionsService(api *APIServer) *sessionsService {
	return &sessionsService{api: api}
}

type configService struct {
	apiv1.UnimplementedConfigServiceServer
	api *APIServer
}

func newConfigService(api *APIServer) *configService {
	return &configService{api: api}
}

type adaptersService struct {
	apiv1.UnimplementedAdaptersServiceServer
	api *APIServer
}

func newAdaptersService(api *APIServer) *adaptersService {
	return &adaptersService{api: api}
}

type quickstartService struct {
	apiv1.UnimplementedQuickstartServiceServer
	api *APIServer
}

func newQuickstartService(api *APIServer) *quickstartService {
	return &quickstartService{api: api}
}

func (s *sessionsService) ListSessions(ctx context.Context, _ *apiv1.ListSessionsRequest) (*apiv1.ListSessionsResponse, error) {
	if _, err := s.api.requireRoleGRPC(ctx, roleAdmin, roleReadOnly); err != nil {
		return nil, err
	}

	sessions := s.api.sessionManager.ListSessions()
	dto := api.ToDTOList(sessions)

	out := make([]*apiv1.Session, 0, len(dto))
	for _, session := range dto {
		out = append(out, &apiv1.Session{
			Id:        session.ID,
			Command:   session.Command,
			Args:      append([]string(nil), session.Args...),
			Status:    session.Status,
			Pid:       int32(session.PID),
			StartUnix: session.StartTime.Unix(),
		})
	}

	return &apiv1.ListSessionsResponse{Sessions: out}, nil
}

func (s *sessionsService) CreateSession(ctx context.Context, req *apiv1.CreateSessionRequest) (*apiv1.CreateSessionResponse, error) {
	if _, err := s.api.requireRoleGRPC(ctx, roleAdmin); err != nil {
		return nil, err
	}

	payload := protocol.CreateSessionData{
		Command:    strings.TrimSpace(req.GetCommand()),
		Args:       append([]string(nil), req.GetArgs()...),
		WorkingDir: strings.TrimSpace(req.GetWorkingDir()),
		Env:        append([]string(nil), req.GetEnv()...),
		Rows:       uint16(req.GetRows()),
		Cols:       uint16(req.GetCols()),
		Detached:   req.GetDetached(),
		Inspect:    req.GetInspect(),
	}

	if payload.Command == "" {
		return nil, status.Error(codes.InvalidArgument, "command is required")
	}

	sess, err := s.api.createSessionFromPayload(payload)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create session: %v", err)
	}

	dto := api.ToDTO(sess)
	sessionPB := &apiv1.Session{
		Id:        dto.ID,
		Command:   dto.Command,
		Args:      append([]string(nil), dto.Args...),
		Status:    dto.Status,
		Pid:       int32(dto.PID),
		StartUnix: dto.StartTime.Unix(),
	}

	return &apiv1.CreateSessionResponse{Session: sessionPB}, nil
}

func (s *sessionsService) KillSession(ctx context.Context, req *apiv1.KillSessionRequest) (*apiv1.KillSessionResponse, error) {
	if _, err := s.api.requireRoleGRPC(ctx, roleAdmin); err != nil {
		return nil, err
	}

	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}

	if err := s.api.sessionManager.KillSession(sessionID); err != nil {
		return nil, status.Errorf(codes.NotFound, "session %s not found", sessionID)
	}

	return &apiv1.KillSessionResponse{}, nil
}

func (s *sessionsService) GetConversation(ctx context.Context, req *apiv1.GetConversationRequest) (*apiv1.GetConversationResponse, error) {
	if _, err := s.api.requireRoleGRPC(ctx, roleAdmin, roleReadOnly); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}

	if s.api.conversation == nil {
		return nil, status.Error(codes.Unavailable, "conversation service unavailable")
	}

	if _, err := s.api.sessionManager.GetSession(sessionID); err != nil {
		return nil, status.Errorf(codes.NotFound, "session %s not found", sessionID)
	}

	offset := int(req.GetOffset())
	limit := int(req.GetLimit())
	if limit < 0 {
		return nil, status.Error(codes.InvalidArgument, "limit must be non-negative")
	}
	if offset < 0 {
		return nil, status.Error(codes.InvalidArgument, "offset must be non-negative")
	}
	if limit > conversationMaxPageLimit {
		limit = conversationMaxPageLimit
	}

	total, turns := s.api.conversation.Slice(sessionID, offset, limit)
	pageLimit := limit
	if pageLimit <= 0 || pageLimit > len(turns) {
		pageLimit = len(turns)
	}

	resp := &apiv1.GetConversationResponse{
		SessionId:  sessionID,
		Turns:      make([]*apiv1.ConversationTurn, 0, len(turns)),
		Offset:     uint32(offset),
		Limit:      uint32(pageLimit),
		Total:      uint32(total),
		HasMore:    offset+len(turns) < total,
		NextOffset: 0,
	}

	for _, turn := range turns {
		var ts *timestamppb.Timestamp
		if !turn.At.IsZero() {
			ts = timestamppb.New(turn.At)
		}

		metadata := make([]*apiv1.ConversationMetadata, 0, len(turn.Meta))
		if len(turn.Meta) > 0 {
			keys := make([]string, 0, len(turn.Meta))
			for k := range turn.Meta {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			metadata = make([]*apiv1.ConversationMetadata, 0, len(keys))
			for _, key := range keys {
				metadata = append(metadata, &apiv1.ConversationMetadata{
					Key:   key,
					Value: turn.Meta[key],
				})
			}
		}

		resp.Turns = append(resp.Turns, &apiv1.ConversationTurn{
			Origin:   string(turn.Origin),
			Text:     turn.Text,
			At:       ts,
			Metadata: metadata,
		})
	}

	if !resp.HasMore {
		resp.NextOffset = 0
	} else {
		resp.NextOffset = uint32(offset + len(turns))
	}

	return resp, nil
}

func RegisterGRPCServices(api *APIServer, registrar grpc.ServiceRegistrar) {
	apiv1.RegisterDaemonServiceServer(registrar, newDaemonService(api))
	apiv1.RegisterSessionsServiceServer(registrar, newSessionsService(api))
	apiv1.RegisterConfigServiceServer(registrar, newConfigService(api))
	apiv1.RegisterAdaptersServiceServer(registrar, newAdaptersService(api))
	apiv1.RegisterQuickstartServiceServer(registrar, newQuickstartService(api))
}

func (c *configService) GetTransportConfig(ctx context.Context, _ *emptypb.Empty) (*apiv1.TransportConfig, error) {
	if c.api.configStore == nil {
		return nil, status.Error(codes.Unavailable, "configuration store unavailable")
	}
	cfg, err := c.api.configStore.GetTransportConfig(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get transport config: %v", err)
	}
	return transportConfigToProto(cfg, c.api.AuthRequired()), nil
}

func (c *configService) UpdateTransportConfig(ctx context.Context, req *apiv1.UpdateTransportConfigRequest) (*apiv1.TransportConfig, error) {
	if c.api.configStore == nil {
		return nil, status.Error(codes.Unavailable, "configuration store unavailable")
	}
	if req == nil || req.Config == nil {
		return nil, status.Error(codes.InvalidArgument, "config is required")
	}

	pbCfg := req.Config
	storeCfg := configstore.TransportConfig{
		Binding:        strings.TrimSpace(pbCfg.GetBinding()),
		Port:           int(pbCfg.GetPort()),
		TLSCertPath:    strings.TrimSpace(pbCfg.GetTlsCertPath()),
		TLSKeyPath:     strings.TrimSpace(pbCfg.GetTlsKeyPath()),
		AllowedOrigins: sanitizeOrigins(pbCfg.GetAllowedOrigins()),
		GRPCPort:       int(pbCfg.GetGrpcPort()),
		GRPCBinding:    strings.TrimSpace(pbCfg.GetGrpcBinding()),
	}
	if strings.TrimSpace(storeCfg.GRPCBinding) == "" {
		storeCfg.GRPCBinding = storeCfg.Binding
	}
	storeCfg.Binding = normalizeBinding(storeCfg.Binding)
	storeCfg.GRPCBinding = normalizeBinding(storeCfg.GRPCBinding)

	if err := validateTransportConfig(storeCfg); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if err := c.api.configStore.SaveTransportConfig(ctx, storeCfg); err != nil {
		return nil, status.Errorf(codes.Internal, "save transport config: %v", err)
	}
	c.api.notifyTransportChanged()

	return transportConfigToProto(storeCfg, c.api.AuthRequired()), nil
}

func (a *adaptersService) ListAdapters(ctx context.Context, _ *emptypb.Empty) (*apiv1.ListAdaptersResponse, error) {
	if a.api.configStore == nil {
		return nil, status.Error(codes.Unavailable, "configuration store unavailable")
	}
	records, err := a.api.configStore.ListAdapters(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list adapters: %v", err)
	}

	resp := &apiv1.ListAdaptersResponse{Adapters: make([]*apiv1.Adapter, 0, len(records))}
	for _, record := range records {
		resp.Adapters = append(resp.Adapters, &apiv1.Adapter{
			Id:        record.ID,
			Source:    record.Source,
			Version:   record.Version,
			Type:      record.Type,
			Name:      record.Name,
			Manifest:  record.Manifest,
			CreatedAt: record.CreatedAt,
			UpdatedAt: record.UpdatedAt,
		})
	}
	return resp, nil
}

func (a *adaptersService) ListAdapterBindings(ctx context.Context, _ *emptypb.Empty) (*apiv1.ListAdapterBindingsResponse, error) {
	if a.api.configStore == nil {
		return nil, status.Error(codes.Unavailable, "configuration store unavailable")
	}
	records, err := a.api.configStore.ListAdapterBindings(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list adapter bindings: %v", err)
	}

	resp := &apiv1.ListAdapterBindingsResponse{Bindings: make([]*apiv1.AdapterBinding, 0, len(records))}
	for _, binding := range records {
		adapterID := ""
		if binding.AdapterID != nil {
			adapterID = *binding.AdapterID
		}
		resp.Bindings = append(resp.Bindings, &apiv1.AdapterBinding{
			Slot:      binding.Slot,
			AdapterId: adapterID,
			Status:    binding.Status,
			Config:    binding.Config,
			UpdatedAt: binding.UpdatedAt,
		})
	}
	return resp, nil
}

func (a *adaptersService) SetAdapterBinding(ctx context.Context, req *apiv1.SetAdapterBindingRequest) (*apiv1.AdapterBinding, error) {
	if a.api.configStore == nil {
		return nil, status.Error(codes.Unavailable, "configuration store unavailable")
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	slot := strings.TrimSpace(req.GetSlot())
	adapterID := strings.TrimSpace(req.GetAdapterId())
	if slot == "" || adapterID == "" {
		return nil, status.Error(codes.InvalidArgument, "slot and adapter_id are required")
	}

	var cfg map[string]any
	if raw := strings.TrimSpace(req.GetConfigJson()); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid config_json: %v", err)
		}
	}

	if err := a.api.configStore.SetActiveAdapter(ctx, slot, adapterID, cfg); err != nil {
		return nil, status.Errorf(codes.Internal, "set adapter binding: %v", err)
	}

	return a.bindingForSlot(ctx, slot)
}

func (a *adaptersService) ClearAdapterBinding(ctx context.Context, req *apiv1.ClearAdapterBindingRequest) (*emptypb.Empty, error) {
	if a.api.configStore == nil {
		return nil, status.Error(codes.Unavailable, "configuration store unavailable")
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	slot := strings.TrimSpace(req.GetSlot())
	if slot == "" {
		return nil, status.Error(codes.InvalidArgument, "slot is required")
	}
	if err := a.api.configStore.ClearAdapterBinding(ctx, slot); err != nil {
		return nil, status.Errorf(codes.Internal, "clear adapter binding: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (a *adaptersService) bindingForSlot(ctx context.Context, slot string) (*apiv1.AdapterBinding, error) {
	records, err := a.api.configStore.ListAdapterBindings(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "refresh adapter bindings: %v", err)
	}
	for _, binding := range records {
		if binding.Slot != slot {
			continue
		}
		adapterID := ""
		if binding.AdapterID != nil {
			adapterID = *binding.AdapterID
		}
		return &apiv1.AdapterBinding{
			Slot:      binding.Slot,
			AdapterId: adapterID,
			Status:    binding.Status,
			Config:    binding.Config,
			UpdatedAt: binding.UpdatedAt,
		}, nil
	}
	return nil, status.Errorf(codes.NotFound, "binding for slot %s not found", slot)
}

func (q *quickstartService) GetStatus(ctx context.Context, _ *emptypb.Empty) (*apiv1.QuickstartStatusResponse, error) {
	return q.fetchStatus(ctx)
}

func (q *quickstartService) Update(ctx context.Context, req *apiv1.UpdateQuickstartRequest) (*apiv1.QuickstartStatusResponse, error) {
	if q.api.configStore == nil {
		return nil, status.Error(codes.Unavailable, "configuration store unavailable")
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	for _, binding := range req.GetBindings() {
		slot := strings.TrimSpace(binding.GetSlot())
		adapterID := strings.TrimSpace(binding.GetAdapterId())
		if slot == "" {
			return nil, status.Error(codes.InvalidArgument, "binding slot is required")
		}
		var err error
		if adapterID == "" {
			err = q.api.configStore.ClearAdapterBinding(ctx, slot)
		} else {
			err = q.api.configStore.SetActiveAdapter(ctx, slot, adapterID, nil)
		}
		if err != nil {
			return nil, status.Errorf(codes.Internal, "update quickstart binding %s: %v", slot, err)
		}
	}

	if wrapper := req.GetComplete(); wrapper != nil {
		markComplete := wrapper.GetValue()
		if markComplete {
			pending, err := q.api.configStore.PendingQuickstartSlots(ctx)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "pending quickstart slots: %v", err)
			}
			if len(pending) > 0 {
				return nil, status.Errorf(codes.FailedPrecondition, "pending slots: %s", strings.Join(pending, ", "))
			}
		}
		if err := q.api.configStore.MarkQuickstartCompleted(ctx, markComplete); err != nil {
			return nil, status.Errorf(codes.Internal, "update quickstart status: %v", err)
		}
	}

	return q.fetchStatus(ctx)
}

func (q *quickstartService) fetchStatus(ctx context.Context) (*apiv1.QuickstartStatusResponse, error) {
	if q.api.configStore == nil {
		return nil, status.Error(codes.Unavailable, "configuration store unavailable")
	}
	completed, completedAt, err := q.api.configStore.QuickstartStatus(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "quickstart status: %v", err)
	}
	pending, err := q.api.configStore.PendingQuickstartSlots(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "pending quickstart slots: %v", err)
	}
	resp := &apiv1.QuickstartStatusResponse{
		Completed:    completed,
		PendingSlots: append([]string{}, pending...),
	}
	if completedAt != nil {
		resp.CompletedAt = completedAt.UTC().Format(time.RFC3339)
	}
	return resp, nil
}

func transportConfigToProto(cfg configstore.TransportConfig, authRequired bool) *apiv1.TransportConfig {
	return &apiv1.TransportConfig{
		Binding:        cfg.Binding,
		Port:           int32(cfg.Port),
		TlsCertPath:    cfg.TLSCertPath,
		TlsKeyPath:     cfg.TLSKeyPath,
		AllowedOrigins: sanitizeOrigins(cfg.AllowedOrigins),
		GrpcPort:       int32(cfg.GRPCPort),
		GrpcBinding:    cfg.GRPCBinding,
		AuthRequired:   authRequired,
	}
}
