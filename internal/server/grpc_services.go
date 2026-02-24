package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nupi-ai/nupi/internal/api"
	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/audio/audiofmt"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/language"
	"github.com/nupi-ai/nupi/internal/mapper"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
	"github.com/nupi-ai/nupi/internal/protocol"
	"github.com/nupi-ai/nupi/internal/sanitize"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
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

func (d *daemonService) Status(ctx context.Context, _ *apiv1.DaemonStatusRequest) (*apiv1.DaemonStatusResponse, error) {
	snapshot, err := d.api.daemonStatus(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to compute daemon status: %v", err)
	}
	return &apiv1.DaemonStatusResponse{
		Version:       snapshot.Version,
		SessionsCount: int32(snapshot.SessionsCount),
		GrpcPort:      int32(snapshot.GRPCPort),
		ConnectPort:   int32(snapshot.ConnectPort),
		Binding:       snapshot.Binding,
		GrpcBinding:   snapshot.GRPCBinding,
		AuthRequired:  snapshot.AuthRequired,
		UptimeSec:     snapshot.UptimeSeconds,
		TlsEnabled:    snapshot.TLSEnabled,
	}, nil
}

type sessionsService struct {
	apiv1.UnimplementedSessionsServiceServer
	api *APIServer
}

type configService struct {
	apiv1.UnimplementedConfigServiceServer
	api *APIServer
}

type adaptersService struct {
	apiv1.UnimplementedAdaptersServiceServer
	api *APIServer
}

type quickstartService struct {
	apiv1.UnimplementedQuickstartServiceServer
	api *APIServer
}

type adapterRuntimeService struct {
	apiv1.UnimplementedAdapterRuntimeServiceServer
	api *APIServer
}

type audioService struct {
	apiv1.UnimplementedAudioServiceServer
	api *APIServer
}

