package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/constants"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/language"
	"github.com/nupi-ai/nupi/internal/sanitize"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type storeIdentity interface {
	InstanceName() string
	ProfileName() string
}

func adapterLogDir(api *APIServer) (string, error) {
	instanceName := config.DefaultInstance
	if api != nil && api.configStore != nil {
		if ident, ok := api.configStore.(storeIdentity); ok {
			if v := strings.TrimSpace(ident.InstanceName()); v != "" {
				instanceName = v
			}
		}
	}
	paths, err := config.EnsureInstanceDirs(instanceName)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(paths.Logs, "plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func parseAdapterLogFilename(name string) (adapterSlug, slotSlug, stream string, ok bool) {
	if !strings.HasSuffix(name, ".log") {
		return "", "", "", false
	}
	base := strings.TrimSuffix(name, ".log")
	parts := strings.Split(base, ".")
	if len(parts) >= 4 && parts[0] == "plugin" {
		stream = parts[len(parts)-1]
		slotSlug = parts[len(parts)-2]
		adapterSlug = strings.Join(parts[1:len(parts)-2], ".")
		if adapterSlug == "" || slotSlug == "" || stream == "" {
			return "", "", "", false
		}
		return adapterSlug, slotSlug, stream, true
	}
	return "", "", "", false
}

func tailLines(path string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	lines := make([]string, 0, limit)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > limit {
			copy(lines, lines[1:])
			lines = lines[:limit]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

func loadAdapterLogEntries(ctx context.Context, api *APIServer, req *apiv1.GetAdapterLogsRequest) ([]*apiv1.AdapterLogStreamEntry, error) {
	limit := 200
	if req != nil && req.GetLimit() > 0 {
		limit = int(req.GetLimit())
	}
	slotFilter := strings.TrimSpace(req.GetSlot())
	adapterFilter := strings.TrimSpace(req.GetAdapter())
	slotSlug := ""
	adapterSlug := ""
	if slotFilter != "" {
		slotSlug = sanitize.SafeSlug(slotFilter)
	}
	if adapterFilter != "" {
		adapterSlug = sanitize.SafeSlug(adapterFilter)
	}

	dir, err := adapterLogDir(api)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "plugin logs dir: %v", err)
	}

	entries := make([]*apiv1.AdapterLogStreamEntry, 0, limit)
	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return entries, nil
		}
		return nil, status.Errorf(codes.Internal, "read plugin logs dir: %v", err)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name() < files[j].Name()
	})

	for _, file := range files {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		if file.IsDir() {
			continue
		}
		adapterName, slotName, stream, ok := parseAdapterLogFilename(file.Name())
		if !ok {
			continue
		}
		if adapterSlug != "" && adapterName != adapterSlug {
			continue
		}
		if slotSlug != "" && slotName != slotSlug {
			continue
		}
		path := filepath.Join(dir, file.Name())
		remaining := limit - len(entries)
		if remaining <= 0 {
			break
		}
		lines, err := tailLines(path, remaining)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read plugin log: %v", err)
		}
		level := "info"
		if stream == "stderr" {
			level = "error"
		}
		adapterID := adapterFilter
		if adapterID == "" {
			adapterID = adapterName
		}
		now := time.Now().UTC()
		for _, line := range lines {
			if line == "" {
				continue
			}
			entries = append(entries, &apiv1.AdapterLogStreamEntry{
				Payload: &apiv1.AdapterLogStreamEntry_Log{
					Log: &apiv1.AdapterLogEntry{
						Timestamp: timestamppb.New(now),
						Level:     level,
						Message:   line,
						Slot:      slotName,
						AdapterId: adapterID,
					},
				},
			})
		}
	}
	return entries, nil
}

