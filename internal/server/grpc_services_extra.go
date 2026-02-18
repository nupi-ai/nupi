package server

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/language"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ─── DaemonService: Shutdown, GetPluginWarnings ───

func (d *daemonService) Shutdown(ctx context.Context, _ *apiv1.ShutdownRequest) (*apiv1.ShutdownResponse, error) {
	if _, err := d.api.requireRoleGRPC(ctx, roleAdmin); err != nil {
		return nil, err
	}

	d.api.lifecycle.shutdownMu.RLock()
	shutdown := d.api.lifecycle.shutdownFn
	d.api.lifecycle.shutdownMu.RUnlock()

	if shutdown == nil {
		return nil, status.Error(codes.Unimplemented, "daemon shutdown not available")
	}

	// Trigger shutdown asynchronously so we can return the response.
	go func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shutdown(shutdownCtx)
	}()

	return &apiv1.ShutdownResponse{Message: "daemon shutdown initiated"}, nil
}

func (d *daemonService) GetPluginWarnings(ctx context.Context, _ *apiv1.GetPluginWarningsRequest) (*apiv1.GetPluginWarningsResponse, error) {
	if _, err := d.api.requireRoleGRPC(ctx, roleAdmin, roleReadOnly); err != nil {
		return nil, err
	}

	resp := &apiv1.GetPluginWarningsResponse{
		Warnings: make([]*apiv1.PluginWarning, 0),
	}

	if d.api.observability.pluginWarnings == nil {
		return resp, nil
	}

	warnings := d.api.observability.pluginWarnings.GetDiscoveryWarnings()
	for _, w := range warnings {
		resp.Warnings = append(resp.Warnings, &apiv1.PluginWarning{
			Dir:   w.Dir,
			Error: w.Error,
		})
	}

	return resp, nil
}

// ListLanguages returns the complete language registry. No auth check — the
// language list is public information and does not require any role.
func (d *daemonService) ListLanguages(_ context.Context, _ *apiv1.ListLanguagesRequest) (*apiv1.ListLanguagesResponse, error) {
	langs := language.All()
	out := make([]*apiv1.LanguageInfo, len(langs))
	for i, l := range langs {
		out[i] = &apiv1.LanguageInfo{
			Iso1:        l.ISO1,
			Bcp47:       l.BCP47,
			EnglishName: l.EnglishName,
			NativeName:  l.NativeName,
		}
	}
	return &apiv1.ListLanguagesResponse{Languages: out}, nil
}

// ─── AdapterRuntimeService: RegisterAdapter, StreamAdapterLogs, GetAdapterLogs ───

