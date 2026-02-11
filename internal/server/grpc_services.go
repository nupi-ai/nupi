package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nupi-ai/nupi/internal/api"
	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
	"github.com/nupi-ai/nupi/internal/protocol"
	"github.com/nupi-ai/nupi/internal/voice/slots"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
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

type adapterRuntimeService struct {
	apiv1.UnimplementedAdapterRuntimeServiceServer
	api *APIServer
}

func newAdapterRuntimeService(api *APIServer) *adapterRuntimeService {
	return &adapterRuntimeService{api: api}
}

type audioService struct {
	apiv1.UnimplementedAudioServiceServer
	api *APIServer
}

func newAudioService(api *APIServer) *audioService {
	return &audioService{api: api}
}

func (a *audioService) StreamAudioIn(stream apiv1.AudioService_StreamAudioInServer) error {
	if a.api.audioIngress == nil {
		return status.Error(codes.Unavailable, "audio ingress service unavailable")
	}
	if _, err := a.api.requireRoleGRPC(stream.Context(), roleAdmin); err != nil {
		return err
	}

	var (
		opened        bool
		ingressStream AudioCaptureStream
		sessionID     string
		streamID      string
		format        eventbus.AudioFormat
		lastSeq       uint64
	)

	defer func() {
		if ingressStream != nil {
			_ = ingressStream.Close()
		}
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			if ingressStream != nil {
				if closeErr := ingressStream.Close(); closeErr != nil {
					return status.Errorf(codes.Internal, "finalise stream: %v", closeErr)
				}
				ingressStream = nil
			}
			if !opened {
				return status.Error(codes.InvalidArgument, "no audio frames received")
			}
			return stream.SendAndClose(&apiv1.StreamAudioInResponse{
				AckSequence: lastSeq,
				Ready:       true,
			})
		}
		if err != nil {
			return status.Errorf(codes.Internal, "receive audio chunk: %v", err)
		}
		if req == nil {
			continue
		}

		chunk := req.GetChunk()
		if !opened {
			sessionID = strings.TrimSpace(req.GetSessionId())
			streamID = strings.TrimSpace(req.GetStreamId())
			if sessionID == "" || streamID == "" {
				return status.Error(codes.InvalidArgument, "session_id and stream_id are required")
			}
			if _, err := a.api.sessionManager.GetSession(sessionID); err != nil {
				return status.Errorf(codes.NotFound, "session %s not found", sessionID)
			}
			fmtProto := req.GetFormat()
			var err error
			format, err = audioFormatFromProto(fmtProto)
			if err != nil {
				return status.Errorf(codes.InvalidArgument, "invalid audio format: %v", err)
			}
			readiness, err := a.api.voiceReadiness(stream.Context())
			if err != nil {
				return status.Errorf(codes.Internal, "voice readiness: %v", err)
			}
			if !readiness.CaptureEnabled {
				diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.STT)
				message := voiceIssueSummary(diags, "voice capture unavailable")
				return status.Error(codes.FailedPrecondition, message)
			}
			var metadata map[string]string
			if chunk != nil {
				metadata = copyStringMap(chunk.GetMetadata())
			}
			ingressStream, err = a.api.audioIngress.OpenStream(sessionID, streamID, format, metadata)
			if err != nil {
				if errors.Is(err, ErrAudioStreamExists) {
					return status.Errorf(codes.AlreadyExists, "audio stream %s/%s already exists", sessionID, streamID)
				}
				return status.Errorf(codes.Internal, "open stream: %v", err)
			}
			opened = true
		} else {
			if id := strings.TrimSpace(req.GetSessionId()); id != "" && id != sessionID {
				return status.Error(codes.InvalidArgument, "session_id mismatch")
			}
			if id := strings.TrimSpace(req.GetStreamId()); id != "" && id != streamID {
				return status.Error(codes.InvalidArgument, "stream_id mismatch")
			}
			if reqFmt := req.GetFormat(); reqFmt != nil {
				candidate, err := audioFormatFromProto(reqFmt)
				if err != nil {
					return status.Errorf(codes.InvalidArgument, "invalid audio format: %v", err)
				}
				if !audioFormatsEqual(candidate, format) {
					return status.Error(codes.InvalidArgument, "audio format mismatch")
				}
			}
			if ingressStream == nil {
				return status.Error(codes.FailedPrecondition, "audio stream already closed")
			}
		}

		if chunk == nil {
			continue
		}

		if data := chunk.GetData(); len(data) > 0 {
			if err := ingressStream.Write(data); err != nil {
				return status.Errorf(codes.Internal, "write audio chunk: %v", err)
			}
		}

		lastSeq = chunk.GetSequence()

		if chunk.GetLast() {
			if err := ingressStream.Close(); err != nil {
				return status.Errorf(codes.Internal, "finalise stream: %v", err)
			}
			ingressStream = nil
		}
	}
}

