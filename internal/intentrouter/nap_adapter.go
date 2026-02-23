package intentrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/napdial"
	maputil "github.com/nupi-ai/nupi/internal/util/maps"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// NAPAdapter implements IntentAdapter using NAP AI gRPC protocol.
// It connects to an external AI adapter process and delegates intent resolution.
type NAPAdapter struct {
	adapterID string
	conn      *grpc.ClientConn
	client    napv1.IntentResolutionServiceClient
	config    map[string]any

	mu    sync.RWMutex
	ready bool
}

// NAPAdapterParams contains parameters for creating a NAP AI adapter.
type NAPAdapterParams struct {
	AdapterID string
	Endpoint  configstore.AdapterEndpoint
	Config    map[string]any
}

// connectTimeout is the minimum time for a single gRPC connection attempt to the adapter.
const connectTimeout = 10 * time.Second

// requestTimeout is the maximum time for a single ResolveIntent RPC call.
const requestTimeout = 30 * time.Second

// capabilitiesProbeTimeout is the time allowed for the best-effort capabilities probe.
const capabilitiesProbeTimeout = 3 * time.Second

// NewNAPAdapter creates a new NAP AI adapter that connects to a gRPC endpoint.
func NewNAPAdapter(ctx context.Context, params NAPAdapterParams) (*NAPAdapter, error) {
	endpoint := params.Endpoint
	endpoint.AdapterID = params.AdapterID

	conn, err := napdial.DialAdapter(ctx, endpoint, grpc.WithConnectParams(grpc.ConnectParams{
		MinConnectTimeout: connectTimeout,
	}))
	if err != nil {
		return nil, fmt.Errorf("ai: %w", err)
	}

	client := napv1.NewIntentResolutionServiceClient(conn)

	adapter := &NAPAdapter{
		adapterID: params.AdapterID,
		conn:      conn,
		client:    client,
		config:    params.Config,
		ready:     true,
	}

	// Best-effort capabilities probe — runs in background, never prevents startup.
	go probeCapabilities(client, params.AdapterID)

	return adapter, nil
}

// probeCapabilities calls GetCapabilities on the adapter to log supported features.
// This is best-effort: UNIMPLEMENTED is expected for older adapters and silently ignored.
func probeCapabilities(client napv1.IntentResolutionServiceClient, adapterID string) {
	probeCtx, probeCancel := context.WithTimeout(context.Background(), capabilitiesProbeTimeout)
	defer probeCancel()

	resp, err := client.GetCapabilities(probeCtx, &napv1.GetCapabilitiesRequest{})
	if err != nil {
		if s, ok := status.FromError(err); ok && s.Code() == codes.Unimplemented {
			log.Printf("[NAPAdapter] %s: GetCapabilities not implemented (ok)", adapterID)
			return
		}
		log.Printf("[NAPAdapter] %s: GetCapabilities probe failed: %v", adapterID, err)
		return
	}
	log.Printf("[NAPAdapter] %s: adapter=%s version=%s capabilities=%d",
		adapterID, resp.GetAdapterName(), resp.GetAdapterVersion(), len(resp.GetCapabilities()))
	for _, c := range resp.GetCapabilities() {
		log.Printf("[NAPAdapter] %s:   capability: %s v%s", adapterID, c.GetName(), c.GetVersion())
	}
}

// Name returns the adapter's identifier.
func (a *NAPAdapter) Name() string {
	return a.adapterID
}

// Ready returns true if the adapter is ready to process requests.
func (a *NAPAdapter) Ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ready
}

// ResolveIntent processes user input and returns intended action(s).
func (a *NAPAdapter) ResolveIntent(ctx context.Context, req IntentRequest) (*IntentResponse, error) {
	a.mu.RLock()
	if !a.ready {
		a.mu.RUnlock()
		return nil, ErrAdapterNotReady
	}
	a.mu.RUnlock()

	// Build gRPC request with all protocol fields
	grpcReq := &napv1.ResolveIntentRequest{
		PromptId:              req.PromptID,
		SessionId:             req.SessionID,
		Transcript:            req.Transcript,
		Metadata:              maputil.Clone(req.Metadata),
		EventType:             eventTypeToProto(req.EventType),
		CurrentTool:           req.CurrentTool,
		SessionOutput:         req.SessionOutput,
		ClarificationQuestion: req.ClarificationQuestion,
		SystemPrompt:          req.SystemPrompt,
		UserPrompt:            req.UserPrompt,
	}

	// Marshal config if present
	if len(a.config) > 0 {
		configJSON, err := json.Marshal(a.config)
		if err != nil {
			return nil, fmt.Errorf("ai: marshal config: %w", err)
		}
		grpcReq.ConfigJson = string(configJSON)
	}

	// Convert conversation history
	for _, turn := range req.ConversationHistory {
		grpcReq.ConversationHistory = append(grpcReq.ConversationHistory, &napv1.ConversationTurn{
			Origin:   contentOriginToProto(turn.Origin),
			Text:     turn.Text,
			At:       timestampOrNil(turn.At),
			Metadata: maputil.Clone(turn.Meta),
		})
	}

	// Convert available tools
	for _, td := range req.AvailableTools {
		grpcReq.AvailableTools = append(grpcReq.AvailableTools, &napv1.ToolDefinition{
			Name:           td.Name,
			Description:    td.Description,
			ParametersJson: td.ParametersJSON,
		})
	}

	// Convert tool history
	for _, ti := range req.ToolHistory {
		grpcReq.ToolHistory = append(grpcReq.ToolHistory, &napv1.ToolInteraction{
			Call: &napv1.ToolCall{
				CallId:        ti.Call.CallID,
				ToolName:      ti.Call.ToolName,
				ArgumentsJson: ti.Call.ArgumentsJSON,
			},
			Result: &napv1.ToolResult{
				CallId:     ti.Result.CallID,
				ResultJson: ti.Result.ResultJSON,
				IsError:    ti.Result.IsError,
			},
		})
	}

	// Convert available sessions
	for _, session := range req.AvailableSessions {
		grpcReq.AvailableSessions = append(grpcReq.AvailableSessions, &napv1.SessionInfo{
			Id:        session.ID,
			Command:   session.Command,
			Args:      session.Args,
			WorkDir:   session.WorkDir,
			Tool:      session.Tool,
			Status:    session.Status,
			StartTime: timestampOrNil(session.StartTime),
			Metadata:  maputil.Clone(session.Metadata),
		})
	}

	// Apply per-request timeout to bound AI processing time.
	reqCtx, reqCancel := context.WithTimeout(ctx, requestTimeout)
	defer reqCancel()

	// Call the adapter
	grpcResp, err := a.client.ResolveIntent(reqCtx, grpcReq)
	if err != nil {
		return nil, fmt.Errorf("ai: resolve intent: %w", err)
	}

	// Check for error in response
	if grpcResp.ErrorMessage != "" {
		return nil, fmt.Errorf("ai: adapter error: %s", grpcResp.ErrorMessage)
	}

	// Convert response
	resp := &IntentResponse{
		PromptID:   grpcResp.PromptId,
		Reasoning:  grpcResp.Reasoning,
		Confidence: grpcResp.Confidence,
		Metadata:   maputil.Clone(grpcResp.Metadata),
	}

	for _, action := range grpcResp.Actions {
		resp.Actions = append(resp.Actions, IntentAction{
			Type:       actionTypeFromProto(action.Type),
			SessionRef: action.SessionRef,
			Command:    action.Command,
			Text:       action.Text,
			Metadata:   maputil.Clone(action.Metadata),
		})
	}

	// Convert tool calls
	for _, tc := range grpcResp.ToolCalls {
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{
			CallID:        tc.CallId,
			ToolName:      tc.ToolName,
			ArgumentsJSON: tc.ArgumentsJson,
		})
	}

	return resp, nil
}