func (m *adapterRuntimeService) RegisterAdapter(ctx context.Context, req *apiv1.RegisterAdapterRequest) (*apiv1.RegisterAdapterResponse, error) {
	if _, err := m.api.requireRoleGRPC(ctx, roleAdmin); err != nil {
		return nil, err
	}
	if m.api.configStore == nil {
		return nil, status.Error(codes.Unavailable, "configuration store unavailable")
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	adapterID := strings.TrimSpace(req.GetAdapterId())
	if adapterID == "" {
		return nil, status.Error(codes.InvalidArgument, "adapter_id is required")
	}
	if len(adapterID) > maxAdapterIDLength {
		return nil, status.Errorf(codes.InvalidArgument, "adapter_id too long (max %d chars)", maxAdapterIDLength)
	}

	adapterType := strings.TrimSpace(req.GetType())
	if adapterType != "" {
		if _, ok := allowedAdapterTypes[adapterType]; !ok {
			return nil, status.Errorf(codes.InvalidArgument, "invalid adapter type: %s", adapterType)
		}
	}

	adapterName := strings.TrimSpace(req.GetName())
	if len(adapterName) > maxAdapterNameLength {
		return nil, status.Errorf(codes.InvalidArgument, "name too long (max %d chars)", maxAdapterNameLength)
	}

	adapterVersion := strings.TrimSpace(req.GetVersion())
	if len(adapterVersion) > maxAdapterVersionLength {
		return nil, status.Errorf(codes.InvalidArgument, "version too long (max %d chars)", maxAdapterVersionLength)
	}

	manifestYAML := strings.TrimSpace(req.GetManifestYaml())
	if len(manifestYAML) > maxAdapterManifestBytes {
		return nil, status.Errorf(codes.InvalidArgument, "manifest too large (max %d bytes)", maxAdapterManifestBytes)
	}

	adapter := configstore.Adapter{
		ID:       adapterID,
		Source:   strings.TrimSpace(req.GetSource()),
		Version:  adapterVersion,
		Type:     adapterType,
		Name:     adapterName,
		Manifest: manifestYAML,
	}

	if err := m.api.configStore.UpsertAdapter(ctx, adapter); err != nil {
		return nil, status.Errorf(codes.Internal, "register adapter: %v", err)
	}

	// Register endpoint if provided.
	if ep := req.GetEndpoint(); ep != nil {
		transport := strings.TrimSpace(ep.GetTransport())
		if transport != "" {
			if _, ok := allowedAdapterTransports[transport]; !ok {
				return nil, status.Errorf(codes.InvalidArgument, "invalid transport: %s", transport)
			}
			address := strings.TrimSpace(ep.GetAddress())
			command := strings.TrimSpace(ep.GetCommand())
			switch transport {
			case "grpc":
				if address == "" {
					return nil, status.Error(codes.InvalidArgument, "endpoint address required for grpc transport")
				}
			case "http":
				if address == "" {
					return nil, status.Error(codes.InvalidArgument, "endpoint address required for http transport")
				}
				if command != "" || len(ep.GetArgs()) > 0 {
					return nil, status.Error(codes.InvalidArgument, "endpoint command/args not allowed for http transport")
				}
			case "process":
				if command == "" {
					return nil, status.Error(codes.InvalidArgument, "endpoint command required for process transport")
				}
				if address != "" {
					return nil, status.Error(codes.InvalidArgument, "endpoint address not used for process transport")
				}
			}
			endpoint := configstore.AdapterEndpoint{
				AdapterID: adapterID,
				Transport: transport,
				Address:   address,
				Command:   command,
			}
			if args := ep.GetArgs(); len(args) > 0 {
				endpoint.Args = append([]string(nil), args...)
			}
			if env := ep.GetEnv(); len(env) > 0 {
				endpoint.Env = cloneStringMap(env)
			}
			if err := m.api.configStore.UpsertAdapterEndpoint(ctx, endpoint); err != nil {
				return nil, status.Errorf(codes.Internal, "register adapter endpoint: %v", err)
			}
		}
	}

	return &apiv1.RegisterAdapterResponse{
		AdapterId:    adapter.ID,
		Name:         adapter.Name,
		Type:         adapter.Type,
		Version:      adapter.Version,
		Source:       adapter.Source,
		ManifestYaml: adapter.Manifest,
	}, nil
}

func (m *adapterRuntimeService) StreamAdapterLogs(req *apiv1.StreamAdapterLogsRequest, srv apiv1.AdapterRuntimeService_StreamAdapterLogsServer) error {
	if _, err := m.api.requireRoleGRPC(srv.Context(), roleAdmin, roleReadOnly); err != nil {
		return err
	}
	if m.api.eventBus == nil {
		return status.Error(codes.Unavailable, "event bus unavailable")
	}

	slotFilter := strings.ToLower(strings.TrimSpace(req.GetSlot()))
	adapterFilter := strings.ToLower(strings.TrimSpace(req.GetAdapter()))

	subLogs := eventbus.Subscribe[eventbus.AdapterLogEvent](m.api.eventBus, eventbus.TopicAdaptersLog,
		eventbus.WithSubscriptionName(fmt.Sprintf("grpc_adapter_logs_%d", time.Now().UnixNano())),
		eventbus.WithSubscriptionBuffer(64),
	)
	defer subLogs.Close()

	subPartial := eventbus.Subscribe[eventbus.SpeechTranscriptEvent](m.api.eventBus, eventbus.TopicSpeechTranscriptPartial,
		eventbus.WithSubscriptionName(fmt.Sprintf("grpc_adapter_logs_partial_%d", time.Now().UnixNano())),
		eventbus.WithSubscriptionBuffer(32),
	)
	defer subPartial.Close()

	subFinal := eventbus.Subscribe[eventbus.SpeechTranscriptEvent](m.api.eventBus, eventbus.TopicSpeechTranscriptFinal,
		eventbus.WithSubscriptionName(fmt.Sprintf("grpc_adapter_logs_final_%d", time.Now().UnixNano())),
		eventbus.WithSubscriptionBuffer(32),
	)
	defer subFinal.Close()

	ctx := srv.Context()

	// Channel variables for subscriptions. Set to nil when the subscription's
	// channel is closed to prevent CPU spin in the select loop.
	logsCh := subLogs.C()
	partialCh := subPartial.C()
	finalCh := subFinal.C()

	for {
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()

		case env, ok := <-logsCh:
			if !ok {
				return nil
			}
			evt := env.Payload
			slotValue := strings.TrimSpace(evt.Fields["slot"])
			if slotFilter != "" && strings.ToLower(slotValue) != slotFilter {
				continue
			}
			if adapterFilter != "" && strings.ToLower(strings.TrimSpace(evt.AdapterID)) != adapterFilter {
				continue
			}
			ts := env.Timestamp
			if ts.IsZero() {
				ts = time.Now().UTC()
			}
			if err := srv.Send(&apiv1.AdapterLogStreamEntry{
				Payload: &apiv1.AdapterLogStreamEntry_Log{
					Log: &apiv1.AdapterLogEntry{
						Timestamp: timestamppb.New(ts),
						Level:     string(evt.Level),
						Message:   evt.Message,
						Slot:      slotValue,
						AdapterId: evt.AdapterID,
					},
				},
			}); err != nil {
				return err
			}

		case env, ok := <-partialCh:
			if !ok {
				partialCh = nil // disable this select case to prevent CPU spin
				continue
			}
			if slotFilter != "" && adapterFilter != "" {
				continue
			}
			if err := srv.Send(transcriptEventToProto(env)); err != nil {
				return err
			}

		case env, ok := <-finalCh:
			if !ok {
				finalCh = nil // disable this select case to prevent CPU spin
				continue
			}
			if slotFilter != "" && adapterFilter != "" {
				continue
			}
			if err := srv.Send(transcriptEventToProto(env)); err != nil {
				return err
			}
		}
	}
}