func (a *audioService) StreamAudioOut(req *apiv1.StreamAudioOutRequest, srv apiv1.AudioService_StreamAudioOutServer) error {
	if _, err := a.api.requireRoleGRPC(srv.Context(), roleAdmin); err != nil {
		return err
	}
	if req == nil {
		return status.Error(codes.InvalidArgument, "request is required")
	}
	if a.api.eventBus == nil {
		return status.Error(codes.Unavailable, "event bus unavailable")
	}

	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID == "" {
		return status.Error(codes.InvalidArgument, "session_id is required")
	}
	streamID := strings.TrimSpace(req.GetStreamId())
	if streamID == "" {
		if a.api.audioEgress != nil {
			streamID = a.api.audioEgress.DefaultStreamID()
		} else {
			streamID = defaultTTSStreamID
		}
	}

	if a.api.sessionManager != nil {
		if _, err := a.api.sessionManager.GetSession(sessionID); err != nil {
			return status.Errorf(codes.NotFound, "session %s not found", sessionID)
		}
	}

	readiness, err := a.api.voiceReadiness(srv.Context())
	if err != nil {
		return status.Errorf(codes.Internal, "voice readiness: %v", err)
	}
	if !readiness.PlaybackEnabled {
		diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.TTS)
		message := voiceIssueSummary(diags, "voice playback unavailable")
		return status.Error(codes.FailedPrecondition, message)
	}

	sub := a.api.eventBus.Subscribe(
		eventbus.TopicAudioEgressPlayback,
		eventbus.WithSubscriptionName(fmt.Sprintf("grpc_audio_out_%s_%s_%d", sessionID, streamID, time.Now().UnixNano())),
		eventbus.WithSubscriptionBuffer(64),
	)
	defer sub.Close()

	ctx := srv.Context()
	firstChunk := true

	for {
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case env, ok := <-sub.C():
			if !ok {
				return status.Error(codes.Unavailable, "playback subscription closed")
			}
			evt, ok := env.Payload.(eventbus.AudioEgressPlaybackEvent)
			if !ok {
				continue
			}
			if evt.SessionID != sessionID || evt.StreamID != streamID {
				continue
			}

			chunkDuration := playbackDuration(evt)
			resp := &apiv1.StreamAudioOutResponse{
				Format: audioFormatToProto(evt.Format),
				Chunk: &apiv1.AudioChunk{
					Data:       append([]byte(nil), evt.Data...),
					Sequence:   evt.Sequence,
					DurationMs: durationToMillis(chunkDuration),
					Last:       evt.Final,
					Metadata:   copyStringMap(evt.Metadata),
				},
			}
			if firstChunk {
				resp.Chunk.First = true
				firstChunk = false
			}

			if err := srv.Send(resp); err != nil {
				return err
			}
		}
	}
}

func (a *audioService) InterruptTTS(ctx context.Context, req *apiv1.InterruptTTSRequest) (*emptypb.Empty, error) {
	if _, err := a.api.requireRoleGRPC(ctx, roleAdmin); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}

	streamID := strings.TrimSpace(req.GetStreamId())
	if streamID == "" {
		if a.api.audioEgress != nil {
			streamID = a.api.audioEgress.DefaultStreamID()
		} else {
			streamID = defaultTTSStreamID
		}
	}

	reason := strings.TrimSpace(req.GetReason())

	if a.api.sessionManager != nil {
		if _, err := a.api.sessionManager.GetSession(sessionID); err != nil {
			return nil, status.Errorf(codes.NotFound, "session %s not found", sessionID)
		}
	}

	readiness, err := a.api.voiceReadiness(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "voice readiness: %v", err)
	}
	if !readiness.PlaybackEnabled {
		diags := filterDiagnostics(mapVoiceDiagnostics(readiness.Issues), slots.TTS)
		message := voiceIssueSummary(diags, "voice playback unavailable")
		return nil, status.Error(codes.FailedPrecondition, message)
	}

	a.api.publishAudioInterrupt(sessionID, streamID, reason, nil)
	if a.api.audioEgress != nil {
		a.api.audioEgress.Interrupt(sessionID, streamID, reason, nil)
	}

	return &emptypb.Empty{}, nil
}