func (a *audioService) StreamAudioIn(stream apiv1.AudioService_StreamAudioInServer) error {
	if a.api.audioIngress == nil {
		return status.Error(codes.Unavailable, "audio ingress service unavailable")
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
			format, err = mapper.ParseProtoAudioFormat(fmtProto)
			if err != nil {
				return status.Errorf(codes.InvalidArgument, "invalid audio format: %v", err)
			}
			var metadata map[string]string
			if chunk != nil {
				metadata, err = normalizeAndValidateAudioChunkMetadata(chunk.GetMetadata())
				if err != nil {
					return status.Errorf(codes.InvalidArgument, "invalid metadata: %v", err)
				}
			}
			readiness, err := a.api.voiceReadiness(stream.Context())
			if err != nil {
				return status.Errorf(codes.Internal, "voice readiness: %v", err)
			}
			if !readiness.CaptureEnabled {
				return status.Error(codes.FailedPrecondition, "voice capture unavailable")
			}
			metadata = language.MergeContextLanguage(stream.Context(), metadata)
			metadata, err = normalizeAndValidateAudioChunkMetadata(metadata)
			if err != nil {
				return status.Errorf(codes.InvalidArgument, "invalid metadata: %v", err)
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
				candidate, err := mapper.ParseProtoAudioFormat(reqFmt)
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
	if req == nil {
		return status.Error(codes.InvalidArgument, "request is required")
	}
	if a.api.eventBus == nil {
		return status.Error(codes.Unavailable, "event bus unavailable")
	}

	_, sessionID, err := a.api.validateAndGetSession(req.GetSessionId())
	if err != nil {
		return err
	}
	streamID := strings.TrimSpace(req.GetStreamId())
	if streamID == "" {
		if a.api.audioEgress != nil {
			streamID = a.api.audioEgress.DefaultStreamID()
		} else {
			streamID = defaultTTSStreamID
		}
	}

	readiness, err := a.api.voiceReadiness(srv.Context())
	if err != nil {
		return status.Errorf(codes.Internal, "voice readiness: %v", err)
	}
	if !readiness.PlaybackEnabled {
		return status.Error(codes.FailedPrecondition, "voice playback unavailable")
	}

	sub := eventbus.Subscribe[eventbus.AudioEgressPlaybackEvent](a.api.eventBus,
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
			evt := env.Payload
			if evt.SessionID != sessionID || evt.StreamID != streamID {
				continue
			}

			chunkDuration := playbackDuration(evt)
			resp := &apiv1.StreamAudioOutResponse{
				Format: mapper.ToProtoAudioFormat(evt.Format),
				Chunk: &apiv1.AudioChunk{
					Data:       append([]byte(nil), evt.Data...),
					Sequence:   evt.Sequence,
					DurationMs: durationToMillis(chunkDuration),
					Last:       evt.Final,
					Metadata:   maputil.Clone(evt.Metadata),
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
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	_, sessionID, err := a.api.validateAndGetSession(req.GetSessionId())
	if err != nil {
		return nil, err
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

	readiness, err := a.api.voiceReadiness(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "voice readiness: %v", err)
	}
	if !readiness.PlaybackEnabled {
		return nil, status.Error(codes.FailedPrecondition, "voice playback unavailable")
	}

	a.api.publishAudioInterrupt(sessionID, streamID, reason, nil)
	if a.api.audioEgress != nil {
		a.api.audioEgress.Interrupt(sessionID, streamID, reason, nil)
	}

	return &emptypb.Empty{}, nil
}

func (a *audioService) GetAudioCapabilities(ctx context.Context, req *apiv1.GetAudioCapabilitiesRequest) (*apiv1.GetAudioCapabilitiesResponse, error) {
	if req == nil {
		req = &apiv1.GetAudioCapabilitiesRequest{}
	}

	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID != "" {
		if _, err := a.api.sessionManager.GetSession(sessionID); err != nil {
			return nil, status.Errorf(codes.NotFound, "session %s not found", sessionID)
		}
	}

	readiness, err := a.api.voiceReadiness(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "voice readiness: %v", err)
	}

	resp := &apiv1.GetAudioCapabilitiesResponse{}

	if a.api.audioIngress != nil {
		meta := map[string]string{
			"recommended": "true",
			"ready":       strconv.FormatBool(readiness.CaptureEnabled),
		}
		resp.Capture = append(resp.Capture, &apiv1.AudioCapability{
			StreamId: DefaultCaptureStreamID,
			Format:   mapper.ToProtoAudioFormat(defaultCaptureFormat),
			Metadata: meta,
		})
	}

	if a.api.audioEgress != nil {
		meta := map[string]string{
			"recommended": "true",
			"ready":       strconv.FormatBool(readiness.PlaybackEnabled),
		}
		resp.Playback = append(resp.Playback, &apiv1.AudioCapability{
			StreamId: a.api.audioEgress.DefaultStreamID(),
			Format:   mapper.ToProtoAudioFormat(a.api.audioEgress.PlaybackFormat()),
			Metadata: meta,
		})
	}

	return resp, nil
}

func (s *sessionsService) ListSessions(ctx context.Context, _ *apiv1.ListSessionsRequest) (*apiv1.ListSessionsResponse, error) {

	sessions := s.api.sessionManager.ListSessions()
	dto := api.ToDTOList(sessions)

	out := make([]*apiv1.Session, 0, len(dto))
	for _, session := range dto {
		out = append(out, dtoToSessionProto(session, s.api))
	}

	return &apiv1.ListSessionsResponse{Sessions: out}, nil
}

func (s *sessionsService) CreateSession(ctx context.Context, req *apiv1.CreateSessionRequest) (*apiv1.CreateSessionResponse, error) {

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
	return &apiv1.CreateSessionResponse{Session: dtoToSessionProto(dto, s.api)}, nil
}

func (s *sessionsService) KillSession(ctx context.Context, req *apiv1.KillSessionRequest) (*apiv1.KillSessionResponse, error) {

	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}

	if err := s.api.sessionManager.KillSession(sessionID); err != nil {
		return nil, status.Errorf(codes.NotFound, "session %s not found", sessionID)
	}

	return &apiv1.KillSessionResponse{}, nil
}

func (s *sessionsService) GetSession(ctx context.Context, req *apiv1.GetSessionRequest) (*apiv1.GetSessionResponse, error) {

	sess, _, err := s.api.validateAndGetSession(req.GetSessionId())
	if err != nil {
		return nil, err
	}

	dto := api.ToDTO(sess)
	return &apiv1.GetSessionResponse{Session: dtoToSessionProto(dto, s.api)}, nil
}

func (s *sessionsService) SendInput(ctx context.Context, req *apiv1.SendInputRequest) (*apiv1.SendInputResponse, error) {

	_, sessionID, err := s.api.validateAndGetSession(req.GetSessionId())
	if err != nil {
		return nil, err
	}

	if input := req.GetInput(); len(input) > 0 {
		if err := s.api.sessionManager.WriteToSession(sessionID, input); err != nil {
			return nil, status.Errorf(codes.Internal, "write to session: %v", err)
		}
	}

	if req.GetEof() {
		// Send Ctrl-D (EOT) to signal EOF.
		if err := s.api.sessionManager.WriteToSession(sessionID, []byte{4}); err != nil {
			return nil, status.Errorf(codes.Internal, "send EOF: %v", err)
		}
	}

	return &apiv1.SendInputResponse{}, nil
}

func (s *sessionsService) GetSessionMode(ctx context.Context, req *apiv1.GetSessionModeRequest) (*apiv1.GetSessionModeResponse, error) {

	_, sessionID, err := s.api.validateAndGetSession(req.GetSessionId())
	if err != nil {
		return nil, err
	}

	mode := ""
	if s.api.resizeManager != nil {
		mode = s.api.resizeManager.GetSessionMode(sessionID)
	}

	return &apiv1.GetSessionModeResponse{SessionId: sessionID, Mode: mode}, nil
}

func (s *sessionsService) SetSessionMode(ctx context.Context, req *apiv1.SetSessionModeRequest) (*apiv1.SetSessionModeResponse, error) {

	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}

	modeStr := strings.TrimSpace(req.GetMode())
	if modeStr == "" {
		return nil, status.Error(codes.InvalidArgument, "mode is required")
	}

	if _, _, err := s.api.validateAndGetSession(sessionID); err != nil {
		return nil, err
	}

	if s.api.resizeManager == nil {
		return nil, status.Error(codes.Unavailable, "resize manager unavailable")
	}

	if err := s.api.resizeManager.SetSessionMode(sessionID, modeStr); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "set session mode: %v", err)
	}

	mode := s.api.resizeManager.GetSessionMode(sessionID)
	return &apiv1.SetSessionModeResponse{SessionId: sessionID, Mode: mode}, nil
}