func (m *adapterRuntimeService) GetAdapterLogs(ctx context.Context, req *apiv1.GetAdapterLogsRequest) (*apiv1.GetAdapterLogsResponse, error) {
	if _, err := m.api.requireRoleGRPC(ctx, roleAdmin, roleReadOnly); err != nil {
		return nil, err
	}

	// GetAdapterLogs returns recent buffered log entries. Since the event bus is
	// real-time and doesn't retain history, return an empty list. Clients should
	// use StreamAdapterLogs for live tailing.
	return &apiv1.GetAdapterLogsResponse{
		Entries: make([]*apiv1.AdapterLogStreamEntry, 0),
	}, nil
}

func transcriptEventToProto(env eventbus.TypedEnvelope[eventbus.SpeechTranscriptEvent]) *apiv1.AdapterLogStreamEntry {
	evt := env.Payload
	ts := env.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	return &apiv1.AdapterLogStreamEntry{
		Payload: &apiv1.AdapterLogStreamEntry_Transcript{
			Transcript: &apiv1.TranscriptEntry{
				Timestamp:  timestamppb.New(ts),
				SessionId:  evt.SessionID,
				StreamId:   evt.StreamID,
				Text:       evt.Text,
				Confidence: float64(evt.Confidence),
				IsFinal:    evt.Final,
			},
		},
	}
}