func (a *audioService) GetAudioCapabilities(ctx context.Context, req *apiv1.GetAudioCapabilitiesRequest) (*apiv1.GetAudioCapabilitiesResponse, error) {
	if _, err := a.api.requireRoleGRPC(ctx, roleAdmin, roleReadOnly); err != nil {
		return nil, err
	}
	if req == nil {
		req = &apiv1.GetAudioCapabilitiesRequest{}
	}

	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID != "" && a.api.sessionManager != nil {
		if _, err := a.api.sessionManager.GetSession(sessionID); err != nil {
			return nil, status.Errorf(codes.NotFound, "session %s not found", sessionID)
		}
	}

	readiness, err := a.api.voiceReadiness(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "voice readiness: %v", err)
	}
	diagnostics := mapVoiceDiagnostics(readiness.Issues)

	resp := &apiv1.GetAudioCapabilitiesResponse{}

	if a.api.audioIngress != nil {
		meta := map[string]string{
			"recommended": "true",
			"ready":       strconv.FormatBool(readiness.CaptureEnabled),
		}
		if !readiness.CaptureEnabled {
			diags := filterDiagnostics(diagnostics, slots.STT)
			meta["diagnostics"] = voiceIssueSummary(diags, "voice capture unavailable")
		}
		resp.Capture = append(resp.Capture, &apiv1.AudioCapability{
			StreamId: defaultCaptureStreamID,
			Format:   audioFormatToProto(defaultCaptureFormat),
			Metadata: meta,
		})
	}

	if a.api.audioEgress != nil {
		meta := map[string]string{
			"recommended": "true",
			"ready":       strconv.FormatBool(readiness.PlaybackEnabled),
		}
		if !readiness.PlaybackEnabled {
			diags := filterDiagnostics(diagnostics, slots.TTS)
			meta["diagnostics"] = voiceIssueSummary(diags, "voice playback unavailable")
		}
		resp.Playback = append(resp.Playback, &apiv1.AudioCapability{
			StreamId: a.api.audioEgress.DefaultStreamID(),
			Format:   audioFormatToProto(a.api.audioEgress.PlaybackFormat()),
			Metadata: meta,
		})
	}

	return resp, nil
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

func (s *sessionsService) GetGlobalConversation(ctx context.Context, req *apiv1.GetGlobalConversationRequest) (*apiv1.GetGlobalConversationResponse, error) {
	if _, err := s.api.requireRoleGRPC(ctx, roleAdmin, roleReadOnly); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	if s.api.conversation == nil {
		return nil, status.Error(codes.Unavailable, "conversation service unavailable")
	}

	offset := int(req.GetOffset())
	limit := int(req.GetLimit())
	if limit > conversationMaxPageLimit {
		limit = conversationMaxPageLimit
	}

	total, turns := s.api.conversation.GlobalSlice(offset, limit)
	// Effective page limit: when no limit was requested (0) or limit exceeds
	// the actual number of turns returned, clamp to the actual count.
	pageLimit := limit
	if pageLimit <= 0 || pageLimit > len(turns) {
		pageLimit = len(turns)
	}

	resp := &apiv1.GetGlobalConversationResponse{
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

		var metadata []*apiv1.ConversationMetadata
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
	apiv1.RegisterAdapterRuntimeServiceServer(registrar, newAdapterRuntimeService(api))
	if api.audioIngress != nil {
		apiv1.RegisterAudioServiceServer(registrar, newAudioService(api))
	}
}

const defaultCaptureStreamID = "mic"

var defaultCaptureFormat = eventbus.AudioFormat{
	Encoding:      eventbus.AudioEncodingPCM16,
	SampleRate:    16000,
	Channels:      1,
	BitDepth:      16,
	FrameDuration: 20 * time.Millisecond,
}

func audioFormatFromProto(pb *apiv1.AudioFormat) (eventbus.AudioFormat, error) {
	if pb == nil {
		return eventbus.AudioFormat{}, fmt.Errorf("audio format is required")
	}
	encoding := strings.TrimSpace(strings.ToLower(pb.GetEncoding()))
	if encoding == "" {
		encoding = string(eventbus.AudioEncodingPCM16)
	}
	if encoding != string(eventbus.AudioEncodingPCM16) {
		return eventbus.AudioFormat{}, fmt.Errorf("unsupported encoding %q", pb.GetEncoding())
	}
	format := eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    int(pb.GetSampleRate()),
		Channels:      int(pb.GetChannels()),
		BitDepth:      int(pb.GetBitDepth()),
		FrameDuration: time.Duration(pb.GetFrameDurationMs()) * time.Millisecond,
	}
	if format.SampleRate <= 0 {
		return eventbus.AudioFormat{}, fmt.Errorf("sample_rate must be positive")
	}
	if format.Channels <= 0 {
		return eventbus.AudioFormat{}, fmt.Errorf("channels must be positive")
	}
	if format.BitDepth <= 0 || format.BitDepth%8 != 0 {
		return eventbus.AudioFormat{}, fmt.Errorf("bit_depth must be divisible by 8")
	}
	return format, nil
}