// Close closes the gRPC connection.
func (a *NAPAdapter) Close() error {
	a.mu.Lock()
	a.ready = false
	a.mu.Unlock()

	if a.conn != nil {
		return a.conn.Close()
	}
	return nil
}

// SetReady sets the adapter's ready state (used by bridge on status events).
func (a *NAPAdapter) SetReady(ready bool) {
	a.mu.Lock()
	a.ready = ready
	a.mu.Unlock()
}

// contentOriginToProto converts eventbus.ContentOrigin to proto enum.
func contentOriginToProto(origin eventbus.ContentOrigin) napv1.ContentOrigin {
	switch origin {
	case eventbus.OriginUser:
		return napv1.ContentOrigin_CONTENT_ORIGIN_USER
	case eventbus.OriginAI:
		return napv1.ContentOrigin_CONTENT_ORIGIN_AI
	case eventbus.OriginTool:
		return napv1.ContentOrigin_CONTENT_ORIGIN_TOOL
	case eventbus.OriginSystem:
		return napv1.ContentOrigin_CONTENT_ORIGIN_SYSTEM
	default:
		return napv1.ContentOrigin_CONTENT_ORIGIN_UNSPECIFIED
	}
}

// eventTypeToProto converts EventType to proto enum.
func eventTypeToProto(et EventType) napv1.EventType {
	switch et {
	case EventTypeUserIntent:
		return napv1.EventType_EVENT_TYPE_USER_INTENT
	case EventTypeSessionOutput:
		return napv1.EventType_EVENT_TYPE_SESSION_OUTPUT
	case EventTypeHistorySummary:
		return napv1.EventType_EVENT_TYPE_HISTORY_SUMMARY
	case EventTypeClarification:
		return napv1.EventType_EVENT_TYPE_CLARIFICATION
	case EventTypeMemoryFlush:
		return napv1.EventType_EVENT_TYPE_MEMORY_FLUSH
	case EventTypeScheduledTask:
		return napv1.EventType_EVENT_TYPE_SCHEDULED_TASK
	case EventTypeSessionSlug:
		return napv1.EventType_EVENT_TYPE_SESSION_SLUG
	case EventTypeOnboarding:
		return napv1.EventType_EVENT_TYPE_ONBOARDING
	default:
		return napv1.EventType_EVENT_TYPE_UNSPECIFIED
	}
}

// actionTypeFromProto converts proto enum to ActionType.
func actionTypeFromProto(t napv1.ActionType) ActionType {
	switch t {
	case napv1.ActionType_ACTION_TYPE_COMMAND:
		return ActionCommand
	case napv1.ActionType_ACTION_TYPE_SPEAK:
		return ActionSpeak
	case napv1.ActionType_ACTION_TYPE_CLARIFY:
		return ActionClarify
	case napv1.ActionType_ACTION_TYPE_NOOP:
		return ActionNoop
	case napv1.ActionType_ACTION_TYPE_TOOL_USE:
		return ActionToolUse
	default:
		return ActionNoop
	}
}

// timestampOrNil converts time.Time to protobuf timestamp, returning nil for zero time.
func timestampOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	ts := timestamppb.New(t)
	if err := ts.CheckValid(); err != nil {
		return nil
	}
	return ts
}

// ContextWithDialer attaches a custom dialer to the context.
// This is primarily for tests using bufconn without real network sockets.
func ContextWithDialer(ctx context.Context, dialer func(context.Context, string) (net.Conn, error)) context.Context {
	return napdial.ContextWithDialer(ctx, dialer)
}
