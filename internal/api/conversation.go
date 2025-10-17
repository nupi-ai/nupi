package api

import (
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

// ConversationTurnDTO exposes conversation history entry for API responses.
type ConversationTurnDTO struct {
	Origin string            `json:"origin"`
	Text   string            `json:"text"`
	At     time.Time         `json:"at"`
	Meta   map[string]string `json:"meta,omitempty"`
}

// ConversationStateDTO wraps conversation history for a session.
type ConversationStateDTO struct {
	SessionID  string                `json:"session_id"`
	Offset     int                   `json:"offset"`
	Limit      int                   `json:"limit"`
	Total      int                   `json:"total"`
	HasMore    bool                  `json:"has_more"`
	NextOffset *int                  `json:"next_offset,omitempty"`
	Turns      []ConversationTurnDTO `json:"turns"`
}

// ToConversationState converts stored turns into DTO representation.
func ToConversationState(sessionID string, total, offset, limit int, turns []eventbus.ConversationTurn) ConversationStateDTO {
	result := ConversationStateDTO{
		SessionID: sessionID,
		Offset:    offset,
		Limit:     limit,
		Total:     total,
		Turns:     make([]ConversationTurnDTO, len(turns)),
	}

	for i, turn := range turns {
		var meta map[string]string
		if len(turn.Meta) > 0 {
			meta = make(map[string]string, len(turn.Meta))
			for k, v := range turn.Meta {
				meta[k] = v
			}
		}

		result.Turns[i] = ConversationTurnDTO{
			Origin: string(turn.Origin),
			Text:   turn.Text,
			At:     turn.At,
			Meta:   meta,
		}
	}

	if limit <= 0 || limit > len(result.Turns) {
		result.Limit = len(result.Turns)
	}
	if result.Offset < 0 {
		result.Offset = 0
	}

	if result.Total < 0 {
		result.Total = len(result.Turns)
	}

	result.HasMore = result.Offset+len(result.Turns) < result.Total
	if result.HasMore {
		next := result.Offset + len(result.Turns)
		result.NextOffset = &next
	}

	return result
}
