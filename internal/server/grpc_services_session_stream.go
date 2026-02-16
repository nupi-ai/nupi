package server

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/termresize"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// grpcStreamSender serializes all stream.Send() calls for a single gRPC
// bidirectional stream. gRPC ServerStream.Send() is NOT safe for concurrent
// use, and AttachSession has 3 concurrent callers (recv goroutine, main
// select loop, and session output pump via GRPCStreamSink.Write).
type grpcStreamSender struct {
	stream apiv1.SessionsService_AttachSessionServer
	mu     sync.Mutex
}

func (s *grpcStreamSender) Send(resp *apiv1.AttachSessionResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(resp)
}

// GRPCStreamSink implements pty.OutputSink for streaming session output over gRPC.
type GRPCStreamSink struct {
	sender  *grpcStreamSender
	utf8acc *grpcUTF8Accumulator
	mu      sync.Mutex
	closed  bool
}

var _ pty.OutputSink = (*GRPCStreamSink)(nil)

func newGRPCStreamSink(sender *grpcStreamSender) *GRPCStreamSink {
	return &GRPCStreamSink{
		sender:  sender,
		utf8acc: &grpcUTF8Accumulator{},
	}
}

func (s *GRPCStreamSink) Write(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}

	chunk := s.utf8acc.Take(data)
	if len(chunk) == 0 {
		return nil
	}

	return s.sender.Send(&apiv1.AttachSessionResponse{
		Payload: &apiv1.AttachSessionResponse_Output{
			Output: chunk,
		},
	})
}

func (s *GRPCStreamSink) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// grpcUTF8Accumulator buffers incomplete UTF-8 runes so that partial sequences
// are not sent to the client.
// Thread safety is provided by the caller (GRPCStreamSink.mu).
type grpcUTF8Accumulator struct {
	pending []byte
}

func (u *grpcUTF8Accumulator) Take(data []byte) []byte {
	if len(data) == 0 && len(u.pending) == 0 {
		return nil
	}

	buf := append(append([]byte{}, u.pending...), data...)
	if len(buf) == 0 {
		return nil
	}

	var out bytes.Buffer
	i := 0
	for i < len(buf) {
		r, size := utf8.DecodeRune(buf[i:])
		if r == utf8.RuneError && size == 1 && !utf8.FullRune(buf[i:]) {
			break
		}
		out.Write(buf[i : i+size])
		i += size
	}

	if i < len(buf) {
		u.pending = append(u.pending[:0], buf[i:]...)
	} else {
		u.pending = u.pending[:0]
	}

	return out.Bytes()
}