func tailAdapterLogEntries(ctx context.Context, api *APIServer, req *apiv1.GetAdapterLogsRequest, startAtEnd bool) (<-chan *apiv1.AdapterLogStreamEntry, <-chan error) {
	out := make(chan *apiv1.AdapterLogStreamEntry, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errCh)

		slotFilter := strings.TrimSpace(req.GetSlot())
		adapterFilter := strings.TrimSpace(req.GetAdapter())
		slotSlug := ""
		adapterSlug := ""
		if slotFilter != "" {
			slotSlug = sanitize.SafeSlug(slotFilter)
		}
		if adapterFilter != "" {
			adapterSlug = sanitize.SafeSlug(adapterFilter)
		}

		dir, err := adapterLogDir(api)
		if err != nil {
			errCh <- status.Errorf(codes.Internal, "plugin logs dir: %v", err)
			return
		}

		offsets := make(map[string]int64)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			files, err := os.ReadDir(dir)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				errCh <- status.Errorf(codes.Internal, "read plugin logs dir: %v", err)
				return
			}
			sort.Slice(files, func(i, j int) bool {
				return files[i].Name() < files[j].Name()
			})

			for _, file := range files {
				if file.IsDir() {
					continue
				}
				adapterName, slotName, stream, ok := parseAdapterLogFilename(file.Name())
				if !ok {
					continue
				}
				if adapterSlug != "" && adapterName != adapterSlug {
					continue
				}
				if slotSlug != "" && slotName != slotSlug {
					continue
				}

				path := filepath.Join(dir, file.Name())
				info, err := file.Info()
				if err != nil {
					continue
				}
				size := info.Size()
				offset, seen := offsets[path]
				if size < offset {
					offset = 0
				}
				if startAtEnd && !seen {
					offsets[path] = size
					continue
				}

				f, err := os.Open(path)
				if err != nil {
					continue
				}
				if offset > 0 {
					if _, err := f.Seek(offset, io.SeekStart); err != nil {
						f.Close()
						continue
					}
				}

				scanner := bufio.NewScanner(f)
				level := "info"
				if stream == "stderr" {
					level = "error"
				}
				adapterID := adapterFilter
				if adapterID == "" {
					adapterID = adapterName
				}
				now := time.Now().UTC()
				for scanner.Scan() {
					line := scanner.Text()
					if line == "" {
						continue
					}
					select {
					case <-ctx.Done():
						f.Close()
						return
					case out <- &apiv1.AdapterLogStreamEntry{
						Payload: &apiv1.AdapterLogStreamEntry_Log{
							Log: &apiv1.AdapterLogEntry{
								Timestamp: timestamppb.New(now),
								Level:     level,
								Message:   line,
								Slot:      slotName,
								AdapterId: adapterID,
							},
						},
					}:
					}
				}
				if err := scanner.Err(); err == nil {
					if pos, err := f.Seek(0, io.SeekCurrent); err == nil {
						offsets[path] = pos
					}
				}
				f.Close()
			}
		}
	}()

	return out, errCh
}

// ─── DaemonService: Shutdown ───

func (d *daemonService) Shutdown(ctx context.Context, _ *apiv1.ShutdownRequest) (*apiv1.ShutdownResponse, error) {
	d.api.lifecycle.shutdownMu.RLock()
	shutdown := d.api.lifecycle.shutdownFn
	d.api.lifecycle.shutdownMu.RUnlock()

	if shutdown == nil {
		return nil, status.Error(codes.Unimplemented, "daemon shutdown not available")
	}

	// Trigger shutdown asynchronously so we can return the response.
	go func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), constants.Duration2Seconds)
		defer cancel()
		_ = shutdown(shutdownCtx)
	}()

	return &apiv1.ShutdownResponse{Message: "daemon shutdown initiated"}, nil
}