func audioFormatsEqual(a, b eventbus.AudioFormat) bool {
	return a.Encoding == b.Encoding &&
		a.SampleRate == b.SampleRate &&
		a.Channels == b.Channels &&
		a.BitDepth == b.BitDepth
}

func audioFormatToProto(format eventbus.AudioFormat) *apiv1.AudioFormat {
	return &apiv1.AudioFormat{
		Encoding:        string(format.Encoding),
		SampleRate:      uint32(maxInt(format.SampleRate)),
		Channels:        uint32(maxInt(format.Channels)),
		BitDepth:        uint32(maxInt(format.BitDepth)),
		FrameDurationMs: durationToMillis(format.FrameDuration),
	}
}

func maxInt(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

func durationToMillis(d time.Duration) uint32 {
	if d <= 0 {
		return 0
	}
	return uint32(d / time.Millisecond)
}

func playbackDuration(evt eventbus.AudioEgressPlaybackEvent) time.Duration {
	if evt.Duration > 0 {
		return evt.Duration
	}
	return pcmDuration(evt.Format, len(evt.Data))
}

func pcmDuration(format eventbus.AudioFormat, dataLen int) time.Duration {
	if dataLen <= 0 || format.SampleRate <= 0 || format.Channels <= 0 || format.BitDepth <= 0 {
		return 0
	}
	bytesPerSample := format.BitDepth / 8
	if bytesPerSample <= 0 {
		return 0
	}
	frameSize := format.Channels * bytesPerSample
	if frameSize <= 0 {
		return 0
	}
	samples := dataLen / frameSize
	if samples <= 0 {
		return 0
	}
	const maxPCMFrames = 1 << 30
	if samples > maxPCMFrames {
		return 0
	}
	return time.Duration(float64(samples) / float64(format.SampleRate) * float64(time.Second))
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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

			missingRefs, err := q.api.missingReferenceAdapters(ctx)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "reference adapter check failed: %v", err)
			}
			if len(missingRefs) > 0 {
				return nil, status.Errorf(codes.FailedPrecondition, "reference adapters missing: %s", strings.Join(missingRefs, ", "))
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

	adapterStatuses, adaptersErr := q.api.quickstartAdapterStatuses(ctx)
	if adaptersErr != nil {
		if errors.Is(adaptersErr, errAdaptersServiceUnavailable) {
			return nil, status.Error(codes.Unavailable, adaptersErr.Error())
		}
		return nil, status.Errorf(codes.Internal, "adapter overview: %v", adaptersErr)
	}
	if len(adapterStatuses) > 0 {
		resp.Adapters = make([]*apiv1.AdapterEntry, 0, len(adapterStatuses))
		for _, status := range adapterStatuses {
			resp.Adapters = append(resp.Adapters, bindingStatusToProto(status))
		}
	}
	missingRefs, err := q.api.missingReferenceAdapters(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reference adapter check failed: %v", err)
	}
	if len(missingRefs) > 0 {
		resp.MissingReferenceAdapters = append(resp.MissingReferenceAdapters, missingRefs...)
	}
	return resp, nil
}

func (m *adapterRuntimeService) Overview(ctx context.Context, _ *emptypb.Empty) (*apiv1.AdaptersOverviewResponse, error) {
	if _, err := m.api.requireRoleGRPC(ctx, roleAdmin, roleReadOnly); err != nil {
		return nil, err
	}
	if m.api.adapters == nil {
		return nil, status.Error(codes.Unavailable, "adapter service unavailable")
	}

	overview, err := m.api.adapters.Overview(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "adapter overview: %v", err)
	}

	resp := &apiv1.AdaptersOverviewResponse{
		Adapters: make([]*apiv1.AdapterEntry, 0, len(overview)),
	}
	for _, entry := range overview {
		resp.Adapters = append(resp.Adapters, bindingStatusToProto(entry))
	}
	return resp, nil
}