func (s *sessionsService) AttachSession(stream apiv1.SessionsService_AttachSessionServer) error {
	if _, err := s.api.requireRoleGRPC(stream.Context(), roleAdmin, roleReadOnly); err != nil {
		return err
	}

	// Wait for the init message first.
	initReq, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "expected init message: %v", err)
	}
	init := initReq.GetInit()
	if init == nil {
		return status.Error(codes.InvalidArgument, "first message must be AttachInit")
	}

	sessionID := strings.TrimSpace(init.GetSessionId())
	if sessionID == "" {
		return status.Error(codes.InvalidArgument, "session_id is required")
	}

	if s.api.sessionManager == nil {
		return status.Error(codes.Unavailable, "session manager unavailable")
	}

	if _, err := s.api.sessionManager.GetSession(sessionID); err != nil {
		return status.Errorf(codes.NotFound, "session %s not found", sessionID)
	}

	// Generate a unique client ID for this gRPC stream.
	clientID := fmt.Sprintf("grpc_%s", uuid.NewString())

	// Create serialized sender to protect concurrent stream.Send() calls.
	sender := &grpcStreamSender{stream: stream}

	// Create the output sink.
	sink := newGRPCStreamSink(sender)
	defer sink.Close()

	// Attach to session (with optional history replay).
	if err := s.api.sessionManager.AttachToSession(sessionID, sink, init.GetIncludeHistory()); err != nil {
		return status.Errorf(codes.Internal, "attach to session: %v", err)
	}
	defer func() {
		if err := s.api.sessionManager.DetachFromSession(sessionID, sink); err != nil {
			log.Printf("[gRPC] detach from session %s: %v", sessionID, err)
		}
	}()

	// Register client with resize manager.
	if s.api.resizeManager != nil {
		s.api.resizeManager.RegisterClient(sessionID, clientID, nil)
		defer s.api.resizeManager.UnregisterClient(sessionID, clientID)

		// Send initial resize state to the newly attached client.
		if state := s.api.resizeManager.Snapshot(sessionID); state != nil && state.Host != nil {
			_ = sender.Send(&apiv1.AttachSessionResponse{
				Payload: &apiv1.AttachSessionResponse_Event{
					Event: &apiv1.SessionEvent{
						Type:      apiv1.SessionEventType_SESSION_EVENT_TYPE_RESIZE_INSTRUCTION,
						SessionId: sessionID,
						Data: map[string]string{
							"cols":   strconv.Itoa(state.Host.Size.Cols),
							"rows":   strconv.Itoa(state.Host.Size.Rows),
							"reason": "host_lock_sync",
						},
					},
				},
			})
		}
	}

	// Subscribe to session lifecycle events.
	subSuffix := fmt.Sprintf("%s_%d", sessionID, time.Now().UnixNano())
	var lifecycleSub *eventbus.TypedSubscription[eventbus.SessionLifecycleEvent]
	if s.api.eventBus != nil {
		lifecycleSub = eventbus.Subscribe[eventbus.SessionLifecycleEvent](s.api.eventBus,
			eventbus.TopicSessionsLifecycle,
			eventbus.WithSubscriptionName("grpc_attach_lifecycle_"+subSuffix),
			eventbus.WithSubscriptionBuffer(16),
		)
		defer lifecycleSub.Close()
	}

	// Subscribe to session tool events.
	var toolSub *eventbus.TypedSubscription[eventbus.SessionToolChangedEvent]
	if s.api.eventBus != nil {
		toolSub = eventbus.Subscribe[eventbus.SessionToolChangedEvent](s.api.eventBus,
			eventbus.TopicSessionsToolChanged,
			eventbus.WithSubscriptionName("grpc_attach_tool_"+subSuffix),
			eventbus.WithSubscriptionBuffer(8),
		)
		defer toolSub.Close()
	}

	ctx := stream.Context()

	// Error channel for the recv goroutine.
	recvErr := make(chan error, 1)

	// Client→daemon goroutine: reads input/resize/control messages.
	go func() {
		defer close(recvErr)
		for {
			req, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				recvErr <- err
				return
			}

			switch payload := req.GetPayload().(type) {
			case *apiv1.AttachSessionRequest_Input:
				if len(payload.Input) > 0 {
					if writeErr := s.api.sessionManager.WriteToSession(sessionID, payload.Input); writeErr != nil {
						log.Printf("[gRPC] write to session %s: %v", sessionID, writeErr)
					}
				}

			case *apiv1.AttachSessionRequest_Resize:
				if payload.Resize != nil {
					s.handleStreamResize(sessionID, clientID, payload.Resize, sender)
				}

			case *apiv1.AttachSessionRequest_Control:
				control := strings.TrimSpace(strings.ToLower(payload.Control))
				switch control {
				case "detach":
					return
				case "kill":
					if killErr := s.api.sessionManager.KillSession(sessionID); killErr != nil {
						log.Printf("[gRPC] kill session %s: %v", sessionID, killErr)
					}
					return
				}
			}
		}
	}()

	// Channel variables for lifecycle/tool subscriptions. Set to nil when
	// the subscription's channel is closed to prevent CPU spin in the select loop.
	lifecycleCh := lifecycleSubChan(lifecycleSub)
	toolCh := toolSubChan(toolSub)

	// Main select loop: wait for context cancel, recv error, or lifecycle events.
	for {
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()

		case err, ok := <-recvErr:
			if !ok {
				// Recv goroutine finished (EOF or detach).
				return nil
			}
			return err

		case env, ok := <-lifecycleCh:
			if !ok {
				lifecycleCh = nil // disable this select case to prevent CPU spin
				continue
			}
			evt := env.Payload
			if evt.SessionID != sessionID {
				continue
			}
			eventType := sessionStateToEventType(evt.State)
			if eventType == apiv1.SessionEventType_SESSION_EVENT_TYPE_UNSPECIFIED {
				continue
			}
			data := map[string]string{}
			if evt.ExitCode != nil {
				data["exit_code"] = strconv.Itoa(*evt.ExitCode)
			}
			if evt.Reason != "" {
				data["reason"] = evt.Reason
			}
			_ = sender.Send(&apiv1.AttachSessionResponse{
				Payload: &apiv1.AttachSessionResponse_Event{
					Event: &apiv1.SessionEvent{
						Type:      eventType,
						SessionId: sessionID,
						Data:      data,
					},
				},
			})

			// If the session stopped, end the stream.
			if evt.State == eventbus.SessionStateStopped {
				return nil
			}

		case env, ok := <-toolCh:
			if !ok {
				toolCh = nil // disable this select case to prevent CPU spin
				continue
			}
			evt := env.Payload
			if evt.SessionID != sessionID {
				continue
			}
			_ = sender.Send(&apiv1.AttachSessionResponse{
				Payload: &apiv1.AttachSessionResponse_Event{
					Event: &apiv1.SessionEvent{
						Type:      apiv1.SessionEventType_SESSION_EVENT_TYPE_TOOL_DETECTED,
						SessionId: sessionID,
						Data: map[string]string{
							"previous_tool": evt.PreviousTool,
							"new_tool":      evt.NewTool,
						},
					},
				},
			})
		}
	}
}

