package server

import (
	"sort"
	"strconv"
	"strings"

	"github.com/nupi-ai/nupi/internal/api"
	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/mapper"
	"github.com/nupi-ai/nupi/internal/plugins/adapters"
	"github.com/nupi-ai/nupi/internal/session"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func conversationTurnsToProto(turns []eventbus.ConversationTurn) []*apiv1.ConversationTurn {
	out := make([]*apiv1.ConversationTurn, 0, len(turns))
	for _, turn := range turns {
		out = append(out, conversationTurnToProto(turn))
	}
	return out
}

func conversationTurnToProto(turn eventbus.ConversationTurn) *apiv1.ConversationTurn {
	return &apiv1.ConversationTurn{
		Origin:   string(turn.Origin),
		Text:     turn.Text,
		At:       mapper.ToProtoTimestamp(turn.At),
		Metadata: conversationMetadataToProto(turn.Meta),
	}
}

func conversationMetadataToProto(meta map[string]string) []*apiv1.ConversationMetadata {
	if len(meta) == 0 {
		return nil
	}

	keys := make([]string, 0, len(meta))
	for key := range meta {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	metadata := make([]*apiv1.ConversationMetadata, 0, len(keys))
	for _, key := range keys {
		metadata = append(metadata, &apiv1.ConversationMetadata{
			Key:   key,
			Value: meta[key],
		})
	}
	return metadata
}

func adapterRecordToProto(record configstore.Adapter) *apiv1.Adapter {
	return &apiv1.Adapter{
		Id:        record.ID,
		Source:    record.Source,
		Version:   record.Version,
		Type:      record.Type,
		Name:      record.Name,
		Manifest:  record.Manifest,
		CreatedAt: mapper.ParseTimestampStringToProto(record.CreatedAt),
		UpdatedAt: mapper.ParseTimestampStringToProto(record.UpdatedAt),
	}
}

func adapterBindingToProto(binding configstore.AdapterBinding) *apiv1.AdapterBinding {
	adapterID := ""
	if binding.AdapterID != nil {
		adapterID = *binding.AdapterID
	}
	return &apiv1.AdapterBinding{
		Slot:       binding.Slot,
		AdapterId:  adapterID,
		Status:     binding.Status,
		ConfigJson: binding.Config,
		UpdatedAt:  mapper.ParseTimestampStringToProto(binding.UpdatedAt),
	}
}

func bindingStatusToProto(status adapters.BindingStatus) *apiv1.AdapterEntry {
	entry := &apiv1.AdapterEntry{
		Slot:       string(status.Slot),
		Status:     status.Status,
		ConfigJson: status.Config,
		UpdatedAt:  mapper.ParseTimestampStringToProto(status.UpdatedAt),
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
	result.StartedAt = mapper.ToProtoTimestampPtr(rt.StartedAt)
	result.UpdatedAt = mapper.ToProtoTimestamp(rt.UpdatedAt)
	return result
}

// dtoToSessionProto converts a SessionDTO to a proto Session message, populating all fields.
func dtoToSessionProto(dto api.SessionDTO, apiServer *APIServer) *apiv1.Session {
	pb := &apiv1.Session{
		Id:           dto.ID,
		Command:      dto.Command,
		Args:         append([]string(nil), dto.Args...),
		Status:       dto.Status,
		Pid:          int32(dto.PID),
		WorkDir:      dto.WorkDir,
		Tool:         dto.Tool,
		ToolIcon:     dto.ToolIcon,
		ToolIconData: dto.ToolIconData,
		Mode:         dto.Mode,
	}
	pb.StartTime = mapper.ToProtoTimestamp(dto.StartTime)
	// Set exit_code only when process has actually exited (status == "stopped").
	// Without this guard, proto3 default 0 is ambiguous with "exited with code 0".
	if dto.Status == string(session.StatusStopped) {
		pb.ExitCode = wrapperspb.Int32(int32(dto.ExitCode))
	}
	// Populate mode from resize manager if available and not already set.
	if pb.Mode == "" && apiServer != nil && apiServer.resizeManager != nil {
		pb.Mode = apiServer.resizeManager.GetSessionMode(dto.ID)
	}
	return pb
}

func attachSessionEventResponse(eventType apiv1.SessionEventType, sessionID string, data map[string]string) *apiv1.AttachSessionResponse {
	return &apiv1.AttachSessionResponse{
		Payload: &apiv1.AttachSessionResponse_Event{
			Event: &apiv1.SessionEvent{
				Type:      eventType,
				SessionId: sessionID,
				Data:      data,
			},
		},
	}
}

func resizeInstructionEventResponse(sessionID string, cols, rows int, reason string) *apiv1.AttachSessionResponse {
	return attachSessionEventResponse(apiv1.SessionEventType_SESSION_EVENT_TYPE_RESIZE_INSTRUCTION, sessionID, map[string]string{
		"cols":   strconv.Itoa(cols),
		"rows":   strconv.Itoa(rows),
		"reason": reason,
	})
}

func sessionLifecycleEventData(evt eventbus.SessionLifecycleEvent) map[string]string {
	data := map[string]string{}
	if evt.ExitCode != nil {
		data["exit_code"] = strconv.Itoa(*evt.ExitCode)
	}
	if evt.Reason != "" {
		data["reason"] = evt.Reason
	}
	return data
}

func sessionToolChangedEventData(evt eventbus.SessionToolChangedEvent) map[string]string {
	return map[string]string{
		"previous_tool": evt.PreviousTool,
		"new_tool":      evt.NewTool,
	}
}