func (s *sessionsService) GetConversation(ctx context.Context, req *apiv1.GetConversationRequest) (*apiv1.GetConversationResponse, error) {
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

	if _, _, err := s.api.validateAndGetSession(sessionID); err != nil {
		return nil, err
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
		Turns:      conversationTurnsToProto(turns),
		Offset:     uint32(offset),
		Limit:      uint32(pageLimit),
		Total:      uint32(total),
		HasMore:    offset+len(turns) < total,
		NextOffset: 0,
	}

	if !resp.HasMore {
		resp.NextOffset = 0
	} else {
		resp.NextOffset = uint32(offset + len(turns))
	}

	return resp, nil
}

func (s *sessionsService) GetGlobalConversation(ctx context.Context, req *apiv1.GetGlobalConversationRequest) (*apiv1.GetGlobalConversationResponse, error) {
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
		Turns:      conversationTurnsToProto(turns),
		Offset:     uint32(offset),
		Limit:      uint32(pageLimit),
		Total:      uint32(total),
		HasMore:    offset+len(turns) < total,
		NextOffset: 0,
	}

	if !resp.HasMore {
		resp.NextOffset = 0
	} else {
		resp.NextOffset = uint32(offset + len(turns))
	}

	return resp, nil
}

// maxVoiceCommandTextLen is the maximum allowed length for voice command text.
const maxVoiceCommandTextLen = 10000