func (d *daemonService) ReloadPlugins(ctx context.Context, _ *apiv1.ReloadPluginsRequest) (*apiv1.ReloadPluginsResponse, error) {
	if d.api.pluginReloader == nil {
		return nil, status.Error(codes.Unavailable, "plugin reloader unavailable")
	}

	if err := d.api.pluginReloader.Reload(); err != nil {
		return nil, status.Errorf(codes.Internal, "reload plugins: %v", err)
	}

	return &apiv1.ReloadPluginsResponse{Message: "plugins reloaded"}, nil
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
			case constants.AdapterTransportGRPC:
				if address == "" {
					return nil, status.Error(codes.InvalidArgument, "endpoint address required for grpc transport")
				}
			case constants.AdapterTransportHTTP:
				if address == "" {
					return nil, status.Error(codes.InvalidArgument, "endpoint address required for http transport")
				}
				if command != "" || len(ep.GetArgs()) > 0 {
					return nil, status.Error(codes.InvalidArgument, "endpoint command/args not allowed for http transport")
				}
			case constants.AdapterTransportProcess:
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
				endpoint.Env = maputil.Clone(env)
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
	if req == nil {
		req = &apiv1.StreamAdapterLogsRequest{}
	}

	ctx := srv.Context()
	slotFilter := strings.ToLower(strings.TrimSpace(req.GetSlot()))
	adapterFilter := strings.ToLower(strings.TrimSpace(req.GetAdapter()))

	if !req.GetFollow() {
		resp, err := m.GetAdapterLogs(ctx, &apiv1.GetAdapterLogsRequest{
			Slot:    req.GetSlot(),
			Limit:   0,
			Adapter: req.GetAdapter(),
		})
		if err != nil {
			return err
		}
		for _, entry := range resp.Entries {
			if err := srv.Send(entry); err != nil {
				return err
			}
		}
		return nil
	}

	bootstrap, err := m.GetAdapterLogs(ctx, &apiv1.GetAdapterLogsRequest{
		Slot:    req.GetSlot(),
		Limit:   200,
		Adapter: req.GetAdapter(),
	})
	if err != nil {
		return err
	}
	for _, entry := range bootstrap.Entries {
		if err := srv.Send(entry); err != nil {
			return err
		}
	}

	logCh, logErrCh := tailAdapterLogEntries(ctx, m.api, &apiv1.GetAdapterLogsRequest{
		Slot:    req.GetSlot(),
		Limit:   0,
		Adapter: req.GetAdapter(),
	}, true)

	var (
		subPartial *eventbus.TypedSubscription[eventbus.SpeechTranscriptEvent]
		subFinal   *eventbus.TypedSubscription[eventbus.SpeechTranscriptEvent]
		partialCh  <-chan eventbus.TypedEnvelope[eventbus.SpeechTranscriptEvent]
		finalCh    <-chan eventbus.TypedEnvelope[eventbus.SpeechTranscriptEvent]
	)
	if m.api.eventBus != nil {
		subPartial = eventbus.Subscribe[eventbus.SpeechTranscriptEvent](m.api.eventBus, eventbus.TopicSpeechTranscriptPartial,
			eventbus.WithSubscriptionName(fmt.Sprintf("grpc_adapter_logs_partial_%d", time.Now().UnixNano())),
			eventbus.WithSubscriptionBuffer(32),
		)
		subFinal = eventbus.Subscribe[eventbus.SpeechTranscriptEvent](m.api.eventBus, eventbus.TopicSpeechTranscriptFinal,
			eventbus.WithSubscriptionName(fmt.Sprintf("grpc_adapter_logs_final_%d", time.Now().UnixNano())),
			eventbus.WithSubscriptionBuffer(32),
		)
		partialCh = subPartial.C()
		finalCh = subFinal.C()
		defer subPartial.Close()
		defer subFinal.Close()
	}

	for {
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case err := <-logErrCh:
			if err != nil {
				return err
			}
		case entry, ok := <-logCh:
			if !ok {
				return nil
			}
			if err := srv.Send(entry); err != nil {
				return err
			}
		case env, ok := <-partialCh:
			if !ok {
				partialCh = nil
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
				finalCh = nil
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
	entries, err := loadAdapterLogEntries(ctx, m.api, req)
	if err != nil {
		return nil, err
	}
	return &apiv1.GetAdapterLogsResponse{Entries: entries}, nil
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

func (r *recordingsService) ListRecordings(ctx context.Context, req *apiv1.ListRecordingsRequest) (*apiv1.ListRecordingsResponse, error) {

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
	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID == "" {
		return status.Error(codes.InvalidArgument, "session_id is required")
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
					"filename":     metadata.Filename,
					"session_id":   metadata.SessionID,
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
