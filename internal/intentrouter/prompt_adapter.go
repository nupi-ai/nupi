package intentrouter

import (
	"log"

	"github.com/nupi-ai/nupi/internal/prompts"
)

// PromptEngineAdapter adapts the prompts.Engine to the PromptEngine interface.
type PromptEngineAdapter struct {
	engine *prompts.Engine
}

// NewPromptEngineAdapter creates a new adapter wrapping a prompts.Engine.
func NewPromptEngineAdapter(engine *prompts.Engine) *PromptEngineAdapter {
	return &PromptEngineAdapter{engine: engine}
}

// Build generates prompts using the wrapped engine.
func (a *PromptEngineAdapter) Build(req PromptBuildRequest) (*PromptBuildResponse, error) {
	if a.engine == nil {
		return nil, nil
	}

	// Convert SessionInfo to prompts.SessionInfo
	sessions := make([]prompts.SessionInfo, len(req.AvailableSessions))
	for i, s := range req.AvailableSessions {
		sessions[i] = prompts.SessionInfo{
			ID:        s.ID,
			Command:   s.Command,
			Tool:      s.Tool,
			Status:    s.Status,
			StartTime: s.StartTime,
			Metadata:  s.Metadata,
		}
	}

	// Map event type
	var eventType prompts.EventType
	switch req.EventType {
	case EventTypeUserIntent:
		eventType = prompts.EventTypeUserIntent
	case EventTypeSessionOutput:
		eventType = prompts.EventTypeSessionOutput
	case EventTypeClarification:
		eventType = prompts.EventTypeClarification
	case EventTypeOnboarding:
		eventType = prompts.EventTypeOnboarding
	case EventTypeHeartbeat:
		eventType = prompts.EventTypeHeartbeat
	case EventTypeJournalCompaction:
		eventType = prompts.EventTypeJournalCompaction
	case EventTypeConversationCompaction:
		eventType = prompts.EventTypeConversationCompaction
	default:
		log.Printf("[PromptAdapter] unknown event type %q for session %q, defaulting to user_intent", req.EventType, req.SessionID)
		eventType = prompts.EventTypeUserIntent
	}

	buildReq := prompts.BuildRequest{
		EventType:             eventType,
		SessionID:             req.SessionID,
		Transcript:            req.Transcript,
		History:               req.History,
		AvailableSessions:     sessions,
		CurrentTool:           req.CurrentTool,
		SessionOutput:         req.SessionOutput,
		ClarificationQuestion: req.ClarificationQuestion,
		Metadata:              req.Metadata,
	}

	resp, err := a.engine.BuildPrompt(buildReq)
	if err != nil {
		return nil, err
	}

	return &PromptBuildResponse{
		SystemPrompt: resp.SystemPrompt,
		UserPrompt:   resp.UserPrompt,
		Context:      resp.Context,
	}, nil
}