// maxVoiceCommandMetadataEntries is the maximum number of caller-supplied metadata entries.
const maxVoiceCommandMetadataEntries = 50

// maxVoiceCommandMetadataKeyLen is the maximum length of a single metadata key.
const maxVoiceCommandMetadataKeyLen = 256

// maxVoiceCommandMetadataValueLen is the maximum length of a single metadata value.
const maxVoiceCommandMetadataValueLen = 1024

func (s *sessionsService) SendVoiceCommand(ctx context.Context, req *apiv1.SendVoiceCommandRequest) (*apiv1.SendVoiceCommandResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	text := strings.TrimSpace(req.GetText())
	if text == "" {
		return nil, status.Error(codes.InvalidArgument, "text is required")
	}
	if utf8.RuneCountInString(text) > maxVoiceCommandTextLen {
		return nil, status.Errorf(codes.InvalidArgument, "text exceeds maximum length of %d characters", maxVoiceCommandTextLen)
	}

	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID != "" {
		if _, err := s.api.sessionManager.GetSession(sessionID); err != nil {
			return nil, status.Errorf(codes.NotFound, "session %s not found", sessionID)
		}
	}

	if s.api.eventBus == nil {
		return nil, status.Error(codes.Unavailable, "event bus unavailable")
	}

	if len(req.GetMetadata()) > maxVoiceCommandMetadataEntries {
		return nil, status.Errorf(codes.InvalidArgument, "metadata exceeds maximum of %d entries", maxVoiceCommandMetadataEntries)
	}

	annotations := map[string]string{
		"input_source": "voice",
	}
	for k, v := range req.GetMetadata() {
		kLen, vLen := utf8.RuneCountInString(k), utf8.RuneCountInString(v)
		if kLen > maxVoiceCommandMetadataKeyLen || vLen > maxVoiceCommandMetadataValueLen {
			return nil, status.Errorf(codes.InvalidArgument,
				"metadata entry %q exceeds limits (key: %d/%d chars, value: %d/%d chars)",
				k, kLen, maxVoiceCommandMetadataKeyLen, vLen, maxVoiceCommandMetadataValueLen)
		}
		switch k {
		case "input_source":
			// Reserved — always "voice" for this RPC.
		default:
			annotations[k] = v
		}
	}
	annotations = language.MergeContextLanguage(ctx, annotations)

	eventbus.Publish(ctx, s.api.eventBus, eventbus.Pipeline.Cleaned, eventbus.SourceClient, eventbus.PipelineMessageEvent{
		SessionID:   sessionID,
		Origin:      eventbus.OriginUser,
		Text:        text,
		Annotations: annotations,
		Sequence:    0,
	})

	return &apiv1.SendVoiceCommandResponse{Accepted: true, Message: "voice command queued"}, nil
}

func RegisterGRPCServices(api *APIServer, registrar grpc.ServiceRegistrar) {
	apiv1.RegisterDaemonServiceServer(registrar, &daemonService{api: api})
	apiv1.RegisterSessionsServiceServer(registrar, &sessionsService{api: api})
	apiv1.RegisterConfigServiceServer(registrar, &configService{api: api})
	apiv1.RegisterAdaptersServiceServer(registrar, &adaptersService{api: api})
	apiv1.RegisterQuickstartServiceServer(registrar, &quickstartService{api: api})
	apiv1.RegisterAdapterRuntimeServiceServer(registrar, &adapterRuntimeService{api: api})
	apiv1.RegisterAuthServiceServer(registrar, &authService{api: api})
	apiv1.RegisterRecordingsServiceServer(registrar, &recordingsService{api: api})
	if api.audioIngress != nil {
		apiv1.RegisterAudioServiceServer(registrar, &audioService{api: api})
	}
}