// ─── RecordingsService ───

type recordingsService struct {
	apiv1.UnimplementedRecordingsServiceServer
	api *APIServer
}

func newRecordingsService(api *APIServer) *recordingsService {
	return &recordingsService{api: api}
}

func (r *recordingsService) ListRecordings(ctx context.Context, req *apiv1.ListRecordingsRequest) (*apiv1.ListRecordingsResponse, error) {
	if _, err := r.api.requireRoleGRPC(ctx, roleAdmin, roleReadOnly); err != nil {
		return nil, err
	}

	if r.api.sessionManager == nil {
		return nil, status.Error(codes.Unavailable, "session manager unavailable")
	}

	store := r.api.sessionManager.GetRecordingStore()
	if store == nil {
		return nil, status.Error(codes.Unavailable, "recording store not available")
	}

	metadata, err := store.LoadAll()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load recordings: %v", err)
	}

	filterSession := strings.TrimSpace(req.GetSessionId())

	resp := &apiv1.ListRecordingsResponse{
		Recordings: make([]*apiv1.Recording, 0, len(metadata)),
	}
	for _, m := range metadata {
		if filterSession != "" && m.SessionID != filterSession {
			continue
		}
		var startTime *timestamppb.Timestamp
		if !m.StartTime.IsZero() {
			startTime = timestamppb.New(m.StartTime.UTC())
		}
		resp.Recordings = append(resp.Recordings, &apiv1.Recording{
			SessionId:     m.SessionID,
			Filename:      m.Filename,
			Command:       m.Command,
			Args:          m.Args,
			WorkDir:       m.WorkDir,
			StartTime:     startTime,
			DurationSec:   m.Duration,
			Rows:          uint32(m.Rows),
			Cols:          uint32(m.Cols),
			Title:         m.Title,
			Tool:          m.Tool,
			RecordingPath: m.RecordingPath,
		})
	}

	return resp, nil
}

func (r *recordingsService) GetRecording(req *apiv1.GetRecordingRequest, srv apiv1.RecordingsService_GetRecordingServer) error {
	if _, err := r.api.requireRoleGRPC(srv.Context(), roleAdmin, roleReadOnly); err != nil {
		return err
	}

	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID == "" {
		return status.Error(codes.InvalidArgument, "session_id is required")
	}

	if r.api.sessionManager == nil {
		return status.Error(codes.Unavailable, "session manager unavailable")
	}

	store := r.api.sessionManager.GetRecordingStore()
	if store == nil {
		return status.Error(codes.Unavailable, "recording store not available")
	}

	metadata, err := store.GetBySessionID(sessionID)
	if err != nil {
		return status.Errorf(codes.NotFound, "recording not found: %v", err)
	}

	file, err := os.Open(metadata.RecordingPath)
	if err != nil {
		return status.Errorf(codes.Internal, "open recording file: %v", err)
	}
	defer file.Close()

	const chunkSize = 32 * 1024
	buf := make([]byte, chunkSize)
	var seq uint64

	for {
		n, readErr := file.Read(buf)
		if n > 0 {
			chunk := &apiv1.RecordingChunk{
				Data:     append([]byte(nil), buf[:n]...),
				Sequence: seq,
			}
			if seq == 0 {
				chunk.Metadata = map[string]string{
					"filename":    metadata.Filename,
					"session_id":  metadata.SessionID,
					"content_type": "application/x-asciicast",
				}
			}
			if err := srv.Send(chunk); err != nil {
				return err
			}
			seq++
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return status.Errorf(codes.Internal, "read recording: %v", readErr)
		}
	}

	return nil
}
