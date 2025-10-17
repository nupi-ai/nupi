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
	SessionID string                `json:"session_id"`
	Turns     []ConversationTurnDTO `json:"turns"`
}

// ToConversationState converts stored turns into DTO representation.
func ToConversationState(sessionID string, turns []eventbus.ConversationTurn) ConversationStateDTO {
	result := ConversationStateDTO{
		SessionID: sessionID,
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

	return result
}