func (m *adapterRuntimeService) BindAdapter(ctx context.Context, req *apiv1.BindAdapterRequest) (*apiv1.AdapterActionResponse, error) {
	if _, err := m.api.requireRoleGRPC(ctx, roleAdmin); err != nil {
		return nil, err
	}
	if m.api.configStore == nil {
		return nil, status.Error(codes.Unavailable, "configuration store unavailable")
	}
	if m.api.adapters == nil {
		return nil, status.Error(codes.Unavailable, "adapter service unavailable")
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

	if err := m.api.configStore.SetActiveAdapter(ctx, slot, adapterID, cfg); err != nil {
		switch {
		case configstore.IsNotFound(err):
			return nil, status.Errorf(codes.NotFound, "set adapter binding: %v", err)
		case strings.Contains(strings.ToLower(err.Error()), "invalid"):
			return nil, status.Errorf(codes.InvalidArgument, "set adapter binding: %v", err)
		default:
			return nil, status.Errorf(codes.Internal, "set adapter binding: %v", err)
		}
	}

	statusEntry, err := m.api.adapters.StartSlot(ctx, adapters.Slot(slot))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "start adapter: %v", err)
	}

	return &apiv1.AdapterActionResponse{Adapter: bindingStatusToProto(*statusEntry)}, nil
}

func (m *adapterRuntimeService) StartAdapter(ctx context.Context, req *apiv1.AdapterSlotRequest) (*apiv1.AdapterActionResponse, error) {
	if _, err := m.api.requireRoleGRPC(ctx, roleAdmin); err != nil {
		return nil, err
	}
	if m.api.adapters == nil {
		return nil, status.Error(codes.Unavailable, "adapter service unavailable")
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	slot := strings.TrimSpace(req.GetSlot())
	if slot == "" {
		return nil, status.Error(codes.InvalidArgument, "slot is required")
	}

	statusEntry, err := m.api.adapters.StartSlot(ctx, adapters.Slot(slot))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "start adapter: %v", err)
	}

	return &apiv1.AdapterActionResponse{Adapter: bindingStatusToProto(*statusEntry)}, nil
}

func (m *adapterRuntimeService) StopAdapter(ctx context.Context, req *apiv1.AdapterSlotRequest) (*apiv1.AdapterActionResponse, error) {
	if _, err := m.api.requireRoleGRPC(ctx, roleAdmin); err != nil {
		return nil, err
	}
	if m.api.adapters == nil {
		return nil, status.Error(codes.Unavailable, "adapter service unavailable")
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	slot := strings.TrimSpace(req.GetSlot())
	if slot == "" {
		return nil, status.Error(codes.InvalidArgument, "slot is required")
	}

	statusEntry, err := m.api.adapters.StopSlot(ctx, adapters.Slot(slot))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stop adapter: %v", err)
	}

	return &apiv1.AdapterActionResponse{Adapter: bindingStatusToProto(*statusEntry)}, nil
}

func bindingStatusToProto(status adapters.BindingStatus) *apiv1.AdapterEntry {
	entry := &apiv1.AdapterEntry{
		Slot:       string(status.Slot),
		Status:     status.Status,
		ConfigJson: status.Config,
		UpdatedAt:  status.UpdatedAt,
	}
	if status.AdapterID != nil {
		if id := strings.TrimSpace(*status.AdapterID); id != "" {
			entry.AdapterId = proto.String(id)
		}
	}
	if status.Runtime != nil {
		entry.Runtime = runtimeStatusToProto(status.Runtime)
	}
	return entry
}

func runtimeStatusToProto(rt *adapters.RuntimeStatus) *apiv1.AdapterRuntime {
	if rt == nil {
		return nil
	}
	result := &apiv1.AdapterRuntime{
		AdapterId: rt.AdapterID,
		Health:    string(rt.Health),
		Message:   rt.Message,
		Extra:     map[string]string{},
	}
	if rt.Extra != nil {
		for k, v := range rt.Extra {
			result.Extra[k] = v
		}
	}
	if rt.StartedAt != nil && !rt.StartedAt.IsZero() {
		result.StartedAt = timestamppb.New(rt.StartedAt.UTC())
	}
	if !rt.UpdatedAt.IsZero() {
		result.UpdatedAt = timestamppb.New(rt.UpdatedAt.UTC())
	}
	return result
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