func (s *sessionsService) handleStreamResize(sessionID, clientID string, resize *apiv1.ResizeRequest, sender *grpcStreamSender) {
	if s.api.resizeManager == nil {
		return
	}

	cols := int(resize.GetCols())
	rows := int(resize.GetRows())
	if cols <= 0 || rows <= 0 {
		return
	}

	source := termresize.SourceClient
	if resize.GetSource() == string(termresize.SourceHost) {
		source = termresize.SourceHost
	}

	meta := make(map[string]any, len(resize.GetMetadata()))
	for k, v := range resize.GetMetadata() {
		meta[k] = v
	}
	size := termresize.ViewportSize{Cols: cols, Rows: rows}

	var decision termresize.ResizeDecision
	var err error

	switch source {
	case termresize.SourceHost:
		decision, err = s.api.resizeManager.HandleHostResize(sessionID, size, meta)
	default:
		decision, err = s.api.resizeManager.HandleClientResize(sessionID, clientID, size, meta)
	}

	if err != nil {
		log.Printf("[gRPC] resize session %s: %v", sessionID, err)
		return
	}

	// Apply PTY resize.
	if decision.PTYSize != nil && s.api.sessionManager != nil {
		if err := s.api.sessionManager.ResizeSession(decision.SessionID, decision.PTYSize.Rows, decision.PTYSize.Cols); err != nil {
			log.Printf("[gRPC] apply PTY resize for session %s: %v", sessionID, err)
		}
	}

	// Send resize instructions as events.
	if len(decision.Broadcast) > 0 {
		for _, instruction := range decision.Broadcast {
			_ = sender.Send(&apiv1.AttachSessionResponse{
				Payload: &apiv1.AttachSessionResponse_Event{
					Event: &apiv1.SessionEvent{
						Type:      apiv1.SessionEventType_SESSION_EVENT_TYPE_RESIZE_INSTRUCTION,
						SessionId: sessionID,
						Data: map[string]string{
							"cols":   strconv.Itoa(instruction.Size.Cols),
							"rows":   strconv.Itoa(instruction.Size.Rows),
							"reason": instruction.Reason,
						},
					},
				},
			})
		}
	}
}

func sessionStateToEventType(state eventbus.SessionState) apiv1.SessionEventType {
	switch state {
	case eventbus.SessionStateCreated:
		return apiv1.SessionEventType_SESSION_EVENT_TYPE_CREATED
	case eventbus.SessionStateStopped:
		return apiv1.SessionEventType_SESSION_EVENT_TYPE_KILLED
	default:
		return apiv1.SessionEventType_SESSION_EVENT_TYPE_STATUS_CHANGED
	}
}

func lifecycleSubChan(sub *eventbus.TypedSubscription[eventbus.SessionLifecycleEvent]) <-chan eventbus.TypedEnvelope[eventbus.SessionLifecycleEvent] {
	if sub == nil {
		return nil
	}
	return sub.C()
}

func toolSubChan(sub *eventbus.TypedSubscription[eventbus.SessionToolChangedEvent]) <-chan eventbus.TypedEnvelope[eventbus.SessionToolChangedEvent] {
	if sub == nil {
		return nil
	}
	return sub.C()
}