// DefaultCaptureStreamID is the default stream identifier for audio capture.
const DefaultCaptureStreamID = "mic"

const (
	maxAudioChunkMetadataEntries    = sanitize.DefaultMetadataMaxEntries
	maxAudioChunkMetadataKeyRunes   = sanitize.DefaultMetadataMaxKeyRunes
	maxAudioChunkMetadataValueRunes = sanitize.DefaultMetadataMaxValueRunes
	maxAudioChunkMetadataTotalBytes = sanitize.DefaultMetadataMaxTotalBytes
)

var defaultCaptureFormat = eventbus.AudioFormat{
	Encoding:      eventbus.AudioEncodingPCM16,
	SampleRate:    16000,
	Channels:      1,
	BitDepth:      16,
	FrameDuration: 20 * time.Millisecond,
}

// DefaultCaptureFormat returns a copy of the default audio capture format.
func DefaultCaptureFormat() eventbus.AudioFormat { return defaultCaptureFormat }

func normalizeAndValidateAudioChunkMetadata(raw map[string]string) (map[string]string, error) {
	return sanitize.NormalizeAndValidateMetadata(raw, sanitize.MetadataLimits{
		MaxEntries:    maxAudioChunkMetadataEntries,
		MaxKeyRunes:   maxAudioChunkMetadataKeyRunes,
		MaxValueRunes: maxAudioChunkMetadataValueRunes,
		MaxTotalBytes: maxAudioChunkMetadataTotalBytes,
	})
}

func audioFormatsEqual(a, b eventbus.AudioFormat) bool {
	return a.Encoding == b.Encoding &&
		a.SampleRate == b.SampleRate &&
		a.Channels == b.Channels &&
		a.BitDepth == b.BitDepth
}

func playbackDuration(evt eventbus.AudioEgressPlaybackEvent) time.Duration {
	if evt.Duration > 0 {
		return evt.Duration
	}
	const maxPCMFrames = 1 << 30
	frames := audiofmt.PCMFrameCountFromBytes(evt.Format, len(evt.Data))
	if frames <= 0 || frames > maxPCMFrames {
		return 0
	}
	return audiofmt.DurationFromFrames(evt.Format.SampleRate, frames)
}

func durationToMillis(d time.Duration) uint32 {
	if d <= 0 {
		return 0
	}
	return uint32(d / time.Millisecond)
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

	if _, err := c.api.applyTransportConfig(ctx, storeCfg); err != nil {
		return nil, status.Errorf(codes.Internal, "apply transport config: %v", err)
	}

	return transportConfigToProto(storeCfg, c.api.AuthRequired()), nil
}

func (c *configService) Migrate(ctx context.Context, _ *apiv1.ConfigMigrateRequest) (*apiv1.ConfigMigrateResponse, error) {
	if c.api.configStore == nil {
		return nil, status.Error(codes.Unavailable, "configuration store unavailable")
	}

	result, err := c.api.configStore.EnsureRequiredAdapterSlots(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "migration failed: %v", err)
	}

	audioUpdated, err := c.api.configStore.EnsureAudioSettings(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "audio settings migration failed: %v", err)
	}

	return &apiv1.ConfigMigrateResponse{
		UpdatedSlots:         append([]string{}, result.UpdatedSlots...),
		PendingSlots:         append([]string{}, result.PendingSlots...),
		AudioSettingsUpdated: audioUpdated,
	}, nil
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
		resp.Adapters = append(resp.Adapters, adapterRecordToProto(record))
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
		resp.Bindings = append(resp.Bindings, adapterBindingToProto(binding))
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
		return adapterBindingToProto(binding), nil
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
		resp.CompletedAt = timestamppb.New(completedAt.UTC())
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
